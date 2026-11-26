package collectors

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"tokenanalyse/internal/model"

	"github.com/shopspring/decimal"
	_ "modernc.org/sqlite"
)

const (
	openCodeSourceID  = "opencode"
	openCodeSourceApp = "opencode"
)

// OpenCodeCollector reads OpenCode's SQLite session store without modifying it.
type OpenCodeCollector struct{}

var _ Collector = OpenCodeCollector{}

func NewOpenCodeCollector() *OpenCodeCollector { return &OpenCodeCollector{} }

func (OpenCodeCollector) Info() Info {
	return Info{
		ID:          openCodeSourceID,
		Name:        "OpenCode",
		AppTypes:    []string{openCodeSourceApp},
		Description: "Read token usage from OPENCODE_DATA_DIR/opencode.db without modifying it",
		Incremental: true,
	}
}

func (c OpenCodeCollector) Collect(ctx Context, cursors []Cursor) (Result, error) {
	root := strings.TrimSpace(ctx.Paths.OpenCode)
	if root == "" {
		root = strings.TrimSpace(ctx.Env["OPENCODE_DATA_DIR"])
	}
	if root == "" {
		return Result{}, nil
	}
	dbPath := filepath.Join(root, "opencode.db")
	info, err := os.Stat(dbPath)
	if err != nil {
		if os.IsNotExist(err) {
			return Result{}, nil
		}
		return Result{}, fmt.Errorf("stat OpenCode database: %w", err)
	}
	if !info.Mode().IsRegular() {
		return Result{}, fmt.Errorf("OpenCode database is not a regular file: %s", dbPath)
	}

	fileModified := info.ModTime().UnixNano()
	fileState := fmt.Sprintf("%d:0", info.Size())
	walPath := strings.TrimSuffix(dbPath, filepath.Ext(dbPath)) + ".db-wal"
	if walInfo, err := os.Stat(walPath); err == nil {
		if walInfo.ModTime().UnixNano() > fileModified {
			fileModified = walInfo.ModTime().UnixNano()
		}
		fileState = fmt.Sprintf("%d:%d", info.Size(), walInfo.Size())
	}

	fileCursor, hasFileCursor := findCursor(cursors, openCodeSourceID, dbPath)
	if hasFileCursor && fileModified <= fileCursor.ModifiedNanos && (fileCursor.State == "" || fileCursor.State == fileState) {
		fileCursor.UpdatedAt = time.Now().UTC()
		return Result{Cursors: []Cursor{fileCursor}, FilesScanned: 1, Metadata: map[string]any{"source": openCodeSourceID}}, nil
	}

	db, err := openReadOnlySQLite(dbPath)
	if err != nil {
		return Result{}, err
	}
	defer db.Close()

	schema := detectOpenCodeSchema(db)
	sessions, err := queryOpenCodeSessions(db, schema)
	if err != nil {
		return Result{}, fmt.Errorf("query OpenCode sessions: %w", err)
	}

	fallbackTime := info.ModTime().UTC()
	result := Result{FilesScanned: 1, Metadata: map[string]any{"source": openCodeSourceID}}
	hadErrors := false
	for _, session := range sessions {
		sessionKey := dbPath + ":" + session.ID
		previous, hasPrevious := findCursor(cursors, openCodeSourceID, sessionKey)
		cutoff := int64(0)
		if hasPrevious && previous.Offset > 0 {
			cutoff = previous.Offset
		}
		if cutoff > 0 && session.Watermark <= cutoff {
			continue
		}

		rows, err := queryOpenCodeMessages(db, schema, session.ID, cutoff)
		if err != nil {
			hadErrors = true
			result.Errors = append(result.Errors, fmt.Sprintf("OpenCode session %s: %v", session.ID, err))
			continue
		}
		for _, row := range rows {
			event, ok := parseOpenCodeMessageEvent(ctx, session.ID, row, fallbackTime)
			if !ok {
				continue
			}
			result.Events = append(result.Events, event)
		}
		result.Cursors = append(result.Cursors, Cursor{
			SourceID:      openCodeSourceID,
			FilePath:      sessionKey,
			Offset:        session.Watermark,
			ModifiedNanos: session.Watermark,
			UpdatedAt:     time.Now().UTC(),
		})
	}

	if !hadErrors {
		result.Cursors = append(result.Cursors, Cursor{
			SourceID:      openCodeSourceID,
			FilePath:      dbPath,
			ModifiedNanos: fileModified,
			State:         fileState,
			UpdatedAt:     time.Now().UTC(),
		})
	}
	return result, nil
}

type openCodeSchema uint8

const (
	openCodeV1 openCodeSchema = iota
	openCodeV2
	openCodeMixed
)

type openCodeLayout struct {
	sessionTable   string
	messageTable   string
	layoutPriority int64
}

func (s openCodeSchema) layouts() []openCodeLayout {
	switch s {
	case openCodeV2:
		return []openCodeLayout{{"session_v2", "session_message", 1}}
	case openCodeMixed:
		return []openCodeLayout{{"session", "message", 0}, {"session_v2", "session_message", 1}}
	default:
		return []openCodeLayout{{"session", "message", 0}}
	}
}

func detectOpenCodeSchema(db *sql.DB) openCodeSchema {
	v1 := openCodeTableExists(db, "session") && openCodeTableExists(db, "message")
	v2 := openCodeTableExists(db, "session_v2") && openCodeTableExists(db, "session_message")
	switch {
	case v1 && v2:
		return openCodeMixed
	case v2:
		return openCodeV2
	default:
		return openCodeV1
	}
}

func openCodeTableExists(db *sql.DB, table string) bool {
	var exists bool
	err := db.QueryRow("SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type = 'table' AND name = ?)", table).Scan(&exists)
	return err == nil && exists
}

func openCodeTableColumns(db *sql.DB, table string) (map[string]bool, error) {
	rows, err := db.Query("PRAGMA table_info(" + quoteSQLiteIdentifier(table) + ")")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	columns := map[string]bool{}
	for rows.Next() {
		var cid, notnull, pk int
		var name, columnType string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &columnType, &notnull, &defaultValue, &pk); err != nil {
			return nil, err
		}
		columns[strings.ToLower(name)] = true
	}
	return columns, rows.Err()
}

func quoteSQLiteIdentifier(value string) string {
	return `"` + strings.ReplaceAll(value, `"`, `""`) + `"`
}

type openCodeSession struct {
	ID        string
	Watermark int64
}

func queryOpenCodeSessions(db *sql.DB, schema openCodeSchema) ([]openCodeSession, error) {
	sessions := map[string]int64{}
	for _, layout := range schema.layouts() {
		if !openCodeTableExists(db, layout.sessionTable) {
			continue
		}
		sessionColumns, err := openCodeTableColumns(db, layout.sessionTable)
		if err != nil {
			return nil, fmt.Errorf("inspect %s: %w", layout.sessionTable, err)
		}
		if !sessionColumns["id"] {
			continue
		}
		sessionUpdated := "0"
		if sessionColumns["time_updated"] {
			sessionUpdated = "COALESCE(s.time_updated, 0)"
		}

		query := "SELECT s.id, " + sessionUpdated + " FROM " + quoteSQLiteIdentifier(layout.sessionTable) + " s"
		if openCodeTableExists(db, layout.messageTable) {
			messageColumns, err := openCodeTableColumns(db, layout.messageTable)
			if err != nil {
				return nil, fmt.Errorf("inspect %s: %w", layout.messageTable, err)
			}
			messageUpdated := ""
			switch {
			case messageColumns["time_updated"]:
				messageUpdated = "COALESCE(m.time_updated, 0)"
			case messageColumns["time_created"]:
				messageUpdated = "COALESCE(m.time_created, 0)"
			}
			if messageColumns["session_id"] && messageUpdated != "" {
				query = "SELECT s.id, MAX(" + sessionUpdated + ", COALESCE(MAX(" + messageUpdated + "), 0)) FROM " +
					quoteSQLiteIdentifier(layout.sessionTable) + " s LEFT JOIN " + quoteSQLiteIdentifier(layout.messageTable) +
					" m ON m.session_id = s.id GROUP BY s.id"
			}
		}

		rows, err := db.Query(query)
		if err != nil {
			return nil, fmt.Errorf("query %s: %w", layout.sessionTable, err)
		}
		for rows.Next() {
			var id string
			var watermark int64
			if err := rows.Scan(&id, &watermark); err != nil {
				rows.Close()
				return nil, err
			}
			if id == "" {
				continue
			}
			if previous, ok := sessions[id]; !ok || watermark > previous {
				sessions[id] = watermark
			}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
	}

	ids := make([]string, 0, len(sessions))
	for id := range sessions {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	result := make([]openCodeSession, 0, len(ids))
	for _, id := range ids {
		result = append(result, openCodeSession{ID: id, Watermark: sessions[id]})
	}
	return result, nil
}

type openCodeMessageRow struct {
	ID             string
	Data           []byte
	Updated        int64
	MessageType    string
	HasMessageType bool
	LayoutPriority int64
}

func queryOpenCodeMessages(db *sql.DB, schema openCodeSchema, sessionID string, cutoff int64) ([]openCodeMessageRow, error) {
	var rows []openCodeMessageRow
	for _, layout := range schema.layouts() {
		if !openCodeTableExists(db, layout.messageTable) {
			continue
		}
		columns, err := openCodeTableColumns(db, layout.messageTable)
		if err != nil {
			return nil, fmt.Errorf("inspect %s: %w", layout.messageTable, err)
		}
		if !columns["id"] || !columns["session_id"] || !columns["data"] {
			continue
		}

		updatedExpression := "0"
		updatedColumn := ""
		switch {
		case columns["time_updated"]:
			updatedExpression = "COALESCE(time_updated, 0)"
			updatedColumn = "time_updated"
		case columns["time_created"]:
			updatedExpression = "COALESCE(time_created, 0)"
			updatedColumn = "time_created"
		}
		typeExpression := "NULL"
		if columns["type"] {
			typeExpression = "type"
		}
		query := "SELECT id, data, " + updatedExpression + ", " + typeExpression +
			" FROM " + quoteSQLiteIdentifier(layout.messageTable) + " WHERE session_id = ?"
		args := []any{sessionID}
		if cutoff > 0 && updatedColumn != "" {
			query += " AND " + quoteSQLiteIdentifier(updatedColumn) + " > ?"
			args = append(args, cutoff)
		}

		result, err := db.Query(query, args...)
		if err != nil {
			return nil, fmt.Errorf("query %s: %w", layout.messageTable, err)
		}
		for result.Next() {
			var row openCodeMessageRow
			var messageType sql.NullString
			if err := result.Scan(&row.ID, &row.Data, &row.Updated, &messageType); err != nil {
				result.Close()
				return nil, err
			}
			row.MessageType = messageType.String
			row.HasMessageType = messageType.Valid
			row.LayoutPriority = layout.layoutPriority
			if row.ID != "" {
				rows = append(rows, row)
			}
		}
		if err := result.Err(); err != nil {
			result.Close()
			return nil, err
		}
		result.Close()
	}

	// Migration can leave the same message in both layouts. Keep the newest
	// snapshot, preferring V2 when timestamps tie.
	latest := map[string]openCodeMessageRow{}
	for _, row := range rows {
		existing, ok := latest[row.ID]
		if !ok || row.Updated > existing.Updated || (row.Updated == existing.Updated && row.LayoutPriority > existing.LayoutPriority) {
			latest[row.ID] = row
		}
	}
	rows = rows[:0]
	for _, row := range latest {
		rows = append(rows, row)
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Updated == rows[j].Updated {
			return rows[i].ID < rows[j].ID
		}
		return rows[i].Updated < rows[j].Updated
	})
	return rows, nil
}

func parseOpenCodeMessageEvent(ctx Context, sessionID string, row openCodeMessageRow, fallbackTime time.Time) (model.UsageEvent, bool) {
	value, ok := decodeJSONObject(row.Data)
	if !ok {
		return model.UsageEvent{}, false
	}
	if row.HasMessageType {
		if row.MessageType != "assistant" && row.MessageType != "compaction" {
			return model.UsageEvent{}, false
		}
		if role := jsonString(value["role"]); role != "" && role != "assistant" {
			return model.UsageEvent{}, false
		}
	} else if jsonString(value["role"]) != "assistant" {
		return model.UsageEvent{}, false
	}

	tokens, ok := jsonObject(value["tokens"])
	if !ok {
		return model.UsageEvent{}, false
	}
	input := jsonUint64(tokens["input"])
	output := jsonUint64(tokens["output"])
	reasoning := firstJSONUint64(tokens, "reasoning", "reasoningTokens", "reasoning_tokens")
	cacheRead, cacheWrite := uint64(0), uint64(0)
	if cache, ok := jsonObject(tokens["cache"]); ok {
		cacheRead = firstJSONUint64(cache, "read", "readTokens", "cacheRead")
		cacheWrite = firstJSONUint64(cache, "write", "writeTokens", "cacheWrite")
	}
	if input == 0 && output == 0 && reasoning == 0 && cacheRead == 0 && cacheWrite == 0 {
		return model.UsageEvent{}, false
	}

	modelID := firstJSONString(value, "modelID", "model_id")
	if modelID == "" {
		switch typed := value["model"].(type) {
		case string:
			modelID = strings.TrimSpace(typed)
		case map[string]any:
			modelID = firstJSONString(typed, "id", "modelID", "modelId")
		}
	}
	if modelID == "" {
		modelID = "unknown"
	}

	timeValue, _ := jsonObject(value["time"])
	createdMS, hasCreated := parseTimestampMillis(timeValue["created"])
	streamedMS, hasStreamed := parseTimestampMillis(timeValue["streamed"])
	completedMS, hasCompleted := parseTimestampMillis(timeValue["completed"])
	var latency *uint64
	if hasCreated {
		endMS := int64(0)
		if hasStreamed && streamedMS > createdMS {
			endMS = streamedMS
		} else if hasCompleted && completedMS > createdMS {
			endMS = completedMS
		}
		if endMS > createdMS {
			value := uint64(endMS - createdMS)
			latency = &value
		}
	}

	occurredAt := fallbackTime
	if hasCreated {
		occurredAt = time.UnixMilli(createdMS).UTC()
	}
	usage := model.TokenUsage{
		InputTokens:         input,
		OutputTokens:        addUint64Saturated(output, reasoning),
		ReasoningTokens:     reasoning,
		CacheReadTokens:     cacheRead,
		CacheCreationTokens: cacheWrite,
		Model:               modelID,
		MessageID:           row.ID,
	}
	recordID := sessionID + ":" + row.ID
	providerID := firstJSONString(value, "providerID", "providerId")
	if providerID == "" {
		if modelValue, ok := jsonObject(value["model"]); ok {
			providerID = firstJSONString(modelValue, "providerID", "providerId")
		}
	}
	if providerID == "" {
		providerID = "_opencode_session"
	}
	event := newSessionUsageEvent(ctx, openCodeSourceApp, providerID, "OpenCode (Session)", recordID, sessionID, modelID, usage, occurredAt)
	event.LatencyMS = latency
	if latency != nil {
		event.TimingQuality = model.TimingExact
	}
	if cost, exists := value["cost"]; exists {
		event.Metadata["cost"] = cost
		applyReportedCost(&event, cost)
	}
	return event, true
}

func openReadOnlySQLite(path string) (*sql.DB, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve OpenCode database path: %w", err)
	}
	uri := url.URL{Scheme: "file", Path: filepath.ToSlash(absolute)}
	query := uri.Query()
	query.Set("mode", "ro")
	query.Set("_pragma", "busy_timeout(5000)")
	uri.RawQuery = query.Encode()
	db, err := sql.Open("sqlite", uri.String())
	if err != nil {
		return nil, fmt.Errorf("open OpenCode database: %w", err)
	}
	db.SetMaxOpenConns(1)
	return db, nil
}

func findCursor(cursors []Cursor, sourceID, filePath string) (Cursor, bool) {
	for _, cursor := range cursors {
		if cursor.SourceID == sourceID && cursor.FilePath == filePath {
			return cursor, true
		}
	}
	return Cursor{}, false
}

func newSessionUsageEvent(ctx Context, sourceApp, providerID, providerName, recordID, sessionID, modelID string, usage model.TokenUsage, occurredAt time.Time) model.UsageEvent {
	event := model.NewUsageEvent(ctx.DeviceID, model.SourceSession, sourceApp, recordID, modelID, usage, occurredAt)
	event.RequestID = recordID
	event.SessionID = sessionID
	event.ProviderID = providerID
	event.ProviderName = providerName
	event.RequestModel = modelID
	event.PricingModel = modelID
	event.InputTokenSemantics = model.SemanticsFresh
	event.IsStreaming = true
	status := 200
	event.StatusCode = &status
	return event
}

func applyReportedCost(event *model.UsageEvent, value any) {
	if object, ok := jsonObject(value); ok {
		event.Cost.InputCostUSD = jsonDecimalString(object["input"])
		event.Cost.OutputCostUSD = jsonDecimalString(object["output"])
		event.Cost.CacheReadCostUSD = jsonDecimalString(firstObjectValue(object, "cacheRead", "cache_read"))
		event.Cost.CacheCreationCostUSD = jsonDecimalString(firstObjectValue(object, "cacheWrite", "cache_creation"))
		event.Cost.TotalCostUSD = jsonDecimalString(object["total"])
		if event.Cost.TotalCostUSD == "0" {
			total := decimal.RequireFromString(event.Cost.InputCostUSD).
				Add(decimal.RequireFromString(event.Cost.OutputCostUSD)).
				Add(decimal.RequireFromString(event.Cost.CacheReadCostUSD)).
				Add(decimal.RequireFromString(event.Cost.CacheCreationCostUSD))
			event.Cost.TotalCostUSD = total.String()
		}
		return
	}
	event.Cost.TotalCostUSD = jsonDecimalString(value)
}

func jsonDecimalString(value any) string {
	var raw string
	switch typed := value.(type) {
	case json.Number:
		raw = typed.String()
	case string:
		raw = strings.TrimSpace(typed)
	case float64:
		raw = strconv.FormatFloat(typed, 'f', -1, 64)
	case int:
		raw = strconv.Itoa(typed)
	case int64:
		raw = strconv.FormatInt(typed, 10)
	default:
		return "0"
	}
	parsed, err := decimal.NewFromString(raw)
	if err != nil || parsed.IsNegative() {
		return "0"
	}
	return parsed.String()
}

func decodeJSONObject(data []byte) (map[string]any, bool) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, false
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, false
	}
	object, ok := value.(map[string]any)
	return object, ok
}

func canonicalJSON(value any) []byte {
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil
	}
	return encoded
}

func jsonObject(value any) (map[string]any, bool) {
	object, ok := value.(map[string]any)
	return object, ok
}

func firstObjectValue(object map[string]any, keys ...string) any {
	for _, key := range keys {
		if value, ok := object[key]; ok {
			return value
		}
	}
	return nil
}

func jsonString(value any) string {
	typed, _ := value.(string)
	return strings.TrimSpace(typed)
}

func firstJSONString(object map[string]any, keys ...string) string {
	for _, key := range keys {
		if value := jsonString(object[key]); value != "" {
			return value
		}
	}
	return ""
}

func jsonUint64(value any) uint64 {
	switch typed := value.(type) {
	case json.Number:
		parsed, err := strconv.ParseUint(typed.String(), 10, 64)
		if err == nil {
			return parsed
		}
		asFloat, err := typed.Float64()
		if err == nil && asFloat >= 0 && asFloat <= math.MaxUint64 {
			return uint64(asFloat)
		}
	case string:
		parsed, err := strconv.ParseUint(strings.TrimSpace(typed), 10, 64)
		if err == nil {
			return parsed
		}
	case float64:
		if typed >= 0 && typed <= math.MaxUint64 {
			return uint64(typed)
		}
	case int:
		if typed >= 0 {
			return uint64(typed)
		}
	case int64:
		if typed >= 0 {
			return uint64(typed)
		}
	case uint64:
		return typed
	}
	return 0
}

func firstJSONUint64(object map[string]any, keys ...string) uint64 {
	for _, key := range keys {
		if value := jsonUint64(object[key]); value != 0 {
			return value
		}
	}
	return 0
}

func jsonInt64(value any) (int64, bool) {
	switch typed := value.(type) {
	case json.Number:
		parsed, err := typed.Int64()
		return parsed, err == nil
	case string:
		parsed, err := strconv.ParseInt(strings.TrimSpace(typed), 10, 64)
		return parsed, err == nil
	case float64:
		if typed >= math.MinInt64 && typed <= math.MaxInt64 {
			return int64(typed), true
		}
	case int:
		return int64(typed), true
	case int64:
		return typed, true
	}
	return 0, false
}

func jsonFloat64(value any) (float64, bool) {
	switch typed := value.(type) {
	case json.Number:
		parsed, err := typed.Float64()
		return parsed, err == nil
	case string:
		parsed, err := strconv.ParseFloat(strings.TrimSpace(typed), 64)
		return parsed, err == nil
	case float64:
		return typed, true
	case int:
		return float64(typed), true
	case int64:
		return float64(typed), true
	}
	return 0, false
}

func parseTimestampMillis(value any) (int64, bool) {
	timestamp, ok := rawTimestampMillis(value)
	if !ok || timestamp < -62_135_596_800_000 || timestamp > 253_402_300_799_999 {
		return 0, false
	}
	return timestamp, true
}

// Pi-family logs have used both Unix seconds and milliseconds. OpenCode's time
// fields are always milliseconds, so only this parser applies the heuristic.
func parsePiTimestampMillis(value any) (int64, bool) {
	timestamp, ok := rawTimestampMillis(value)
	if !ok {
		return 0, false
	}
	if timestamp >= -100_000_000_000 && timestamp <= 100_000_000_000 {
		if timestamp > math.MaxInt64/1000 || timestamp < math.MinInt64/1000 {
			return 0, false
		}
		timestamp *= 1000
	}
	if timestamp < -62_135_596_800_000 || timestamp > 253_402_300_799_999 {
		return 0, false
	}
	return timestamp, true
}

func rawTimestampMillis(value any) (int64, bool) {
	switch typed := value.(type) {
	case json.Number:
		if parsed, err := typed.Int64(); err == nil {
			return parsed, true
		}
		parsed, err := typed.Float64()
		if err != nil || parsed < math.MinInt64 || parsed > math.MaxInt64 {
			return 0, false
		}
		return int64(parsed), true
	case string:
		raw := strings.TrimSpace(typed)
		if raw == "" {
			return 0, false
		}
		if parsed, err := strconv.ParseInt(raw, 10, 64); err == nil {
			return parsed, true
		}
		parsedTime, err := time.Parse(time.RFC3339Nano, raw)
		if err != nil {
			return 0, false
		}
		return parsedTime.UnixMilli(), true
	default:
		return jsonInt64(value)
	}
}

func addUint64Saturated(a, b uint64) uint64 {
	if math.MaxUint64-a < b {
		return math.MaxUint64
	}
	return a + b
}

func hashBytes(value []byte) string {
	digest := sha256.Sum256(value)
	return hex.EncodeToString(digest[:])
}
