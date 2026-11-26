package collectors

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"tokenanalyse/internal/model"
)

const (
	piSourceID              = "pi"
	piSourceApp             = "pi"
	piProviderPlaceholder   = "_pi_session"
	piFamilyMaxSessionBytes = 128 * 1024 * 1024
	piFamilyTailBytes       = 4096
)

type piFamilyFlavor struct {
	sourceID            string
	sourceApp           string
	name                string
	providerPlaceholder string
	allowModelUsage     bool
	titleSlot           bool
	subagentToolResults bool
}

var piFlavor = piFamilyFlavor{
	sourceID:            piSourceID,
	sourceApp:           piSourceApp,
	name:                "Pi",
	providerPlaceholder: piProviderPlaceholder,
}

// PiCollector imports usage from Pi's append-only session JSONL files.
type PiCollector struct{}

var _ Collector = PiCollector{}

func NewPiCollector() *PiCollector { return &PiCollector{} }

func (PiCollector) Info() Info {
	return Info{
		ID:          piSourceID,
		Name:        "Pi",
		AppTypes:    []string{piSourceApp},
		Description: "Incremental Pi session JSONL collector",
		Incremental: true,
	}
}

func (PiCollector) Collect(ctx Context, cursors []Cursor) (Result, error) {
	return collectPiFamily(ctx, cursors, piFlavor)
}

type piCursorState struct {
	Version             int    `json:"version"`
	SessionID           string `json:"sessionId,omitempty"`
	SessionTimestampMS  int64  `json:"sessionTimestampMs,omitempty"`
	HasSessionTimestamp bool   `json:"hasSessionTimestamp,omitempty"`
	LineNumber          int64  `json:"lineNumber,omitempty"`
	TailHash            string `json:"tailHash,omitempty"`
	Size                int64  `json:"size"`
}

func collectPiFamily(ctx Context, cursors []Cursor, flavor piFamilyFlavor) (Result, error) {
	result := Result{Metadata: map[string]any{"source": flavor.sourceID}}
	root := piFamilyRoot(ctx, flavor)
	if root == "" {
		return result, nil
	}
	files := discoverPiFamilyFiles(root, &result)
	rootsOverlap := piFamilyRootsOverlap(ctx)
	seenEvents := map[string]bool{}
	for _, path := range files {
		hasTitle := false
		if flavor.sourceID == piSourceID || rootsOverlap {
			hasTitle = piFileOpensWithTitle(path)
		}
		// A title slot is Oh My Pi's marker. Pi never owns those files, even
		// when one configured root contains the other agent's store.
		if flavor.sourceID == piSourceID && hasTitle {
			continue
		}
		if flavor.sourceID != piSourceID && rootsOverlap && !hasTitle {
			continue
		}
		result.FilesScanned++
		cursor, hasCursor := findCursor(cursors, flavor.sourceID, path)
		events, next, bytesRead, err := collectPiFamilyFile(ctx, path, flavor, cursor, hasCursor)
		result.BytesRead += bytesRead
		if err != nil {
			result.Errors = append(result.Errors, fmt.Sprintf("%s: %v", path, err))
			continue
		}
		for _, event := range events {
			if seenEvents[event.EventID] {
				continue
			}
			seenEvents[event.EventID] = true
			result.Events = append(result.Events, event)
		}
		if next != nil {
			result.Cursors = append(result.Cursors, *next)
		}
	}
	return result, nil
}

func piFamilyRoot(ctx Context, flavor piFamilyFlavor) string {
	if configured := strings.TrimSpace(ctx.Env["PI_CODING_AGENT_SESSION_DIR"]); configured != "" {
		return expandHomePath(configured, ctx.Home)
	}
	root := ""
	switch flavor.sourceID {
	case piSourceID:
		root = ctx.Paths.Pi
		if root == "" {
			root = ctx.Env["PI_AGENT_DIR"]
		}
	default:
		root = ctx.Paths.OMP
		if root == "" {
			root = ctx.Env["OMP_HOME"]
		}
	}
	if root == "" {
		home := ctx.Home
		if home == "" {
			home = "."
		}
		if flavor.sourceID == piSourceID {
			root = filepath.Join(home, ".pi")
		} else {
			root = filepath.Join(home, ".omp")
		}
	}
	return root
}

func expandHomePath(path, home string) string {
	if path == "~" {
		if home != "" {
			return home
		}
		return path
	}
	if strings.HasPrefix(path, "~/") || strings.HasPrefix(path, "~\\") {
		if home != "" {
			return filepath.Join(home, path[2:])
		}
	}
	return path
}

func piFamilyRootsOverlap(ctx Context) bool {
	piRoot := canonicalPiFamilyRoot(piFamilyRoot(ctx, piFlavor))
	ompRoot := canonicalPiFamilyRoot(piFamilyRoot(ctx, ompFlavor))
	if piRoot == "" || ompRoot == "" {
		return false
	}
	contains := func(parent, child string) bool {
		relative, err := filepath.Rel(parent, child)
		return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
	}
	return contains(piRoot, ompRoot) || contains(ompRoot, piRoot)
}

func canonicalPiFamilyRoot(path string) string {
	if path == "" {
		return ""
	}
	absolute, err := filepath.Abs(path)
	if err == nil {
		path = absolute
	}
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		path = resolved
	}
	return filepath.Clean(path)
}

func discoverPiFamilyFiles(root string, result *Result) []string {
	info, err := os.Stat(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		result.Errors = append(result.Errors, fmt.Sprintf("%s: %v", root, err))
		return nil
	}
	if info.Mode().IsRegular() {
		if strings.EqualFold(filepath.Ext(root), ".jsonl") {
			return []string{root}
		}
		return nil
	}
	files := make([]string, 0)
	walkErr := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			result.Errors = append(result.Errors, fmt.Sprintf("%s: %v", path, walkErr))
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
	return files
}

func piFileOpensWithTitle(path string) bool {
	file, err := os.Open(path)
	if err != nil {
		return false
	}
	defer file.Close()
	reader := bufio.NewReader(io.LimitReader(file, piFamilyTailBytes))
	line, err := reader.ReadBytes('\n')
	if err != nil && err != io.EOF {
		return false
	}
	value, ok := decodeJSONObject(bytes.TrimSpace(line))
	return ok && jsonString(value["type"]) == "title"
}

func collectPiFamilyFile(ctx Context, path string, flavor piFamilyFlavor, cursor Cursor, hasCursor bool) ([]model.UsageEvent, *Cursor, int64, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, nil, 0, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, nil, 0, err
	}
	if !info.Mode().IsRegular() || !strings.EqualFold(filepath.Ext(path), ".jsonl") {
		return nil, nil, 0, fmt.Errorf("not a regular JSONL file")
	}
	if info.Size() > piFamilyMaxSessionBytes {
		return nil, nil, 0, fmt.Errorf("session exceeds %d-byte safety limit", piFamilyMaxSessionBytes)
	}

	state := piCursorState{Version: 1}
	stateValid := false
	if hasCursor && cursor.State != "" {
		if err := json.Unmarshal([]byte(cursor.State), &state); err == nil && state.Version == 1 {
			stateValid = true
		}
	}
	offset := int64(0)
	resume := false
	if stateValid && cursor.Offset >= 0 && cursor.Offset <= info.Size() {
		tailHash, err := piFamilyTailHash(file, cursor.Offset)
		if err == nil && tailHash == state.TailHash {
			offset = cursor.Offset
			resume = true
		}
	}
	if !resume {
		state = piCursorState{Version: 1}
		offset = 0
	}
	if resume && state.Size == info.Size() {
		cursor.UpdatedAt = time.Now().UTC()
		return nil, &cursor, 0, nil
	}
	if _, err := file.Seek(offset, io.SeekStart); err != nil {
		return nil, nil, 0, err
	}

	fallbackSessionID := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	sessionID := state.SessionID
	if sessionID == "" {
		sessionID = fallbackSessionID
	}
	sessionTimestampMS := state.SessionTimestampMS
	hasSessionTimestamp := state.HasSessionTimestamp
	lineNumber := state.LineNumber
	nextOffset := offset
	var bytesRead int64
	events := make([]model.UsageEvent, 0)
	seen := map[string]bool{}
	reader := bufio.NewReader(&io.LimitedReader{R: file, N: info.Size() - offset})

	for {
		line, readErr := reader.ReadBytes('\n')
		if len(line) > 0 {
			bytesRead += int64(len(line))
			hasNewline := line[len(line)-1] == '\n'
			trimmed := bytes.TrimSpace(line)
			value, decoded := decodeJSONObject(trimmed)
			if !decoded && !hasNewline {
				break
			}
			if hasNewline || decoded {
				nextOffset += int64(len(line))
			}
			lineNumber++
			if decoded {
				switch jsonString(value["type"]) {
				case "session":
					if id := jsonString(value["id"]); id != "" {
						sessionID = id
					}
					if timestamp, ok := parsePiTimestampMillis(value["timestamp"]); ok {
						sessionTimestampMS = timestamp
						hasSessionTimestamp = true
					}
				case "title":
					// Oh My Pi reserves the first line for a fixed-width title slot.
				default:
					for _, event := range parsePiFamilyEntry(ctx, flavor, value, sessionID, sessionTimestampMS, hasSessionTimestamp, info.ModTime()) {
						if seen[event.EventID] {
							continue
						}
						seen[event.EventID] = true
						events = append(events, event)
					}
				}
			}
		}
		if readErr != nil {
			if readErr == io.EOF {
				break
			}
			return nil, nil, bytesRead, readErr
		}
	}

	state.SessionID = sessionID
	state.SessionTimestampMS = sessionTimestampMS
	state.HasSessionTimestamp = hasSessionTimestamp
	state.LineNumber = lineNumber
	state.Size = info.Size()
	state.TailHash, err = piFamilyTailHash(file, nextOffset)
	if err != nil {
		return nil, nil, bytesRead, err
	}
	encoded, err := json.Marshal(state)
	if err != nil {
		return nil, nil, bytesRead, err
	}
	next := Cursor{
		SourceID:      flavor.sourceID,
		FilePath:      path,
		Offset:        nextOffset,
		ModifiedNanos: info.ModTime().UnixNano(),
		State:         string(encoded),
		UpdatedAt:     time.Now().UTC(),
	}
	return events, &next, bytesRead, nil
}

func piFamilyTailHash(file *os.File, offset int64) (string, error) {
	start := offset - piFamilyTailBytes
	if start < 0 {
		start = 0
	}
	length := offset - start
	buffer := make([]byte, int(length))
	if length == 0 {
		return hashBytes(nil), nil
	}
	if _, err := file.ReadAt(buffer, start); err != nil {
		return "", err
	}
	return hashBytes(buffer), nil
}

type piUsageCarrier struct {
	entry      map[string]any
	usage      map[string]any
	carrier    map[string]any
	kind       string
	attributed bool
}

func parsePiFamilyEntry(ctx Context, flavor piFamilyFlavor, entry map[string]any, sessionID string, sessionTimestampMS int64, hasSessionTimestamp bool, fileTimestamp time.Time) []model.UsageEvent {
	if flavor.subagentToolResults {
		if events, recognized := parsePiSubagentEntry(ctx, flavor, entry, sessionID, sessionTimestampMS, hasSessionTimestamp, fileTimestamp); recognized {
			return events
		}
	}
	carrier, ok := parsePiUsageCarrier(flavor, entry)
	if !ok {
		return nil
	}
	event, ok := buildPiFamilyEvent(ctx, flavor, carrier, sessionID, sessionTimestampMS, hasSessionTimestamp, fileTimestamp)
	if !ok {
		return nil
	}
	return []model.UsageEvent{event}
}

func parsePiUsageCarrier(flavor piFamilyFlavor, entry map[string]any) (piUsageCarrier, bool) {
	entryType := jsonString(entry["type"])
	switch entryType {
	case "message":
		message, ok := jsonObject(entry["message"])
		if !ok {
			return piUsageCarrier{}, false
		}
		role := jsonString(message["role"])
		if role != "assistant" && role != "toolResult" {
			return piUsageCarrier{}, false
		}
		usage, ok := jsonObject(message["usage"])
		if !ok {
			return piUsageCarrier{}, false
		}
		return piUsageCarrier{
			entry:      entry,
			usage:      usage,
			carrier:    message,
			kind:       role,
			attributed: role == "assistant",
		}, true
	case "compaction", "branch_summary":
		usage, ok := jsonObject(entry["usage"])
		if !ok {
			return piUsageCarrier{}, false
		}
		return piUsageCarrier{entry: entry, usage: usage, kind: entryType}, true
	case "model_usage":
		if !flavor.allowModelUsage {
			return piUsageCarrier{}, false
		}
		usage, ok := jsonObject(entry["usage"])
		if !ok {
			return piUsageCarrier{}, false
		}
		return piUsageCarrier{entry: entry, usage: usage, carrier: entry, kind: entryType, attributed: true}, true
	default:
		return piUsageCarrier{}, false
	}
}

func buildPiFamilyEvent(ctx Context, flavor piFamilyFlavor, carrier piUsageCarrier, sessionID string, sessionTimestampMS int64, hasSessionTimestamp bool, fileTimestamp time.Time) (model.UsageEvent, bool) {
	usageValue := carrier.usage
	input := firstJSONUint64(usageValue, "input", "inputTokens", "promptTokens", "prompt_tokens")
	output := firstJSONUint64(usageValue, "output", "outputTokens", "completionTokens", "completion_tokens")
	reasoning := firstJSONUint64(usageValue, "reasoning", "reasoningTokens", "reasoning_tokens")
	if reasoning == 0 {
		if details, ok := jsonObject(usageValue["outputDetails"]); ok {
			reasoning = firstJSONUint64(details, "reasoningTokens", "reasoning_tokens", "reasoning")
		}
	}
	cacheRead := firstJSONUint64(usageValue, "cacheRead", "cacheReadTokens", "cache_read_input_tokens")
	cacheWrite := firstJSONUint64(usageValue, "cacheWrite", "cacheWriteTokens", "cache_creation_input_tokens")

	stopReason := ""
	if carrier.attributed {
		stopReason = jsonString(carrier.carrier["stopReason"])
	}
	failed := stopReason == "error" || stopReason == "aborted"
	if input == 0 && output == 0 && reasoning == 0 && cacheRead == 0 && cacheWrite == 0 && !failed {
		return model.UsageEvent{}, false
	}

	providerID := flavor.providerPlaceholder
	requestedModel := "unknown"
	actualModel := "unknown"
	if carrier.attributed {
		if provider := jsonString(carrier.carrier["provider"]); provider != "" {
			providerID = provider
		}
		if requested := jsonString(carrier.carrier["model"]); requested != "" {
			requestedModel = requested
		}
		actualModel = requestedModel
		if actual := jsonString(carrier.carrier["responseModel"]); actual != "" {
			actualModel = actual
		}
	}

	occurredAt := fileTimestamp
	if hasSessionTimestamp {
		occurredAt = time.UnixMilli(sessionTimestampMS).UTC()
	}
	entryTimestampMS, hasEntryTimestamp := parsePiTimestampMillis(carrier.entry["timestamp"])
	if !hasEntryTimestamp {
		entryTimestampMS, hasEntryTimestamp = parsePiTimestampMillis(carrier.carrier["timestamp"])
	}
	if hasEntryTimestamp {
		occurredAt = time.UnixMilli(entryTimestampMS).UTC()
	}

	entryID := jsonString(carrier.entry["id"])
	if entryID == "" {
		entryID = "sha256-" + hashBytes(canonicalJSON(carrier.entry))
	}
	if sessionID == "" {
		sessionID = strings.TrimSuffix(filepath.Base("session"), filepath.Ext("session"))
	}
	recordID := sessionID + ":" + entryID
	usage := model.TokenUsage{
		InputTokens:         input,
		OutputTokens:        addUint64Saturated(output, reasoning),
		ReasoningTokens:     reasoning,
		CacheReadTokens:     cacheRead,
		CacheCreationTokens: cacheWrite,
		Model:               actualModel,
		MessageID:           entryID,
	}
	event := newSessionUsageEvent(ctx, flavor.sourceApp, providerID, flavor.name+" (Session)", recordID, sessionID, actualModel, usage, occurredAt)
	event.ModelRaw = requestedModel
	event.RequestModel = requestedModel
	event.PricingModel = actualModel

	if carrier.attributed {
		if duration, ok := jsonFloat64(carrier.carrier["duration"]); ok && duration > 0 && duration <= 24*60*60*1000 {
			latency := uint64(math.Round(duration))
			event.LatencyMS = &latency
			event.TimingQuality = model.TimingExact
		} else if start, ok := parsePiTimestampMillis(carrier.carrier["timestamp"]); ok {
			if end, ok := parsePiTimestampMillis(carrier.entry["timestamp"]); ok && end > start && end-start <= 24*60*60*1000 {
				latency := uint64(end - start)
				event.LatencyMS = &latency
				event.TimingQuality = model.TimingEstimated
			}
		}
		if firstToken, ok := jsonFloat64(firstObjectValue(carrier.carrier, "ttft", "timeToFirstTokenMs")); ok && firstToken > 0 {
			first := uint64(math.Round(firstToken))
			if event.LatencyMS == nil || first <= *event.LatencyMS {
				event.FirstTokenMS = &first
			}
		}
	}
	if event.LatencyMS != nil {
		duration := *event.LatencyMS
		event.DurationMS = &duration
		if event.FirstTokenMS != nil && duration > *event.FirstTokenMS {
			generation := duration - *event.FirstTokenMS
			event.GenerationDurationMS = &generation
			if generation > 0 && usage.OutputTokens > 0 {
				speed := float64(usage.OutputTokens) * 1000 / float64(generation)
				event.SpeedTPS = &speed
			}
		}
	}

	if failed {
		status := 500
		fallback := flavor.name + " request failed"
		if stopReason == "aborted" {
			status = 499
			fallback = flavor.name + " request aborted"
		}
		event.StatusCode = &status
		event.ErrorMessage = jsonString(carrier.carrier["errorMessage"])
		if event.ErrorMessage == "" {
			event.ErrorMessage = fallback
		}
	}
	event.Metadata["recordType"] = carrier.kind
	if cost, exists := usageValue["cost"]; exists {
		event.Metadata["cost"] = cost
		applyReportedCost(&event, cost)
	}
	return event, true
}

func parsePiSubagentEntry(ctx Context, flavor piFamilyFlavor, entry map[string]any, sessionID string, sessionTimestampMS int64, hasSessionTimestamp bool, fileTimestamp time.Time) ([]model.UsageEvent, bool) {
	if jsonString(entry["type"]) != "message" {
		return nil, false
	}
	message, ok := jsonObject(entry["message"])
	if !ok || jsonString(message["role"]) != "toolResult" || jsonString(message["toolName"]) != "subagent" {
		return nil, false
	}
	details, ok := jsonObject(message["details"])
	if !ok {
		return nil, false
	}
	switch jsonString(details["mode"]) {
	case "single", "parallel", "chain":
	default:
		return nil, false
	}
	// pi-subagents links durable child session files. Those files are scanned
	// directly, so importing the projection as well would double count usage.
	if piDetailsContainsSessionFile(details) {
		return nil, true
	}
	if _, exists := message["usage"]; exists {
		return nil, false
	}
	results, ok := details["results"].([]any)
	if !ok {
		return nil, false
	}
	return parsePiSubagentResults(ctx, flavor, entry, results, sessionID, sessionTimestampMS, hasSessionTimestamp, fileTimestamp), true
}

func parsePiSubagentResults(ctx Context, flavor piFamilyFlavor, parent map[string]any, results []any, sessionID string, sessionTimestampMS int64, hasSessionTimestamp bool, fileTimestamp time.Time) []model.UsageEvent {
	parentID := jsonString(parent["id"])
	if parentID == "" {
		parentID = "sha256-" + hashBytes(canonicalJSON(parent))
	}
	events := make([]model.UsageEvent, 0)
	for resultIndex, rawResult := range results {
		result, ok := jsonObject(rawResult)
		if !ok {
			continue
		}
		hasRequestUsage := false
		if messages, ok := result["messages"].([]any); ok {
			for messageIndex, rawChild := range messages {
				child, ok := jsonObject(rawChild)
				if !ok || jsonString(child["role"]) != "assistant" {
					continue
				}
				childUsage, ok := jsonObject(child["usage"])
				if !ok {
					continue
				}
				hasRequestUsage = true
				synthetic := map[string]any{
					"type":      "message",
					"id":        fmt.Sprintf("%s:result:%d:message:%d", parentID, resultIndex, messageIndex),
					"timestamp": firstObjectValue(child, "timestamp"),
				}
				if synthetic["timestamp"] == nil {
					synthetic["timestamp"] = parent["timestamp"]
				}
				synthetic["message"] = child
				if event, ok := buildPiFamilyEvent(ctx, flavor, piUsageCarrier{entry: synthetic, usage: childUsage, carrier: child, kind: "assistant", attributed: true}, sessionID, sessionTimestampMS, hasSessionTimestamp, fileTimestamp); ok {
					events = append(events, event)
				}
			}
		}
		if hasRequestUsage {
			continue
		}
		usage, ok := jsonObject(result["usage"])
		if !ok {
			continue
		}
		usageCopy := make(map[string]any, len(usage)+1)
		for key, value := range usage {
			usageCopy[key] = value
		}
		if cost, exists := usageCopy["cost"]; exists {
			if _, ok := jsonObject(cost); !ok {
				usageCopy["cost"] = map[string]any{"total": cost}
			}
		}
		child := map[string]any{
			"role":          "assistant",
			"provider":      result["provider"],
			"model":         result["model"],
			"responseModel": result["responseModel"],
			"stopReason":    result["stopReason"],
			"errorMessage":  result["errorMessage"],
			"usage":         usageCopy,
		}
		synthetic := map[string]any{
			"type":      "message",
			"id":        fmt.Sprintf("%s:result:%d:aggregate", parentID, resultIndex),
			"timestamp": parent["timestamp"],
			"message":   child,
		}
		if event, ok := buildPiFamilyEvent(ctx, flavor, piUsageCarrier{entry: synthetic, usage: usageCopy, carrier: child, kind: "assistant", attributed: true}, sessionID, sessionTimestampMS, hasSessionTimestamp, fileTimestamp); ok {
			events = append(events, event)
		}
	}
	return events
}

func piDetailsContainsSessionFile(value any) bool {
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			if key == "sessionFile" && jsonString(child) != "" {
				return true
			}
			if piDetailsContainsSessionFile(child) {
				return true
			}
		}
	case []any:
		for _, child := range typed {
			if piDetailsContainsSessionFile(child) {
				return true
			}
		}
	}
	return false
}
