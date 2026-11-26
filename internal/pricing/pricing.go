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
	default:
		return nil, fmt.Errorf("unsupported custom pricing JSON")
	}
	hash := HashModels(entries)
	return &Snapshot{VersionID: source + "-" + hash[:12], Source: source,
		CreatedAt: time.Now().UTC(), Models: entries, ContentHash: hash}, nil
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
	for _, entry := range local {
		localMap[entry.ModelID] = entry
	}
	result := append([]model.ModelPricing(nil), local...)
	conflicts := 0
	for _, remoteEntry := range remote {
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
			result = append(result, remoteEntry)
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

func decimalFrom(value any) decimal.Decimal {
	switch typed := value.(type) {
	case float64:
		return decimal.NewFromFloat(typed)
	case string:
		return decimal.RequireFromString(typed)
	case json.Number:
		return decimal.RequireFromString(typed.String())
	case int:
		return decimal.NewFromInt(int64(typed))
	case int64:
		return decimal.NewFromInt(typed)
	default:
		return decimal.Zero
	}
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
