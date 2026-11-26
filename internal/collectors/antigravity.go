package collectors

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"tokenanalyse/internal/model"

	_ "modernc.org/sqlite"
)

const (
	agySourceID      = "agy"
	agyAppType       = "agy"
	agyCursorVersion = 4
)

// AntigravityCollector imports exact per-step token counters from Antigravity
// transcripts and uses the companion conversation database for model provenance.
type AntigravityCollector struct{}

var _ Collector = (*AntigravityCollector)(nil)

// NewAntigravityCollector creates an Antigravity collector.
func NewAntigravityCollector() *AntigravityCollector { return &AntigravityCollector{} }

// NewAGYCollector is a short alias for NewAntigravityCollector.
func NewAGYCollector() *AntigravityCollector { return NewAntigravityCollector() }

func (c *AntigravityCollector) Info() Info {
	return Info{
		ID:          agySourceID,
		Name:        "Antigravity",
		AppTypes:    []string{agyAppType},
		Description: "Reads Antigravity transcripts and native conversation model metadata",
		Incremental: true,
	}
}

type agyCursorState struct {
	Version     int               `json:"version"`
	Fingerprint string            `json:"fingerprint"`
	NativeOK    bool              `json:"nativeOk"`
	Steps       map[string]string `json:"steps,omitempty"`
}

type agyTiming struct {
	LatencyMS    uint64
	FirstTokenMS uint64
}

type agyGeneration struct {
	BaseModel   string
	TieredModel string
	Timing      *agyTiming
}

type agyStep struct {
	Index uint64
	Value map[string]any
	Hash  string
}

func (c *AntigravityCollector) Collect(ctx Context, cursors []Cursor) (Result, error) {
	result := Result{Metadata: map[string]any{"source": agySourceID}}
	cursorByPath := make(map[string]Cursor, len(cursors))
	for _, cursor := range cursors {
		if cursor.SourceID == agySourceID {
			cursorByPath[filepath.Clean(cursor.FilePath)] = cursor
		}
	}

	paths := make([]string, 0)
	seenPaths := map[string]struct{}{}
	for _, root := range agyBrainRoots(ctx) {
		_ = filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				if !os.IsNotExist(walkErr) {
					result.Errors = append(result.Errors, fmt.Sprintf("Antigravity walk %s: %v", path, walkErr))
				}
				return nil
			}
			if entry.IsDir() {
				return nil
			}
			if entry.Type()&os.ModeSymlink != 0 || entry.Name() != "transcript_full.jsonl" {
				return nil
			}
			clean := filepath.Clean(path)
			if _, exists := seenPaths[clean]; exists {
				return nil
			}
			seenPaths[clean] = struct{}{}
			paths = append(paths, path)
			return nil
		})
	}
	sort.Strings(paths)

	seenEvents := make(map[string]struct{})
	for _, path := range paths {
		result.FilesScanned++
		events, cursor, err := c.collectTranscript(ctx, path, cursorByPath[filepath.Clean(path)])
		if err != nil {
			result.Errors = append(result.Errors, fmt.Sprintf("Antigravity %s: %v", path, err))
			continue
		}
		if cursor != nil {
			result.Cursors = append(result.Cursors, *cursor)
		}
		for _, event := range events {
			if _, exists := seenEvents[event.EventID]; exists {
				continue
			}
			seenEvents[event.EventID] = struct{}{}
			result.Events = append(result.Events, event)
		}
	}
	return result, nil
}

func agyBrainRoots(ctx Context) []string {
	bases := make([]string, 0, 8)
	for _, value := range []string{ctx.Paths.Antigravity, ctx.Env["AGY_HOME"], ctx.Paths.Gemini} {
		if value = strings.TrimSpace(value); value != "" {
			bases = append(bases, value)
		}
	}
	if strings.TrimSpace(ctx.Home) != "" {
		bases = append(bases,
			filepath.Join(ctx.Home, ".agy"),
			filepath.Join(ctx.Home, ".gemini"),
		)
	}
	roots := make([]string, 0, len(bases)*4)
	seen := map[string]struct{}{}
	add := func(path string) {
		path = filepath.Clean(path)
		if _, exists := seen[path]; exists {
			return
		}
		seen[path] = struct{}{}
		if info, err := os.Stat(path); err == nil && info.IsDir() {
			roots = append(roots, path)
		}
	}
	for _, base := range bases {
		if filepath.Base(base) == "brain" {
			add(base)
			continue
		}
		add(filepath.Join(base, "brain"))
		add(filepath.Join(base, "antigravity-cli", "brain"))
		add(filepath.Join(base, "antigravity", "brain"))
		add(filepath.Join(base, "antigravity-acp", "brain"))
	}
	return roots
}

func (c *AntigravityCollector) collectTranscript(ctx Context, path string, existing Cursor) ([]model.UsageEvent, *Cursor, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, nil, nil
	}
	modified := info.ModTime().UnixNano()
	size := info.Size()
	fingerprint := agyNativeFingerprint(path)
	state := agyCursorState{Version: agyCursorVersion, Fingerprint: fingerprint, NativeOK: true}
	stateOK := false
	if existing.State != "" {
		decoded := agyCursorState{}
		if json.Unmarshal([]byte(existing.State), &decoded) == nil && decoded.Version == agyCursorVersion {
			state = decoded
			stateOK = true
		}
	}
	if stateOK && existing.Offset == size && existing.ModifiedNanos == modified && state.Fingerprint == fingerprint && state.NativeOK {
		cursor := existing
		return nil, &cursor, nil
	}
	forceAll := !stateOK || state.Fingerprint != fingerprint || !state.NativeOK

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}
	steps, damaged := agyParseTranscript(data)
	sessionID := agySessionID(path)
	modelByStep := agyModelsByStep(steps)

	wanted := make([]uint64, 0, len(steps))
	for _, step := range steps {
		if !agyIsDonePlannerResponse(step.Value) {
			continue
		}
		if agyHasTokens(step.Value) {
			wanted = append(wanted, step.Index)
		}
	}
	generations := map[uint64]agyGeneration{}
	nativeOK := true
	if len(wanted) > 0 {
		var nativeErr error
		generations, nativeErr = agyReadGenerations(path, wanted)
		if nativeErr != nil {
			nativeOK = false
			// Model/timing metadata is an enrichment. Transcript counters remain
			// usable when the native database is absent, locked, or damaged.
			generations = map[uint64]agyGeneration{}
		}
	}

	events := make([]model.UsageEvent, 0, len(wanted))
	nextSteps := make(map[string]string, len(steps))
	for _, step := range steps {
		key := strconv.FormatUint(step.Index, 10)
		nextSteps[key] = step.Hash
		if !agyIsDonePlannerResponse(step.Value) || !agyHasTokens(step.Value) {
			continue
		}
		if !forceAll && state.Steps[key] == step.Hash {
			continue
		}
		generation := generations[step.Index]
		event := agyNewEvent(ctx, sessionID, step, modelByStep[step.Index], generation)
		events = append(events, event)
	}

	encoded, _ := json.Marshal(agyCursorState{Version: agyCursorVersion, Fingerprint: fingerprint, NativeOK: nativeOK, Steps: nextSteps})
	cursor := Cursor{
		SourceID:      agySourceID,
		FilePath:      path,
		Offset:        size,
		ModifiedNanos: modified,
		State:         string(encoded),
		UpdatedAt:     time.Now().UTC(),
	}
	if damaged > 0 {
		// The cursor still advances: malformed JSONL records are skipped, while a
		// later append is picked up normally on the next collection.
		_ = damaged
	}
	return events, &cursor, nil
}

func agyParseTranscript(data []byte) ([]agyStep, int) {
	latest := map[uint64]agyStep{}
	damaged := 0
	for _, line := range bytes.Split(data, []byte{'\n'}) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		decoder := json.NewDecoder(bytes.NewReader(line))
		decoder.UseNumber()
		var value map[string]any
		if err := decoder.Decode(&value); err != nil || value == nil {
			damaged++
			continue
		}
		rawIndex, exists := value["step_index"]
		if !exists {
			continue
		}
		index := agyUint64(rawIndex)
		sum := sha256.Sum256(line)
		latest[index] = agyStep{Index: index, Value: value, Hash: hex.EncodeToString(sum[:])}
	}
	steps := make([]agyStep, 0, len(latest))
	for _, step := range latest {
		steps = append(steps, step)
	}
	sort.Slice(steps, func(i, j int) bool { return steps[i].Index < steps[j].Index })
	return steps, damaged
}

func agySessionID(path string) string {
	sessionDir := filepath.Dir(filepath.Dir(filepath.Dir(path)))
	if sessionDir == "." || sessionDir == string(filepath.Separator) {
		return strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	}
	return filepath.Base(sessionDir)
}

func agyModelsByStep(steps []agyStep) map[uint64]string {
	models := map[uint64]string{}
	current := ""
	for _, step := range steps {
		value := step.Value
		if agyString(value["type"]) == "USER_INPUT" {
			if selected := agyModelFromSettingsChange(agyString(value["content"])); selected != "" {
				current = selected
			}
			continue
		}
		if agyString(value["type"]) == "PLANNER_RESPONSE" && current != "" {
			if _, exists := models[step.Index]; !exists {
				models[step.Index] = current
			}
		}
	}
	return models
}

func agyIsDonePlannerResponse(value map[string]any) bool {
	return agyString(value["type"]) == "PLANNER_RESPONSE" && agyString(value["status"]) == "DONE"
}

func agyHasTokens(value map[string]any) bool {
	return agyUint64(value["input_tokens"]) > 0 ||
		agyUint64(value["output_tokens"]) > 0 ||
		agyUint64(value["cache_read_tokens"]) > 0 ||
		agyUint64(value["cache_write_tokens"]) > 0
}

func agyNewEvent(ctx Context, sessionID string, step agyStep, selectedModel string, generation agyGeneration) model.UsageEvent {
	value := step.Value
	display := agyKnownModel(value)
	if display == "" {
		display = selectedModel
	}
	if display == "" {
		display = generation.TieredModel
	}
	if display == "" {
		display = generation.BaseModel
	}
	if display == "" {
		display = "unknown"
	}

	actual := display
	role := ""
	orchestratorMode := agyOrchestratorMode(display)
	nativeModel := agyConcreteNativeModel(generation)
	if orchestratorMode != "" {
		role = "orchestrator"
		if nativeModel != "" {
			actual = nativeModel
		}
	} else if agyIsOrchestratorWorker(display) {
		role = "orchestrator-worker"
		if nativeModel != "" {
			actual = nativeModel
		} else {
			actual = "gemini-4-hydrogen"
		}
	}
	modelID := agyModelID(actual)
	usage := model.TokenUsage{
		InputTokens:         agyUint64(value["input_tokens"]),
		OutputTokens:        agyUint64(value["output_tokens"]),
		CacheReadTokens:     agyUint64(value["cache_read_tokens"]),
		CacheCreationTokens: agyUint64(value["cache_write_tokens"]),
		Model:               modelID,
	}
	sourceRecord := fmt.Sprintf("agy:%s:%d", sessionID, step.Index)
	event := model.NewUsageEvent(ctx.DeviceID, model.SourceSession, agyAppType, sourceRecord, modelID, usage, time.UnixMilli(agyTimestampMS(value["created_at"])).UTC())
	event.RequestID = sourceRecord
	event.SessionID = sessionID
	event.ProviderID = "_agy_session"
	event.ProviderName = "Antigravity session log"
	event.ModelRaw = display
	event.RequestModel = display
	event.CanonicalModel = model.NormalizeModelID(modelID)
	if modelID != display {
		event.PricingModel = modelID
	}
	event.InputTokenSemantics = model.SemanticsFresh
	event.IsStreaming = true
	status := 200
	event.StatusCode = &status
	if generation.Timing != nil {
		latency := generation.Timing.LatencyMS
		first := generation.Timing.FirstTokenMS
		event.LatencyMS = &latency
		event.FirstTokenMS = &first
		event.DurationMS = &latency
		if first <= latency {
			generationDuration := latency - first
			event.GenerationDurationMS = &generationDuration
			if generationDuration > 0 && usage.OutputTokens > 0 {
				speed := float64(usage.OutputTokens) / (float64(generationDuration) / 1000)
				event.SpeedTPS = &speed
			}
		}
		event.TimingQuality = model.TimingExact
	}
	event.Metadata = map[string]any{
		"step":                   step.Index,
		"role":                   role,
		"orchestrator":           orchestratorMode,
		"selectedModel":          selectedModel,
		"nativeModel":            nativeModel,
		"nativeBase":             generation.BaseModel,
		"nativeTier":             generation.TieredModel,
		"inputFresh":             true,
		"outputIncludesThinking": true,
	}
	return event
}

func agyConcreteNativeModel(generation agyGeneration) string {
	if generation.TieredModel != "" && !agyIsUnresolvedWorkerTarget(generation.TieredModel) {
		return generation.TieredModel
	}
	switch strings.ToLower(strings.TrimSpace(generation.BaseModel)) {
	case "gemini-pro-orchestrator":
		return "gemini-4-carbon"
	case "gemini-pro-orchestrator-argon", "gemini-pro-orchestrator-g1-pro":
		return "gemini-4-argon"
	case "gemini-pro-orchestrator-neon":
		return "gemini-4-neon"
	case "gemini-flash-worker-agy":
		return "gemini-4-hydrogen"
	}
	if generation.BaseModel != "" && !agyIsUnresolvedWorkerTarget(generation.BaseModel) {
		return generation.BaseModel
	}
	return ""
}

func agyKnownModel(value map[string]any) string {
	modelName := strings.TrimSpace(agyString(value["model"]))
	if modelName == "" || strings.EqualFold(modelName, "unknown") {
		return ""
	}
	return modelName
}

func agyModelFromSettingsChange(content string) string {
	index := strings.Index(content, "Model Selection")
	if index < 0 {
		return ""
	}
	rest := content[index:]
	to := strings.Index(rest, " to ")
	if to < 0 {
		return ""
	}
	tail := strings.TrimSpace(rest[to+4:])
	if tail == "" {
		return ""
	}
	end := len(tail)
	for _, delimiter := range []string{". ", "\n", "`"} {
		if position := strings.Index(tail, delimiter); position >= 0 && position < end {
			end = position
		}
	}
	modelName := strings.Trim(strings.TrimSpace(tail[:end]), ".")
	modelName = strings.TrimSpace(modelName)
	if modelName == "" || strings.EqualFold(modelName, "none") {
		return ""
	}
	return modelName
}

func agyModelID(display string) string {
	display = strings.TrimSpace(display)
	if !strings.Contains(display, " ") {
		return display
	}
	display = strings.ToLower(display)
	display = strings.ReplaceAll(display, " (", "-")
	display = strings.ReplaceAll(display, "(", "")
	display = strings.ReplaceAll(display, ")", "")
	return strings.Join(strings.Fields(display), "-")
}

func agyOrchestratorMode(modelName string) string {
	switch strings.ToLower(agyModelID(modelName)) {
	case "orchestrator-carbon", "gemini-pro-orchestrator":
		return "carbon"
	case "orchestrator-argon", "gemini-pro-orchestrator-argon", "gemini-pro-orchestrator-g1-pro":
		return "argon"
	case "orchestrator-neon", "gemini-pro-orchestrator-neon":
		return "neon"
	default:
		return ""
	}
}

func agyIsOrchestratorWorker(modelName string) bool {
	return strings.EqualFold(agyModelID(modelName), "gemini-flash-worker-agy")
}

func agyIsUnresolvedWorkerTarget(modelName string) bool {
	id := strings.ToLower(agyModelID(modelName))
	return id == "" || id == "unknown" || agyOrchestratorMode(id) != "" || agyIsOrchestratorWorker(id) || id == "gemini-flash"
}

func agyNativeFingerprint(transcript string) string {
	databasePath, ok := agyNativeDatabasePath(transcript)
	if !ok {
		return "none"
	}
	signature := func(path string) string {
		info, err := os.Stat(path)
		if err != nil {
			return "0:0"
		}
		return fmt.Sprintf("%d:%d", info.ModTime().UnixNano(), info.Size())
	}
	return signature(databasePath) + ":" + signature(databasePath+"-wal")
}

func agyNativeDatabasePath(transcript string) (string, bool) {
	sessionDir := filepath.Dir(filepath.Dir(filepath.Dir(transcript)))
	brainRoot := filepath.Dir(sessionDir)
	if filepath.Base(brainRoot) != "brain" {
		return "", false
	}
	root := filepath.Dir(brainRoot)
	path := filepath.Join(root, "conversations", filepath.Base(sessionDir)+".db")
	if info, err := os.Stat(path); err != nil || !info.Mode().IsRegular() {
		return "", false
	}
	return path, true
}

func agyReadGenerations(transcript string, steps []uint64) (map[uint64]agyGeneration, error) {
	generations := map[uint64]agyGeneration{}
	path, ok := agyNativeDatabasePath(transcript)
	if !ok {
		return generations, nil
	}
	database, err := agyOpenReadOnly(path)
	if err != nil {
		return generations, err
	}
	defer database.Close()

	for _, step := range steps {
		if step > math.MaxInt64 {
			continue
		}
		var metadata []byte
		err := database.QueryRow("SELECT metadata FROM steps WHERE idx=? AND step_type=15", int64(step)).Scan(&metadata)
		if err == sql.ErrNoRows {
			continue
		}
		if err != nil {
			return generations, err
		}
		generationIndex, ok := agyStepGenerationIndex(metadata)
		if !ok || generationIndex > math.MaxInt64 {
			continue
		}
		var data []byte
		err = database.QueryRow("SELECT data FROM gen_metadata WHERE idx=?", int64(generationIndex)).Scan(&data)
		if err == sql.ErrNoRows {
			continue
		}
		if err != nil {
			return generations, err
		}
		if generation, ok := agyParseGeneration(data); ok {
			generations[step] = generation
		}
	}
	return generations, nil
}

func agyOpenReadOnly(path string) (*sql.DB, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	location := &url.URL{Scheme: "file", Path: filepath.ToSlash(absolute)}
	query := location.Query()
	query.Set("mode", "ro")
	query.Add("_pragma", "busy_timeout(0)")
	query.Add("_pragma", "query_only(1)")
	location.RawQuery = query.Encode()
	database, err := sql.Open("sqlite", location.String())
	if err != nil {
		return nil, err
	}
	database.SetMaxOpenConns(1)
	database.SetMaxIdleConns(1)
	if err := database.Ping(); err != nil {
		database.Close()
		return nil, err
	}
	return database, nil
}

type agyProtoField struct {
	Number int
	Wire   int
	Varint uint64
	Bytes  []byte
}

func agyParseProto(data []byte) ([]agyProtoField, bool) {
	fields := make([]agyProtoField, 0, 8)
	for offset := 0; offset < len(data); {
		key, next, ok := agyReadVarint(data, offset)
		if !ok || key == 0 {
			return nil, false
		}
		offset = next
		field := agyProtoField{Number: int(key >> 3), Wire: int(key & 7)}
		switch field.Wire {
		case 0:
			value, after, ok := agyReadVarint(data, offset)
			if !ok {
				return nil, false
			}
			field.Varint = value
			offset = after
		case 1:
			if len(data)-offset < 8 {
				return nil, false
			}
			field.Bytes = data[offset : offset+8]
			offset += 8
		case 2:
			length, after, ok := agyReadVarint(data, offset)
			if !ok || length > uint64(len(data)-after) {
				return nil, false
			}
			end := after + int(length)
			field.Bytes = data[after:end]
			offset = end
		case 5:
			if len(data)-offset < 4 {
				return nil, false
			}
			field.Bytes = data[offset : offset+4]
			offset += 4
		default:
			return nil, false
		}
		fields = append(fields, field)
	}
	return fields, true
}

func agyReadVarint(data []byte, offset int) (uint64, int, bool) {
	var value uint64
	for shift := uint(0); shift < 64; shift += 7 {
		if offset >= len(data) {
			return 0, offset, false
		}
		current := data[offset]
		offset++
		if shift == 63 && current > 1 {
			return 0, offset, false
		}
		value |= uint64(current&0x7f) << shift
		if current&0x80 == 0 {
			return value, offset, true
		}
	}
	return 0, offset, false
}

func agyStepGenerationIndex(metadata []byte) (uint64, bool) {
	fields, ok := agyParseProto(metadata)
	if !ok {
		return 0, false
	}
	for _, field := range fields {
		if field.Number != 20 || field.Wire != 2 {
			continue
		}
		reference, ok := agyParseProto(field.Bytes)
		if !ok {
			continue
		}
		for _, referenceField := range reference {
			if referenceField.Number == 3 && referenceField.Wire == 0 {
				return referenceField.Varint, true
			}
		}
	}
	return 0, false
}

func agyParseGeneration(data []byte) (agyGeneration, bool) {
	generation := agyGeneration{}
	fields, ok := agyParseProto(data)
	if !ok {
		return generation, false
	}
	for _, field := range fields {
		if field.Number != 1 || field.Wire != 2 {
			continue
		}
		response, ok := agyParseProto(field.Bytes)
		if !ok {
			continue
		}
		var firstToken, streaming *uint64
		for _, responseField := range response {
			switch {
			case responseField.Number == 19 && responseField.Wire == 2:
				generation.BaseModel = strings.TrimSpace(string(responseField.Bytes))
			case responseField.Number == 21 && responseField.Wire == 2:
				generation.TieredModel = strings.TrimSpace(string(responseField.Bytes))
			case responseField.Number == 11 && responseField.Wire == 2:
				if value, ok := agyParseDuration(responseField.Bytes); ok {
					firstToken = &value
				}
			case responseField.Number == 12 && responseField.Wire == 2:
				if value, ok := agyParseDuration(responseField.Bytes); ok {
					streaming = &value
				}
			}
		}
		if firstToken != nil && streaming != nil {
			latency := *firstToken + *streaming
			if latency > 0 {
				generation.Timing = &agyTiming{LatencyMS: latency, FirstTokenMS: *firstToken}
			}
		}
		break
	}
	if generation.BaseModel == "" && generation.TieredModel == "" && generation.Timing == nil {
		return generation, false
	}
	return generation, true
}

func agyParseDuration(data []byte) (uint64, bool) {
	fields, ok := agyParseProto(data)
	if !ok {
		return 0, false
	}
	var seconds int64
	var nanos int64
	for _, field := range fields {
		if field.Wire != 0 {
			continue
		}
		switch field.Number {
		case 1:
			seconds = int64(field.Varint)
		case 2:
			nanos = int64(field.Varint)
		}
	}
	if seconds < 0 || nanos < 0 || seconds > math.MaxInt64/1000 {
		return 0, false
	}
	milliseconds := seconds*1000 + nanos/1_000_000
	if milliseconds < 0 {
		return 0, false
	}
	return uint64(milliseconds), true
}

func agyTimestampMS(value any) int64 {
	switch typed := value.(type) {
	case json.Number:
		if parsed, err := typed.Int64(); err == nil {
			return agyNormalizeTimestamp(parsed)
		}
		if parsed, err := typed.Float64(); err == nil {
			return agyNormalizeTimestamp(int64(parsed))
		}
	case float64:
		return agyNormalizeTimestamp(int64(typed))
	case int64:
		return agyNormalizeTimestamp(typed)
	case int:
		return agyNormalizeTimestamp(int64(typed))
	case string:
		text := strings.TrimSpace(typed)
		if text == "" {
			return 0
		}
		if parsed, err := strconv.ParseInt(text, 10, 64); err == nil {
			return agyNormalizeTimestamp(parsed)
		}
		for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02 15:04:05.999999999Z07:00", "2006-01-02 15:04:05"} {
			if parsed, err := time.Parse(layout, text); err == nil {
				return parsed.UnixMilli()
			}
		}
	}
	return 0
}

func agyNormalizeTimestamp(value int64) int64 {
	if value > 0 && value < 1_000_000_000_000 {
		return value * 1000
	}
	return value
}

func agyUint64(value any) uint64 {
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

func agyString(value any) string {
	text, _ := value.(string)
	return text
}
