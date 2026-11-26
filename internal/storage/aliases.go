package storage

import (
	"database/sql"
	"fmt"
	"regexp"
	"strings"
	"time"

	"tokenanalyse/internal/model"
)

type ModelAlias = model.ModelAlias

type modelAliasRule struct {
	alias ModelAlias
	regex *regexp.Regexp
}

func (db *DB) UpsertModelAlias(alias ModelAlias) error {
	if alias.AliasPattern == "" || alias.CanonicalModelID == "" {
		return fmt.Errorf("alias pattern and canonical model id are required")
	}
	if alias.MatchType == "" {
		alias.MatchType = "exact"
	}
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
	return db.listModelAliasesTx(nil)
}

func (db *DB) listModelAliasesTx(tx *sql.Tx) ([]ModelAlias, error) {
	query := `SELECT alias_id, alias_pattern, canonical_model_id, match_type, scope_app, priority, created_at
		FROM model_aliases ORDER BY priority DESC, alias_pattern, match_type, scope_app, alias_id`
	var rows *sql.Rows
	var err error
	if tx != nil {
		rows, err = tx.Query(query)
	} else {
		rows, err = db.conn.Query(query)
	}
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
	return db.resolveModelWithFallbackTx(nil, appType, rawModel, "")
}

func (db *DB) resolveModelTx(tx *sql.Tx, appType, rawModel string) (string, error) {
	return db.resolveModelWithFallbackTx(tx, appType, rawModel, "")
}

func (db *DB) resolveModelWithFallbackTx(tx *sql.Tx, appType, rawModel, fallback string) (string, error) {
	rules, err := db.loadAliasRulesTx(tx, appType)
	if err != nil {
		return "", err
	}
	return resolveModelWithAliasRules(rawModel, fallback, rules), nil
}

func resolveModelWithAliasRules(rawModel, fallback string, rules []modelAliasRule) string {
	if resolved, matched := resolveWithAliasRules(rawModel, rules); matched {
		return resolved
	}
	if fallback != "" && !strings.EqualFold(fallback, rawModel) {
		if resolved, matched := resolveWithAliasRules(fallback, rules); matched {
			return resolved
		}
		return fallback
	}
	return model.NormalizeModelID(rawModel)
}

func (db *DB) loadAliasRulesTx(tx *sql.Tx, appType string) ([]modelAliasRule, error) {
	query := `SELECT alias_pattern, canonical_model_id, match_type
		FROM model_aliases WHERE scope_app='' OR scope_app=?
		ORDER BY priority DESC, CASE WHEN scope_app=? THEN 0 ELSE 1 END, created_at DESC, alias_id`
	args := []any{appType, appType}
	var rows *sql.Rows
	var err error
	if tx != nil {
		rows, err = tx.Query(query, args...)
	} else {
		rows, err = db.conn.Query(query, args...)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var rules []modelAliasRule
	for rows.Next() {
		var pattern, canonical, matchType string
		if err := rows.Scan(&pattern, &canonical, &matchType); err != nil {
			return nil, err
		}
		rule := modelAliasRule{alias: ModelAlias{AliasPattern: pattern, CanonicalModelID: canonical, MatchType: matchType}}
		if matchType == "regex" {
			compiled, compileErr := regexp.Compile("(?i)" + pattern)
			if compileErr != nil {
				// Invalid expressions are ignored rather than making every event fail.
				continue
			}
			rule.regex = compiled
		}
		rules = append(rules, rule)
	}
	return rules, rows.Err()
}

func compileAliasRules(aliases []ModelAlias) []modelAliasRule {
	rules := make([]modelAliasRule, 0, len(aliases))
	for _, alias := range aliases {
		rule := modelAliasRule{alias: alias}
		if alias.MatchType == "regex" {
			compiled, err := regexp.Compile("(?i)" + alias.AliasPattern)
			if err != nil {
				continue
			}
			rule.regex = compiled
		}
		rules = append(rules, rule)
	}
	return rules
}

func aliasRulesForApp(rules []modelAliasRule, appType string) []modelAliasRule {
	filtered := make([]modelAliasRule, 0, len(rules))
	for _, rule := range rules {
		if rule.alias.ScopeApp == "" || rule.alias.ScopeApp == appType {
			filtered = append(filtered, rule)
		}
	}
	// listModelAliasesTx returns priority order. Keep application-specific rules
	// ahead of global rules at the same priority.
	for i := 1; i < len(filtered); i++ {
		for j := i; j > 0 && aliasRuleLess(filtered[j], filtered[j-1]); j-- {
			filtered[j], filtered[j-1] = filtered[j-1], filtered[j]
		}
	}
	return filtered
}

func aliasRuleLess(a, b modelAliasRule) bool {
	if a.alias.Priority != b.alias.Priority {
		return a.alias.Priority > b.alias.Priority
	}
	aSpecific := a.alias.ScopeApp != ""
	bSpecific := b.alias.ScopeApp != ""
	if aSpecific != bSpecific {
		return aSpecific
	}
	if a.alias.CreatedAt != b.alias.CreatedAt {
		return a.alias.CreatedAt.After(b.alias.CreatedAt)
	}
	return a.alias.AliasID < b.alias.AliasID
}

func resolveWithAliasRules(rawModel string, rules []modelAliasRule) (string, bool) {
	current := rawModel
	visited := map[string]bool{}
	matchedAny := false
	for step := 0; step <= len(rules); step++ {
		key := strings.ToLower(strings.TrimSpace(current))
		if visited[key] {
			return model.NormalizeModelID(current), matchedAny
		}
		visited[key] = true
		matched := false
		next := ""
		for _, rule := range rules {
			if aliasRuleMatches(rule, current) {
				matched = true
				next = rule.alias.CanonicalModelID
				break
			}
		}
		if !matched || next == "" || strings.EqualFold(next, current) {
			return current, matchedAny
		}
		matchedAny = true
		current = next
	}
	return model.NormalizeModelID(current), matchedAny
}

func aliasRuleMatches(rule modelAliasRule, rawModel string) bool {
	rawLower := strings.ToLower(rawModel)
	switch rule.alias.MatchType {
	case "prefix":
		return strings.HasPrefix(rawLower, strings.ToLower(rule.alias.AliasPattern))
	case "regex":
		return rule.regex != nil && rule.regex.MatchString(rawModel)
	default:
		return strings.EqualFold(rawModel, rule.alias.AliasPattern)
	}
}

func deterministicID(parts ...string) string {
	return model.DeterministicEventID(parts...)
}
