package storage

import (
	"testing"
	"time"

	"tokenanalyse/internal/model"

	"github.com/shopspring/decimal"
)

func TestRealtimePricingAndIdempotency(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	pricingV1 := []model.ModelPricing{{
		ModelID: "test-model", DisplayName: "Test Model",
		InputCostPerMillion: decimal.NewFromInt(1), OutputCostPerMillion: decimal.NewFromInt(2),
		CacheReadCostPerMillion: decimal.Zero, CacheCreationCostPerMillion: decimal.Zero,
		Currency: model.CurrencyUSD, Source: "test",
	}}
	if err := db.CreatePricingVersion("v1", "test", pricingV1); err != nil {
		t.Fatal(err)
	}
	if err := db.SetActivePricingVersion("v1"); err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC()
	event := model.NewUsageEvent("device-a", model.SourceSession, "claude", "session-1:msg-1", "test-model",
		model.TokenUsage{InputTokens: 1_000_000, OutputTokens: 1_000_000}, now)
	first, err := db.InsertEvents([]model.UsageEvent{event}, true)
	if err != nil {
		t.Fatal(err)
	}
	if first.Inserted != 1 || first.Skipped != 0 {
		t.Fatalf("unexpected first insert: %+v", first)
	}
	second, err := db.InsertEvents([]model.UsageEvent{event}, true)
	if err != nil {
		t.Fatal(err)
	}
	if second.Inserted != 0 || second.Skipped != 1 {
		t.Fatalf("unexpected duplicate insert: %+v", second)
	}

	summary, err := db.GetSummary(model.UsageFilters{})
	if err != nil {
		t.Fatal(err)
	}
	if summary.TotalCost != "3" {
		t.Fatalf("expected cost 3, got %s", summary.TotalCost)
	}
	if summary.RealTotalTokens != 2_000_000 {
		t.Fatalf("unexpected tokens: %d", summary.RealTotalTokens)
	}

	pricingV2 := []model.ModelPricing{{
		ModelID: "test-model", DisplayName: "Test Model",
		InputCostPerMillion: decimal.NewFromInt(10), OutputCostPerMillion: decimal.NewFromInt(20),
		CacheReadCostPerMillion: decimal.Zero, CacheCreationCostPerMillion: decimal.Zero,
		Currency: model.CurrencyUSD, Source: "test",
	}}
	if err := db.CreatePricingVersion("v2", "test", pricingV2); err != nil {
		t.Fatal(err)
	}
	if err := db.SetActivePricingVersion("v2"); err != nil {
		t.Fatal(err)
	}
	summary, err = db.GetSummary(model.UsageFilters{})
	if err != nil {
		t.Fatal(err)
	}
	if summary.TotalCost != "30" {
		t.Fatalf("expected realtime repriced cost 30, got %s", summary.TotalCost)
	}
}

func TestAliasResolutionAndRemoteEventDoesNotEnqueue(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.UpsertModelAlias(model.ModelAlias{AliasPattern: "vendor/model-v1", CanonicalModelID: "model", MatchType: "exact", Priority: 100}); err != nil {
		t.Fatal(err)
	}
	resolved, err := db.ResolveModel("claude", "vendor/model-v1")
	if err != nil {
		t.Fatal(err)
	}
	if resolved != "model" {
		t.Fatalf("expected model, got %s", resolved)
	}

	event := model.NewUsageEvent("device-b", model.SourceImport, "claude", "remote-1", "vendor/model-v1",
		model.TokenUsage{InputTokens: 10}, time.Now().UTC())
	if _, err := db.InsertEvents([]model.UsageEvent{event}, false); err != nil {
		t.Fatal(err)
	}
	pending, err := db.PendingOutbox(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Fatalf("remote event should not enqueue, got %d", len(pending))
	}
}
