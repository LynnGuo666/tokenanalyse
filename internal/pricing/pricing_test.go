package pricing

import (
	"testing"

	"tokenanalyse/internal/model"

	"github.com/shopspring/decimal"
)

func TestMergeKeepBothUsesUniqueModelIDs(t *testing.T) {
	local := []model.ModelPricing{{
		ModelID: "model", DisplayName: "Local", InputCostPerMillion: decimal.NewFromInt(1),
		OutputCostPerMillion: decimal.Zero, CacheReadCostPerMillion: decimal.Zero,
		CacheCreationCostPerMillion: decimal.Zero, Currency: model.CurrencyUSD, Source: "local",
	}}
	remote := []model.ModelPricing{{
		ModelID: "model", DisplayName: "Remote", InputCostPerMillion: decimal.NewFromInt(2),
		OutputCostPerMillion: decimal.Zero, CacheReadCostPerMillion: decimal.Zero,
		CacheCreationCostPerMillion: decimal.Zero, Currency: model.CurrencyUSD, Source: "remote",
	}}
	merged, conflicts := MergeModels(local, remote, MergeKeepBoth)
	if conflicts != 1 || len(merged) != 2 {
		t.Fatalf("unexpected keep-both merge: conflicts=%d models=%+v", conflicts, merged)
	}
	if merged[0].ModelID == merged[1].ModelID {
		t.Fatalf("keep-both produced duplicate model ids: %+v", merged)
	}
	again, _ := MergeModels(merged, remote, MergeKeepBoth)
	if len(again) != 2 {
		t.Fatalf("keep-both merge is not idempotent: %+v", again)
	}
}

func TestMergeAliasesHonorsPolicyAndDeduplicates(t *testing.T) {
	local := []model.ModelAlias{{AliasPattern: "vendor/model", CanonicalModelID: "local", MatchType: "exact", Priority: 100}}
	remote := []model.ModelAlias{
		{AliasPattern: "vendor/model", CanonicalModelID: "remote", MatchType: "exact", Priority: 100},
		{AliasPattern: "vendor/model", CanonicalModelID: "remote", MatchType: "exact", Priority: 100},
		{AliasPattern: "vendor/other", CanonicalModelID: "other", MatchType: "prefix", Priority: 10},
	}
	merged := MergeAliases(local, remote, MergePreferRemote)
	if len(merged) != 2 {
		t.Fatalf("expected two unique aliases, got %+v", merged)
	}
	if merged[0].CanonicalModelID != "remote" {
		t.Fatalf("remote alias did not win: %+v", merged)
	}
	if HashAliases(merged) != HashAliases(append([]model.ModelAlias(nil), merged...)) {
		t.Fatal("alias hash is not stable")
	}
}

func TestLoadCustomIncludesAliases(t *testing.T) {
	snapshot, err := LoadCustom([]byte(`{
		"models": [{"modelId":"model","inputCostPerMillion":"1"}],
		"aliases": [{"aliasPattern":"vendor/model","canonicalModelId":"model","matchType":"exact","priority":100}]
	}`), "custom")
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Aliases) != 1 || snapshot.Aliases[0].CanonicalModelID != "model" {
		t.Fatalf("custom aliases were not loaded: %+v", snapshot.Aliases)
	}
	if _, err := LoadCustom([]byte(`[{"modelId":"model","inputCostPerMillion":"not-a-number"}]`), "custom"); err != nil {
		t.Fatalf("invalid decimal should be handled without panic: %v", err)
	}
}

func TestParseModelsDevAndMerge(t *testing.T) {
	body := []byte(`{
		"provider-a": {
			"models": {
				"vendor/model-v1": {
					"id": "vendor/model-v1",
					"name": "Model V1",
					"cost": {"input": 1, "output": 2, "cache_read": 0.1},
					"canonical_model_id": "model"
				}
			}
		}
	}`)
	snapshot, err := ParseModelsDev(body)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Models) != 1 {
		t.Fatalf("expected 1 model, got %d", len(snapshot.Models))
	}
	if snapshot.Models[0].ModelID != "model" {
		t.Fatalf("unexpected model id: %s", snapshot.Models[0].ModelID)
	}
	if len(snapshot.Aliases) != 1 {
		t.Fatalf("expected alias, got %d", len(snapshot.Aliases))
	}

	local := []model.ModelPricing{snapshot.Models[0]}
	remote := []model.ModelPricing{{
		ModelID: "model", DisplayName: "Model V1",
		InputCostPerMillion: decimal.NewFromInt(5), OutputCostPerMillion: decimal.NewFromInt(10),
		CacheReadCostPerMillion: decimal.NewFromFloat(0.5), CacheCreationCostPerMillion: decimal.NewFromFloat(1.25),
		Currency: model.CurrencyUSD, Source: "remote",
	}}
	merged, conflicts := MergeModels(local, remote, MergePreferRemote)
	if conflicts != 1 {
		t.Fatalf("expected conflict, got %d", conflicts)
	}
	if !merged[0].InputCostPerMillion.Equal(decimal.NewFromInt(5)) {
		t.Fatalf("remote price not selected: %s", merged[0].InputCostPerMillion)
	}
}
