package pricing

import (
	"testing"

	"tokenanalyse/internal/model"

	"github.com/shopspring/decimal"
)

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
