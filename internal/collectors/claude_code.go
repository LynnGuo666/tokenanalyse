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

const (
	claudeCodeSourceID = "claude-code"
	claudeCodeApp      = "claude"
	claudeChainSeed    = 256 * 1024
)

// ClaudeCodeCollector imports assistant usage records from Claude Code JSONL
// session transcripts. Cursors are byte offsets at complete-line boundaries.
type ClaudeCodeCollector struct{}

var _ Collector = (*ClaudeCodeCollector)(nil)

func NewClaudeCodeCollector() *ClaudeCodeCollector { return &ClaudeCodeCollector{} }

func (c *ClaudeCodeCollector) Info() Info {
	return Info{
		ID:          claudeCodeSourceID,
		Name:        "Claude Code",
		AppTypes:    []string{claudeCodeApp},
		Description: "Incremental Claude Code session JSONL collector",
		Incremental: true,
	}
}

type claudeChainNode struct {
	parent       string
	timestampMS  int64
	isAttachment bool
}

type claudeCandidate struct {
	messageID          string
	model              string
	usage              model.TokenUsage
	stopReason         string
	sessionID          string
	timestamp          time.Time
	filePath           string
	lineNumber         int
	requestRef         string
	parentUUID         string
	thinkingMS         uint64
	firstBlockType     string
	fallbackBoundaryMS int64
	latencyMS          uint64
	firstTokenMS       uint64
}

func (c *ClaudeCodeCollector) Collect(ctx Context, cursors []Cursor) (Result, error) {
	result := Result{Metadata: map[string]any{"source": claudeCodeSourceID}}
	root := ctx.Paths.ClaudeCode
	if root == "" {
		root = ctx.Env["CLAUDE_CONFIG_DIR"]
	}
	if root == "" {
		home := ctx.Home
		if home == "" {
			home = "."
		}
		root = filepath.Join(home, ".claude")
	}

	files := make([]string, 0)
	walkErr := filepath.WalkDir(filepath.Join(root, "projects"), func(path string, entry os.DirEntry, err error) error {
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
		if cursor.SourceID == claudeCodeSourceID {
			cursorByPath[filepath.Clean(cursor.FilePath)] = cursor
		}
	}

	// A message can be split over several assistant content-block records and
	// can also appear in more than one transcript. Select one representative
	// globally, preferring a completed record and then the largest output.
	candidates := map[string]claudeCandidate{}
	for _, path := range files {
		result.FilesScanned++
		cursor := cursorByPath[filepath.Clean(path)]
		fileCandidates, next, err := c.collectFile(path, cursor, &result)
		if err != nil {
			result.Errors = append(result.Errors, fmt.Sprintf("%s: %v", path, err))
		}
		for _, candidate := range fileCandidates {
			if existing, ok := candidates[candidate.messageID]; !ok || claudeCandidateBetter(candidate, existing) {
				candidates[candidate.messageID] = candidate
			}
		}
		if next != nil {
			result.Cursors = append(result.Cursors, *next)
		}
	}

	ids := make([]string, 0, len(candidates))
	for id := range candidates {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		candidate := candidates[id]
		if !candidate.usage.HasBillableTokens() {
			continue
		}
		result.Events = append(result.Events, c.buildEvent(ctx.DeviceID, candidate))
	}
	return result, nil
}

func (c *ClaudeCodeCollector) collectFile(path string, cursor Cursor, result *Result) ([]claudeCandidate, *Cursor, error) {
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
	}
	next := Cursor{
		SourceID:      claudeCodeSourceID,
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

	chain := map[string]claudeChainNode{}
	apiRequests := map[string]int64{}
	var lastBoundaryMS int64
	if offset > 0 {
		claudeReadChainSeed(file, offset, chain, apiRequests)
	}

	candidates := make([]claudeCandidate, 0)
	reader := bufio.NewReader(file)
	lineNumber := 0
	for {
		line, readErr := reader.ReadBytes('\n')
		if len(line) > 0 {
			lineNumber++
			complete := len(line) > 0 && line[len(line)-1] == '\n'
			if complete {
				next.Offset += int64(len(line))
			}
			if value, ok := claudeDecodeLine(line); ok {
				claudeRecordChain(value, chain, apiRequests, &lastBoundaryMS)
				if candidate, ok := claudeParseAssistant(value, path, lineNumber, info.ModTime()); ok {
					candidate.fallbackBoundaryMS = lastBoundaryMS
					candidates = append(candidates, candidate)
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

	// Timing needs the chain and api-request records, so resolve it only after
	// the whole new byte range has been read.
	for i := range candidates {
		claudeResolveTiming(&candidates[i], chain, apiRequests, lastBoundaryMS)
	}
	return candidates, &next, nil
}

func claudeReadChainSeed(file *os.File, offset int64, chain map[string]claudeChainNode, apiRequests map[string]int64) {
	start := offset - claudeChainSeed
	if start < 0 {
		start = 0
	}
	data := make([]byte, offset-start)
	if _, err := file.ReadAt(data, start); err != nil {
		return
	}
	if start > 0 {
		if newline := bytes.IndexByte(data, '\n'); newline >= 0 {
			data = data[newline+1:]
		} else {
			return
		}
	}
	var boundary int64
	for _, line := range bytes.Split(data, []byte{'\n'}) {
		if value, ok := claudeDecodeLine(line); ok {
			claudeRecordChain(value, chain, apiRequests, &boundary)
		}
	}
}

func claudeRecordChain(value map[string]any, chain map[string]claudeChainNode, apiRequests map[string]int64, lastBoundary *int64) {
	timestampMS := claudeTimeMS(value["timestamp"])
	kind := claudeString(value["type"])
	if kind == "api-request" {
		if id := claudeString(value["id"]); id != "" && timestampMS > 0 {
			apiRequests[id] = timestampMS
		}
		return
	}
	if uuid := claudeString(value["uuid"]); uuid != "" {
		chain[uuid] = claudeChainNode{
			parent:       claudeString(value["parentUuid"]),
			timestampMS:  timestampMS,
			isAttachment: kind == "attachment",
		}
	}
	if kind == "user" || kind == "system" {
		if timestampMS > 0 {
			*lastBoundary = timestampMS
		}
	}
}

func claudeParseAssistant(value map[string]any, path string, lineNumber int, fallback time.Time) (claudeCandidate, bool) {
	if claudeString(value["type"]) != "assistant" {
		return claudeCandidate{}, false
	}
	message, ok := value["message"].(map[string]any)
	if !ok {
		return claudeCandidate{}, false
	}
	messageID := claudeString(message["id"])
	if messageID == "" {
		return claudeCandidate{}, false
	}
	usage, ok := message["usage"].(map[string]any)
	if !ok {
		return claudeCandidate{}, false
	}
	modelName := claudeString(message["model"])
	if modelName == "" {
		modelName = "unknown"
	}
	cacheCreation := claudeUint(usage["cache_creation_input_tokens"])
	cacheCreationDetails, _ := usage["cache_creation"].(map[string]any)
	cache1h := claudeUint(cacheCreationDetails["ephemeral_1h_input_tokens"])
	cache5m := claudeUint(cacheCreationDetails["ephemeral_5m_input_tokens"])
	if cache1h > cacheCreation {
		cache1h = 0
	}
	if cache5m == 0 && cacheCreation > cache1h {
		cache5m = cacheCreation - cache1h
	}
	occurredAt := fallback
	if timestamp := claudeString(value["timestamp"]); timestamp != "" {
		if parsed, err := time.Parse(time.RFC3339Nano, timestamp); err == nil {
			occurredAt = parsed
		}
	}
	firstBlockType := ""
	if content, ok := message["content"].([]any); ok && len(content) > 0 {
		if block, ok := content[0].(map[string]any); ok {
			firstBlockType = claudeString(block["type"])
		}
	}
	return claudeCandidate{
		messageID: messageID,
		model:     modelName,
		usage: model.TokenUsage{
			InputTokens:           claudeUint(usage["input_tokens"]),
			OutputTokens:          claudeUint(usage["output_tokens"]),
			ReasoningTokens:       claudeReasoningTokens(usage),
			CacheReadTokens:       claudeUint(usage["cache_read_input_tokens"]),
			CacheCreationTokens:   cacheCreation,
			CacheCreation5MTokens: cache5m,
			CacheCreation1HTokens: cache1h,
			Model:                 modelName,
			MessageID:             messageID,
		},
		stopReason:     claudeString(message["stop_reason"]),
		sessionID:      claudeString(value["sessionId"]),
		timestamp:      occurredAt,
		filePath:       path,
		lineNumber:     lineNumber,
		requestRef:     claudeString(value["requestRef"]),
		parentUUID:     claudeString(value["parentUuid"]),
		thinkingMS:     claudeUint(value["thinkingDurationMs"]),
		firstBlockType: firstBlockType,
	}, true
}

func claudeCandidateBetter(candidate, existing claudeCandidate) bool {
	candidateComplete := candidate.stopReason != ""
	existingComplete := existing.stopReason != ""
	if candidateComplete != existingComplete {
		return candidateComplete
	}
	if candidate.usage.OutputTokens != existing.usage.OutputTokens {
		return candidate.usage.OutputTokens > existing.usage.OutputTokens
	}
	if candidate.timestamp.UnixNano() != existing.timestamp.UnixNano() {
		return candidate.timestamp.Before(existing.timestamp)
	}
	if candidate.filePath != existing.filePath {
		return candidate.filePath < existing.filePath
	}
	return candidate.lineNumber < existing.lineNumber
}

func claudeResolveTiming(candidate *claudeCandidate, chain map[string]claudeChainNode, apiRequests map[string]int64, _ int64) {
	if candidate.stopReason == "" || candidate.timestamp.IsZero() {
		return
	}
	endMS := candidate.timestamp.UnixMilli()
	startMS := int64(0)
	if sent := apiRequests[candidate.requestRef]; sent > 0 {
		startMS = sent
	}
	parent := candidate.parentUUID
	for hops := 0; hops < 32 && parent != ""; hops++ {
		node, ok := chain[parent]
		if !ok {
			break
		}
		if !node.isAttachment && node.timestampMS > 0 {
			if node.timestampMS > startMS {
				startMS = node.timestampMS
			}
			break
		}
		parent = node.parent
	}
	if startMS == 0 {
		startMS = candidate.fallbackBoundaryMS
	}
	latency := endMS - startMS
	if latency < 100 || latency > 60*60*1000 {
		return
	}
	candidate.latencyMS = uint64(latency)
	candidate.firstTokenMS = 0
	if candidate.firstBlockType == "thinking" && candidate.thinkingMS > 0 && candidate.thinkingMS < uint64(latency) {
		candidate.firstTokenMS = uint64(latency) - candidate.thinkingMS
	}
}

func (c *ClaudeCodeCollector) buildEvent(deviceID string, candidate claudeCandidate) model.UsageEvent {
	event := model.NewUsageEvent(deviceID, model.SourceSession, claudeCodeApp, candidate.messageID, candidate.model, candidate.usage, candidate.timestamp)
	event.RequestID = "claude_session:" + candidate.messageID
	event.SessionID = candidate.sessionID
	event.ProviderID = "anthropic"
	event.ProviderName = "Anthropic"
	event.RequestModel = candidate.model
	event.PricingModel = model.NormalizeModelID(candidate.model)
	event.InputTokenSemantics = model.SemanticsFresh
	event.IsStreaming = true
	status := 200
	event.StatusCode = &status
	if candidate.latencyMS > 0 {
		latency := candidate.latencyMS
		event.LatencyMS = &latency
		event.DurationMS = &latency
		generation := latency
		if candidate.firstTokenMS > 0 && candidate.firstTokenMS <= latency {
			first := candidate.firstTokenMS
			event.FirstTokenMS = &first
			generation = latency - first
		}
		event.GenerationDurationMS = &generation
		if generation > 0 && candidate.usage.OutputTokens > 0 {
			speed := float64(candidate.usage.OutputTokens) * 1000 / float64(generation)
			event.SpeedTPS = &speed
		}
		event.TimingQuality = model.TimingEstimated
	}
	event.Metadata["filePath"] = candidate.filePath
	event.Metadata["line"] = candidate.lineNumber
	event.Metadata["stopReason"] = candidate.stopReason
	return event
}

func claudeDecodeLine(line []byte) (map[string]any, bool) {
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

func claudeString(value any) string {
	text, _ := value.(string)
	return strings.TrimSpace(text)
}

func claudeUint(value any) uint64 {
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

func claudeReasoningTokens(usage map[string]any) uint64 {
	if reasoning := claudeUint(usage["reasoning_tokens"]); reasoning > 0 {
		return reasoning
	}
	if details, ok := usage["output_tokens_details"].(map[string]any); ok {
		return claudeUint(details["reasoning_tokens"])
	}
	return 0
}

func claudeTimeMS(value any) int64 {
	text := claudeString(value)
	if text == "" {
		return 0
	}
	parsed, err := time.Parse(time.RFC3339Nano, text)
	if err != nil {
		return 0
	}
	return parsed.UnixMilli()
}
