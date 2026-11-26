package collectors

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"tokenanalyse/internal/model"
)

// KimiCollector imports committed usage from each agent's wire.jsonl.
type KimiCollector struct{}

func NewKimiCollector() *KimiCollector { return &KimiCollector{} }

func (c *KimiCollector) Info() Info {
	return Info{ID: "kimi", Name: "Kimi Code", AppTypes: []string{"kimi"}, Description: "Import committed request usage from Kimi Code session wires", Incremental: true}
}

type kimiUsageKey struct {
	input, output, cacheRead, cacheCreation uint64
}

type kimiRequestInfo struct {
	model         string
	requestMS     int64
	latencyMS     uint64
	firstTokenMS  *uint64
	generationMS  *uint64
	timingQuality model.TimingQuality
}

type kimiStep struct {
	uuid          string
	turnStep      string
	model         string
	requestMS     int64
	hasRequestMS  bool
	lastContentMS int64
	hasContentMS  bool
	hasTools      bool
}

type kimiRequestIndex struct {
	agent    string
	step     *kimiStep
	messages map[string]kimiRequestInfo
}

func newKimiRequestIndex(agent string) *kimiRequestIndex {
	return &kimiRequestIndex{agent: agent, messages: map[string]kimiRequestInfo{}}
}

func kimiModelName(value any) string {
	raw := strings.TrimSpace(text(value))
	if raw == "" || raw == "agent-loop" || raw == "unknown" {
		return ""
	}
	return raw
}

func kimiDuration(value any) (uint64, bool) {
	if value == nil || uintValue(value) > 3_600_000 {
		return 0, false
	}
	return uintValue(value), true
}

func plausibleKimiLatency(start, end int64) (uint64, bool) {
	if start <= 0 || end < start {
		return 0, false
	}
	latency := end - start
	return uint64(latency), latency >= 100 && latency <= 60*60*1000
}

func (index *kimiRequestIndex) observe(record map[string]any) {
	own := index.agent
	if own == "" {
		own = "main"
	}
	if agent := objectText(record, "agentId"); agent != "" && agent != own {
		return
	}
	switch objectText(record, "type") {
	case "agent.switched", "agent.turn.started":
		index.step = nil
	case "llm.request":
		if objectText(record, "kind") != "loop" {
			return
		}
		step := index.step
		if step == nil {
			return
		}
		if step.turnStep != "" {
			if turnStep := objectText(record, "turnStep"); turnStep != "" && turnStep != step.turnStep {
				return
			}
		}
		// A retry supersedes the failed attempt and its metadata.
		step.model = kimiModelName(record["model"])
		if started, ok := timestampMillis(record["time"]); ok {
			step.requestMS = started.UnixMilli()
			step.hasRequestMS = true
		}
		step.lastContentMS, step.hasContentMS = 0, false
		step.hasTools = false
	case "context.append_loop_event":
		event := asObject(record["event"])
		switch objectText(event, "type") {
		case "step.begin":
			step := &kimiStep{uuid: objectText(event, "uuid")}
			if turn, stepNumber := objectText(event, "turnId"), identityText(event["step"]); turn != "" && stepNumber != "" {
				step.turnStep = turn + "." + stepNumber
			}
			index.step = step
		case "content.part", "tool.call":
			step := index.step
			if step == nil || objectText(event, "stepUuid") != step.uuid {
				return
			}
			if objectText(event, "type") == "tool.call" {
				step.hasTools = true
				return
			}
			if completed, ok := timestampMillis(record["time"]); ok {
				millis := completed.UnixMilli()
				if !step.hasContentMS || millis > step.lastContentMS {
					step.lastContentMS, step.hasContentMS = millis, true
				}
			}
		case "step.end":
			step := index.step
			if step != nil && objectText(event, "uuid") != step.uuid {
				return
			}
			if step == nil {
				step = &kimiStep{}
			}
			index.step = nil
			messageID := objectText(event, "messageId")
			if messageID == "" {
				return
			}
			first, hasFirst := kimiDuration(event["llmFirstTokenLatencyMs"])
			stream, hasStream := kimiDuration(event["llmStreamDurationMs"])
			var nativeLatency uint64
			if hasFirst && hasStream {
				nativeLatency = saturatingAdd(first, stream)
			}
			var latency uint64
			var quality model.TimingQuality
			completed, hasCompleted := timestampMillis(record["time"])
			if nativeLatency > 0 {
				latency, quality = nativeLatency, model.TimingExact
			} else {
				var endMS int64
				var hasEnd bool
				if step.hasContentMS {
					endMS, hasEnd = step.lastContentMS, true
				} else if !step.hasTools && hasCompleted {
					endMS, hasEnd = completed.UnixMilli(), true
				}
				if step.hasRequestMS && hasEnd {
					if estimated, ok := plausibleKimiLatency(step.requestMS, endMS); ok {
						latency, quality = estimated, model.TimingEstimated
					}
				}
			}
			if quality == "" {
				quality = model.TimingUnknown
			}
			info := index.messages[messageID]
			if step.model != "" {
				info.model = step.model
			}
			if step.hasRequestMS {
				info.requestMS = step.requestMS
			}
			if latency > 0 {
				info.latencyMS, info.timingQuality = latency, quality
				var firstToken *uint64
				if hasFirst && first <= latency {
					copy := first
					firstToken = &copy
				}
				info.firstTokenMS = firstToken
			}
			if nativeLatency > 0 && hasStream {
				generation := stream
				info.generationMS = &generation
			}
			index.messages[messageID] = info
		}
	}
}

func kimiUsage(usage map[string]any) (model.TokenUsage, kimiUsageKey, bool) {
	normalized := model.TokenUsage{
		InputTokens:         uintValue(usage["inputOther"]),
		OutputTokens:        uintValue(usage["output"]),
		CacheReadTokens:     uintValue(usage["inputCacheRead"]),
		CacheCreationTokens: uintValue(usage["inputCacheCreation"]),
	}
	if !normalized.HasBillableTokens() {
		return model.TokenUsage{}, kimiUsageKey{}, false
	}
	return normalized, kimiUsageKey{normalized.InputTokens, normalized.OutputTokens, normalized.CacheReadTokens, normalized.CacheCreationTokens}, true
}

func (c *KimiCollector) Collect(ctx Context, cursors []Cursor) (Result, error) {
	root := filepath.Join(ctx.Paths.Kimi, "sessions")
	workspaces, err := os.ReadDir(root)
	if os.IsNotExist(err) {
		return Result{}, nil
	}
	if err != nil {
		return Result{}, err
	}

	indexed := cursorMap(cursors)
	result := Result{}
	for _, workspace := range workspaces {
		if !workspace.IsDir() {
			continue
		}
		sessionDir := filepath.Join(root, workspace.Name())
		sessions, err := os.ReadDir(sessionDir)
		if err != nil {
			result.Errors = append(result.Errors, fmt.Sprintf("Kimi Code sessions %s: %v", sessionDir, err))
			continue
		}
		for _, session := range sessions {
			if !session.IsDir() {
				continue
			}
			agentsDir := filepath.Join(sessionDir, session.Name(), "agents")
			agents, err := os.ReadDir(agentsDir)
			if os.IsNotExist(err) {
				continue
			}
			if err != nil {
				result.Errors = append(result.Errors, fmt.Sprintf("Kimi Code agents %s: %v", agentsDir, err))
				continue
			}
			for _, agent := range agents {
				if !agent.IsDir() {
					continue
				}
				path := filepath.Join(agentsDir, agent.Name(), "wire.jsonl")
				stat, err := os.Stat(path)
				if os.IsNotExist(err) || err != nil {
					continue
				}
				if !stat.Mode().IsRegular() {
					continue
				}
				key := cursorKey(c.Info().ID, path)
				modified := stat.ModTime().UnixNano()
				if cursor, ok := indexed[key]; ok && cursor.ModifiedNanos == modified && cursor.Offset == stat.Size() {
					continue
				}
				partial := c.collectWire(ctx, path, agent.Name(), session.Name(), incrementalOffset(indexed[key], cursorExists(indexed, key), stat.Size()), modified)
				result.Merge(partial)
			}
		}
	}
	return result, nil
}

func cursorExists(cursors map[string]Cursor, key string) bool {
	_, ok := cursors[key]
	return ok
}

func (c *KimiCollector) collectWire(ctx Context, path, agent, sessionID string, offset int64, modified int64) Result {
	records, next, read, damaged, incomplete, err := readJSONLines(path, offset)
	if err != nil {
		return Result{Errors: []string{fmt.Sprintf("Kimi Code session %s: %v", path, err)}}
	}
	result := Result{FilesScanned: 1, BytesRead: read}
	if damaged > 0 {
		result.Errors = append(result.Errors, fmt.Sprintf("Kimi Code session %s: skipped %d damaged record(s)", path, damaged))
	}

	index := newKimiRequestIndex(agent)
	provider := "agent-loop"
	if agent != "main" {
		provider = "subagent"
	}
	usageModels := map[kimiUsageKey][]string{}
	seen := map[string]struct{}{}
	for _, record := range records {
		recordAgent := objectText(record, "agentId")
		if recordAgent != "" && recordAgent != agent {
			continue
		}
		switch objectText(record, "type") {
		case "agent.switched", "agent.turn.started":
			usageModels = map[kimiUsageKey][]string{}
		case "context.append_loop_event":
			if objectText(asObject(record["event"]), "type") == "step.begin" {
				usageModels = map[kimiUsageKey][]string{}
			}
		case "usage.record":
			if _, usageKey, ok := kimiUsage(asObject(record["usage"])); ok {
				modelName := objectText(record, "model")
				usageModels[usageKey] = append(usageModels[usageKey], modelName)
			}
		}

		index.observe(record)
		var stepModel string
		var stepHasModel bool
		var stepUsageKey kimiUsageKey
		var stepHasUsageKey bool
		if objectText(record, "type") == "context.append_loop_event" {
			event := asObject(record["event"])
			if objectText(event, "type") == "step.end" {
				if _, key, ok := kimiUsage(asObject(event["usage"])); ok {
					stepUsageKey, stepHasUsageKey = key, true
				}
			}
		}
		legacy := agent == "main" && (objectText(record, "type") == "agent.message.appended" || objectText(record, "type") == "human.agent.message.appended")
		message := asObject(record["message"])
		innerMessage := asObject(message["message"])
		meta := asObject(message["meta"])
		var usage map[string]any
		var messageID string
		var modelObject map[string]any
		if legacy {
			if objectText(innerMessage, "role") != "assistant" {
				continue
			}
			usage = asObject(meta["usage"])
			messageID = objectText(meta, "messageId")
			modelObject = asObject(meta["model"])
		} else {
			event := asObject(record["event"])
			if objectText(record, "type") != "context.append_loop_event" || objectText(event, "type") != "step.end" {
				continue
			}
			usage = asObject(event["usage"])
			messageID = objectText(event, "messageId")
			// Every boundary discards unmatched usage, even if this step has no usage.
			if stepHasUsageKey {
				queue := usageModels[stepUsageKey]
				if len(queue) > 0 {
					stepModel = queue[0]
					stepHasModel = stepModel != ""
					if len(queue) == 1 {
						delete(usageModels, stepUsageKey)
					} else {
						usageModels[stepUsageKey] = queue[1:]
					}
				}
			}
			usageModels = map[kimiUsageKey][]string{}
		}
		normalized, _, ok := kimiUsage(usage)
		if !ok {
			continue
		}
		committed, ok := timestampMillis(record["time"])
		if !ok {
			continue
		}
		info := index.messages[messageID]
		modelName := "unknown"
		if info.model != "" {
			modelName = info.model
		} else if stepHasModel {
			modelName = stepModel
		} else if candidate := kimiModelName(modelObject["model"]); candidate != "" {
			modelName = candidate
		}
		if messageID == "" {
			encoded, encodeErr := json.Marshal(record)
			if encodeErr != nil {
				continue
			}
			messageID = model.DeterministicEventID(path, string(encoded))
		}
		occurredAt := committed
		if info.requestMS > 0 {
			end := info.requestMS + int64(info.latencyMS)
			if info.latencyMS == 0 || end > committed.UnixMilli() {
				end = committed.UnixMilli()
			}
			occurredAt = time.UnixMilli(end).UTC()
		}
		requestID := "kimi:" + provider + ":" + messageID
		if _, duplicate := seen[requestID]; duplicate {
			continue
		}
		seen[requestID] = struct{}{}
		event := sessionEvent(ctx.DeviceID, c.Info().ID, requestID, requestID, sessionID, modelName, normalized, occurredAt)
		event.Metadata = map[string]any{"dataSource": "kimi_session", "usageScope": objectText(record, "usageScope")}
		event.LatencyMS = &info.latencyMS
		event.FirstTokenMS = info.firstTokenMS
		event.GenerationDurationMS = info.generationMS
		event.TimingQuality = info.timingQuality
		result.Events = append(result.Events, event)
	}
	if !incomplete {
		if stat, statErr := os.Stat(path); statErr == nil {
			modified = stat.ModTime().UnixNano()
		}
	}
	result.Cursors = append(result.Cursors, Cursor{SourceID: c.Info().ID, FilePath: path, Offset: next, ModifiedNanos: modified, UpdatedAt: time.Now().UTC()})
	return result
}

var _ Collector = (*KimiCollector)(nil)
