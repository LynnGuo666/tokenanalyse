package collectors

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"math/big"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"tokenanalyse/internal/model"

	"github.com/shopspring/decimal"
)

const (
	grokBuildSourceID  = "grok-build"
	grokBuildSourceApp = "grokbuild"
	grokBuildMaxLine   = 8 * 1024 * 1024
	grokBuildMaxDepth  = 16
)

// GrokBuildCollector reads per-turn usage from Grok Build updates.jsonl files.
type GrokBuildCollector struct{}

var _ Collector = (*GrokBuildCollector)(nil)

func NewGrokBuildCollector() *GrokBuildCollector { return &GrokBuildCollector{} }

func (c *GrokBuildCollector) Info() Info {
	return Info{
		ID:          grokBuildSourceID,
		Name:        "Grok Build",
		AppTypes:    []string{grokBuildSourceApp},
		Description: "Read per-turn token usage from Grok Build session updates",
		Incremental: true,
	}
}

type grokBuildCounters struct {
	Input         uint64
	Output        uint64
	Cached        uint64
	APIDurationMS uint64
	ModelCalls    uint64
	CostTicks     uint64
	CostIsPartial bool
}

type grokBuildUsageEvent struct {
	OccurredAt    time.Time
	PromptID      string
	CostPartial   bool
	ModelCounters map[string]grokBuildCounters
}

func (c *GrokBuildCollector) Collect(ctx Context, cursors []Cursor) (Result, error) {
	result := Result{}
	if strings.TrimSpace(ctx.Paths.Grok) == "" {
		return result, nil
	}

	cursorByPath := make(map[string]Cursor, len(cursors))
	for _, cursor := range cursors {
		if cursor.SourceID == grokBuildSourceID {
			cursorByPath[cursor.FilePath] = cursor
		}
	}

	for _, root := range []string{"sessions", "archived_sessions"} {
		paths := grokBuildUpdatesFiles(filepath.Join(ctx.Paths.Grok, root), &result)
		for _, path := range paths {
			result.FilesScanned++
			partial, err := c.collectFile(ctx, path, cursorByPath[path])
			if err != nil {
				result.Errors = append(result.Errors, fmt.Sprintf("Grok Build session %s: %v", path, err))
				continue
			}
			result.Merge(partial)
		}
	}
	return result, nil
}

func grokBuildUpdatesFiles(root string, result *Result) []string {
	var files []string
	_ = filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			if !os.IsNotExist(walkErr) {
				result.Errors = append(result.Errors, fmt.Sprintf("Grok Build scan %s: %v", path, walkErr))
			}
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return nil
		}
		depth := 0
		if relative != "." {
			depth = strings.Count(relative, string(filepath.Separator)) + 1
		}
		if entry.IsDir() {
			if depth > grokBuildMaxDepth {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.Name() == "updates.jsonl" {
			files = append(files, path)
		}
		return nil
	})
	sort.Strings(files)
	return files
}

func (c *GrokBuildCollector) collectFile(ctx Context, path string, existing Cursor) (Result, error) {
	result := Result{}
	info, err := os.Stat(path)
	if err != nil {
		return result, err
	}
	if !info.Mode().IsRegular() {
		return result, fmt.Errorf("not a regular file")
	}
	modified := info.ModTime().UnixNano()
	size := info.Size()
	hasCursor := existing.FilePath != ""
	if hasCursor && modified <= existing.ModifiedNanos && size <= existing.Offset {
		result.Cursors = append(result.Cursors, existing)
		return result, nil
	}

	start := int64(0)
	if hasCursor && existing.Offset >= 0 {
		start = existing.Offset
	}
	if start > size {
		// The file was truncated or replaced. Stable IDs make a replay safe;
		// resetting also lets newly appended turns be discovered immediately.
		start = 0
	}
	file, err := os.Open(path)
	if err != nil {
		return result, err
	}
	defer file.Close()
	if _, err := file.Seek(start, io.SeekStart); err != nil {
		return result, err
	}

	reader := bufio.NewReader(file)
	if start > 0 {
		aligned, err := grokBuildAlignToLine(file, reader, start)
		if err != nil {
			return result, err
		}
		if aligned > start {
			start = aligned
			result.BytesRead += aligned - existing.Offset
		}
	}

	sessionID := filepath.Base(filepath.Dir(path))
	committed := start
	usageEventIndex := int64(0)
	incomplete := false
	for {
		lineStart := committed
		line, readErr := grokBuildReadLine(reader)
		result.BytesRead += line.consumed
		if readErr != nil {
			result.Errors = append(result.Errors, fmt.Sprintf("Grok Build read %s at offset %d: %v", path, lineStart, readErr))
			incomplete = true
			break
		}
		if !line.terminated {
			// Do not advance past a partial final record. A later append will
			// complete it, and the unchanged mtime is deliberately not saved.
			incomplete = true
			break
		}
		committed += line.consumed
		if line.oversized || len(bytes.TrimSpace(line.data)) == 0 {
			continue
		}
		record, ok := grokBuildDecodeObject(line.data)
		if !ok {
			continue
		}
		event, ok := grokBuildParseUsageEvent(record)
		if !ok {
			continue
		}

		usageEventIndex++
		models := make([]string, 0, len(event.ModelCounters))
		for modelName := range event.ModelCounters {
			models = append(models, modelName)
		}
		sort.Strings(models)
		for _, modelName := range models {
			counters := event.ModelCounters[modelName]
			if counters.Input == 0 && counters.Output == 0 && counters.Cached == 0 {
				continue
			}
			turnKey := event.PromptID
			if turnKey == "" {
				if start == 0 {
					turnKey = fmt.Sprintf("idx%d", usageEventIndex-1)
				} else {
					turnKey = fmt.Sprintf("off%d", lineStart)
				}
			}
			recordID := sessionID + ":" + turnKey + ":" + modelName
			usage := model.TokenUsage{
				InputTokens:           counters.Input,
				OutputTokens:          counters.Output,
				CacheReadTokens:       counters.Cached,
				CacheCreationTokens:   0,
				CacheCreation5MTokens: 0,
				CacheCreation1HTokens: 0,
				Model:                 modelName,
			}
			usageEvent := model.NewUsageEvent(ctx.DeviceID, model.SourceSession, grokBuildSourceApp, recordID, modelName, usage, event.OccurredAt)
			usageEvent.RequestID = "grok_session:" + recordID
			usageEvent.SessionID = sessionID
			usageEvent.ProviderID = "_grok_session"
			usageEvent.ProviderName = "Grok Build"
			usageEvent.RequestModel = modelName
			usageEvent.PricingModel = modelName
			usageEvent.InputTokenSemantics = model.SemanticsTotal
			usageEvent.TimingQuality = model.TimingUnknown
			usageEvent.IsStreaming = true
			statusCode := 200
			usageEvent.StatusCode = &statusCode
			if counters.APIDurationMS > 0 {
				latency := counters.APIDurationMS
				usageEvent.LatencyMS = &latency
			}
			costPartial := event.CostPartial || counters.CostIsPartial
			usageEvent.Metadata["dataSource"] = "grok_session"
			usageEvent.Metadata["providerType"] = "grok_session"
			usageEvent.Metadata["modelCalls"] = counters.ModelCalls
			usageEvent.Metadata["costIsPartial"] = costPartial
			if counters.CostTicks > 0 {
				usageEvent.Metadata["costUsdTicks"] = strconv.FormatUint(counters.CostTicks, 10)
				ticks := decimal.NewFromBigInt(new(big.Int).SetUint64(counters.CostTicks), 0)
				usageEvent.Cost.TotalCostUSD = ticks.Div(decimal.NewFromInt(10_000_000_000)).String()
			}
			result.Events = append(result.Events, usageEvent)
		}
	}

	cursorModified := modified
	if incomplete && hasCursor {
		// Retry the partial tail even on filesystems with coarse mtimes.
		cursorModified = existing.ModifiedNanos
	}
	result.Cursors = append(result.Cursors, Cursor{
		SourceID:      grokBuildSourceID,
		FilePath:      path,
		Offset:        committed,
		ModifiedNanos: cursorModified,
		UpdatedAt:     time.Now().UTC(),
	})
	return result, nil
}

// grokBuildAlignToLine recovers from a cursor that points into a record. It
// returns the offset immediately after the next newline, or EOF if none exists.
func grokBuildAlignToLine(file *os.File, reader *bufio.Reader, offset int64) (int64, error) {
	var previous [1]byte
	if _, err := file.ReadAt(previous[:], offset-1); err != nil {
		return offset, err
	}
	if previous[0] == '\n' {
		return offset, nil
	}
	aligned := offset
	for {
		fragment, err := reader.ReadSlice('\n')
		aligned += int64(len(fragment))
		if err == nil {
			return aligned, nil
		}
		if err == bufio.ErrBufferFull {
			continue
		}
		if err == io.EOF {
			return aligned, nil
		}
		return aligned, err
	}
}

type grokBuildLine struct {
	data       []byte
	consumed   int64
	terminated bool
	oversized  bool
}

// grokBuildReadLine reads at most grokBuildMaxLine bytes into memory. Oversized
// tool-result lines are drained and reported so later usage records survive.
func grokBuildReadLine(reader *bufio.Reader) (grokBuildLine, error) {
	line := grokBuildLine{}
	for {
		fragment, err := reader.ReadSlice('\n')
		line.consumed += int64(len(fragment))
		if line.consumed <= grokBuildMaxLine {
			line.data = append(line.data, fragment...)
		} else {
			line.oversized = true
			line.data = line.data[:0]
		}
		if err == nil {
			line.terminated = true
			return line, nil
		}
		if err == bufio.ErrBufferFull {
			continue
		}
		if err == io.EOF {
			return line, nil
		}
		return line, err
	}
}

func grokBuildDecodeObject(data []byte) (map[string]any, bool) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var object map[string]any
	if err := decoder.Decode(&object); err != nil || object == nil {
		return nil, false
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, false
	}
	return object, true
}

func grokBuildParseUsageEvent(record map[string]any) (grokBuildUsageEvent, bool) {
	if grokBuildText(record["method"]) != "_x.ai/session/update" {
		return grokBuildUsageEvent{}, false
	}
	params, _ := record["params"].(map[string]any)
	update, _ := params["update"].(map[string]any)
	if update == nil {
		return grokBuildUsageEvent{}, false
	}
	if kind, exists := update["sessionUpdate"]; exists && grokBuildText(kind) != "turn_completed" {
		return grokBuildUsageEvent{}, false
	}
	usage, _ := update["usage"].(map[string]any)
	if usage == nil {
		return grokBuildUsageEvent{}, false
	}
	occurredAt, ok := grokBuildTimestamp(record["timestamp"])
	if !ok {
		return grokBuildUsageEvent{}, false
	}

	event := grokBuildUsageEvent{
		OccurredAt:    occurredAt,
		PromptID:      grokBuildText(update["prompt_id"]),
		CostPartial:   grokBuildBool(usage["costIsPartial"]),
		ModelCounters: map[string]grokBuildCounters{},
	}
	modelUsage, _ := usage["modelUsage"].(map[string]any)
	for modelName, value := range modelUsage {
		if object, ok := value.(map[string]any); ok {
			event.ModelCounters[modelName] = grokBuildParseCounters(object)
		}
	}
	if len(event.ModelCounters) == 0 {
		event.ModelCounters["unknown"] = grokBuildParseCounters(usage)
	}
	return event, true
}

func grokBuildParseCounters(object map[string]any) grokBuildCounters {
	return grokBuildCounters{
		Input:         grokBuildUint64(object["inputTokens"]),
		Output:        grokBuildUint64(object["outputTokens"]),
		Cached:        grokBuildUint64(object["cachedReadTokens"]),
		APIDurationMS: grokBuildUint64(object["apiDurationMs"]),
		ModelCalls:    grokBuildUint64(object["modelCalls"]),
		CostTicks:     grokBuildUint64(object["costUsdTicks"]),
		CostIsPartial: grokBuildBool(object["costIsPartial"]),
	}
}

func grokBuildTimestamp(value any) (time.Time, bool) {
	switch typed := value.(type) {
	case json.Number:
		seconds, err := strconv.ParseInt(typed.String(), 10, 64)
		if err != nil {
			return time.Time{}, false
		}
		if seconds > 100_000_000_000 {
			seconds /= 1000
		}
		return time.Unix(seconds, 0).UTC(), true
	case float64:
		seconds := int64(typed)
		if seconds > 100_000_000_000 {
			seconds /= 1000
		}
		return time.Unix(seconds, 0).UTC(), true
	case string:
		if parsed, err := time.Parse(time.RFC3339Nano, typed); err == nil {
			return parsed.UTC(), true
		}
		seconds, err := strconv.ParseInt(strings.TrimSpace(typed), 10, 64)
		if err != nil {
			return time.Time{}, false
		}
		return grokBuildTimestamp(json.Number(strconv.FormatInt(seconds, 10)))
	default:
		return time.Time{}, false
	}
}

func grokBuildText(value any) string {
	text, _ := value.(string)
	return text
}

func grokBuildBool(value any) bool {
	boolean, _ := value.(bool)
	return boolean
}

func grokBuildUint64(value any) uint64 {
	switch typed := value.(type) {
	case json.Number:
		parsed, err := strconv.ParseUint(typed.String(), 10, 64)
		if err == nil {
			return parsed
		}
	case float64:
		if typed >= 0 {
			return uint64(typed)
		}
	case string:
		parsed, err := strconv.ParseUint(strings.TrimSpace(typed), 10, 64)
		if err == nil {
			return parsed
		}
	}
	return 0
}
