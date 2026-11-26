package storage

import (
	"database/sql"
	"fmt"
	"time"

	"token-mirror/internal/model"
	"token-mirror/internal/pricing"

	"github.com/shopspring/decimal"
)

type PricingVersion struct {
	VersionID   string    `json:"versionId"`
	Source      string    `json:"source"`
	CreatedAt   time.Time `json:"createdAt"`
	ModelCount  int       `json:"modelCount"`
	ContentHash string    `json:"contentHash"`
}

func (db *DB) CreatePricingVersion(versionID, source string, models []model.ModelPricing) error {
	tx, err := db.conn.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`INSERT INTO pricing_versions(version_id, source, created_at, model_count, content_hash)
		VALUES (?, ?, ?, ?, ?)`, versionID, source, formatTime(time.Now().UTC()), len(models), pricing.HashModels(models)); err != nil {
		return fmt.Errorf("create pricing version: %w", err)
	}
	for _, pricing := range models {
		if _, err := tx.Exec(`
			INSERT INTO model_pricing(version_id, model_id, display_name, input_cost_per_million,
				output_cost_per_million, cache_read_cost_per_million, cache_creation_cost_per_million,
				currency, source)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			versionID, pricing.ModelID, pricing.DisplayName,
			pricing.InputCostPerMillion.String(), pricing.OutputCostPerMillion.String(),
			pricing.CacheReadCostPerMillion.String(), pricing.CacheCreationCostPerMillion.String(),
			pricing.Currency, pricing.Source); err != nil {
			return fmt.Errorf("insert pricing %s: %w", pricing.ModelID, err)
		}
	}
	return tx.Commit()
}

func (db *DB) SetActivePricingVersion(versionID string) error {
	result, err := db.conn.Exec(`UPDATE settings SET value=? WHERE key='active_pricing_version'`, versionID)
	if err != nil {
		return err
	}
	if rows, _ := result.RowsAffected(); rows == 0 {
		_, err = db.conn.Exec(`INSERT INTO settings(key, value) VALUES ('active_pricing_version', ?)`, versionID)
	}
	return err
}

func (db *DB) ActivePricingVersion() (string, error) {
	var version string
	err := db.conn.QueryRow(`SELECT value FROM settings WHERE key='active_pricing_version'`).Scan(&version)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return version, err
}

func (db *DB) GetPricingVersion(versionID string) (PricingVersion, error) {
	var version PricingVersion
	var createdAt string
	err := db.conn.QueryRow(`SELECT version_id, source, created_at, model_count, content_hash
		FROM pricing_versions WHERE version_id=?`, versionID).
		Scan(&version.VersionID, &version.Source, &createdAt, &version.ModelCount, &version.ContentHash)
	if err != nil {
		return version, err
	}
	version.CreatedAt, _ = parseTime(createdAt)
	return version, nil
}

func (db *DB) ListModelPricing() ([]model.ModelPricing, error) {
	versionID, err := db.ActivePricingVersion()
	if err != nil || versionID == "" {
		return nil, err
	}
	return db.ListModelPricingForVersion(versionID)
}

func (db *DB) ListModelPricingForVersion(versionID string) ([]model.ModelPricing, error) {
	rows, err := db.conn.Query(`
		SELECT model_id, display_name, input_cost_per_million, output_cost_per_million,
			cache_read_cost_per_million, cache_creation_cost_per_million, currency, source
		FROM model_pricing WHERE version_id=? ORDER BY display_name, model_id`, versionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []model.ModelPricing
	for rows.Next() {
		var pricing model.ModelPricing
		var input, output, cacheRead, cacheCreation string
		if err := rows.Scan(&pricing.ModelID, &pricing.DisplayName, &input, &output,
			&cacheRead, &cacheCreation, &pricing.Currency, &pricing.Source); err != nil {
			return nil, err
		}
		pricing.InputCostPerMillion = decimal.RequireFromString(input)
		pricing.OutputCostPerMillion = decimal.RequireFromString(output)
		pricing.CacheReadCostPerMillion = decimal.RequireFromString(cacheRead)
		pricing.CacheCreationCostPerMillion = decimal.RequireFromString(cacheCreation)
		result = append(result, pricing)
	}
	return result, rows.Err()
}

func (db *DB) PricingForModel(versionID, modelID string) (model.ModelPricing, bool, error) {
	if versionID == "" {
		var err error
		versionID, err = db.ActivePricingVersion()
		if err != nil || versionID == "" {
			return model.ModelPricing{}, false, err
		}
	}
	candidates := []string{modelID, model.NormalizeModelID(modelID)}
	seen := map[string]bool{}
	for _, candidate := range candidates {
		if candidate == "" || seen[candidate] {
			continue
		}
		seen[candidate] = true
		pricing, err := db.pricingExact(versionID, candidate)
		if err == nil {
			return pricing, true, nil
		}
		if err != sql.ErrNoRows {
			return model.ModelPricing{}, false, err
		}
	}
	return model.ModelPricing{}, false, nil
}

func (db *DB) pricingExact(versionID, modelID string) (model.ModelPricing, error) {
	var pricing model.ModelPricing
	var input, output, cacheRead, cacheCreation string
	err := db.conn.QueryRow(`
		SELECT model_id, display_name, input_cost_per_million, output_cost_per_million,
			cache_read_cost_per_million, cache_creation_cost_per_million, currency, source
		FROM model_pricing WHERE version_id=? AND model_id=?`, versionID, modelID).
		Scan(&pricing.ModelID, &pricing.DisplayName, &input, &output,
			&cacheRead, &cacheCreation, &pricing.Currency, &pricing.Source)
	if err != nil {
		return pricing, err
	}
	pricing.InputCostPerMillion = decimal.RequireFromString(input)
	pricing.OutputCostPerMillion = decimal.RequireFromString(output)
	pricing.CacheReadCostPerMillion = decimal.RequireFromString(cacheRead)
	pricing.CacheCreationCostPerMillion = decimal.RequireFromString(cacheCreation)
	return pricing, nil
}

func (db *DB) CNYExchangeRate() (decimal.Decimal, error) {
	var value string
	if err := db.conn.QueryRow(`SELECT value FROM settings WHERE key='cny_exchange_rate'`).Scan(&value); err != nil {
		return decimal.NewFromFloat(7.2), nil
	}
	rate, err := decimal.NewFromString(value)
	if err != nil || rate.LessThanOrEqual(decimal.Zero) {
		return decimal.NewFromFloat(7.2), nil
	}
	return rate, nil
}

func (db *DB) SetCNYExchangeRate(value string) error {
	rate, err := decimal.NewFromString(value)
	if err != nil || rate.LessThanOrEqual(decimal.Zero) {
		return fmt.Errorf("invalid CNY exchange rate: %s", value)
	}
	_, err = db.conn.Exec(`UPDATE settings SET value=? WHERE key='cny_exchange_rate'`, rate.String())
	return err
}
