package storage

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"runtime"
	"sync"
	"time"

	"tokenanalyse/internal/model"

	"github.com/shopspring/decimal"
)

type repriceEventRecord struct {
	id           string
	appType      string
	model        string
	modelRaw     string
	canonical    string
	pricingModel string
	input        int64
	output       int64
	cacheRead    int64
	cacheCreate  int64
	cache1h      int64
	semantics    int
	multiplier   string
	metadata     string
	currentCost  model.CostSnapshot
}

type repriceComputeResult struct {
	record       repriceEventRecord
	canonical    string
	pricingModel string
	cost         model.CostSnapshot
	metadata     string
	changed      bool
	err          error
}

func (db *DB) RepriceAll() (int, error) {
	db.repriceMu.Lock()
	defer db.repriceMu.Unlock()

	if err := db.beginRepriceJob(0); err != nil {
		return 0, err
	}
	fail := func(err error) (int, error) {
		_ = db.finishRepriceJob(RepriceJobFailed, 0, err)
		return 0, err
	}

	readTx, err := db.conn.Begin()
	if err != nil {
		return fail(err)
	}
	versionID, err := db.activePricingVersionTx(readTx)
	if err != nil {
		_ = readTx.Rollback()
		return fail(err)
	}
	if versionID == "" {
		_ = readTx.Rollback()
		return fail(fmt.Errorf("no active pricing version"))
	}
	rate, err := db.cnyExchangeRateTx(readTx)
	if err != nil {
		_ = readTx.Rollback()
		return fail(err)
	}
	catalog, err := loadPricingCatalogTx(readTx, versionID)
	if err != nil {
		_ = readTx.Rollback()
		return fail(err)
	}
	aliases, err := db.listModelAliasesTx(readTx)
	if err != nil {
		_ = readTx.Rollback()
		return fail(err)
	}
	records, err := db.loadRepriceEventRecordsTx(readTx)
	_ = readTx.Rollback()
	if err != nil {
		return fail(err)
	}
	if err := db.setRepriceJobTotal(len(records)); err != nil {
		return fail(err)
	}

	rulesByApp := buildRepriceRulesByApp(records, aliases)
	results, err := runRepriceWorkers(records, catalog, rulesByApp, rate, versionID, db.updateRepriceJobProgress)
	if err != nil {
		return fail(err)
	}

	writeTx, err := db.conn.Begin()
	if err != nil {
		return fail(err)
	}
	defer writeTx.Rollback()
	statement, err := writeTx.Prepare(`UPDATE usage_events SET canonical_model=?, pricing_model=?, pricing_version=?, cost_usd=?, cost_cny=?,
		input_cost_usd=?, output_cost_usd=?, cache_read_cost_usd=?, cache_creation_cost_usd=?,
		input_cost_cny=?, output_cost_cny=?, cache_read_cost_cny=?, cache_creation_cost_cny=?, metadata=?
		WHERE event_id=?`)
	if err != nil {
		_ = writeTx.Rollback()
		return fail(err)
	}
	for _, result := range results {
		if !result.changed {
			continue
		}
		if _, err := statement.Exec(result.canonical, result.pricingModel, result.cost.PricingVersion,
			result.cost.TotalCostUSD, result.cost.TotalCostCNY,
			result.cost.InputCostUSD, result.cost.OutputCostUSD, result.cost.CacheReadCostUSD, result.cost.CacheCreationCostUSD,
			result.cost.InputCostCNY, result.cost.OutputCostCNY, result.cost.CacheReadCostCNY, result.cost.CacheCreationCostCNY,
			result.metadata, result.record.id); err != nil {
			_ = writeTx.Rollback()
			return fail(err)
		}
	}
	if err := statement.Close(); err != nil {
		_ = writeTx.Rollback()
		return fail(err)
	}
	if err := writeTx.Commit(); err != nil {
		_ = writeTx.Rollback()
		return fail(err)
	}
	if err := db.finishRepriceJob(RepriceJobCompleted, len(records), nil); err != nil {
		return len(records), err
	}
	return len(records), nil
}

func (db *DB) loadRepriceEventRecordsTx(tx *sql.Tx) ([]repriceEventRecord, error) {
	rows, err := tx.Query(`SELECT event_id, source_app, model, model_raw, canonical_model, pricing_model,
		input_tokens, output_tokens, cache_read_tokens, cache_creation_tokens, cache_creation_1h_tokens,
		input_token_semantics, cost_multiplier, metadata,
		pricing_version, cost_usd, cost_cny,
		input_cost_usd, output_cost_usd, cache_read_cost_usd, cache_creation_cost_usd,
		input_cost_cny, output_cost_cny, cache_read_cost_cny, cache_creation_cost_cny
		FROM usage_events`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	records := make([]repriceEventRecord, 0)
	for rows.Next() {
		var record repriceEventRecord
		if err := rows.Scan(&record.id, &record.appType, &record.model, &record.modelRaw,
			&record.canonical, &record.pricingModel, &record.input, &record.output,
			&record.cacheRead, &record.cacheCreate, &record.cache1h, &record.semantics,
			&record.multiplier, &record.metadata,
			&record.currentCost.PricingVersion, &record.currentCost.TotalCostUSD, &record.currentCost.TotalCostCNY,
			&record.currentCost.InputCostUSD, &record.currentCost.OutputCostUSD,
			&record.currentCost.CacheReadCostUSD, &record.currentCost.CacheCreationCostUSD,
			&record.currentCost.InputCostCNY, &record.currentCost.OutputCostCNY,
			&record.currentCost.CacheReadCostCNY, &record.currentCost.CacheCreationCostCNY); err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	return records, rows.Err()
}

func buildRepriceRulesByApp(records []repriceEventRecord, aliases []ModelAlias) map[string][]modelAliasRule {
	allRules := compileAliasRules(aliases)
	rulesByApp := map[string][]modelAliasRule{}
	for _, record := range records {
		if _, ok := rulesByApp[record.appType]; !ok {
			rulesByApp[record.appType] = aliasRulesForApp(allRules, record.appType)
		}
	}
	return rulesByApp
}

func runRepriceWorkers(records []repriceEventRecord, catalog map[string]model.ModelPricing,
	rulesByApp map[string][]modelAliasRule, rate decimal.Decimal, versionID string,
	progress func(int) error) ([]repriceComputeResult, error) {
	if len(records) == 0 {
		return []repriceComputeResult{}, nil
	}
	workerCount := runtime.GOMAXPROCS(0)
	if workerCount < 2 {
		workerCount = 2
	}
	if workerCount > 8 {
		workerCount = 8
	}
	if workerCount > len(records) {
		workerCount = len(records)
	}

	jobs := make(chan repriceEventRecord, workerCount)
	results := make(chan repriceComputeResult, workerCount)
	var workers sync.WaitGroup
	for i := 0; i < workerCount; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for record := range jobs {
				results <- calculateRepriceResult(record, catalog, rulesByApp[record.appType], rate, versionID)
			}
		}()
	}
	go func() {
		for _, record := range records {
			jobs <- record
		}
		close(jobs)
		workers.Wait()
		close(results)
	}()

	computed := make([]repriceComputeResult, 0, len(records))
	var firstErr error
	var progressErr error
	for result := range results {
		computed = append(computed, result)
		if result.err != nil && firstErr == nil {
			firstErr = result.err
		}
		if progress != nil && len(computed)%500 == 0 && progressErr == nil {
			progressErr = progress(len(computed))
		}
	}
	if firstErr != nil {
		return nil, firstErr
	}
	if progressErr != nil {
		return nil, progressErr
	}
	return computed, nil
}

func calculateRepriceResult(record repriceEventRecord, catalog map[string]model.ModelPricing,
	rules []modelAliasRule, rate decimal.Decimal, versionID string) repriceComputeResult {
	result := repriceComputeResult{record: record}
	aliasInput := record.modelRaw
	if aliasInput == "" {
		aliasInput = record.model
	}
	aliasFallback := record.canonical
	if aliasFallback == "" {
		aliasFallback = record.model
	}
	result.canonical = resolveModelWithAliasRules(aliasInput, aliasFallback, rules)
	result.pricingModel = record.pricingModel
	if result.pricingModel == "" || result.pricingModel == record.canonical || result.pricingModel == record.model ||
		result.pricingModel == record.modelRaw || result.pricingModel == model.NormalizeModelID(record.model) {
		result.pricingModel = result.canonical
	}

	multiplier := decimal.NewFromInt(1)
	if record.multiplier != "" {
		parsed, err := decimal.NewFromString(record.multiplier)
		if err != nil {
			result.err = fmt.Errorf("invalid cost multiplier for event %s: %w", record.id, err)
			return result
		}
		multiplier = parsed
	}
	usage := model.TokenUsage{
		InputTokens: int64ToUint64(record.input), OutputTokens: int64ToUint64(record.output),
		CacheReadTokens: int64ToUint64(record.cacheRead), CacheCreationTokens: int64ToUint64(record.cacheCreate),
		CacheCreation1HTokens: int64ToUint64(record.cache1h),
	}
	pricingEntry, found := lookupPricingCatalog(catalog, result.pricingModel, result.canonical, record.model)
	breakdown := model.CostBreakdown{
		InputCost: decimal.Zero, OutputCost: decimal.Zero, CacheReadCost: decimal.Zero,
		CacheCreationCost: decimal.Zero, TotalCost: decimal.Zero,
	}
	currency := model.CurrencyUSD
	if found {
		breakdown = model.CalculateCost(record.appType, usage, pricingEntry, model.InputSemantics(record.semantics), multiplier)
		currency = pricingEntry.Currency
	}
	result.cost = buildCostSnapshot(breakdown, currency, rate, versionID)
	result.changed = result.canonical != record.canonical || result.pricingModel != record.pricingModel ||
		result.cost != record.currentCost
	result.metadata = record.metadata
	if result.changed {
		var meta map[string]any
		_ = json.Unmarshal([]byte(record.metadata), &meta)
		if meta == nil {
			meta = map[string]any{}
		}
		meta["repricedAt"] = time.Now().UTC().Format(time.RFC3339Nano)
		encoded, err := json.Marshal(meta)
		if err != nil {
			result.err = err
			return result
		}
		result.metadata = string(encoded)
	}
	return result
}

func loadPricingCatalogTx(tx *sql.Tx, versionID string) (map[string]model.ModelPricing, error) {
	rows, err := tx.Query(`SELECT model_id, display_name, input_cost_per_million, output_cost_per_million,
		cache_read_cost_per_million, cache_creation_cost_per_million, currency, source
		FROM model_pricing WHERE version_id=?`, versionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	catalog := map[string]model.ModelPricing{}
	for rows.Next() {
		var entry model.ModelPricing
		var input, output, cacheRead, cacheCreation string
		if err := rows.Scan(&entry.ModelID, &entry.DisplayName, &input, &output,
			&cacheRead, &cacheCreation, &entry.Currency, &entry.Source); err != nil {
			return nil, err
		}
		entry.InputCostPerMillion = decimal.RequireFromString(input)
		entry.OutputCostPerMillion = decimal.RequireFromString(output)
		entry.CacheReadCostPerMillion = decimal.RequireFromString(cacheRead)
		entry.CacheCreationCostPerMillion = decimal.RequireFromString(cacheCreation)
		catalog[entry.ModelID] = entry
		catalog[model.NormalizeModelID(entry.ModelID)] = entry
	}
	return catalog, rows.Err()
}

func lookupPricingCatalog(catalog map[string]model.ModelPricing, candidates ...string) (model.ModelPricing, bool) {
	for _, candidate := range uniqueStrings(candidates...) {
		if entry, ok := catalog[candidate]; ok {
			return entry, true
		}
		if entry, ok := catalog[model.NormalizeModelID(candidate)]; ok {
			return entry, true
		}
	}
	return model.ModelPricing{}, false
}
