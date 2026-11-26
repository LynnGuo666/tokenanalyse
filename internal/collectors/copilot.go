package collectors

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"tokenanalyse/internal/model"
)

// CopilotCollector imports exact usage from GitHub Copilot CLI session events.
type CopilotCollector struct{}

func NewCopilotCollector() *CopilotCollector { return &CopilotCollector{} }

func (c *CopilotCollector) Info() Info {
	return Info{ID: "copilot", Name: "GitHub Copilot CLI", AppTypes: []string{"copilot"}, Description: "Import request usage from Copilot CLI session.usage_record events", Incremental: true}
}

func copilotDetail(usage map[string]any, tokenType string) (uint64, bool) {
	copilot := asObject(usage["copilotUsage"])
	details := asArray(copilot["tokenDetails"])
	for _, value := range details {
		detail := asObject(value)
		if objectText(detail, "tokenType") != tokenType {
			continue
		}
		if _, exists := detail["tokenCount"]; !exists {
			return 0, false
		}
		return uintValue(detail["tokenCount"]), true
	}
	return 0, false
}

func copilotUsage(record map[string]any) (model.TokenUsage, string, time.Time, uint64, *uint64, bool) {
	if objectText(record, "type") != "session.usage_record" {
		return model.TokenUsage{}, "", time.Time{}, 0, nil, false
	}
	data := asObject(record["data"])
	usage := asObject(data["usage"])
	cacheRead := uintValue(usage["cacheReadTokens"])
	if value, ok := copilotDetail(usage, "cache_read"); ok {
		cacheRead = value
	}
	cacheWrite := uintValue(usage["cacheWriteTokens"])
	if value, ok := copilotDetail(usage, "cache_write"); ok {
		cacheWrite = value
	}
	input := saturatingSub(uintValue(usage["inputTokens"]), cacheRead, cacheWrite)
	if value, ok := copilotDetail(usage, "input"); ok {
		input = value
	}
	output := uintValue(usage["outputTokens"])
	if value, ok := copilotDetail(usage, "output"); ok {
		output = value
	}
	thinking := maxUint(uintValue(usage["reasoningTokens"]), uintValue(usage["thinkingTokens"]), uintValue(usage["outputReasoningTokens"]),
		uintValue(nested(usage, "outputTokensDetails", "reasoning_tokens")), uintValue(nested(usage, "output_tokens_details", "reasoning_tokens")),
		uintValue(nested(usage, "completionTokensDetails", "reasoning_tokens")))
	// Copilot's output counter includes thinking. Keep the complete counter in
	// OutputTokens for totals/cost, and expose thinking as a separate breakdown.
	if thinking > output {
		thinking = output
	}

	modelName := strings.TrimSpace(text(usage["model"]))
	if modelName == "" {
		modelName = "unknown"
	}
	identity := objectText(record, "id")
	if identity == "" {
		if sequence := uintValue(nested(usage, "accounting", "sequence")); sequence > 0 {
			identity = fmt.Sprintf("seq%d", sequence)
		}
	}
	if identity == "" {
		encoded, err := json.Marshal(record)
		if err != nil || len(encoded) == 0 {
			return model.TokenUsage{}, "", time.Time{}, 0, nil, false
		}
		identity = model.DeterministicEventID(string(encoded))
	}
	occurredAt, ok := timestampMillis(record["timestamp"])
	if !ok {
		occurredAt, ok = timestampMillis(record["time"])
	}
	if !ok {
		occurredAt, ok = timestampMillis(record["createdAt"])
	}
	if !ok {
		occurredAt = time.UnixMilli(0).UTC()
	}
	normalized := model.TokenUsage{
		InputTokens:           input,
		OutputTokens:          output,
		ReasoningTokens:       thinking,
		CacheReadTokens:       cacheRead,
		CacheCreationTokens:   cacheWrite,
		CacheCreation1HTokens: 0,
		Model:                 modelName,
	}
	if !normalized.HasBillableTokens() {
		return model.TokenUsage{}, "", time.Time{}, 0, nil, false
	}
	var firstToken *uint64
	if rawDuration, exists := usage["timeToFirstTokenMs"]; exists && rawDuration != nil {
		switch value := rawDuration.(type) {
		case json.Number:
			if parsed, err := strconv.ParseFloat(value.String(), 64); err == nil && parsed > 0 && parsed <= 3_600_000 {
				duration := uint64(parsed + 0.5)
				firstToken = &duration
			}
		default:
			if duration, ok := kimiDuration(rawDuration); ok && duration > 0 {
				firstToken = &duration
			}
		}
	}
	var latency uint64
	if duration, ok := kimiDuration(usage["duration"]); ok {
		latency = duration
	}
	return normalized, identity, occurredAt, latency, firstToken, true
}

func copilotSelectedModel(record map[string]any) (string, bool) {
	data := asObject(record["data"])
	switch objectText(record, "type") {
	case "session.start", "session.resume":
		return objectText(data, "selectedModel"), true
	case "session.model_change":
		return objectText(data, "newModel"), true
	default:
		return "", false
	}
}

func (c *CopilotCollector) Collect(ctx Context, cursors []Cursor) (Result, error) {
	root := filepath.Join(ctx.Paths.Copilot, "session-state")
	sessions, err := os.ReadDir(root)
	if os.IsNotExist(err) {
		return Result{}, nil
	}
	if err != nil {
		return Result{}, err
	}
	sort.Slice(sessions, func(i, j int) bool { return sessions[i].Name() < sessions[j].Name() })
	indexed := cursorMap(cursors)
	result := Result{}
	for _, session := range sessions {
		if !session.IsDir() {
			continue
		}
		path := filepath.Join(root, session.Name(), "events.jsonl")
		stat, err := os.Stat(path)
		if os.IsNotExist(err) || err != nil || !stat.Mode().IsRegular() {
			continue
		}
		key := cursorKey(c.Info().ID, path)
		modified := stat.ModTime().UnixNano()
		if cursor, ok := indexed[key]; ok && cursor.ModifiedNanos == modified && cursor.Offset == stat.Size() {
			continue
		}
		cursor := indexed[key]
		selected := cursor.State
		if cached, ok := copilotSelectionCache[key]; ok {
			selected = cached
		}
		result.Merge(c.collectSession(ctx, path, session.Name(), key, incrementalOffset(cursor, cursorExists(indexed, key), stat.Size()), modified, selected))
	}
	return result, nil
}

func (c *CopilotCollector) collectSession(ctx Context, path, sessionID, key string, offset int64, modified int64, selected string) Result {
	records, next, read, damaged, incomplete, err := readJSONLines(path, offset)
	if err != nil {
		return Result{Errors: []string{fmt.Sprintf("GitHub Copilot CLI session %s: %v", path, err)}}
	}
	result := Result{FilesScanned: 1, BytesRead: read}
	if damaged > 0 {
		result.Errors = append(result.Errors, fmt.Sprintf("GitHub Copilot CLI session %s: skipped %d damaged record(s)", path, damaged))
	}

	seen := map[string]struct{}{}
	for _, record := range records {
		if modelName, ok := copilotSelectedModel(record); ok {
			selected = modelName
			continue
		}
		usage, identity, occurredAt, latency, firstToken, ok := copilotUsage(record)
		if !ok {
			continue
		}
		requestID := "copilot:" + sessionID + ":" + identity
		if _, duplicate := seen[requestID]; duplicate {
			continue
		}
		seen[requestID] = struct{}{}
		event := sessionEvent(ctx.DeviceID, c.Info().ID, requestID, requestID, sessionID, usage.Model, usage, occurredAt)
		event.LatencyMS = &latency
		event.FirstTokenMS = firstToken
		event.TimingQuality = model.TimingUnknown
		if latency > 0 {
			event.TimingQuality = model.TimingExact
		}
		status := 200
		event.StatusCode = &status
		event.ProviderID = "_copilot_session"
		if selected != "" {
			event.Metadata["selectedModel"] = selected
		}
		result.Events = append(result.Events, event)
	}
	copilotSelectionCache[key] = selected
	if !incomplete {
		stat, statErr := os.Stat(path)
		if statErr == nil {
			modified = stat.ModTime().UnixNano()
		}
	}
	result.Cursors = append(result.Cursors, Cursor{SourceID: c.Info().ID, FilePath: path, Offset: next, ModifiedNanos: modified, State: selected, UpdatedAt: time.Now().UTC()})
	return result
}

// Selection state is keyed by the same stable file path as the persisted cursor.
var copilotSelectionCache = map[string]string{}

var _ Collector = (*CopilotCollector)(nil)
