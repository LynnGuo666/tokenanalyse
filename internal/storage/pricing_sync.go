package storage

import (
	"database/sql"
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
	if snapshot.VersionID == "" {
		if snapshot.ContentHash == "" {
			snapshot.ContentHash = pricing.HashModels(snapshot.Models)
		}
		snapshot.VersionID = "pricing-" + hashPrefix(snapshot.ContentHash, 12)
	}
	if snapshot.ContentHash == "" {
		snapshot.ContentHash = pricing.HashModels(snapshot.Models)
	}
	if snapshot.CreatedAt.IsZero() {
		snapshot.CreatedAt = time.Now().UTC()
	}
	tx, err := db.conn.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var existingHash string
	err = tx.QueryRow(`SELECT content_hash FROM pricing_versions WHERE version_id=?`, snapshot.VersionID).Scan(&existingHash)
	if err == nil {
		if existingHash != snapshot.ContentHash {
			return fmt.Errorf("pricing version %s already exists with different content", snapshot.VersionID)
		}
	} else if err == sql.ErrNoRows {
		if _, err := tx.Exec(`INSERT INTO pricing_versions(version_id, source, created_at, model_count, content_hash)
			VALUES (?, ?, ?, ?, ?)`, snapshot.VersionID, snapshot.Source, formatTime(snapshot.CreatedAt),
			len(snapshot.Models), snapshot.ContentHash); err != nil {
			return fmt.Errorf("create pricing version: %w", err)
		}
		for _, entry := range snapshot.Models {
			if entry.ModelID == "" {
				return fmt.Errorf("pricing entry missing model id")
			}
			if _, err := tx.Exec(`INSERT INTO model_pricing(version_id, model_id, display_name, input_cost_per_million,
				output_cost_per_million, cache_read_cost_per_million, cache_creation_cost_per_million, currency, source)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`, snapshot.VersionID, entry.ModelID, entry.DisplayName,
				entry.InputCostPerMillion.String(), entry.OutputCostPerMillion.String(),
				entry.CacheReadCostPerMillion.String(), entry.CacheCreationCostPerMillion.String(),
				entry.Currency, entry.Source); err != nil {
				return fmt.Errorf("insert pricing %s: %w", entry.ModelID, err)
			}
		}
	} else {
		return err
	}

	for _, alias := range snapshot.Aliases {
		if alias.AliasPattern == "" || alias.CanonicalModelID == "" {
			return fmt.Errorf("pricing alias pattern and canonical model id are required")
		}
		if alias.MatchType == "" {
			alias.MatchType = "exact"
		}
		if alias.AliasID == "" {
			alias.AliasID = deterministicID("alias", alias.AliasPattern, alias.MatchType, alias.ScopeApp)
		}
		if alias.CreatedAt.IsZero() {
			alias.CreatedAt = snapshot.CreatedAt
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
		if _, err := tx.Exec(`INSERT INTO settings(key, value) VALUES ('active_pricing_version', ?)
			ON CONFLICT(key) DO UPDATE SET value=excluded.value`, snapshot.VersionID); err != nil {
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
	if value, err := db.GetSetting("pricing_last_updated_at"); err != nil {
		return status, err
	} else if value != "" {
		parsed, err := parseTime(value)
		if err != nil {
			return status, err
		}
		status.LastUpdatedAt = &parsed
	}
	if value, err := db.GetSetting("pricing_last_checked_at"); err != nil {
		return status, err
	} else if value != "" {
		parsed, err := parseTime(value)
		if err != nil {
			return status, err
		}
		status.LastCheckedAt = &parsed
	}
	if value, err := db.GetSetting("pricing_last_synced_at"); err != nil {
		return status, err
	} else if value != "" {
		parsed, err := parseTime(value)
		if err != nil {
			return status, err
		}
		status.LastSyncedAt = &parsed
	}
	var err error
	status.RemoteHash, err = db.GetSetting("pricing_remote_hash")
	if err != nil {
		return status, err
	}
	status.LastError, err = db.GetSetting("pricing_last_error")
	return status, err
}

func (db *DB) MarkPricingSync(remoteHash string, checkedAt, syncedAt time.Time, syncError string) error {
	tx, err := db.conn.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	settings := map[string]string{
		"pricing_last_checked_at": formatTime(checkedAt),
		"pricing_remote_hash":     remoteHash,
		"pricing_last_error":      syncError,
	}
	if !syncedAt.IsZero() {
		settings["pricing_last_synced_at"] = formatTime(syncedAt)
	}
	for key, value := range settings {
		if _, err := tx.Exec(`INSERT INTO settings(key, value) VALUES (?, ?)
			ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value); err != nil {
			return err
		}
	}
	return tx.Commit()
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
	syncStatus, err := db.PricingSyncStatus()
	if err != nil {
		return PricingSnapshot{}, err
	}
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
	aliases := pricing.MergeAliases(local.Aliases, remote.Aliases, policy)
	hash := pricing.HashModels(merged)
	if hash == local.ContentHash && pricing.HashAliases(aliases) == pricing.HashAliases(local.Aliases) {
		return local, conflicts, nil
	}
	aliasHash := pricing.HashAliases(aliases)
	versionID := "merged-" + hashPrefix(hash, 12)
	if aliasHash != pricing.HashAliases(nil) {
		versionID += "-" + hashPrefix(aliasHash, 8)
	}
	now := time.Now().UTC()
	snapshot := PricingSnapshot{
		VersionID: versionID, Source: "merged", CreatedAt: now,
		Models: merged, Aliases: aliases, ContentHash: hash,
	}
	if err := db.ApplyPricingSnapshot(snapshot, true); err != nil {
		return PricingSnapshot{}, 0, err
	}
	return snapshot, conflicts, nil
}

func hashPrefix(value string, length int) string {
	if len(value) <= length {
		return value
	}
	return value[:length]
}
