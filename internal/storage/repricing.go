package storage

import (
	"encoding/json"
	"fmt"
	"time"

	"token-mirror/internal/model"

	"github.com/shopspring/decimal"
)

func (db *DB) RepriceAll() (int, error) {
	tx, err := db.conn.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	versionID, err := db.activePricingVersionTx(tx)
	if err != nil {
		return 0, err
	}
	if versionID == "" {
		return 0, fmt.Errorf("no active pricing version")
	}
	rate, err := db.cnyExchangeRateTx(tx)
	if err != nil {
		return 0, err
	}
	rows, err := tx.Query(`SELECT event_id, source_app, model, canonical_model, pricing_model,
		input_tokens, output_tokens, cache_read_tokens, cache_creation_tokens, cache_creation_1h_tokens,
		input_token_semantics, cost_multiplier, metadata FROM usage_events`)
	if err != nil {
		return 0, err
	}
	type update struct {
		id       string
		cost     model.CostSnapshot
		metadata string
	}
	updates := []update{}
	for rows.Next() {
		var id, appType, rawModel, canonical, pricingModel, multiplier, metadata string
		var input, output, cacheRead, cacheCreation, cache1h int64
		var semantics int
		if err := rows.Scan(&id, &appType, &rawModel, &canonical, &pricingModel,
			&input, &output, &cacheRead, &cacheCreation, &cache1h, &semantics, &multiplier, &metadata); err != nil {
			rows.Close()
			return 0, err
		}
		usage := model.TokenUsage{InputTokens: int64ToUint64(input), OutputTokens: int64ToUint64(output),
			CacheReadTokens: int64ToUint64(cacheRead), CacheCreationTokens: int64ToUint64(cacheCreation),
			CacheCreation1HTokens: int64ToUint64(cache1h)}
		pricingEntry := model.ModelPricing{}
		found := false
		for _, candidate := range uniqueStrings(pricingModel, canonical, rawModel) {
			pricingEntry, found, err = db.pricingForModelTx(tx, versionID, candidate)
			if err != nil {
				rows.Close()
				return 0, err
			}
			if found {
				break
			}
		}
		breakdown := model.CostBreakdown{InputCost: decimal.Zero, OutputCost: decimal.Zero,
			CacheReadCost: decimal.Zero, CacheCreationCost: decimal.Zero, TotalCost: decimal.Zero}
		currency := model.CurrencyUSD
		if found {
			breakdown = model.CalculateCost(appType, usage, pricingEntry, model.InputSemantics(semantics), decimal.RequireFromString(multiplier))
			currency = pricingEntry.Currency
		}
		cost := buildCostSnapshot(breakdown, currency, rate, versionID)
		var meta map[string]any
		_ = json.Unmarshal([]byte(metadata), &meta)
		if meta == nil {
			meta = map[string]any{}
		}
		meta["repricedAt"] = time.Now().UTC().Format(time.RFC3339Nano)
		metaBytes, _ := json.Marshal(meta)
		updates = append(updates, update{id: id, cost: cost, metadata: string(metaBytes)})
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, err
	}
	rows.Close()
	for _, item := range updates {
		_, err := tx.Exec(`UPDATE usage_events SET pricing_version=?, cost_usd=?, cost_cny=?,
			input_cost_usd=?, output_cost_usd=?, cache_read_cost_usd=?, cache_creation_cost_usd=?,
			input_cost_cny=?, output_cost_cny=?, cache_read_cost_cny=?, cache_creation_cost_cny=?, metadata=?
			WHERE event_id=?`, item.cost.PricingVersion, item.cost.TotalCostUSD, item.cost.TotalCostCNY,
			item.cost.InputCostUSD, item.cost.OutputCostUSD, item.cost.CacheReadCostUSD, item.cost.CacheCreationCostUSD,
			item.cost.InputCostCNY, item.cost.OutputCostCNY, item.cost.CacheReadCostCNY, item.cost.CacheCreationCostCNY,
			item.metadata, item.id)
		if err != nil {
			return 0, err
		}
	}
	return len(updates), tx.Commit()
}
