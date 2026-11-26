package storage

import (
	"fmt"
	"time"

	"tokenanalyse/internal/model"
	"tokenanalyse/internal/pricing"
)

type PricingSyncStatus struct {
	LastUpdatedAt *time.Time `json:"lastUpdatedAt,omitempty"`
	LastCheckedAt *time.Time `json:"lastCheckedAt,omitempty"`
	LastSyncedAt  *time.Time `json:"lastSyncedAt,omitempty"`
	RemoteHash    string     `json:"remoteHash,omitempty"`
	LastError     string     `json:"lastError,omitempty"`
}

type PricingSnapshot struct {
	VersionID   string               `json:"versionId"`
	Source      string               `json:"source"`
	CreatedAt   time.Time            `json:"createdAt"`
	Models      []model.ModelPricing `json:"models"`
	Aliases     []model.ModelAlias   `json:"aliases"`
	ContentHash string               `json:"contentHash"`
	Sync        PricingSyncStatus    `json:"sync"`
}

func (db *DB) ApplyPricingSnapshot(snapshot PricingSnapshot, activate bool) error {
	tx, err := db.conn.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`INSERT INTO pricing_versions(version_id, source, created_at, model_count, content_hash)
		VALUES (?, ?, ?, ?, ?)`, snapshot.VersionID, snapshot.Source, formatTime(snapshot.CreatedAt),
		len(snapshot.Models), snapshot.ContentHash); err != nil {
		return fmt.Errorf("create pricing version: %w", err)
	}
	for _, entry := range snapshot.Models {
		if _, err := tx.Exec(`INSERT INTO model_pricing(version_id, model_id, display_name, input_cost_per_million,
			output_cost_per_million, cache_read_cost_per_million, cache_creation_cost_per_million, currency, source)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`, snapshot.VersionID, entry.ModelID, entry.DisplayName,
			entry.InputCostPerMillion.String(), entry.OutputCostPerMillion.String(),
			entry.CacheReadCostPerMillion.String(), entry.CacheCreationCostPerMillion.String(),
			entry.Currency, entry.Source); err != nil {
			return fmt.Errorf("insert pricing %s: %w", entry.ModelID, err)
		}
	}
	for _, alias := range snapshot.Aliases {
		if alias.AliasID == "" {
			alias.AliasID = deterministicID("alias", alias.AliasPattern, alias.MatchType, alias.ScopeApp)
		}
		if alias.CreatedAt.IsZero() {
			alias.CreatedAt = time.Now().UTC()
		}
		if _, err := tx.Exec(`INSERT INTO model_aliases(alias_id, alias_pattern, canonical_model_id, match_type, scope_app, priority, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(alias_pattern, match_type, scope_app) DO UPDATE SET
				canonical_model_id=excluded.canonical_model_id, priority=excluded.priority`,
			alias.AliasID, alias.AliasPattern, alias.CanonicalModelID, alias.MatchType,
			alias.ScopeApp, alias.Priority, formatTime(alias.CreatedAt)); err != nil {
			return err
		}
	}
	if activate {
		if _, err := tx.Exec(`UPDATE settings SET value=? WHERE key='active_pricing_version'`, snapshot.VersionID); err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO settings(key,value) VALUES('pricing_last_updated_at',?)
			ON CONFLICT(key) DO UPDATE SET value=excluded.value`, formatTime(snapshot.CreatedAt)); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (db *DB) PricingSyncStatus() (PricingSyncStatus, error) {
	status := PricingSyncStatus{}
	if value, _ := db.GetSetting("pricing_last_updated_at"); value != "" {
		parsed, _ := parseTime(value)
		status.LastUpdatedAt = &parsed
	}
	if value, _ := db.GetSetting("pricing_last_checked_at"); value != "" {
		parsed, _ := parseTime(value)
		status.LastCheckedAt = &parsed
	}
	if value, _ := db.GetSetting("pricing_last_synced_at"); value != "" {
		parsed, _ := parseTime(value)
		status.LastSyncedAt = &parsed
	}
	status.RemoteHash, _ = db.GetSetting("pricing_remote_hash")
	status.LastError, _ = db.GetSetting("pricing_last_error")
	return status, nil
}

func (db *DB) MarkPricingSync(remoteHash string, checkedAt, syncedAt time.Time, syncError string) error {
	_ = db.SetSetting("pricing_last_checked_at", formatTime(checkedAt))
	_ = db.SetSetting("pricing_remote_hash", remoteHash)
	_ = db.SetSetting("pricing_last_error", syncError)
	if !syncedAt.IsZero() {
		_ = db.SetSetting("pricing_last_synced_at", formatTime(syncedAt))
	}
	return nil
}

func (db *DB) ActivePricingSnapshot() (PricingSnapshot, error) {
	versionID, err := db.ActivePricingVersion()
	if err != nil {
		return PricingSnapshot{}, err
	}
	if versionID == "" {
		return PricingSnapshot{}, nil
	}
	version, err := db.GetPricingVersion(versionID)
	if err != nil {
		return PricingSnapshot{}, err
	}
	models, err := db.ListModelPricingForVersion(versionID)
	if err != nil {
		return PricingSnapshot{}, err
	}
	aliases, err := db.ListModelAliases()
	if err != nil {
		return PricingSnapshot{}, err
	}
	syncStatus, _ := db.PricingSyncStatus()
	return PricingSnapshot{VersionID: version.VersionID, Source: version.Source,
		CreatedAt: version.CreatedAt, Models: models, Aliases: aliases, ContentHash: version.ContentHash,
		Sync: syncStatus}, nil
}

func (db *DB) MergePricingSnapshot(remote PricingSnapshot, policy pricing.MergePolicy) (PricingSnapshot, int, error) {
	local, err := db.ActivePricingSnapshot()
	if err != nil {
		return PricingSnapshot{}, 0, err
	}
	merged, conflicts := pricing.MergeModels(local.Models, remote.Models, policy)
	hash := pricing.HashModels(merged)
	now := time.Now().UTC()
	snapshot := PricingSnapshot{
		VersionID: "merged-" + hash[:12], Source: "merged", CreatedAt: now,
		Models: merged, Aliases: append(local.Aliases, remote.Aliases...), ContentHash: hash,
	}
	if err := db.ApplyPricingSnapshot(snapshot, true); err != nil {
		return PricingSnapshot{}, 0, err
	}
	return snapshot, conflicts, nil
}
