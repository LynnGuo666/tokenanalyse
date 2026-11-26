package storage

import (
	"database/sql"
	"fmt"
	"regexp"
	"strings"
	"time"

	"token-mirror/internal/model"
)

type ModelAlias = model.ModelAlias

func (db *DB) UpsertModelAlias(alias ModelAlias) error {
	if alias.AliasID == "" {
		alias.AliasID = deterministicID("alias", alias.AliasPattern, alias.MatchType, alias.ScopeApp)
	}
	if alias.CreatedAt.IsZero() {
		alias.CreatedAt = time.Now().UTC()
	}
	_, err := db.conn.Exec(`
		INSERT INTO model_aliases(alias_id, alias_pattern, canonical_model_id, match_type, scope_app, priority, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(alias_pattern, match_type, scope_app) DO UPDATE SET
			canonical_model_id=excluded.canonical_model_id,
			priority=excluded.priority
	`, alias.AliasID, alias.AliasPattern, alias.CanonicalModelID, alias.MatchType,
		alias.ScopeApp, alias.Priority, formatTime(alias.CreatedAt))
	if err != nil {
		return fmt.Errorf("upsert model alias: %w", err)
	}
	return nil
}

func (db *DB) ListModelAliases() ([]ModelAlias, error) {
	rows, err := db.conn.Query(`SELECT alias_id, alias_pattern, canonical_model_id, match_type, scope_app, priority, created_at
		FROM model_aliases ORDER BY priority DESC, alias_pattern`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var aliases []ModelAlias
	for rows.Next() {
		var alias ModelAlias
		var createdAt string
		if err := rows.Scan(&alias.AliasID, &alias.AliasPattern, &alias.CanonicalModelID,
			&alias.MatchType, &alias.ScopeApp, &alias.Priority, &createdAt); err != nil {
			return nil, err
		}
		alias.CreatedAt, _ = parseTime(createdAt)
		aliases = append(aliases, alias)
	}
	return aliases, rows.Err()
}

func (db *DB) DeleteModelAlias(aliasID string) error {
	_, err := db.conn.Exec(`DELETE FROM model_aliases WHERE alias_id=?`, aliasID)
	return err
}

func (db *DB) ResolveModel(appType, rawModel string) (string, error) {
	return db.resolveModelTx(nil, appType, rawModel)
}

func (db *DB) resolveModelTx(tx *sql.Tx, appType, rawModel string) (string, error) {
	query := `SELECT alias_pattern, canonical_model_id, match_type
		FROM model_aliases WHERE scope_app='' OR scope_app=? ORDER BY priority DESC, created_at DESC`
	args := []any{appType}
	var rows *sql.Rows
	var err error
	if tx != nil {
		rows, err = tx.Query(query, args...)
	} else {
		rows, err = db.conn.Query(query, args...)
	}
	if err != nil {
		return "", err
	}
	defer rows.Close()
	rawLower := strings.ToLower(rawModel)
	for rows.Next() {
		var pattern, canonical, matchType string
		if err := rows.Scan(&pattern, &canonical, &matchType); err != nil {
			return "", err
		}
		matched := false
		switch matchType {
		case "prefix":
			matched = strings.HasPrefix(rawLower, strings.ToLower(pattern))
		case "regex":
			compiled, compileErr := regexp.Compile("(?i)" + pattern)
			matched = compileErr == nil && compiled.MatchString(rawModel)
		default:
			matched = strings.EqualFold(rawModel, pattern)
		}
		if matched {
			return canonical, nil
		}
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	return model.NormalizeModelID(rawModel), nil
}

func deterministicID(parts ...string) string {
	return model.DeterministicEventID(parts...)
}
