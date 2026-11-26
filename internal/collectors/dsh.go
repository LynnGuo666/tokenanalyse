package collectors

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"tokenanalyse/internal/model"

	"github.com/klauspost/compress/zstd"
)

const (
	dshSourceID       = "dsh"
	dshAppType        = "dsh"
	dshCursorVersion  = 1
	zstdFrameMagic    = uint32(0xFD2FB528)
	zstdSkippableBase = uint32(0x184D2A50)
)

// DSHCollector imports usage from DeepSeek Harness session logs.
type DSHCollector struct{}

var _ Collector = (*DSHCollector)(nil)

// NewDSHCollector creates a DeepSeek Harness collector.
func NewDSHCollector() *DSHCollector { return &DSHCollector{} }

// NewDeepSeekHarnessCollector is an explicit alias for NewDSHCollector.
func NewDeepSeekHarnessCollector() *DSHCollector { return NewDSHCollector() }

func (DSHCollector) Info() Info {
	return Info{
		ID:          dshSourceID,
		Name:        "DeepSeek Harness",
		AppTypes:    []string{dshAppType},
		Description: "Reads DeepSeek Harness session.v4 JSONL and concatenated zstd frames",
		Incremental: true,
	}
}

type dshCursorState struct {
	Version int              `json:"version"`
	Steps   map[string]int64 `json:"steps,omitempty"`
	Pending []byte           `json:"pending,omitempty"`
}

type dshPendingRequest struct {
	requestID    string
	messageID    string
	sessionID    string
	provider     string
	model        string
	usage        model.TokenUsage
	occurredAtMS int64
	latencyMS    uint64
	firstTokenMS *uint64
	generationMS *uint64
	turn         uint64
	step         uint64
	viaAccount   bool
	isStreaming  bool
	sourceRecord string
}

func (c DSHCollector) Collect(ctx Context, cursors []Cursor) (Result, error) {
	result := Result{Metadata: map[string]any{"source": dshSourceID}}
	root := strings.TrimSpace(ctx.Paths.DSH)
	if root == "" {
		root = strings.TrimSpace(ctx.Env["DSH_HOME"])
	}
	if root == "" && strings.TrimSpace(ctx.Home) != "" {
		root = filepath.Join(ctx.Home, ".deepseek")
	}
	if root == "" {
		return result, nil
	}
	sessionsRoot := filepath.Join(root, "sessions")
	if info, err := os.Stat(sessionsRoot); err != nil || !info.IsDir() {
		return result, nil
	}

	cursorByPath := make(map[string]Cursor, len(cursors))
	for _, cursor := range cursors {
		if cursor.SourceID == dshSourceID {
			cursorByPath[filepath.Clean(cursor.FilePath)] = cursor
		}
	}

	paths := make([]string, 0)
	_ = filepath.WalkDir(sessionsRoot, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			result.Errors = append(result.Errors, fmt.Sprintf("DSH walk %s: %v", path, walkErr))
			return nil
		}
		if entry.IsDir() {
			return nil
		}
		if dshIsSessionLog(entry.Name()) {
			paths = append(paths, path)
		}
		return nil
	})
	sort.Strings(paths)

	seenEvents := make(map[string]struct{})
	for _, path := range paths {
		partial, err := c.collectFile(ctx, path, cursorByPath[filepath.Clean(path)])
		if err != nil {
			result.Errors = append(result.Errors, fmt.Sprintf("DSH %s: %v", path, err))
			continue
		}
		result.FilesScanned += partial.FilesScanned
		result.BytesRead += partial.BytesRead
		result.Cursors = append(result.Cursors, partial.Cursors...)
		for _, event := range partial.Events {
			if _, exists := seenEvents[event.EventID]; exists {
				continue
			}
			seenEvents[event.EventID] = struct{}{}
			result.Events = append(result.Events, event)
		}
	}
	return result, nil
}

func dshIsSessionLog(name string) bool {
	if !strings.HasPrefix(name, "session.") {
		return false
	}
	return strings.HasSuffix(name, ".jsonl") || strings.HasSuffix(name, ".jsonl.zstd")
}

func (c DSHCollector) collectFile(ctx Context, path string, cursor Cursor) (Result, error) {
	result := Result{}
	info, err := os.Stat(path)
	if err != nil {
		return result, err
	}
	if !info.Mode().IsRegular() {
		return result, nil
	}
	modified := info.ModTime().UnixNano()
	size := info.Size()
	state := dshCursorState{Version: dshCursorVersion, Steps: map[string]int64{}}
	stateOK := true
	if cursor.State != "" {
		stateOK = json.Unmarshal([]byte(cursor.State), &state) == nil && state.Version == dshCursorVersion
	}
	if !stateOK {
		state = dshCursorState{Version: dshCursorVersion, Steps: map[string]int64{}}
	}
	if state.Steps == nil {
		state.Steps = map[string]int64{}
	}

	if cursor.Offset == size && cursor.ModifiedNanos == modified && stateOK && cursor.State != "" {
		return result, nil
	}

	start := cursor.Offset
	if start < 0 || start > size || (start > 0 && !stateOK) || (start == size && cursor.State != "" && modified != cursor.ModifiedNanos) {
		start = 0
		state = dshCursorState{Version: dshCursorVersion, Steps: map[string]int64{}}
	}

	file, err := os.Open(path)
	if err != nil {
		return result, err
	}
	defer file.Close()

	compressed := strings.HasSuffix(path, ".zstd")
	if start > 0 {
		boundaryOK, boundaryErr := dshBoundaryOK(file, start, size, compressed)
		if boundaryErr != nil {
			return result, boundaryErr
		}
		if !boundaryOK {
			start = 0
			state = dshCursorState{Version: dshCursorVersion, Steps: map[string]int64{}}
		}
	}
	if _, err := file.Seek(start, io.SeekStart); err != nil {
		return result, err
	}
	tail, err := io.ReadAll(file)
	if err != nil {
		return result, err
	}
	result.BytesRead = int64(len(tail))

	text := []byte{}
	nextOffset := start
	switch {
	case compressed:
		frames, boundaryOK := scanZstdFrames(tail)
		if !boundaryOK && start > 0 {
			// The cursor no longer points at a frame header: replay from the
			// beginning. Stable event IDs make the replay idempotent.
			start = 0
			state = dshCursorState{Version: dshCursorVersion, Steps: map[string]int64{}}
			if _, err := file.Seek(0, io.SeekStart); err != nil {
				return result, err
			}
			tail, err = io.ReadAll(file)
			if err != nil {
				return result, err
			}
			result.BytesRead += int64(len(tail))
			frames, boundaryOK = scanZstdFrames(tail)
		}
		_ = boundaryOK
		for _, frameRange := range frames {
			decoded, decodeErr := decodeZstdFrame(tail[frameRange[0]:frameRange[1]])
			if decodeErr != nil {
				result.Errors = append(result.Errors, fmt.Sprintf("%s: damaged zstd frame at byte %d: %v", path, start+int64(frameRange[0]), decodeErr))
				continue
			}
			text = append(text, decoded...)
			nextOffset = start + int64(frameRange[1])
		}
	default:
		if start > 0 {
			var previous [1]byte
			if _, err := file.ReadAt(previous[:], start-1); err != nil {
				return result, err
			}
			if previous[0] != '\n' {
				start = 0
				state = dshCursorState{Version: dshCursorVersion, Steps: map[string]int64{}}
				if _, err := file.Seek(0, io.SeekStart); err != nil {
					return result, err
				}
				tail, err = io.ReadAll(file)
				if err != nil {
					return result, err
				}
				result.BytesRead += int64(len(tail))
			}
		}
		end := bytes.LastIndexByte(tail, '\n') + 1
		if end > 0 {
			text = append(text, tail[:end]...)
			nextOffset = start + int64(end)
		}
	}

	result.FilesScanned = 1
	if len(text) == 0 && len(state.Pending) == 0 {
		return result, nil
	}
	if len(state.Pending) > 0 {
		text = append(append([]byte(nil), state.Pending...), text...)
	}
	lastNewline := bytes.LastIndexByte(text, '\n')
	if lastNewline < 0 {
		state.Pending = append([]byte(nil), text...)
		text = nil
	} else {
		complete := text[:lastNewline+1]
		state.Pending = append([]byte(nil), text[lastNewline+1:]...)
		text = complete
	}

	sessionID := filepath.Base(filepath.Dir(path))
	requests := collectDSHRequests(text, sessionID, &state)
	for _, request := range requests {
		event := dshNewEvent(ctx, request)
		result.Events = append(result.Events, event)
	}

	encodedState, err := json.Marshal(state)
	if err != nil {
		return result, err
	}
	result.FilesScanned = 1
	result.Cursors = append(result.Cursors, Cursor{
		SourceID:      dshSourceID,
		FilePath:      path,
		Offset:        nextOffset,
		ModifiedNanos: modified,
		State:         string(encodedState),
		UpdatedAt:     time.Now().UTC(),
	})
	return result, nil
}

func dshBoundaryOK(file *os.File, offset, size int64, compressed bool) (bool, error) {
	if offset == size {
		return true, nil
	}
	if !compressed {
		var previous [1]byte
		if _, err := file.ReadAt(previous[:], offset-1); err != nil {
			return false, err
		}
		return previous[0] == '\n', nil
	}
	var magic [4]byte
	if _, err := file.ReadAt(magic[:], offset); err != nil {
		return false, err
	}
	value := uint32(magic[0]) | uint32(magic[1])<<8 | uint32(magic[2])<<16 | uint32(magic[3])<<24
	return value == zstdFrameMagic || (value >= zstdSkippableBase && value <= zstdSkippableBase+15), nil
}

// scanZstdFrames scans a concatenated zstd stream without decompressing it. It
// returns complete frame ranges and whether the first byte begins a valid frame.
// A partial or damaged tail is deliberately left for a later collection.
func scanZstdFrames(buffer []byte) ([][2]int, bool) {
	frames := make([][2]int, 0, 4)
	offset := 0
	for offset < len(buffer) {
		start := offset
		if len(buffer)-offset < 4 {
			return frames, start == 0
		}
		magic := uint32(buffer[offset]) | uint32(buffer[offset+1])<<8 | uint32(buffer[offset+2])<<16 | uint32(buffer[offset+3])<<24
		if magic >= zstdSkippableBase && magic <= zstdSkippableBase+15 {
			if len(buffer)-offset < 8 {
				return frames, start == 0
			}
			payload := int(uint32(buffer[offset+4]) | uint32(buffer[offset+5])<<8 | uint32(buffer[offset+6])<<16 | uint32(buffer[offset+7])<<24)
			if payload < 0 || len(buffer)-offset-8 < payload {
				return frames, start == 0
			}
			offset += 8 + payload
			frames = append(frames, [2]int{start, offset})
			continue
		}
		if magic != zstdFrameMagic {
			return frames, start == 0
		}
		offset += 4
		if offset >= len(buffer) {
			return frames, start == 0
		}
		descriptor := buffer[offset]
		offset++
		if descriptor&0x18 != 0 {
			return frames, start == 0
		}
		contentSizeFlag := descriptor >> 6
		singleSegment := descriptor&0x20 != 0
		checksum := descriptor&0x04 != 0
		dictionaryFlag := descriptor & 0x03
		dictionaryBytes := 0
		switch dictionaryFlag {
		case 1:
			dictionaryBytes = 1
		case 2:
			dictionaryBytes = 2
		case 3:
			dictionaryBytes = 4
		}
		contentSizeBytes := 0
		if contentSizeFlag == 0 {
			if singleSegment {
				contentSizeBytes = 1
			}
		} else {
			contentSizeBytes = 1 << contentSizeFlag
		}
		windowDescriptorBytes := 0
		if !singleSegment {
			windowDescriptorBytes = 1
		}
		remaining := windowDescriptorBytes + dictionaryBytes + contentSizeBytes
		if remaining < 0 || len(buffer)-offset < remaining {
			return frames, start == 0
		}
		offset += remaining

		for {
			if len(buffer)-offset < 3 {
				return frames, start == 0
			}
			header := uint32(buffer[offset]) | uint32(buffer[offset+1])<<8 | uint32(buffer[offset+2])<<16
			offset += 3
			lastBlock := header&1 != 0
			blockType := (header >> 1) & 0x03
			blockSize := int(header >> 3)
			if blockType == 0x03 {
				return frames, start == 0
			}
			payloadBytes := blockSize
			if blockType == 0x01 {
				payloadBytes = 1
			}
			if payloadBytes < 0 || len(buffer)-offset < payloadBytes {
				return frames, start == 0
			}
			offset += payloadBytes
			if lastBlock {
				break
			}
		}
		if checksum {
			if len(buffer)-offset < 4 {
				return frames, start == 0
			}
			offset += 4
		}
		frames = append(frames, [2]int{start, offset})
	}
	return frames, true
}

func decodeZstdFrame(frame []byte) ([]byte, error) {
	if len(frame) == 0 {
		return nil, errors.New("empty zstd frame")
	}
	decoder, err := zstd.NewReader(nil)
	if err != nil {
		return nil, err
	}
	defer decoder.Close()
	return decoder.DecodeAll(frame, nil)
}

func collectDSHRequests(text []byte, sessionID string, state *dshCursorState) []dshPendingRequest {
	requests := make([]dshPendingRequest, 0)
	scanner := bufio.NewScanner(bytes.NewReader(text))
	scanner.Buffer(make([]byte, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		value, ok := dshDecodeObject(line)
		if !ok {
			continue
		}
		switch dshString(value["type"]) {
		case "step/start":
			data := dshObject(value["data"])
			key := dshStepKey(data)
			if timestamp, ok := dshInt64(value["time"]); ok {
				state.Steps[key] = timestamp
			}
		case "assistant/message":
			if request, ok := parseDSHAssistantMessage(value, sessionID, state); ok {
				requests = append(requests, request)
				delete(state.Steps, dshStepKey(dshObject(value["data"])))
			}
		}
	}
	return requests
}

func parseDSHAssistantMessage(value map[string]any, sessionID string, state *dshCursorState) (dshPendingRequest, bool) {
	data := dshObject(value["data"])
	usage := dshObject(data["usage"])
	tokens := model.TokenUsage{
		InputTokens:         dshUint64(usage["inputTokens"]),
		OutputTokens:        dshUint64(usage["outputTokens"]),
		CacheReadTokens:     dshUint64(usage["cacheReadTokens"]),
		CacheCreationTokens: dshUint64(usage["cacheWriteTokens"]),
	}
	if !tokens.HasBillableTokens() {
		return dshPendingRequest{}, false
	}
	message := dshObject(data["message"])
	source := dshObject(message["source"])
	messageID := strings.TrimSpace(dshString(message["id"]))
	sourceRecord := messageID
	if sourceRecord == "" {
		sum := sha256.Sum256(dshMustJSON(value))
		sourceRecord = hex.EncodeToString(sum[:])
	}
	modelName := strings.TrimSpace(dshString(source["model"]))
	if modelName == "" {
		modelName = "unknown"
	}
	provider := strings.TrimSpace(dshString(source["provider"]))
	occurredAtMS, _ := dshInt64(value["time"])
	key := dshStepKey(data)
	latency := uint64(0)
	var firstToken *uint64
	var generation *uint64
	if start, ok := state.Steps[key]; ok && occurredAtMS >= start {
		latency = uint64(occurredAtMS - start)
		if firstOutput, ok := dshFirstOutputTime(data["stream"]); ok && firstOutput >= start && uint64(firstOutput-start) <= latency {
			first := uint64(firstOutput - start)
			firstToken = &first
			generationDuration := latency - first
			generation = &generationDuration
		}
	}
	tokens.Model = modelName
	tokens.MessageID = messageID
	turn := dshUint64(data["turn"])
	step := dshUint64(data["step"])
	return dshPendingRequest{
		requestID:    "dsh:" + sourceRecord,
		messageID:    messageID,
		sessionID:    sessionID,
		provider:     provider,
		model:        modelName,
		usage:        tokens,
		occurredAtMS: occurredAtMS,
		latencyMS:    latency,
		firstTokenMS: firstToken,
		generationMS: generation,
		turn:         turn,
		step:         step,
		viaAccount:   provider == "deepseek-account",
		isStreaming:  true,
		sourceRecord: sourceRecord,
	}, true
}

func dshNewEvent(ctx Context, request dshPendingRequest) model.UsageEvent {
	occurredAt := time.UnixMilli(request.occurredAtMS).UTC()
	event := model.NewUsageEvent(ctx.DeviceID, model.SourceSession, dshAppType, request.sourceRecord, request.model, request.usage, occurredAt)
	event.RequestID = request.requestID
	event.SessionID = request.sessionID
	event.ProviderID = "_dsh_session"
	event.ProviderName = request.provider
	if event.ProviderName == "" {
		event.ProviderName = "DeepSeek Harness session log"
	}
	if request.viaAccount {
		event.ProviderID = "_dsh_account_session"
	}
	event.RequestModel = request.model
	event.PricingModel = dshPricingModel(request.model)
	event.CanonicalModel = model.NormalizeModelID(request.model)
	event.InputTokenSemantics = model.SemanticsFresh
	event.IsStreaming = request.isStreaming
	status := 200
	event.StatusCode = &status
	if request.latencyMS > 0 {
		latency := request.latencyMS
		event.LatencyMS = &latency
		event.DurationMS = &latency
		event.TimingQuality = model.TimingEstimated
	}
	if request.firstTokenMS != nil {
		event.FirstTokenMS = request.firstTokenMS
	}
	if request.generationMS != nil && *request.generationMS > 0 && request.usage.OutputTokens > 0 {
		generation := *request.generationMS
		event.GenerationDurationMS = &generation
		speed := float64(request.usage.OutputTokens) / (float64(generation) / 1000)
		event.SpeedTPS = &speed
	}
	event.Metadata = map[string]any{
		"turn":         request.turn,
		"step":         request.step,
		"provider":     request.provider,
		"viaAccount":   request.viaAccount,
		"messageId":    request.messageID,
		"parser":       "dsh-session-v1",
		"inputFresh":   true,
		"sourceFormat": "session.v4.jsonl",
	}
	return event
}

func dshPricingModel(modelName string) string {
	if modelName == "unknown" {
		return ""
	}
	if strings.Contains(modelName, ":") {
		parts := strings.SplitN(modelName, ":", 2)
		if strings.TrimSpace(parts[0]) != "" && strings.TrimSpace(parts[1]) != "" {
			return strings.TrimSpace(parts[1])
		}
	}
	return ""
}

func dshFirstOutputTime(value any) (int64, bool) {
	entries, ok := value.([]any)
	if !ok {
		return 0, false
	}
	var minimum int64
	found := false
	for _, entry := range entries {
		object := dshObject(entry)
		for _, key := range []string{"time", "time0"} {
			if timestamp, ok := dshInt64(object[key]); ok && (!found || timestamp < minimum) {
				minimum = timestamp
				found = true
			}
		}
	}
	return minimum, found
}

func dshStepKey(data map[string]any) string {
	turn := dshUint64(data["turn"])
	step := dshUint64(data["step"])
	return strconv.FormatUint(turn, 10) + ":" + strconv.FormatUint(step, 10)
}

func dshDecodeObject(data []byte) (map[string]any, bool) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var value map[string]any
	if err := decoder.Decode(&value); err != nil {
		return nil, false
	}
	if value == nil {
		return nil, false
	}
	return value, true
}

func dshObject(value any) map[string]any {
	object, _ := value.(map[string]any)
	return object
}

func dshString(value any) string {
	text, _ := value.(string)
	return text
}

func dshUint64(value any) uint64 {
	switch typed := value.(type) {
	case json.Number:
		if parsed, err := strconv.ParseUint(typed.String(), 10, 64); err == nil {
			return parsed
		}
		if parsed, err := strconv.ParseFloat(typed.String(), 64); err == nil && parsed >= 0 {
			return uint64(parsed)
		}
	case float64:
		if typed >= 0 {
			return uint64(typed)
		}
	case int64:
		if typed >= 0 {
			return uint64(typed)
		}
	case uint64:
		return typed
	case int:
		if typed >= 0 {
			return uint64(typed)
		}
	case string:
		if parsed, err := strconv.ParseUint(strings.TrimSpace(typed), 10, 64); err == nil {
			return parsed
		}
	}
	return 0
}

func dshInt64(value any) (int64, bool) {
	switch typed := value.(type) {
	case json.Number:
		if parsed, err := typed.Int64(); err == nil {
			return parsed, true
		}
		if parsed, err := typed.Float64(); err == nil {
			return int64(parsed), true
		}
	case float64:
		return int64(typed), true
	case int64:
		return typed, true
	case int:
		return int64(typed), true
	case string:
		if parsed, err := strconv.ParseInt(strings.TrimSpace(typed), 10, 64); err == nil {
			return parsed, true
		}
	}
	return 0, false
}

func dshMustJSON(value any) []byte {
	encoded, err := json.Marshal(value)
	if err != nil {
		return []byte(fmt.Sprint(value))
	}
	return encoded
}
