package collectors

import (
	"fmt"
	"os"
	"strings"
	"time"

	"tokenanalyse/internal/model"
)

type buddyCollector struct {
	id          string
	name        string
	description string
}

func (c *buddyCollector) Info() Info {
	return Info{ID: c.id, Name: c.name, AppTypes: []string{c.id}, Description: c.description, Incremental: true}
}

func (c *buddyCollector) InfoWithDescription(description string) Info {
	info := c.Info()
	info.Description = description
	return info
}

type buddyUsage struct {
	model      string
	input      uint64
	output     uint64
	cacheRead  uint64
	cacheWrite uint64
	reasoning  uint64
}

func (u buddyUsage) empty() bool {
	return u.input == 0 && u.output == 0 && u.cacheRead == 0 && u.cacheWrite == 0
}

func buddyCacheRead(raw map[string]any) uint64 {
	return maxUint(uintValue(raw["prompt_cache_hit_tokens"]), uintValue(raw["cache_read_input_tokens"]),
		uintValue(raw["cached_tokens"]), uintValue(nested(raw, "prompt_tokens_details", "cached_tokens")),
		uintValue(nested(raw, "input_tokens_details", "cached_tokens")))
}

func buddyCacheWrite(raw map[string]any) uint64 {
	return maxUint(uintValue(raw["prompt_cache_write_tokens"]), uintValue(raw["cache_creation_input_tokens"]),
		uintValue(nested(raw, "input_tokens_details", "cache_write_tokens")))
}

func buddyReasoning(raw map[string]any) uint64 {
	return maxUint(uintValue(raw["completion_thinking_tokens"]), uintValue(raw["thinking_tokens"]),
		uintValue(nested(raw, "completion_tokens_details", "reasoning_tokens")),
		uintValue(nested(raw, "output_tokens_details", "reasoning_tokens")))
}

func buddyRawUsage(raw map[string]any) buddyUsage {
	cacheRead, cacheWrite := buddyCacheRead(raw), buddyCacheWrite(raw)
	return buddyUsage{
		input:      saturatingSub(uintValue(raw["prompt_tokens"]), cacheRead, cacheWrite),
		output:     uintValue(raw["completion_tokens"]),
		cacheRead:  cacheRead,
		cacheWrite: cacheWrite,
		reasoning:  buddyReasoning(raw),
	}
}

func detailTokenSum(details []any, key string) uint64 {
	var total uint64
	for _, value := range details {
		total = saturatingAdd(total, uintValue(asObject(value)[key]))
	}
	return total
}

func buddyNormalizedUsage(usage map[string]any) buddyUsage {
	cacheRead := detailTokenSum(asArray(usage["inputTokensDetails"]), "cached_tokens")
	return buddyUsage{
		input:      saturatingSub(uintValue(usage["inputTokens"]), cacheRead),
		output:     uintValue(usage["outputTokens"]),
		cacheRead:  cacheRead,
		cacheWrite: 0,
		reasoning:  detailTokenSum(asArray(usage["outputTokensDetails"]), "reasoning_tokens"),
	}
}

func buddyModel(provider map[string]any) string {
	for _, key := range []string{"requestModelId", "model"} {
		candidate := strings.TrimSpace(text(provider[key]))
		if candidate == "" {
			continue
		}
		if strings.HasPrefix(candidate, "custom-") {
			if colon := strings.IndexByte(candidate, ':'); colon > 0 && strings.TrimSpace(candidate[colon+1:]) != "" {
				candidate = strings.TrimSpace(candidate[colon+1:])
			}
		}
		return candidate
	}
	return "unknown"
}

func buddyRecordUsage(record map[string]any, onlyEndingEvents bool) (buddyUsage, map[string]any, bool) {
	provider := asObject(record["providerData"])
	if rawObject := asObject(provider["rawUsage"]); rawObject != nil {
		usage := buddyRawUsage(rawObject)
		usage.model = buddyModel(provider)
		return usage, provider, !usage.empty()
	}
	if !onlyEndingEvents || objectText(record, "type") != "message" || objectText(record, "role") != "assistant" {
		return buddyUsage{}, provider, false
	}
	usageObject := asObject(provider["usage"])
	if usageObject == nil {
		return buddyUsage{}, provider, false
	}
	usage := buddyNormalizedUsage(usageObject)
	return usage, provider, !usage.empty()
}

func (c *buddyCollector) Collect(ctx Context, cursors []Cursor) (Result, error) {
	root := c.root(ctx)
	files, err := findUsageFiles(root)
	if err != nil {
		return Result{}, err
	}
	return c.collectFiles(ctx, files, cursors), nil
}

func (c *buddyCollector) root(ctx Context) string {
	var root string
	switch c.id {
	case "codebuddy":
		root = ctx.Paths.CodeBuddy
	case "workbuddy":
		root = ctx.Paths.WorkBuddy
	}
	return root
}

func (c *buddyCollector) collectFiles(ctx Context, files []usageFile, cursors []Cursor) Result {
	indexed := cursorMap(cursors)
	result := Result{}
	for _, file := range files {
		stat, err := os.Stat(file.path)
		if os.IsNotExist(err) || err != nil || !stat.Mode().IsRegular() {
			continue
		}
		key := cursorKey(c.id, file.path)
		modified := stat.ModTime().UnixNano()
		cursor, hasCursor := indexed[key]
		if hasCursor && cursor.ModifiedNanos == modified && cursor.Offset == stat.Size() {
			continue
		}
		records, next, read, damaged, incomplete, readErr := readJSONLines(file.path, incrementalOffset(cursor, hasCursor, stat.Size()))
		partial := Result{FilesScanned: 1, BytesRead: read}
		if readErr != nil {
			partial.Errors = append(partial.Errors, fmt.Sprintf("%s session %s: %v", c.name, file.path, readErr))
			result.Merge(partial)
			continue
		}
		if damaged > 0 {
			partial.Errors = append(partial.Errors, fmt.Sprintf("%s session %s: skipped %d damaged record(s)", c.name, file.path, damaged))
		}
		if incomplete {
			// Do not memoize the mtime observed while the tail was incomplete; the
			// next collection must retry those bytes even if mtime granularity is coarse.
			modified = 0
		}
		partial.Merge(c.parseRecords(ctx, records, file, next, modified))
		result.Merge(partial)
	}
	return result
}

func (c *buddyCollector) parseRecords(ctx Context, records []map[string]any, file usageFile, next int64, modified int64) Result {
	identityKind := "record"
	if c.id == "codebuddy" {
		identityKind = "step"
	}
	result := Result{}
	seen := map[string]struct{}{}
	for _, record := range records {
		usage, provider, ok := buddyRecordUsage(record, true)
		if ok && c.id == "workbuddy" && usage.reasoning > usage.output {
			usage.reasoning = usage.output
		}
		if !ok {
			continue
		}
		ownSession := objectText(record, "sessionId")
		if ownSession == "" {
			ownSession = "unknown"
		}
		sessionID := ownSession
		if file.parentSession != "" {
			sessionID = file.parentSession
		}
		identitySession := ownSession
		if file.parentSession != "" {
			identitySession = file.parentSession
		}
		messageID := objectText(provider, "messageId")
		step := identityText(provider["stepSeq"])
		if step == "" {
			step = "0"
		}
		var sourceRecordID string
		switch {
		case messageID != "" && objectHas(provider, "stepSeq"):
			sourceRecordID = c.id + ":" + identitySession + ":" + messageID + ":" + step
		default:
			id := objectText(record, "id")
			if id == "" {
				// A usage record without an identity cannot be safely deduplicated.
				continue
			}
			sourceRecordID = c.id + ":" + identitySession + ":" + id
		}
		if _, duplicate := seen[sourceRecordID]; duplicate {
			continue
		}
		seen[sourceRecordID] = struct{}{}
		occurredAt, ok := timestampMillis(record["timestamp"])
		if !ok {
			continue
		}
		normalized := model.TokenUsage{
			InputTokens:         usage.input,
			OutputTokens:        usage.output,
			ReasoningTokens:     usage.reasoning,
			CacheReadTokens:     usage.cacheRead,
			CacheCreationTokens: usage.cacheWrite,
			Model:               usage.model,
		}
		modelName := usage.model
		if modelName == "" {
			modelName = "unknown"
		}
		normalized.Model = modelName
		event := sessionEvent(ctx.DeviceID, c.id, sourceRecordID, sourceRecordID, sessionID, modelName, normalized, occurredAt)
		status := 200
		event.StatusCode = &status
		event.Metadata["dataSource"] = c.id + "_session"
		event.Metadata["identity"] = identityKind
		if raw := asObject(asObject(record["providerData"])["rawUsage"]); raw != nil {
			if credit, exists := raw["credit"]; exists {
				event.Metadata["credit"] = credit
			}
		}
		result.Events = append(result.Events, event)
	}
	result.Cursors = append(result.Cursors, Cursor{SourceID: c.id, FilePath: file.path, Offset: next, ModifiedNanos: modified, UpdatedAt: time.Now().UTC()})
	return result
}
