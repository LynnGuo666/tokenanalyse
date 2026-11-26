package collectors

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"tokenanalyse/internal/model"
)

const codexSourceID = "codex"

// CodexCollector imports token_count events from Codex rollout JSONL files.
// Cursor.State carries the cumulative counter baseline between incremental runs.
type CodexCollector struct{}

var _ Collector = (*CodexCollector)(nil)

func NewCodexCollector() *CodexCollector { return &CodexCollector{} }

func (c *CodexCollector) Info() Info {
	return Info{
		ID:          codexSourceID,
		Name:        "Codex",
		AppTypes:    []string{"codex"},
		Description: "Incremental Codex session JSONL collector",
		Incremental: true,
	}
}

type codexCounters struct {
	Input      uint64 `json:"inputTokens"`
	CacheRead  uint64 `json:"cacheReadTokens"`
	CacheWrite uint64 `json:"cacheWriteTokens"`
	Output     uint64 `json:"outputTokens"`
	Reasoning  uint64 `json:"reasoningTokens"`
}

type codexCursorState struct {
	Version        int           `json:"version"`
	HasBaseline    bool          `json:"hasBaseline"`
	Baseline       codexCounters `json:"baseline"`
	SeenUsage      bool          `json:"seenUsage"`
	Model          string        `json:"model,omitempty"`
	ModelRaw       string        `json:"modelRaw,omitempty"`
	SessionID      string        `json:"sessionId,omitempty"`
	LastSignature  string        `json:"lastSignature,omitempty"`
	LastBoundaryMS int64         `json:"lastBoundaryMs,omitempty"`
	RequestStartMS int64         `json:"requestStartMs,omitempty"`
	FirstOutputMS  int64         `json:"firstOutputMs,omitempty"`
	LineNumber     int64         `json:"lineNumber,omitempty"`
}

type codexTokenEvent struct {
	lineNumber  int
	usage       model.TokenUsage
	model       string
	modelRaw    string
	sessionID   string
	timestamp   time.Time
	usageSource string
	latencyMS   uint64
	firstMS     uint64
}

func (c *CodexCollector) Collect(ctx Context, cursors []Cursor) (Result, error) {
	result := Result{Metadata: map[string]any{"source": codexSourceID}}
	root := ctx.Paths.Codex
	if root == "" {
		root = ctx.Env["CODEX_HOME"]
	}
	if root == "" {
		home := ctx.Home
		if home == "" {
			home = "."
		}
		root = filepath.Join(home, ".codex")
	}

	files := make([]string, 0)
	walkErr := filepath.WalkDir(filepath.Join(root, "sessions"), func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			result.Errors = append(result.Errors, fmt.Sprintf("%s: %v", path, err))
			return nil
		}
		if entry.Type().IsRegular() && strings.EqualFold(filepath.Ext(entry.Name()), ".jsonl") {
			files = append(files, path)
		}
		return nil
	})
	if walkErr != nil {
		result.Errors = append(result.Errors, walkErr.Error())
	}
	sort.Strings(files)

	cursorByPath := make(map[string]Cursor, len(cursors))
	for _, cursor := range cursors {
		if cursor.SourceID == codexSourceID {
			cursorByPath[filepath.Clean(cursor.FilePath)] = cursor
		}
	}
	for _, path := range files {
		result.FilesScanned++
		cursor := cursorByPath[filepath.Clean(path)]
		events, next, err := c.collectFile(path, cursor, &result)
		if err != nil {
			result.Errors = append(result.Errors, fmt.Sprintf("%s: %v", path, err))
		}
		for _, tokenEvent := range events {
			result.Events = append(result.Events, c.buildEvent(ctx.DeviceID, path, tokenEvent))
		}
		if next != nil {
			result.Cursors = append(result.Cursors, *next)
		}
	}
	return result, nil
}

func (c *CodexCollector) collectFile(path string, cursor Cursor, result *Result) ([]codexTokenEvent, *Cursor, error) {
	state := codexCursorState{Version: 1}
	if cursor.State != "" {
		if err := json.Unmarshal([]byte(cursor.State), &state); err != nil {
			result.Errors = append(result.Errors, fmt.Sprintf("%s: invalid cursor state, rebuilding: %v", path, err))
			cursor.Offset = 0
			state = codexCursorState{Version: 1}
		}
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, nil, err
	}
	offset := cursor.Offset
	if offset < 0 || offset > info.Size() {
		offset = 0
		state = codexCursorState{Version: 1}
	}
	next := Cursor{
		SourceID:      codexSourceID,
		FilePath:      path,
		Offset:        offset,
		ModifiedNanos: info.ModTime().UnixNano(),
		UpdatedAt:     time.Now().UTC(),
	}
	if offset == info.Size() && info.ModTime().UnixNano() <= cursor.ModifiedNanos {
		next.State = cursor.State
		return nil, &next, nil
	}
	if _, err := file.Seek(offset, io.SeekStart); err != nil {
		return nil, &next, err
	}

	events := make([]codexTokenEvent, 0)
	reader := bufio.NewReader(file)
	lineNumber := state.LineNumber
	for {
		line, readErr := reader.ReadBytes('\n')
		if len(line) > 0 {
			currentLine := lineNumber + 1
			if line[len(line)-1] == '\n' {
				lineNumber = currentLine
				next.Offset += int64(len(line))
			}
			if value, ok := codexDecodeLine(line); ok {
				c.observeMetadata(value, &state)
				if tokenEvent, ok := c.parseTokenCount(value, path, int(currentLine), info.ModTime(), &state); ok {
					events = append(events, tokenEvent)
				}
			}
		}
		if readErr != nil {
			if readErr != io.EOF {
				result.Errors = append(result.Errors, fmt.Sprintf("%s: read after line %d: %v", path, lineNumber, readErr))
			}
			break
		}
	}
	state.LineNumber = lineNumber
	encoded, err := json.Marshal(state)
	if err != nil {
		return events, &next, err
	}
	next.State = string(encoded)
	return events, &next, nil
}

func (c *CodexCollector) observeMetadata(value map[string]any, state *codexCursorState) {
	timestampMS := codexTimeMS(value["timestamp"])
	switch codexString(value["type"]) {
	case "session_meta":
		if state.SessionID == "" {
			payload := codexObject(value["payload"])
			state.SessionID = codexString(payload["id"])
			if state.SessionID == "" {
				state.SessionID = codexString(payload["thread_id"])
			}
		}
	case "turn_context":
		payload := codexObject(value["payload"])
		if raw := codexString(payload["model"]); raw != "" {
			state.ModelRaw = raw
			state.Model = model.NormalizeModelID(raw)
		}
		if timestampMS > 0 {
			state.LastBoundaryMS = timestampMS
		}
	case "response_item":
		payload := codexObject(value["payload"])
		itemType := codexString(payload["type"])
		switch {
		case itemType == "message" && codexString(payload["role"]) == "user":
			state.LastBoundaryMS = codexMaxTime(state.LastBoundaryMS, timestampMS)
		case strings.HasSuffix(itemType, "_output"):
			state.LastBoundaryMS = codexMaxTime(state.LastBoundaryMS, timestampMS)
		case itemType == "message" || itemType == "reasoning" || strings.HasSuffix(itemType, "_call"):
			if state.RequestStartMS == 0 {
				state.RequestStartMS = state.LastBoundaryMS
			}
		}
	case "event_msg":
		payload := codexObject(value["payload"])
		switch codexString(payload["type"]) {
		case "task_started", "turn_aborted":
			state.LastBoundaryMS = codexMaxTime(state.LastBoundaryMS, timestampMS)
			state.RequestStartMS = 0
			state.FirstOutputMS = 0
		case "user_message":
			state.LastBoundaryMS = codexMaxTime(state.LastBoundaryMS, timestampMS)
		case "thread_settings_applied":
			settings := codexObject(payload["thread_settings"])
			if raw := codexString(settings["model"]); raw != "" {
				state.ModelRaw = raw
				state.Model = model.NormalizeModelID(raw)
			}
		case "item_completed":
			item := codexObject(payload["item"])
			if itemType := codexString(item["type"]); itemType == "Reasoning" || itemType == "AgentMessage" {
				if started := codexInt64(payload["started_at_ms"]); started > 0 && (state.FirstOutputMS == 0 || started < state.FirstOutputMS) {
					state.FirstOutputMS = started
				}
			}
		}
	}
}

func (c *CodexCollector) parseTokenCount(value map[string]any, path string, lineNumber int, fallback time.Time, state *codexCursorState) (codexTokenEvent, bool) {
	if codexString(value["type"]) != "event_msg" {
		return codexTokenEvent{}, false
	}
	payload := codexObject(value["payload"])
	if codexString(payload["type"]) != "token_count" {
		return codexTokenEvent{}, false
	}
	info := codexObject(payload["info"])
	total, hasTotal := codexParseCounters(info["total_token_usage"])
	last, hasLast := codexParseCounters(info["last_token_usage"])
	if !hasTotal && !hasLast {
		return codexTokenEvent{}, false
	}
	if raw := codexString(info["model"]); raw != "" {
		state.ModelRaw = raw
		state.Model = model.NormalizeModelID(raw)
	} else if raw := codexString(info["model_name"]); raw != "" {
		state.ModelRaw = raw
		state.Model = model.NormalizeModelID(raw)
	} else if raw := codexString(payload["model"]); raw != "" {
		state.ModelRaw = raw
		state.Model = model.NormalizeModelID(raw)
	}

	signature := fmt.Sprintf("%+v|%+v|%t|%t", total, last, hasTotal, hasLast)
	duplicate := hasTotal && signature == state.LastSignature
	state.LastSignature = signature

	var delta codexCounters
	usageSource := ""
	switch {
	case duplicate:
		return codexTokenEvent{}, false
	case hasLast && !codexCountersZero(last):
		delta = last
		usageSource = "last_token_usage"
	case hasLast:
		// An explicit zero last snapshot is not a request. Falling back to the
		// cumulative delta here would replay the preceding request, but its
		// cumulative snapshot still advances the baseline for later records.
		if hasTotal {
			codexUpdateBaseline(state, total)
		}
		return codexTokenEvent{}, false
	case hasTotal:
		if state.HasBaseline && codexCountersReset(state.Baseline, total) {
			state.Baseline = codexCounters{}
		}
		if state.HasBaseline {
			delta = codexCounterDelta(state.Baseline, total)
		} else if state.SeenUsage {
			// Exact per-request records were seen before the first cumulative
			// snapshot. Establish a baseline without billing history twice.
			delta = codexCounters{}
		} else {
			delta = total
		}
		usageSource = "total_token_usage"
	default:
		return codexTokenEvent{}, false
	}
	if hasTotal {
		codexUpdateBaseline(state, total)
	}
	if codexCountersZero(delta) {
		return codexTokenEvent{}, false
	}
	state.SeenUsage = true

	delta.CacheRead = codexMin(delta.CacheRead, delta.Input)
	delta.CacheWrite = codexMin(delta.CacheWrite, delta.Input-delta.CacheRead)
	occurredAt := fallback
	endMS := int64(0)
	if timestamp := codexString(value["timestamp"]); timestamp != "" {
		if parsed, err := time.Parse(time.RFC3339Nano, timestamp); err == nil {
			occurredAt = parsed
			endMS = parsed.UnixMilli()
		}
	}
	modelName := state.Model
	if modelName == "" {
		modelName = "unknown"
	}
	usage := model.TokenUsage{
		InputTokens:         delta.Input,
		OutputTokens:        delta.Output + delta.Reasoning,
		ReasoningTokens:     delta.Reasoning,
		CacheReadTokens:     delta.CacheRead,
		CacheCreationTokens: delta.CacheWrite,
		Model:               modelName,
	}
	event := codexTokenEvent{
		lineNumber:  lineNumber,
		usage:       usage,
		model:       modelName,
		modelRaw:    state.ModelRaw,
		sessionID:   state.SessionID,
		timestamp:   occurredAt,
		usageSource: usageSource,
	}
	startMS := state.RequestStartMS
	if startMS == 0 {
		startMS = state.LastBoundaryMS
	}
	if latency := endMS - startMS; latency >= 100 && latency <= 60*60*1000 {
		event.latencyMS = uint64(latency)
		if state.FirstOutputMS > startMS && state.FirstOutputMS <= endMS {
			event.firstMS = uint64(state.FirstOutputMS - startMS)
		}
	}
	state.LastBoundaryMS = codexMaxTime(state.LastBoundaryMS, endMS)
	state.RequestStartMS = 0
	state.FirstOutputMS = 0
	return event, true
}

func (c *CodexCollector) buildEvent(deviceID, path string, tokenEvent codexTokenEvent) model.UsageEvent {
	sourceRecordID := fmt.Sprintf("%s:%d", path, tokenEvent.lineNumber)
	event := model.NewUsageEvent(deviceID, model.SourceSession, "codex", sourceRecordID, tokenEvent.model, tokenEvent.usage, tokenEvent.timestamp)
	event.RequestID = "codex_session:" + sourceRecordID
	event.SessionID = tokenEvent.sessionID
	event.ProviderID = "openai"
	event.ProviderName = "OpenAI"
	event.ModelRaw = tokenEvent.modelRaw
	if event.ModelRaw == "" {
		event.ModelRaw = tokenEvent.model
	}
	event.CanonicalModel = tokenEvent.model
	event.RequestModel = event.ModelRaw
	event.PricingModel = tokenEvent.model
	event.InputTokenSemantics = model.SemanticsTotal
	event.IsStreaming = true
	status := 200
	event.StatusCode = &status
	if tokenEvent.latencyMS > 0 {
		latency := tokenEvent.latencyMS
		event.LatencyMS = &latency
		event.DurationMS = &latency
		generation := latency
		if tokenEvent.firstMS > 0 && tokenEvent.firstMS <= latency {
			first := tokenEvent.firstMS
			event.FirstTokenMS = &first
			generation = latency - first
		}
		event.GenerationDurationMS = &generation
		if generation > 0 && tokenEvent.usage.OutputTokens > 0 {
			speed := float64(tokenEvent.usage.OutputTokens) * 1000 / float64(generation)
			event.SpeedTPS = &speed
		}
		event.TimingQuality = model.TimingEstimated
	}
	event.Metadata["filePath"] = path
	event.Metadata["line"] = tokenEvent.lineNumber
	event.Metadata["usageSource"] = tokenEvent.usageSource
	return event
}

func codexParseCounters(value any) (codexCounters, bool) {
	object, ok := value.(map[string]any)
	if !ok {
		return codexCounters{}, false
	}
	recognized := false
	for _, field := range []string{
		"input_tokens", "output_tokens", "reasoning_output_tokens", "total_tokens",
		"cached_input_tokens", "cache_read_input_tokens", "cache_write_input_tokens", "cache_creation_input_tokens",
	} {
		if _, ok := object[field]; ok {
			recognized = true
			break
		}
	}
	if !recognized {
		return codexCounters{}, false
	}
	cacheRead := codexUint(object["cached_input_tokens"])
	if cacheRead == 0 {
		cacheRead = codexUint(object["cache_read_input_tokens"])
	}
	if cacheRead == 0 {
		cacheRead = codexUint(codexObject(object["input_tokens_details"])["cached_tokens"])
	}
	cacheWrite := codexUint(object["cache_write_input_tokens"])
	if cacheWrite == 0 {
		cacheWrite = codexUint(object["cache_creation_input_tokens"])
	}
	return codexCounters{
		Input:      codexUint(object["input_tokens"]),
		CacheRead:  cacheRead,
		CacheWrite: cacheWrite,
		Output:     codexUint(object["output_tokens"]),
		Reasoning:  codexUint(object["reasoning_output_tokens"]),
	}, true
}

func codexUpdateBaseline(state *codexCursorState, current codexCounters) {
	if !state.HasBaseline || codexCountersReset(state.Baseline, current) {
		state.Baseline = current
	} else {
		state.Baseline = codexCounterMax(state.Baseline, current)
	}
	state.HasBaseline = true
}

func codexCounterDelta(previous, current codexCounters) codexCounters {
	return codexCounters{
		Input:      codexSub(current.Input, previous.Input),
		CacheRead:  codexSub(current.CacheRead, previous.CacheRead),
		CacheWrite: codexSub(current.CacheWrite, previous.CacheWrite),
		Output:     codexSub(current.Output, previous.Output),
		Reasoning:  codexSub(current.Reasoning, previous.Reasoning),
	}
}

func codexCountersReset(previous, current codexCounters) bool {
	return current.Input < previous.Input || current.CacheRead < previous.CacheRead || current.CacheWrite < previous.CacheWrite || current.Output < previous.Output || current.Reasoning < previous.Reasoning
}

func codexCounterMax(a, b codexCounters) codexCounters {
	return codexCounters{
		Input:      codexMax(a.Input, b.Input),
		CacheRead:  codexMax(a.CacheRead, b.CacheRead),
		CacheWrite: codexMax(a.CacheWrite, b.CacheWrite),
		Output:     codexMax(a.Output, b.Output),
		Reasoning:  codexMax(a.Reasoning, b.Reasoning),
	}
}

func codexCountersZero(counters codexCounters) bool {
	return counters.Input == 0 && counters.CacheRead == 0 && counters.CacheWrite == 0 && counters.Output == 0 && counters.Reasoning == 0
}

func codexDecodeLine(line []byte) (map[string]any, bool) {
	line = bytes.TrimSpace(line)
	if len(line) == 0 {
		return nil, false
	}
	decoder := json.NewDecoder(bytes.NewReader(line))
	decoder.UseNumber()
	var value map[string]any
	if err := decoder.Decode(&value); err != nil {
		return nil, false
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return nil, false
	}
	return value, true
}

func codexObject(value any) map[string]any {
	object, _ := value.(map[string]any)
	return object
}

func codexString(value any) string {
	text, _ := value.(string)
	return strings.TrimSpace(text)
}

func codexUint(value any) uint64 {
	switch typed := value.(type) {
	case json.Number:
		parsed, err := strconv.ParseUint(typed.String(), 10, 64)
		if err == nil {
			return parsed
		}
	case string:
		parsed, err := strconv.ParseUint(strings.TrimSpace(typed), 10, 64)
		if err == nil {
			return parsed
		}
	case float64:
		if typed > 0 {
			return uint64(typed)
		}
	}
	return 0
}

func codexInt64(value any) int64 {
	switch typed := value.(type) {
	case json.Number:
		parsed, err := strconv.ParseInt(typed.String(), 10, 64)
		if err == nil {
			return parsed
		}
	case string:
		parsed, err := strconv.ParseInt(strings.TrimSpace(typed), 10, 64)
		if err == nil {
			return parsed
		}
	case float64:
		return int64(typed)
	}
	return 0
}

func codexTimeMS(value any) int64 {
	text := codexString(value)
	if text == "" {
		return 0
	}
	parsed, err := time.Parse(time.RFC3339Nano, text)
	if err != nil {
		return 0
	}
	return parsed.UnixMilli()
}

func codexSub(a, b uint64) uint64 {
	if b >= a {
		return 0
	}
	return a - b
}

func codexMin(a, b uint64) uint64 {
	if a < b {
		return a
	}
	return b
}

func codexMax(a, b uint64) uint64 {
	if a > b {
		return a
	}
	return b
}

func codexMaxTime(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
