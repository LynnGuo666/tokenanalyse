package collectors

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"tokenanalyse/internal/model"

	"github.com/shopspring/decimal"
	_ "modernc.org/sqlite"
)

const (
	mcodeSourceID      = "mcode"
	mcodeAppType       = "mcode"
	mcodeCursorVersion = 1
)

// MCodeCollector imports MCode's committed token-usage projection. The source
// database is always opened read-only; collection never writes to it.
type MCodeCollector struct{}

var _ Collector = (*MCodeCollector)(nil)

// NewMCodeCollector creates an MCode collector.
func NewMCodeCollector() *MCodeCollector { return &MCodeCollector{} }

func (c *MCodeCollector) Info() Info {
	return Info{
		ID:          mcodeSourceID,
		Name:        "MCode",
		AppTypes:    []string{mcodeAppType},
		Description: "Reads MCode's committed local runtime token usage without modifying its SQLite database",
		Incremental: true,
	}
}

type mcodeCursorState struct {
	Version int   `json:"version"`
	Size    int64 `json:"size"`
}

type mcodeUsageRow struct {
	ID          int64
	SessionID   string
	TurnID      string
	Model       string
	TimestampMS int64
	Input       uint64
	Output      uint64
	Reasoning   uint64
	CacheRead   uint64
	CacheWrite  uint64
	NativeCost  *float64
}

func (c *MCodeCollector) Collect(ctx Context, cursors []Cursor) (Result, error) {
	result := Result{Metadata: map[string]any{"source": mcodeSourceID}}
	path := mcodeDatabasePath(ctx)
	if path == "" {
		return result, nil
	}
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return result, nil
		}
		result.Errors = append(result.Errors, fmt.Sprintf("MCode database %s: %v", path, err))
		return result, nil
	}
	if !info.Mode().IsRegular() {
		return result, nil
	}

	cursor := Cursor{SourceID: mcodeSourceID, FilePath: path}
	for _, candidate := range cursors {
		if candidate.SourceID == mcodeSourceID && filepath.Clean(candidate.FilePath) == filepath.Clean(path) {
			cursor = candidate
			break
		}
	}
	if cursor.Offset < 0 {
		cursor.Offset = 0
	}

	source, err := mcodeOpenReadOnly(path)
	if err != nil {
		result.Errors = append(result.Errors, fmt.Sprintf("MCode database %s: %v", path, err))
		return result, nil
	}
	defer source.Close()

	exists, err := mcodeTableExists(source, "local_runtime_token_usage")
	if err != nil {
		result.Errors = append(result.Errors, fmt.Sprintf("MCode database %s: %v", path, err))
		return result, nil
	}
	if !exists {
		return result, nil
	}
	columns, err := mcodeTableColumns(source, "local_runtime_token_usage")
	if err != nil {
		result.Errors = append(result.Errors, fmt.Sprintf("MCode database %s: %v", path, err))
		return result, nil
	}
	for _, required := range []string{"id", "session_id"} {
		if !columns[required] {
			result.Errors = append(result.Errors, fmt.Sprintf("MCode database %s: local_runtime_token_usage is missing %s", path, required))
			return result, nil
		}
	}

	tx, err := source.BeginTx(context.Background(), &sql.TxOptions{ReadOnly: true})
	if err != nil {
		result.Errors = append(result.Errors, fmt.Sprintf("MCode database %s: %v", path, err))
		return result, nil
	}
	defer tx.Rollback()

	selectParts := []string{"id", "session_id"}
	selectParts = append(selectParts, mcodeColumnOrNull(columns, "turn_id"))
	selectParts = append(selectParts, mcodeColumnOrNull(columns, "model"))
	selectParts = append(selectParts, mcodeColumnOrZero(columns, "ts"))
	for _, column := range []string{"input_tokens", "output_tokens", "reasoning_tokens", "cache_read_tokens", "cache_write_tokens"} {
		selectParts = append(selectParts, mcodeColumnOrZero(columns, column))
	}
	selectParts = append(selectParts, mcodeColumnOrNull(columns, "cost_usd"))
	query := "SELECT " + strings.Join(selectParts, ",") + " FROM local_runtime_token_usage WHERE id > ? ORDER BY id"
	rows, err := tx.Query(query, cursor.Offset)
	if err != nil {
		result.Errors = append(result.Errors, fmt.Sprintf("MCode usage query: %v", err))
		return result, nil
	}

	lastID := cursor.Offset
	for rows.Next() {
		row, scanErr := mcodeScanUsage(rows)
		if scanErr != nil {
			result.Errors = append(result.Errors, fmt.Sprintf("MCode usage row: %v", scanErr))
			continue
		}
		lastID = row.ID
		modelName := row.Model
		modelRaw := row.Model
		if modelName == "" {
			modelName = mcodeHistoryModel(tx, row.SessionID, row.TimestampMS)
			modelRaw = modelName
		} else {
			modelName = mcodeStripProviderNamespace(modelName)
		}
		if modelName == "" {
			modelName = "unknown"
		}

		output := mcodeSaturatingAdd(row.Output, row.Reasoning)
		usage := model.TokenUsage{
			InputTokens:         row.Input,
			OutputTokens:        output,
			ReasoningTokens:     row.Reasoning,
			CacheReadTokens:     row.CacheRead,
			CacheCreationTokens: row.CacheWrite,
			Model:               modelName,
		}
		event := mcodeNewEvent(ctx, row, usage, modelName, modelRaw)
		if latency, firstToken, ok := mcodeTiming(tx, row); ok {
			event.LatencyMS = &latency
			event.FirstTokenMS = &firstToken
			event.DurationMS = &latency
			generation := latency
			if firstToken <= latency {
				generation = latency - firstToken
				event.GenerationDurationMS = &generation
				if generation > 0 {
					speed := float64(output) / (float64(generation) / 1000)
					event.SpeedTPS = &speed
				}
			}
			event.TimingQuality = model.TimingExact
		}
		if row.NativeCost != nil && !math.IsNaN(*row.NativeCost) && !math.IsInf(*row.NativeCost, 0) && *row.NativeCost >= 0 {
			event.Cost.TotalCostUSD = decimal.NewFromFloat(*row.NativeCost).String()
		}
		result.Events = append(result.Events, event)
	}
	if err := rows.Err(); err != nil {
		result.Errors = append(result.Errors, fmt.Sprintf("MCode usage rows: %v", err))
	}
	rows.Close()

	state, _ := json.Marshal(mcodeCursorState{Version: mcodeCursorVersion, Size: info.Size()})
	result.FilesScanned = 1
	result.Cursors = append(result.Cursors, Cursor{
		SourceID:      mcodeSourceID,
		FilePath:      path,
		Offset:        lastID,
		ModifiedNanos: info.ModTime().UnixNano(),
		State:         string(state),
		UpdatedAt:     time.Now().UTC(),
	})
	return result, nil
}

func mcodeDatabasePath(ctx Context) string {
	bases := make([]string, 0, 6)
	if value := strings.TrimSpace(ctx.Paths.MCode); value != "" {
		bases = append(bases, value)
	}
	for _, key := range []string{"MCODE_HOME", "MINIMAX_DATA_DIR", "MAVIS_DATA_DIR"} {
		if value := strings.TrimSpace(ctx.Env[key]); value != "" {
			bases = append(bases, value)
		}
	}
	if strings.TrimSpace(ctx.Home) != "" {
		bases = append(bases, filepath.Join(ctx.Home, ".mcode"), filepath.Join(ctx.Home, ".minimax"))
	}
	seen := map[string]struct{}{}
	for _, base := range bases {
		base = filepath.Clean(base)
		if _, exists := seen[base]; exists {
			continue
		}
		seen[base] = struct{}{}
		if info, err := os.Stat(base); err == nil && info.Mode().IsRegular() {
			return base
		}
		for _, relative := range []string{
			filepath.Join("v2", "sqlite", "runtime-state.sqlite"),
			filepath.Join("sqlite", "runtime-state.sqlite"),
			"runtime-state.sqlite",
		} {
			candidate := filepath.Join(base, relative)
			if info, err := os.Stat(candidate); err == nil && info.Mode().IsRegular() {
				return candidate
			}
		}
	}
	return ""
}

func mcodeOpenReadOnly(path string) (*sql.DB, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	location := (&url.URL{Scheme: "file", Path: filepath.ToSlash(absolute)})
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

func mcodeTableExists(database *sql.DB, table string) (bool, error) {
	var name string
	err := database.QueryRow("SELECT name FROM sqlite_master WHERE type='table' AND name=?", table).Scan(&name)
	if err == sql.ErrNoRows {
		return false, nil
	}
	return err == nil, err
}

func mcodeTableColumns(database *sql.DB, table string) (map[string]bool, error) {
	rows, err := database.Query(`PRAGMA table_info("` + table + `")`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	columns := map[string]bool{}
	for rows.Next() {
		var cid int
		var name, columnType string
		var notNull, primaryKey int
		var defaultValue any
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			return nil, err
		}
		columns[strings.ToLower(name)] = true
	}
	return columns, rows.Err()
}

func mcodeColumnOrNull(columns map[string]bool, column string) string {
	if columns[column] {
		return column
	}
	return "NULL"
}

func mcodeColumnOrZero(columns map[string]bool, column string) string {
	if columns[column] {
		return column
	}
	return "0"
}

func mcodeScanUsage(rows *sql.Rows) (mcodeUsageRow, error) {
	var row mcodeUsageRow
	var turnID, modelName sql.NullString
	var timestamp, input, output, reasoning, cacheRead, cacheWrite sql.NullInt64
	var nativeCost any
	err := rows.Scan(
		&row.ID,
		&row.SessionID,
		&turnID,
		&modelName,
		&timestamp,
		&input,
		&output,
		&reasoning,
		&cacheRead,
		&cacheWrite,
		&nativeCost,
	)
	if err != nil {
		return row, err
	}
	row.TurnID = turnID.String
	row.Model = strings.TrimSpace(modelName.String)
	row.TimestampMS = timestamp.Int64
	row.Input = mcodeNonNegative(input.Int64)
	row.Output = mcodeNonNegative(output.Int64)
	row.Reasoning = mcodeNonNegative(reasoning.Int64)
	row.CacheRead = mcodeNonNegative(cacheRead.Int64)
	row.CacheWrite = mcodeNonNegative(cacheWrite.Int64)
	if value, ok := mcodeFloat(nativeCost); ok {
		row.NativeCost = &value
	}
	return row, nil
}

func mcodeHistoryModel(queryable interface {
	Query(query string, args ...any) (*sql.Rows, error)
}, sessionID string, timestampMS int64) string {
	if rows, err := queryable.Query(`SELECT data_json FROM local_runtime_pi_history_rows WHERE session_id=? AND created_at_ms=? AND role='assistant'`, sessionID, timestampMS); err == nil {
		defer rows.Close()
		for rows.Next() {
			var data string
			if rows.Scan(&data) == nil {
				if modelName := mcodeModelFromJSON([]byte(data)); modelName != "" {
					return modelName
				}
			}
		}
	}
	rows, err := queryable.Query(`SELECT pi_history_json FROM local_runtime_messages WHERE session_id=?`, sessionID)
	if err != nil {
		return ""
	}
	defer rows.Close()
	for rows.Next() {
		var data string
		if rows.Scan(&data) != nil {
			continue
		}
		var messages []any
		if json.Unmarshal([]byte(data), &messages) != nil {
			continue
		}
		for _, value := range messages {
			message, _ := value.(map[string]any)
			if mcodeString(message["role"]) != "assistant" {
				continue
			}
			if timestamp, ok := mcodeJSONInt(message["timestamp"]); ok && timestamp == timestampMS {
				if modelName := mcodeString(message["model"]); strings.TrimSpace(modelName) != "" {
					return strings.TrimSpace(modelName)
				}
			}
		}
	}
	return ""
}

func mcodeModelFromJSON(data []byte) string {
	var value map[string]any
	if json.Unmarshal(data, &value) != nil {
		return ""
	}
	return strings.TrimSpace(mcodeString(value["model"]))
}

func mcodeTiming(queryable interface {
	Query(query string, args ...any) (*sql.Rows, error)
}, usage mcodeUsageRow) (uint64, uint64, bool) {
	query := `SELECT data_json FROM local_runtime_message_rows WHERE session_id=? AND role='assistant' AND created_at_ms>=?`
	args := []any{usage.SessionID, usage.TimestampMS}
	if usage.TurnID != "" {
		query += ` AND turn_id=?`
		args = append(args, usage.TurnID)
	}
	query += ` ORDER BY created_at_ms LIMIT 32`
	rows, err := queryable.Query(query, args...)
	if err != nil {
		return 0, 0, false
	}
	defer rows.Close()
	for rows.Next() {
		var data string
		if rows.Scan(&data) != nil {
			continue
		}
		var message map[string]any
		if json.Unmarshal([]byte(data), &message) != nil {
			continue
		}
		messageUsage, _ := message["usage"].(map[string]any)
		if messageUsage == nil {
			continue
		}
		input, inputOK := mcodeJSONInt(messageUsage["input_tokens"])
		output, outputOK := mcodeJSONInt(messageUsage["output_tokens"])
		if !inputOK || !outputOK || input < 0 || output < 0 || uint64(input) != usage.Input || uint64(output) != usage.Output {
			continue
		}
		request, requestOK := mcodeJSONFloat(messageUsage["request_duration_ms"])
		if !requestOK || math.IsNaN(request) || math.IsInf(request, 0) || request <= 0 {
			continue
		}
		latency := uint64(math.Round(request))
		firstToken := uint64(0)
		if decode, ok := mcodeJSONFloat(messageUsage["decode_duration_ms"]); ok && !math.IsNaN(decode) && !math.IsInf(decode, 0) && decode >= 0 && decode <= request {
			firstToken = uint64(math.Round(request - decode))
		}
		return latency, firstToken, true
	}
	return 0, 0, false
}

func mcodeNewEvent(ctx Context, row mcodeUsageRow, usage model.TokenUsage, modelName, modelRaw string) model.UsageEvent {
	sourceRecord := fmt.Sprintf("mcode:%s:%d", row.SessionID, row.ID)
	event := model.NewUsageEvent(ctx.DeviceID, model.SourceSession, mcodeAppType, sourceRecord, modelName, usage, time.UnixMilli(row.TimestampMS).UTC())
	event.RequestID = sourceRecord
	event.SessionID = row.SessionID
	event.ProviderID = "_mcode_session"
	event.ProviderName = "MCode session log"
	event.RequestModel = modelName
	if modelRaw != "" && modelRaw != modelName {
		event.ModelRaw = modelRaw
		event.PricingModel = modelName
	}
	event.CanonicalModel = model.NormalizeModelID(modelName)
	event.InputTokenSemantics = model.SemanticsFresh
	event.IsStreaming = true
	status := 200
	event.StatusCode = &status
	event.Metadata = map[string]any{
		"rowId":       row.ID,
		"turnId":      row.TurnID,
		"nativeModel": row.Model,
		"reasoning":   row.Reasoning,
		"inputFresh":  true,
		"costSource":  "native",
		"projection":  "local_runtime_token_usage",
		"readOnly":    true,
	}
	if row.NativeCost == nil {
		event.Metadata["costSource"] = "unknown"
	}
	return event
}

func mcodeStripProviderNamespace(modelName string) string {
	modelName = strings.TrimSpace(modelName)
	if index := strings.IndexByte(modelName, '/'); index >= 0 && index+1 < len(modelName) {
		return strings.TrimSpace(modelName[index+1:])
	}
	return modelName
}

func mcodeSaturatingAdd(left, right uint64) uint64 {
	if math.MaxUint64-left < right {
		return math.MaxUint64
	}
	return left + right
}

func mcodeNonNegative(value int64) uint64 {
	if value <= 0 {
		return 0
	}
	return uint64(value)
}

func mcodeFloat(value any) (float64, bool) {
	switch typed := value.(type) {
	case float64:
		return typed, true
	case float32:
		return float64(typed), true
	case int64:
		return float64(typed), true
	case int:
		return float64(typed), true
	case []byte:
		parsed, err := strconv.ParseFloat(strings.TrimSpace(string(typed)), 64)
		return parsed, err == nil
	case string:
		parsed, err := strconv.ParseFloat(strings.TrimSpace(typed), 64)
		return parsed, err == nil
	default:
		return 0, false
	}
}

func mcodeJSONFloat(value any) (float64, bool) {
	switch typed := value.(type) {
	case float64:
		return typed, true
	case json.Number:
		parsed, err := typed.Float64()
		return parsed, err == nil
	case string:
		parsed, err := strconv.ParseFloat(strings.TrimSpace(typed), 64)
		return parsed, err == nil
	default:
		return 0, false
	}
}

func mcodeJSONInt(value any) (int64, bool) {
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
		parsed, err := strconv.ParseInt(strings.TrimSpace(typed), 10, 64)
		return parsed, err == nil
	}
	return 0, false
}

func mcodeString(value any) string {
	text, _ := value.(string)
	return text
}
