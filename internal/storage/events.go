package storage

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"tokenanalyse/internal/model"

	"github.com/shopspring/decimal"
)

type InsertResult struct {
	Inserted int `json:"inserted"`
	Skipped  int `json:"skipped"`
}

type EventCost struct {
	EventID        string `json:"eventId"`
	CostUSD        string `json:"costUsd"`
	CostCNY        string `json:"costCny"`
	PricingVersion string `json:"pricingVersion"`
}

// InsertEvents stores immutable usage facts. When enqueue is true, newly
// inserted events are added to the upstream outbox. Remote events pulled from
// an upstream must call this with enqueue=false to avoid sync loops.
func (db *DB) InsertEvents(events []model.UsageEvent, enqueue bool) (InsertResult, error) {
	var result InsertResult
	if len(events) == 0 {
		return result, nil
	}
	tx, err := db.conn.Begin()
	if err != nil {
		return result, err
	}
	defer tx.Rollback()

	activeVersion, err := db.activePricingVersionTx(tx)
	if err != nil {
		return result, err
	}
	exchangeRate, err := db.cnyExchangeRateTx(tx)
	if err != nil {
		return result, err
	}
	pricingCache := map[string]model.ModelPricing{}

	for _, event := range events {
		if event.ModelRaw == "" {
			event.ModelRaw = event.Model
		}
		canonical := event.CanonicalModel
		if canonical == "" || canonical == event.Model {
			canonical, err = db.resolveModelTx(tx, event.SourceApp, event.Model)
			if err != nil {
				return result, err
			}
		}
		event.CanonicalModel = canonical
		if event.PricingModel == "" {
			event.PricingModel = canonical
		}
		versionID := event.PricingVersion
		if versionID == "" {
			versionID = activeVersion
		}
		var pricing model.ModelPricing
		found := false
		if versionID != "" {
			cacheKey := versionID + "\x00" + event.PricingModel
			if cached, ok := pricingCache[cacheKey]; ok {
				pricing, found = cached, true
			} else {
				for _, candidate := range uniqueStrings(event.PricingModel, canonical, event.Model) {
					pricing, found, err = db.pricingForModelTx(tx, versionID, candidate)
					if err != nil {
						return result, err
					}
					if found {
						break
					}
				}
				pricingCache[cacheKey] = pricing
			}
		}

		freshInput := event.Usage.FreshInput(event.SourceApp, event.InputTokenSemantics)
		realTotal := event.Usage.RealTotal(event.SourceApp, event.InputTokenSemantics)
		breakdown := model.CostBreakdown{
			InputCost: decimal.Zero, OutputCost: decimal.Zero, CacheReadCost: decimal.Zero,
			CacheCreationCost: decimal.Zero, TotalCost: decimal.Zero,
		}
		if found {
			multiplier := decimal.RequireFromString(event.CostMultiplier)
			breakdown = model.CalculateCost(event.SourceApp, event.Usage, pricing, event.InputTokenSemantics, multiplier)
		}
		costSnapshot := buildCostSnapshot(breakdown, pricing.Currency, exchangeRate, versionID)
		event.Cost = costSnapshot
		event.PricingVersion = versionID
		if event.Usage.CacheCreation5MTokens == 0 && event.Usage.CacheCreationTokens > event.Usage.CacheCreation1HTokens {
			event.Usage.CacheCreation5MTokens = event.Usage.CacheCreationTokens - event.Usage.CacheCreation1HTokens
		}
		if event.DurationMS == nil && event.LatencyMS != nil {
			duration := *event.LatencyMS
			event.DurationMS = &duration
		}
		if event.GenerationDurationMS == nil && event.LatencyMS != nil && event.FirstTokenMS != nil && *event.LatencyMS > *event.FirstTokenMS {
			generation := *event.LatencyMS - *event.FirstTokenMS
			event.GenerationDurationMS = &generation
		}
		if event.SpeedTPS == nil && event.GenerationDurationMS != nil && *event.GenerationDurationMS > 0 && event.Usage.OutputTokens > 0 {
			speed := float64(event.Usage.OutputTokens) * 1000 / float64(*event.GenerationDurationMS)
			event.SpeedTPS = &speed
		}

		metadata, err := json.Marshal(event.Metadata)
		if err != nil {
			return result, err
		}
		if string(metadata) == "null" {
			metadata = []byte("{}")
		}
		placeholders := strings.TrimSuffix(strings.Repeat("?,", 50), ",")
		insert, err := tx.Exec(`
			INSERT OR IGNORE INTO usage_events(
				event_id, device_id, source, source_app, source_record_id, request_id, session_id,
				provider_id, provider_name, model, model_raw, canonical_model, request_model, pricing_model,
				occurred_at, ingested_at, input_tokens, output_tokens, reasoning_tokens, cache_read_tokens,
				cache_creation_tokens, cache_creation_5m_tokens, cache_creation_1h_tokens, usage_model, message_id,
				input_token_semantics, fresh_input_tokens, real_total_tokens,
				latency_ms, first_token_ms, duration_ms, generation_duration_ms, speed_tps, timing_quality,
				status_code, error_message, is_streaming, cost_multiplier, pricing_version, cost_usd, cost_cny,
				input_cost_usd, output_cost_usd, cache_read_cost_usd, cache_creation_cost_usd,
				input_cost_cny, output_cost_cny, cache_read_cost_cny, cache_creation_cost_cny, metadata
			) VALUES (`+placeholders+`)`,
			event.EventID, event.DeviceID, event.Source, event.SourceApp, event.SourceRecordID,
			event.RequestID, event.SessionID, event.ProviderID, event.ProviderName, event.Model,
			event.ModelRaw, event.CanonicalModel, event.RequestModel, event.PricingModel,
			formatTime(event.OccurredAt), formatTime(event.IngestedAt),
			uint64ToInt64(event.Usage.InputTokens), uint64ToInt64(event.Usage.OutputTokens),
			uint64ToInt64(event.Usage.ReasoningTokens), uint64ToInt64(event.Usage.CacheReadTokens),
			uint64ToInt64(event.Usage.CacheCreationTokens), uint64ToInt64(event.Usage.CacheCreation5MTokens),
			uint64ToInt64(event.Usage.CacheCreation1HTokens), event.Usage.Model, event.Usage.MessageID,
			int(event.InputTokenSemantics), uint64ToInt64(freshInput), uint64ToInt64(realTotal),
			nullableUint64(event.LatencyMS), nullableUint64(event.FirstTokenMS), nullableUint64(event.DurationMS),
			nullableUint64(event.GenerationDurationMS), event.SpeedTPS, event.TimingQuality,
			event.StatusCode, event.ErrorMessage, boolToInt(event.IsStreaming), event.CostMultiplier,
			versionID, costSnapshot.TotalCostUSD, costSnapshot.TotalCostCNY,
			costSnapshot.InputCostUSD, costSnapshot.OutputCostUSD, costSnapshot.CacheReadCostUSD,
			costSnapshot.CacheCreationCostUSD, costSnapshot.InputCostCNY, costSnapshot.OutputCostCNY,
			costSnapshot.CacheReadCostCNY, costSnapshot.CacheCreationCostCNY, string(metadata))
		if err != nil {
			return result, fmt.Errorf("insert event: %w", err)
		}
		rows, _ := insert.RowsAffected()
		if rows == 0 {
			result.Skipped++
			continue
		}
		result.Inserted++

		if enqueue {
			syncEvent := event
			syncEvent.Cost = model.CostSnapshot{}
			syncEvent.PricingVersion = ""
			payload, err := json.Marshal(syncEvent)
			if err != nil {
				return result, err
			}
			if _, err := tx.Exec(`INSERT INTO outbox(event_id, device_id, event_type, target, payload, status, attempts, created_at)
				VALUES (?, ?, 'usage_event', 'upstream', ?, 'pending', 0, ?)`,
				event.EventID, event.DeviceID, string(payload), formatTime(time.Now().UTC())); err != nil {
				return result, err
			}
		}
		if _, err := tx.Exec(`INSERT INTO sync_state(device_id, last_event_at, updated_at)
			VALUES (?, ?, ?)
			ON CONFLICT(device_id) DO UPDATE SET
				last_event_at=MAX(COALESCE(sync_state.last_event_at, ''), excluded.last_event_at),
				updated_at=excluded.updated_at`,
			event.DeviceID, formatTime(event.OccurredAt), formatTime(time.Now().UTC())); err != nil {
			return result, err
		}
	}
	return result, tx.Commit()
}

type eventFact struct {
	event   model.UsageEvent
	costUSD decimal.Decimal
	costCNY decimal.Decimal
}

func (db *DB) loadFacts(filters model.UsageFilters) ([]eventFact, error) {
	where, args := buildFilters(filters)
	activeVersion, _ := db.ActivePricingVersion()
	exchangeRate, _ := db.CNYExchangeRate()
	pricingCatalog := db.loadPricingCatalog(activeVersion)
	query := `SELECT ` + eventColumns + ` FROM usage_events` + where + ` ORDER BY occurred_at ASC`
	rows, err := db.conn.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var facts []eventFact
	for rows.Next() {
		fact, err := scanEventFact(rows)
		if err != nil {
			return nil, err
		}
		fact = db.repriceFact(fact, activeVersion, exchangeRate, pricingCatalog)
		facts = append(facts, fact)
	}
	return facts, rows.Err()
}

func (db *DB) loadPricingCatalog(versionID string) map[string]model.ModelPricing {
	catalog := map[string]model.ModelPricing{}
	if versionID == "" {
		return catalog
	}
	models, _ := db.ListModelPricingForVersion(versionID)
	for _, entry := range models {
		catalog[entry.ModelID] = entry
		catalog[model.NormalizeModelID(entry.ModelID)] = entry
	}
	return catalog
}

func (db *DB) repriceFact(fact eventFact, versionID string, rate decimal.Decimal, catalog map[string]model.ModelPricing) eventFact {
	if versionID == "" {
		return fact
	}
	var pricing model.ModelPricing
	found := false
	for _, candidate := range uniqueStrings(fact.event.PricingModel, fact.event.CanonicalModel, fact.event.Model) {
		if entry, ok := catalog[candidate]; ok {
			pricing, found = entry, true
			break
		}
		if entry, ok := catalog[model.NormalizeModelID(candidate)]; ok {
			pricing, found = entry, true
			break
		}
	}
	if !found {
		return fact
	}
	breakdown := model.CalculateCost(fact.event.SourceApp, fact.event.Usage, pricing,
		fact.event.InputTokenSemantics, decimal.RequireFromString(fact.event.CostMultiplier))
	fact.costUSD, fact.costCNY = breakdown.TotalCost, breakdown.TotalCost
	if pricing.Currency == model.CurrencyCNY {
		fact.costUSD = breakdown.TotalCost.Div(rate)
	} else {
		fact.costCNY = breakdown.TotalCost.Mul(rate)
	}
	fact.event.Cost = buildCostSnapshot(breakdown, pricing.Currency, rate, versionID)
	fact.event.PricingVersion = versionID
	return fact
}

func (db *DB) GetSummary(filters model.UsageFilters) (model.UsageSummary, error) {
	facts, err := db.loadFacts(filters)
	if err != nil {
		return model.UsageSummary{}, err
	}
	return summarizeFacts(facts), nil
}

func summarizeFacts(facts []eventFact) model.UsageSummary {
	summary := model.UsageSummary{}
	cost := decimal.Zero
	success := uint64(0)
	for _, fact := range facts {
		event := fact.event
		summary.TotalRequests++
		if event.StatusCode != nil && *event.StatusCode >= 200 && *event.StatusCode < 300 {
			success++
		}
		fresh := event.Usage.FreshInput(event.SourceApp, event.InputTokenSemantics)
		summary.TotalInputTokens += fresh
		summary.TotalOutputTokens += event.Usage.OutputTokens
		summary.TotalCacheCreationTokens += event.Usage.CacheCreationTokens
		summary.TotalCacheReadTokens += event.Usage.CacheReadTokens
		summary.RealTotalTokens += event.Usage.RealTotal(event.SourceApp, event.InputTokenSemantics)
		cost = cost.Add(fact.costUSD)
	}
	if summary.TotalRequests > 0 {
		summary.SuccessRate = float64(success) / float64(summary.TotalRequests) * 100
	}
	cacheable := summary.TotalInputTokens + summary.TotalCacheCreationTokens + summary.TotalCacheReadTokens
	if cacheable > 0 {
		summary.CacheHitRate = float64(summary.TotalCacheReadTokens) / float64(cacheable)
	}
	summary.TotalCost = cost.String()
	return summary
}

func (db *DB) GetDailyTrends(filters model.UsageFilters, bucket string) ([]model.DailyStats, error) {
	facts, err := db.loadFacts(filters)
	if err != nil {
		return nil, err
	}
	groups := map[string][]eventFact{}
	for _, fact := range facts {
		key := bucketKey(fact.event.OccurredAt, bucket)
		groups[key] = append(groups[key], fact)
	}
	keys := make([]string, 0, len(groups))
	for key := range groups {
		keys = append(keys, key)
	}
	sortStrings(keys)
	result := make([]model.DailyStats, 0, len(keys))
	for _, key := range keys {
		summary := summarizeFacts(groups[key])
		result = append(result, model.DailyStats{
			Date: key, RequestCount: summary.TotalRequests, TotalCost: summary.TotalCost,
			TotalTokens: summary.RealTotalTokens, TotalInputTokens: summary.TotalInputTokens,
			TotalOutputTokens:        summary.TotalOutputTokens,
			TotalCacheCreationTokens: summary.TotalCacheCreationTokens,
			TotalCacheReadTokens:     summary.TotalCacheReadTokens,
		})
	}
	return result, nil
}

func (db *DB) GetProviderStats(filters model.UsageFilters) ([]model.ProviderStats, error) {
	facts, err := db.loadFacts(filters)
	if err != nil {
		return nil, err
	}
	type key struct{ provider, app string }
	groups := map[key][]eventFact{}
	names := map[key]string{}
	for _, fact := range facts {
		k := key{fact.event.ProviderID, fact.event.SourceApp}
		groups[k] = append(groups[k], fact)
		if names[k] == "" {
			names[k] = fact.event.ProviderName
		}
	}
	keys := make([]key, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].provider == keys[j].provider {
			return keys[i].app < keys[j].app
		}
		return keys[i].provider < keys[j].provider
	})
	result := make([]model.ProviderStats, 0, len(keys))
	for _, k := range keys {
		summary := summarizeFacts(groups[k])
		avg := averageLatency(groups[k])
		result = append(result, model.ProviderStats{
			ProviderID: k.provider, ProviderName: names[k], AppType: k.app,
			RequestCount: summary.TotalRequests, TotalTokens: summary.RealTotalTokens,
			TotalCost: summary.TotalCost, SuccessRate: summary.SuccessRate, AvgLatencyMS: avg,
		})
	}
	return result, nil
}

func (db *DB) GetModelStats(filters model.UsageFilters) ([]model.ModelStats, error) {
	facts, err := db.loadFacts(filters)
	if err != nil {
		return nil, err
	}
	groups := map[string][]eventFact{}
	for _, fact := range facts {
		modelName := fact.event.CanonicalModel
		if modelName == "" {
			modelName = fact.event.Model
		}
		groups[modelName] = append(groups[modelName], fact)
	}
	keys := make([]string, 0, len(groups))
	for key := range groups {
		keys = append(keys, key)
	}
	sortStrings(keys)
	result := make([]model.ModelStats, 0, len(keys))
	for _, key := range keys {
		summary := summarizeFacts(groups[key])
		result = append(result, model.ModelStats{
			Model: key, RequestCount: summary.TotalRequests, TotalTokens: summary.RealTotalTokens,
			TotalCost: summary.TotalCost, SuccessRate: summary.SuccessRate,
			AvgLatencyMS: averageLatency(groups[key]),
		})
	}
	return result, nil
}

type RequestLogPage struct {
	Items      []RequestLogItem `json:"items"`
	Total      int64            `json:"total"`
	Page       int              `json:"page"`
	PageSize   int              `json:"pageSize"`
	TotalPages int              `json:"totalPages"`
}

type RequestLogItem struct {
	model.UsageEvent
	DeviceName      string `json:"deviceName,omitempty"`
	RealTotalTokens uint64 `json:"realTotalTokens"`
	TotalCostUSD    string `json:"totalCostUsd"`
	TotalCostCNY    string `json:"totalCostCny"`
}

func (db *DB) GetRequestLogs(filters model.UsageFilters, page, pageSize int) (RequestLogPage, error) {
	if page < 1 {
		page = 1
	}
	if pageSize < 1 {
		pageSize = 50
	}
	if pageSize > 1000 {
		pageSize = 1000
	}
	where, args := buildFilters(filters)
	var total int64
	if err := db.conn.QueryRow(`SELECT COUNT(*) FROM usage_events`+where, args...).Scan(&total); err != nil {
		return RequestLogPage{}, err
	}
	queryArgs := append([]any{}, args...)
	queryArgs = append(queryArgs, pageSize, (page-1)*pageSize)
	activeVersion, _ := db.ActivePricingVersion()
	exchangeRate, _ := db.CNYExchangeRate()
	pricingCatalog := db.loadPricingCatalog(activeVersion)
	rows, err := db.conn.Query(`SELECT `+eventColumns+` FROM usage_events`+where+
		` ORDER BY occurred_at DESC, ingested_at DESC, event_id DESC LIMIT ? OFFSET ?`, queryArgs...)
	if err != nil {
		return RequestLogPage{}, err
	}
	defer rows.Close()
	items := []RequestLogItem{}
	for rows.Next() {
		fact, err := scanEventFact(rows)
		if err != nil {
			return RequestLogPage{}, err
		}
		fact = db.repriceFact(fact, activeVersion, exchangeRate, pricingCatalog)
		items = append(items, RequestLogItem{
			UsageEvent:      fact.event,
			RealTotalTokens: fact.event.Usage.RealTotal(fact.event.SourceApp, fact.event.InputTokenSemantics),
			TotalCostUSD:    fact.costUSD.String(), TotalCostCNY: fact.costCNY.String(),
		})
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return RequestLogPage{}, err
	}
	rows.Close()
	devices, err := db.ListDevices()
	if err != nil {
		return RequestLogPage{}, err
	}
	names := map[string]string{}
	for _, device := range devices {
		names[device.DeviceID] = device.Name
	}
	for i := range items {
		items[i].DeviceName = names[items[i].DeviceID]
	}
	totalPages := int((total + int64(pageSize) - 1) / int64(pageSize))
	return RequestLogPage{Items: items, Total: total, Page: page, PageSize: pageSize, TotalPages: totalPages}, nil
}

func (db *DB) GetRequestDetail(eventID string) (RequestLogItem, error) {
	row := db.conn.QueryRow(`SELECT `+eventColumns+` FROM usage_events WHERE event_id=?`, eventID)
	fact, err := scanEventFact(row)
	if err == sql.ErrNoRows {
		return RequestLogItem{}, fmt.Errorf("request not found: %s", eventID)
	}
	if err != nil {
		return RequestLogItem{}, err
	}
	activeVersion, _ := db.ActivePricingVersion()
	exchangeRate, _ := db.CNYExchangeRate()
	pricingCatalog := db.loadPricingCatalog(activeVersion)
	fact = db.repriceFact(fact, activeVersion, exchangeRate, pricingCatalog)
	item := RequestLogItem{UsageEvent: fact.event,
		RealTotalTokens: fact.event.Usage.RealTotal(fact.event.SourceApp, fact.event.InputTokenSemantics),
		TotalCostUSD:    fact.costUSD.String(), TotalCostCNY: fact.costCNY.String()}
	if device, err := db.GetDevice(fact.event.DeviceID); err == nil {
		item.DeviceName = device.Name
	}
	return item, nil
}

type rowIDScanner struct {
	rows *sql.Rows
	id   int64
}

func (r *rowIDScanner) Scan(dest ...any) error {
	args := make([]any, 0, len(dest)+1)
	args = append(args, &r.id)
	args = append(args, dest...)
	return r.rows.Scan(args...)
}

func (db *DB) GetEventsAfterRowID(after int64, limit int) ([]model.UsageEvent, int64, error) {
	if limit <= 0 {
		limit = 1000
	}
	rows, err := db.conn.Query(`SELECT rowid, `+eventColumns+` FROM usage_events WHERE rowid>? ORDER BY rowid LIMIT ?`, after, limit)
	if err != nil {
		return nil, after, err
	}
	defer rows.Close()
	events := []model.UsageEvent{}
	last := after
	for rows.Next() {
		scanner := &rowIDScanner{rows: rows}
		fact, err := scanEventFact(scanner)
		if err != nil {
			return nil, after, err
		}
		events = append(events, fact.event)
		last = scanner.id
	}
	return events, last, rows.Err()
}

func (db *DB) GetEventCost(eventID string) (EventCost, error) {
	var cost EventCost
	err := db.conn.QueryRow(`SELECT event_id, cost_usd, cost_cny, pricing_version FROM usage_events WHERE event_id=?`, eventID).
		Scan(&cost.EventID, &cost.CostUSD, &cost.CostCNY, &cost.PricingVersion)
	return cost, err
}

type DeviceStats struct {
	Device       model.Device       `json:"device"`
	Summary      model.UsageSummary `json:"summary"`
	TotalCostCNY string             `json:"totalCostCny"`
}

func (db *DB) GetDeviceStats(filters model.UsageFilters) ([]DeviceStats, error) {
	devices, err := db.ListDevices()
	if err != nil {
		return nil, err
	}
	result := []DeviceStats{}
	for _, device := range devices {
		if filters.DeviceID != "" && filters.DeviceID != device.DeviceID {
			continue
		}
		deviceFilters := filters
		deviceFilters.DeviceID = device.DeviceID
		summary, err := db.GetSummary(deviceFilters)
		if err != nil {
			return nil, err
		}
		rate, _ := db.CNYExchangeRate()
		cny := decimal.RequireFromString(summary.TotalCost).Mul(rate)
		result = append(result, DeviceStats{Device: device, Summary: summary, TotalCostCNY: cny.String()})
	}
	return result, nil
}

const eventColumns = `event_id, device_id, source, source_app, source_record_id, request_id, session_id,
	provider_id, provider_name, model, model_raw, canonical_model, request_model, pricing_model,
	occurred_at, ingested_at, input_tokens, output_tokens, reasoning_tokens, cache_read_tokens,
	cache_creation_tokens, cache_creation_5m_tokens, cache_creation_1h_tokens, usage_model, message_id,
	input_token_semantics, fresh_input_tokens, real_total_tokens,
	latency_ms, first_token_ms, duration_ms, generation_duration_ms, speed_tps, timing_quality,
	status_code, error_message, is_streaming, cost_multiplier, pricing_version, cost_usd, cost_cny,
	input_cost_usd, output_cost_usd, cache_read_cost_usd, cache_creation_cost_usd,
	input_cost_cny, output_cost_cny, cache_read_cost_cny, cache_creation_cost_cny, metadata`

func scanEventFact(row rowScanner) (eventFact, error) {
	var fact eventFact
	var occurredAt, ingestedAt, timing, multiplier, version, costUSD, costCNY, metadata string
	var semantics int
	var input, output, reasoning, cacheRead, cacheCreation, cache5m, cache1h int64
	var latency, firstToken, duration, generation sql.NullInt64
	var speed sql.NullFloat64
	var status sql.NullInt64
	var streaming int
	err := row.Scan(&fact.event.EventID, &fact.event.DeviceID, &fact.event.Source, &fact.event.SourceApp,
		&fact.event.SourceRecordID, &fact.event.RequestID, &fact.event.SessionID,
		&fact.event.ProviderID, &fact.event.ProviderName, &fact.event.Model, &fact.event.ModelRaw,
		&fact.event.CanonicalModel, &fact.event.RequestModel, &fact.event.PricingModel,
		&occurredAt, &ingestedAt, &input, &output, &reasoning,
		&cacheRead, &cacheCreation, &cache5m, &cache1h, &fact.event.Usage.Model, &fact.event.Usage.MessageID,
		&semantics, new(int64), new(int64), &latency, &firstToken, &duration, &generation, &speed,
		&timing, &status, &fact.event.ErrorMessage, &streaming, &multiplier, &version, &costUSD, &costCNY,
		&fact.event.Cost.InputCostUSD, &fact.event.Cost.OutputCostUSD, &fact.event.Cost.CacheReadCostUSD,
		&fact.event.Cost.CacheCreationCostUSD, &fact.event.Cost.InputCostCNY, &fact.event.Cost.OutputCostCNY,
		&fact.event.Cost.CacheReadCostCNY, &fact.event.Cost.CacheCreationCostCNY, &metadata)
	if err != nil {
		return fact, err
	}
	fact.event.OccurredAt, _ = parseTime(occurredAt)
	fact.event.IngestedAt, _ = parseTime(ingestedAt)
	fact.event.Usage.InputTokens = int64ToUint64(input)
	fact.event.Usage.OutputTokens = int64ToUint64(output)
	fact.event.Usage.ReasoningTokens = int64ToUint64(reasoning)
	fact.event.Usage.CacheReadTokens = int64ToUint64(cacheRead)
	fact.event.Usage.CacheCreationTokens = int64ToUint64(cacheCreation)
	fact.event.Usage.CacheCreation5MTokens = int64ToUint64(cache5m)
	fact.event.Usage.CacheCreation1HTokens = int64ToUint64(cache1h)
	fact.event.InputTokenSemantics = model.InputSemantics(semantics)
	if latency.Valid {
		value := uint64(latency.Int64)
		fact.event.LatencyMS = &value
	}
	if firstToken.Valid {
		value := uint64(firstToken.Int64)
		fact.event.FirstTokenMS = &value
	}
	if duration.Valid {
		value := uint64(duration.Int64)
		fact.event.DurationMS = &value
	}
	if generation.Valid {
		value := uint64(generation.Int64)
		fact.event.GenerationDurationMS = &value
	}
	if speed.Valid {
		value := speed.Float64
		fact.event.SpeedTPS = &value
	}
	if status.Valid {
		value := int(status.Int64)
		fact.event.StatusCode = &value
	}
	fact.event.TimingQuality = model.TimingQuality(timing)
	fact.event.IsStreaming = streaming != 0
	fact.event.CostMultiplier = multiplier
	fact.event.PricingVersion = version
	fact.event.Cost.TotalCostUSD = costUSD
	fact.event.Cost.TotalCostCNY = costCNY
	fact.event.Cost.PricingVersion = version
	_ = json.Unmarshal([]byte(metadata), &fact.event.Metadata)
	fact.costUSD = decimal.RequireFromString(costUSD)
	fact.costCNY = decimal.RequireFromString(costCNY)
	return fact, nil
}

func buildFilters(filters model.UsageFilters) (string, []any) {
	conditions := []string{"1=1"}
	args := []any{}
	if filters.From != nil {
		conditions = append(conditions, "occurred_at >= ?")
		args = append(args, formatTime(*filters.From))
	}
	if filters.To != nil {
		conditions = append(conditions, "occurred_at <= ?")
		args = append(args, formatTime(*filters.To))
	}
	if filters.DeviceID != "" {
		conditions = append(conditions, "device_id = ?")
		args = append(args, filters.DeviceID)
	}
	if filters.AppType != "" {
		conditions = append(conditions, "source_app = ?")
		args = append(args, filters.AppType)
	}
	if filters.ProviderID != "" {
		conditions = append(conditions, "provider_id = ?")
		args = append(args, filters.ProviderID)
	}
	if filters.ProviderName != "" {
		conditions = append(conditions, "provider_name = ?")
		args = append(args, filters.ProviderName)
	}
	if filters.Model != "" {
		conditions = append(conditions, "(model = ? OR canonical_model = ? OR model_raw = ?)")
		args = append(args, filters.Model, filters.Model, filters.Model)
	}
	if filters.SessionID != "" {
		conditions = append(conditions, "session_id = ?")
		args = append(args, filters.SessionID)
	}
	return " WHERE " + strings.Join(conditions, " AND "), args
}

func bucketKey(t time.Time, bucket string) string {
	t = t.UTC()
	switch bucket {
	case "hour":
		return t.Format("2006-01-02T15:00:00Z")
	case "week":
		return t.AddDate(0, 0, -int(t.Weekday())).Format("2006-01-02")
	case "month":
		return t.Format("2006-01")
	default:
		return t.Format("2006-01-02")
	}
}

func averageLatency(facts []eventFact) uint64 {
	var total, count uint64
	for _, fact := range facts {
		if fact.event.LatencyMS != nil {
			total += *fact.event.LatencyMS
			count++
		}
	}
	if count == 0 {
		return 0
	}
	return total / count
}

func (db *DB) activePricingVersionTx(tx *sql.Tx) (string, error) {
	var version string
	err := tx.QueryRow(`SELECT value FROM settings WHERE key='active_pricing_version'`).Scan(&version)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return version, err
}

func (db *DB) cnyExchangeRateTx(tx *sql.Tx) (decimal.Decimal, error) {
	var value string
	if err := tx.QueryRow(`SELECT value FROM settings WHERE key='cny_exchange_rate'`).Scan(&value); err != nil {
		return decimal.NewFromFloat(7.2), nil
	}
	rate, err := decimal.NewFromString(value)
	if err != nil {
		return decimal.Zero, err
	}
	return rate, nil
}

func (db *DB) pricingForModelTx(tx *sql.Tx, versionID, modelID string) (model.ModelPricing, bool, error) {
	for _, candidate := range []string{modelID, model.NormalizeModelID(modelID)} {
		if candidate == "" {
			continue
		}
		var pricing model.ModelPricing
		var input, output, cacheRead, cacheCreation string
		err := tx.QueryRow(`SELECT model_id, display_name, input_cost_per_million, output_cost_per_million,
			cache_read_cost_per_million, cache_creation_cost_per_million, currency, source
			FROM model_pricing WHERE version_id=? AND model_id=?`, versionID, candidate).
			Scan(&pricing.ModelID, &pricing.DisplayName, &input, &output, &cacheRead, &cacheCreation, &pricing.Currency, &pricing.Source)
		if err == nil {
			pricing.InputCostPerMillion = decimal.RequireFromString(input)
			pricing.OutputCostPerMillion = decimal.RequireFromString(output)
			pricing.CacheReadCostPerMillion = decimal.RequireFromString(cacheRead)
			pricing.CacheCreationCostPerMillion = decimal.RequireFromString(cacheCreation)
			return pricing, true, nil
		}
		if err != sql.ErrNoRows {
			return model.ModelPricing{}, false, err
		}
	}
	return model.ModelPricing{}, false, nil
}

func buildCostSnapshot(breakdown model.CostBreakdown, currency model.Currency, rate decimal.Decimal, versionID string) model.CostSnapshot {
	if currency == model.CurrencyCNY {
		return model.CostSnapshot{
			InputCostUSD: breakdown.InputCost.Div(rate).String(), OutputCostUSD: breakdown.OutputCost.Div(rate).String(),
			CacheReadCostUSD: breakdown.CacheReadCost.Div(rate).String(), CacheCreationCostUSD: breakdown.CacheCreationCost.Div(rate).String(),
			TotalCostUSD: breakdown.TotalCost.Div(rate).String(),
			InputCostCNY: breakdown.InputCost.String(), OutputCostCNY: breakdown.OutputCost.String(),
			CacheReadCostCNY: breakdown.CacheReadCost.String(), CacheCreationCostCNY: breakdown.CacheCreationCost.String(),
			TotalCostCNY: breakdown.TotalCost.String(), PricingVersion: versionID,
		}
	}
	return model.CostSnapshot{
		InputCostUSD: breakdown.InputCost.String(), OutputCostUSD: breakdown.OutputCost.String(),
		CacheReadCostUSD: breakdown.CacheReadCost.String(), CacheCreationCostUSD: breakdown.CacheCreationCost.String(),
		TotalCostUSD: breakdown.TotalCost.String(),
		InputCostCNY: breakdown.InputCost.Mul(rate).String(), OutputCostCNY: breakdown.OutputCost.Mul(rate).String(),
		CacheReadCostCNY: breakdown.CacheReadCost.Mul(rate).String(), CacheCreationCostCNY: breakdown.CacheCreationCost.Mul(rate).String(),
		TotalCostCNY: breakdown.TotalCost.Mul(rate).String(), PricingVersion: versionID,
	}
}

func uint64ToInt64(value uint64) int64 {
	if value > uint64(^uint64(0)>>1) {
		return int64(^uint64(0) >> 1)
	}
	return int64(value)
}

func int64ToUint64(value int64) uint64 {
	if value <= 0 {
		return 0
	}
	return uint64(value)
}

func nullableUint64(value *uint64) any {
	if value == nil {
		return nil
	}
	return uint64ToInt64(*value)
}

func boolToInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func uniqueStrings(values ...string) []string {
	result := make([]string, 0, len(values))
	seen := map[string]bool{}
	for _, value := range values {
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		result = append(result, value)
	}
	return result
}

func sortStrings(values []string) {
	for i := 1; i < len(values); i++ {
		for j := i; j > 0 && values[j] < values[j-1]; j-- {
			values[j], values[j-1] = values[j-1], values[j]
		}
	}
}

type providerKey struct{ provider, app string }

func sortProviderKeys(keys []providerKey) {
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && (keys[j].provider < keys[j-1].provider || (keys[j].provider == keys[j-1].provider && keys[j].app < keys[j-1].app)); j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
}
