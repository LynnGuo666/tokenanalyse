package pricing

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"tokenanalyse/internal/model"

	"github.com/shopspring/decimal"
)

const ModelsDevURL = "https://models.dev/api.json"

type Snapshot struct {
	VersionID   string               `json:"versionId"`
	Source      string               `json:"source"`
	CreatedAt   time.Time            `json:"createdAt"`
	Models      []model.ModelPricing `json:"models"`
	Aliases     []model.ModelAlias   `json:"aliases"`
	ContentHash string               `json:"contentHash"`
}

func FetchModelsDev(ctx context.Context) (*Snapshot, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, ModelsDevURL, nil)
	if err != nil {
		return nil, err
	}
	client := &http.Client{Timeout: 60 * time.Second}
	response, err := client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("fetch models.dev: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("models.dev HTTP %d", response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 32<<20))
	if err != nil {
		return nil, err
	}
	return ParseModelsDev(body)
}

func ParseModelsDev(body []byte) (*Snapshot, error) {
	var providers map[string]struct {
		Models map[string]struct {
			ID               string         `json:"id"`
			Name             string         `json:"name"`
			Cost             map[string]any `json:"cost"`
			CanonicalModelID string         `json:"canonical_model_id"`
		} `json:"models"`
	}
	if err := json.Unmarshal(body, &providers); err != nil {
		return nil, fmt.Errorf("parse models.dev: %w", err)
	}
	models := []model.ModelPricing{}
	aliases := []model.ModelAlias{}
	seen := map[string]bool{}
	providerIDs := make([]string, 0, len(providers))
	for providerID := range providers {
		providerIDs = append(providerIDs, providerID)
	}
	sort.Strings(providerIDs)
	for _, providerID := range providerIDs {
		provider := providers[providerID]
		modelIDs := make([]string, 0, len(provider.Models))
		for modelID := range provider.Models {
			modelIDs = append(modelIDs, modelID)
		}
		sort.Strings(modelIDs)
		for _, rawID := range modelIDs {
			entry := provider.Models[rawID]
			modelID := entry.ID
			if modelID == "" {
				modelID = rawID
			}
			cost := entry.Cost
			input := decimalFrom(cost["input"])
			output := decimalFrom(cost["output"])
			cacheRead := decimalFrom(cost["cache_read"])
			cacheWrite := decimalFrom(cost["cache_write"])
			if cacheRead.IsZero() && !input.IsZero() {
				cacheRead = input.Mul(decimal.NewFromFloat(0.1))
			}
			if cacheWrite.IsZero() && !input.IsZero() {
				cacheWrite = input.Mul(decimal.NewFromFloat(1.25))
			}
			displayName := entry.Name
			if displayName == "" {
				displayName = modelID
			}
			canonical := entry.CanonicalModelID
			if canonical == "" {
				canonical = model.NormalizeModelID(modelID)
			}
			if seen[canonical] {
				continue
			}
			seen[canonical] = true
			models = append(models, model.ModelPricing{
				ModelID: canonical, DisplayName: displayName,
				InputCostPerMillion: input, OutputCostPerMillion: output,
				CacheReadCostPerMillion: cacheRead, CacheCreationCostPerMillion: cacheWrite,
				Currency: model.CurrencyUSD, Source: "models.dev",
			})
			if !strings.EqualFold(modelID, canonical) {
				aliases = append(aliases, model.ModelAlias{
					AliasID:      model.DeterministicEventID("models.dev", modelID),
					AliasPattern: modelID, CanonicalModelID: canonical,
					MatchType: "exact", Priority: 100,
					CreatedAt: time.Now().UTC(),
				})
			}
		}
	}
	sort.Slice(models, func(i, j int) bool { return models[i].ModelID < models[j].ModelID })
	hash := HashModels(models)
	now := time.Now().UTC()
	return &Snapshot{VersionID: "models-dev-" + hash[:12], Source: "models.dev", CreatedAt: now,
		Models: models, Aliases: aliases, ContentHash: hash}, nil
}

func LoadCustom(body []byte, source string) (*Snapshot, error) {
	var payload any
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, err
	}
	var entries []model.ModelPricing
	var aliases []model.ModelAlias
	switch typed := payload.(type) {
	case []any:
		for _, item := range typed {
			entry, err := parseCustomEntry(item)
			if err != nil {
				return nil, err
			}
			entries = append(entries, entry)
		}
	case map[string]any:
		if list, ok := typed["models"].([]any); ok {
			for _, item := range list {
				entry, err := parseCustomEntry(item)
				if err != nil {
					return nil, err
				}
				entries = append(entries, entry)
			}
		} else {
			return nil, fmt.Errorf("custom pricing JSON must be an array or contain a models array")
		}
		if rawAliases, ok := typed["aliases"].([]any); ok {
			encoded, err := json.Marshal(rawAliases)
			if err != nil {
				return nil, err
			}
			if err := json.Unmarshal(encoded, &aliases); err != nil {
				return nil, fmt.Errorf("parse custom pricing aliases: %w", err)
			}
			for i := range aliases {
				if aliases[i].AliasPattern == "" || aliases[i].CanonicalModelID == "" {
					return nil, fmt.Errorf("custom pricing alias requires aliasPattern and canonicalModelId")
				}
				if aliases[i].MatchType == "" {
					aliases[i].MatchType = "exact"
				}
			}
		}
	default:
		return nil, fmt.Errorf("unsupported custom pricing JSON")
	}
	hash := HashModels(entries)
	versionID := source + "-" + hash[:12]
	if aliasHash := HashAliases(aliases); aliasHash != HashAliases(nil) {
		versionID += "-" + aliasHash[:8]
	}
	return &Snapshot{VersionID: versionID, Source: source,
		CreatedAt: time.Now().UTC(), Models: entries, Aliases: aliases, ContentHash: hash}, nil
}

func parseCustomEntry(value any) (model.ModelPricing, error) {
	object, ok := value.(map[string]any)
	if !ok {
		return model.ModelPricing{}, fmt.Errorf("pricing entry must be an object")
	}
	modelID := stringValue(object["modelId"])
	if modelID == "" {
		modelID = stringValue(object["model_id"])
	}
	if modelID == "" {
		modelID = stringValue(object["id"])
	}
	if modelID == "" {
		return model.ModelPricing{}, fmt.Errorf("pricing entry missing modelId")
	}
	displayName := stringValue(object["displayName"])
	if displayName == "" {
		displayName = stringValue(object["name"])
	}
	if displayName == "" {
		displayName = modelID
	}
	currency := model.Currency(strings.ToUpper(stringValue(object["currency"])))
	if currency == "" {
		currency = model.CurrencyUSD
	}
	return model.ModelPricing{
		ModelID: modelID, DisplayName: displayName,
		InputCostPerMillion:         decimalFrom(object["inputCostPerMillion"]),
		OutputCostPerMillion:        decimalFrom(object["outputCostPerMillion"]),
		CacheReadCostPerMillion:     decimalFrom(object["cacheReadCostPerMillion"]),
		CacheCreationCostPerMillion: decimalFrom(object["cacheCreationCostPerMillion"]),
		Currency:                    currency, Source: "custom",
	}, nil
}

type MergePolicy string

const (
	MergePreferNewer  MergePolicy = "prefer_newer"
	MergePreferLocal  MergePolicy = "prefer_local"
	MergePreferRemote MergePolicy = "prefer_remote"
	MergeKeepBoth     MergePolicy = "keep_both"
)

func MergeModels(local, remote []model.ModelPricing, policy MergePolicy) ([]model.ModelPricing, int) {
	localMap := map[string]model.ModelPricing{}
	result := make([]model.ModelPricing, 0, len(local)+len(remote))
	for _, entry := range local {
		if entry.ModelID == "" {
			continue
		}
		localMap[entry.ModelID] = entry
	}
	seen := map[string]bool{}
	for _, entry := range local {
		if entry.ModelID == "" || seen[entry.ModelID] {
			continue
		}
		seen[entry.ModelID] = true
		result = append(result, localMap[entry.ModelID])
	}
	conflicts := 0
	for _, remoteEntry := range remote {
		if remoteEntry.ModelID == "" {
			continue
		}
		localEntry, exists := localMap[remoteEntry.ModelID]
		if !exists {
			result = append(result, remoteEntry)
			localMap[remoteEntry.ModelID] = remoteEntry
			continue
		}
		if pricingEqual(localEntry, remoteEntry) {
			continue
		}
		conflicts++
		switch policy {
		case MergePreferRemote:
			for i := range result {
				if result[i].ModelID == remoteEntry.ModelID {
					result[i] = remoteEntry
				}
			}
			localMap[remoteEntry.ModelID] = remoteEntry
		case MergeKeepBoth:
			// model_pricing has a (version_id, model_id) primary key. Keep the
			// remote conflicting price under a deterministic namespaced ID so the
			// merged snapshot remains representable and idempotent.
			remoteEntry.ModelID = keepBothModelID(remoteEntry)
			if existing, ok := localMap[remoteEntry.ModelID]; ok {
				if pricingEqual(existing, remoteEntry) {
					continue
				}
				for {
					remoteEntry.ModelID += "-remote"
					if _, exists := localMap[remoteEntry.ModelID]; !exists {
						break
					}
				}
			}
			result = append(result, remoteEntry)
			localMap[remoteEntry.ModelID] = remoteEntry
		case MergePreferLocal:
			// keep local
		default:
			// Deterministic default: prefer the lexicographically larger content hash.
			if HashModels([]model.ModelPricing{remoteEntry}) > HashModels([]model.ModelPricing{localEntry}) {
				for i := range result {
					if result[i].ModelID == remoteEntry.ModelID {
						result[i] = remoteEntry
					}
				}
				localMap[remoteEntry.ModelID] = remoteEntry
			}
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ModelID < result[j].ModelID })
	return result, conflicts
}

func keepBothModelID(entry model.ModelPricing) string {
	return entry.ModelID + "@" + HashModels([]model.ModelPricing{entry})[:8]
}

// MergeAliases merges alias rules using the same conflict policy as model
// prices. Alias identity matches the database unique key.
func MergeAliases(local, remote []model.ModelAlias, policy MergePolicy) []model.ModelAlias {
	type aliasKey struct {
		pattern, matchType, scope string
	}
	keyFor := func(alias model.ModelAlias) aliasKey {
		return aliasKey{alias.AliasPattern, alias.MatchType, alias.ScopeApp}
	}
	result := make([]model.ModelAlias, 0, len(local)+len(remote))
	index := map[aliasKey]int{}
	for _, alias := range local {
		if alias.MatchType == "" {
			alias.MatchType = "exact"
		}
		key := keyFor(alias)
		if _, exists := index[key]; exists {
			continue
		}
		index[key] = len(result)
		result = append(result, alias)
	}
	for _, alias := range remote {
		if alias.MatchType == "" {
			alias.MatchType = "exact"
		}
		key := keyFor(alias)
		if i, exists := index[key]; exists {
			if policy == MergePreferRemote || (policy != MergePreferLocal && policy != MergeKeepBoth && !aliasesEqual(result[i], alias)) {
				result[i] = alias
			}
			continue
		}
		index[key] = len(result)
		result = append(result, alias)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Priority != result[j].Priority {
			return result[i].Priority > result[j].Priority
		}
		if result[i].AliasPattern != result[j].AliasPattern {
			return result[i].AliasPattern < result[j].AliasPattern
		}
		if result[i].MatchType != result[j].MatchType {
			return result[i].MatchType < result[j].MatchType
		}
		return result[i].ScopeApp < result[j].ScopeApp
	})
	return result
}

func aliasesEqual(a, b model.ModelAlias) bool {
	return a.AliasPattern == b.AliasPattern && a.CanonicalModelID == b.CanonicalModelID &&
		a.MatchType == b.MatchType && a.ScopeApp == b.ScopeApp && a.Priority == b.Priority
}

func HashModels(models []model.ModelPricing) string {
	sorted := append([]model.ModelPricing(nil), models...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].ModelID < sorted[j].ModelID })
	hash := sha256.New()
	for _, entry := range sorted {
		hash.Write([]byte(entry.ModelID))
		hash.Write([]byte{0})
		hash.Write([]byte(entry.InputCostPerMillion.String()))
		hash.Write([]byte{0})
		hash.Write([]byte(entry.OutputCostPerMillion.String()))
		hash.Write([]byte{0})
		hash.Write([]byte(entry.CacheReadCostPerMillion.String()))
		hash.Write([]byte{0})
		hash.Write([]byte(entry.CacheCreationCostPerMillion.String()))
		hash.Write([]byte{0})
		hash.Write([]byte(string(entry.Currency)))
		hash.Write([]byte{0})
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func pricingEqual(a, b model.ModelPricing) bool {
	return a.ModelID == b.ModelID && a.InputCostPerMillion.Equal(b.InputCostPerMillion) &&
		a.OutputCostPerMillion.Equal(b.OutputCostPerMillion) &&
		a.CacheReadCostPerMillion.Equal(b.CacheReadCostPerMillion) &&
		a.CacheCreationCostPerMillion.Equal(b.CacheCreationCostPerMillion) && a.Currency == b.Currency
}

// HashAliases returns a stable content hash for alias configuration. IDs and
// creation timestamps are deliberately excluded because they are local
// bookkeeping rather than alias semantics.
func HashAliases(aliases []model.ModelAlias) string {
	sorted := append([]model.ModelAlias(nil), aliases...)
	for i := range sorted {
		if sorted[i].MatchType == "" {
			sorted[i].MatchType = "exact"
		}
	}
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].AliasPattern != sorted[j].AliasPattern {
			return sorted[i].AliasPattern < sorted[j].AliasPattern
		}
		if sorted[i].MatchType != sorted[j].MatchType {
			return sorted[i].MatchType < sorted[j].MatchType
		}
		if sorted[i].ScopeApp != sorted[j].ScopeApp {
			return sorted[i].ScopeApp < sorted[j].ScopeApp
		}
		if sorted[i].CanonicalModelID != sorted[j].CanonicalModelID {
			return sorted[i].CanonicalModelID < sorted[j].CanonicalModelID
		}
		return sorted[i].Priority < sorted[j].Priority
	})
	hash := sha256.New()
	for _, alias := range sorted {
		hash.Write([]byte(alias.AliasPattern))
		hash.Write([]byte{0})
		hash.Write([]byte(alias.CanonicalModelID))
		hash.Write([]byte{0})
		hash.Write([]byte(alias.MatchType))
		hash.Write([]byte{0})
		hash.Write([]byte(alias.ScopeApp))
		hash.Write([]byte{0})
		hash.Write([]byte(fmt.Sprintf("%d", alias.Priority)))
		hash.Write([]byte{0})
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func decimalFrom(value any) decimal.Decimal {
	switch typed := value.(type) {
	case float64:
		return decimal.NewFromFloat(typed)
	case string:
		parsed, err := decimal.NewFromString(strings.TrimSpace(typed))
		if err == nil {
			return parsed
		}
	case json.Number:
		parsed, err := decimal.NewFromString(typed.String())
		if err == nil {
			return parsed
		}
	case int:
		return decimal.NewFromInt(int64(typed))
	case int64:
		return decimal.NewFromInt(typed)
	}
	return decimal.Zero
}

func stringValue(value any) string {
	if value == nil {
		return ""
	}
	if typed, ok := value.(string); ok {
		return typed
	}
	return fmt.Sprint(value)
}
