package storage

import (
	"testing"
	"time"

	"tokenanalyse/internal/model"
	"tokenanalyse/internal/pricing"

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

func TestRepriceAllRefreshesAliasesAndCosts(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	priceV1 := []model.ModelPricing{{
		ModelID: "model", DisplayName: "Model", InputCostPerMillion: decimal.NewFromInt(1),
		OutputCostPerMillion: decimal.Zero, CacheReadCostPerMillion: decimal.Zero,
		CacheCreationCostPerMillion: decimal.Zero, Currency: model.CurrencyUSD, Source: "test",
	}}
	if err := db.CreatePricingVersion("v1", "test", priceV1); err != nil {
		t.Fatal(err)
	}
	if err := db.SetActivePricingVersion("v1"); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertModelAlias(model.ModelAlias{AliasPattern: "vendor/model-v1", CanonicalModelID: "model", MatchType: "exact", Priority: 100}); err != nil {
		t.Fatal(err)
	}
	event := model.NewUsageEvent("device-a", model.SourceSession, "claude", "s1:m1", "vendor/model-v1",
		model.TokenUsage{InputTokens: 1_000_000}, time.Now().UTC())
	event.CostMultiplier = ""
	if _, err := db.InsertEvents([]model.UsageEvent{event}, false); err != nil {
		t.Fatal(err)
	}
	summary, err := db.GetSummary(model.UsageFilters{})
	if err != nil || summary.TotalCost != "1" {
		t.Fatalf("unexpected initial summary: %+v err=%v", summary, err)
	}

	if err := db.UpsertModelAlias(model.ModelAlias{AliasPattern: "vendor/model-v1", CanonicalModelID: "renamed-model", MatchType: "exact", Priority: 100}); err != nil {
		t.Fatal(err)
	}
	priceV2 := []model.ModelPricing{{
		ModelID: "renamed-model", DisplayName: "Renamed", InputCostPerMillion: decimal.NewFromInt(10),
		OutputCostPerMillion: decimal.Zero, CacheReadCostPerMillion: decimal.Zero,
		CacheCreationCostPerMillion: decimal.Zero, Currency: model.CurrencyUSD, Source: "test",
	}}
	if err := db.CreatePricingVersion("v2", "test", priceV2); err != nil {
		t.Fatal(err)
	}
	if err := db.SetActivePricingVersion("v2"); err != nil {
		t.Fatal(err)
	}
	count, err := db.RepriceAll()
	if err != nil || count != 1 {
		t.Fatalf("reprice failed: count=%d err=%v", count, err)
	}
	detail, err := db.GetRequestDetail(event.EventID)
	if err != nil {
		t.Fatal(err)
	}
	if detail.CanonicalModel != "renamed-model" || detail.PricingModel != "renamed-model" || detail.TotalCostUSD != "10" {
		t.Fatalf("alias/cost was not refreshed: %+v", detail)
	}
	if detail.Metadata["repricedAt"] == nil {
		t.Fatalf("missing repricedAt metadata: %+v", detail.Metadata)
	}
}

func TestMergePricingSnapshotIdempotentAndAliasOnlyChanges(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	models := []model.ModelPricing{{
		ModelID: "model", DisplayName: "Model", InputCostPerMillion: decimal.NewFromInt(1),
		OutputCostPerMillion: decimal.Zero, CacheReadCostPerMillion: decimal.Zero,
		CacheCreationCostPerMillion: decimal.Zero, Currency: model.CurrencyUSD, Source: "test",
	}}
	if err := db.CreatePricingVersion("v1", "test", models); err != nil {
		t.Fatal(err)
	}
	if err := db.SetActivePricingVersion("v1"); err != nil {
		t.Fatal(err)
	}
	remote := PricingSnapshot{VersionID: "remote-v1", Source: "remote", CreatedAt: time.Now().UTC(), Models: models,
		Aliases: []model.ModelAlias{{AliasPattern: "vendor/model", CanonicalModelID: "model", MatchType: "exact", Priority: 100}}}
	remote.ContentHash = pricing.HashModels(remote.Models)
	first, _, err := db.MergePricingSnapshot(remote, pricing.MergePreferRemote)
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := db.MergePricingSnapshot(remote, pricing.MergePreferRemote)
	if err != nil {
		t.Fatal(err)
	}
	if first.VersionID != second.VersionID {
		t.Fatalf("identical merge created a new version: %s -> %s", first.VersionID, second.VersionID)
	}

	remote.Aliases[0].CanonicalModelID = "other-model"
	third, _, err := db.MergePricingSnapshot(remote, pricing.MergePreferRemote)
	if err != nil {
		t.Fatal(err)
	}
	if third.VersionID == second.VersionID {
		t.Fatalf("alias-only change did not create a new version: %s", third.VersionID)
	}
	resolved, err := db.ResolveModel("claude", "vendor/model")
	if err != nil || resolved != "other-model" {
		t.Fatalf("alias-only change was not applied: resolved=%q err=%v", resolved, err)
	}
}

func TestApplyPricingSnapshotIsIdempotent(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	models := []model.ModelPricing{{
		ModelID: "model", DisplayName: "Model", InputCostPerMillion: decimal.NewFromInt(1),
		OutputCostPerMillion: decimal.Zero, CacheReadCostPerMillion: decimal.Zero,
		CacheCreationCostPerMillion: decimal.Zero, Currency: model.CurrencyUSD, Source: "test",
	}}
	snapshot := PricingSnapshot{VersionID: "same-version", Source: "test", CreatedAt: time.Now().UTC(),
		Models: models, ContentHash: pricing.HashModels(models)}
	if err := db.ApplyPricingSnapshot(snapshot, true); err != nil {
		t.Fatal(err)
	}
	if err := db.ApplyPricingSnapshot(snapshot, true); err != nil {
		t.Fatalf("reapplying the same pricing snapshot should be idempotent: %v", err)
	}
	active, err := db.ActivePricingSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if active.VersionID != snapshot.VersionID || len(active.Models) != 1 {
		t.Fatalf("unexpected active snapshot: %+v", active)
	}
}

func TestPricingSyncMetadata(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	checked := time.Now().UTC().Truncate(time.Second)
	synced := checked.Add(time.Second)
	if err := db.MarkPricingSync("remote-1", checked, synced, ""); err != nil {
		t.Fatal(err)
	}
	if err := db.MarkPricingSync("remote-2", checked.Add(2*time.Second), time.Time{}, "boom"); err != nil {
		t.Fatal(err)
	}
	status, err := db.PricingSyncStatus()
	if err != nil {
		t.Fatal(err)
	}
	if status.RemoteHash != "remote-2" || status.LastError != "boom" || status.LastCheckedAt == nil || status.LastSyncedAt == nil {
		t.Fatalf("unexpected pricing sync status: %+v", status)
	}
	if !status.LastSyncedAt.Equal(synced) {
		t.Fatalf("last synced timestamp should be preserved on failed sync: %v", status.LastSyncedAt)
	}
}

func TestResolveModelAliasChainAndCycle(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, alias := range []model.ModelAlias{
		{AliasPattern: "a", CanonicalModelID: "b", MatchType: "exact", Priority: 100},
		{AliasPattern: "b", CanonicalModelID: "c", MatchType: "exact", Priority: 90},
		{AliasPattern: "x", CanonicalModelID: "y", MatchType: "exact", Priority: 100},
		{AliasPattern: "y", CanonicalModelID: "x", MatchType: "exact", Priority: 90},
	} {
		if err := db.UpsertModelAlias(alias); err != nil {
			t.Fatal(err)
		}
	}
	resolved, err := db.ResolveModel("claude", "a")
	if err != nil || resolved != "c" {
		t.Fatalf("alias chain was not resolved: resolved=%q err=%v", resolved, err)
	}
	done := make(chan string, 1)
	go func() {
		value, _ := db.ResolveModel("claude", "x")
		done <- value
	}()
	select {
	case resolved := <-done:
		if resolved == "" {
			t.Fatal("alias cycle resolved to an empty model")
		}
	case <-time.After(time.Second):
		t.Fatal("alias cycle caused resolution to hang")
	}
}

func TestSessionSummaryThenQueryDoesNotDeadlock(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	event := model.NewUsageEvent("device-a", model.SourceSession, "claude", "s1:m1", "model",
		model.TokenUsage{InputTokens: 10}, time.Now().UTC())
	event.SessionID = "s1"
	if _, err := db.InsertEvents([]model.UsageEvent{event}, false); err != nil {
		t.Fatal(err)
	}
	sessions, err := db.GetSessionSummaries(model.UsageFilters{})
	if err != nil || len(sessions) != 1 {
		t.Fatalf("unexpected sessions: %+v err=%v", sessions, err)
	}
	if _, err := db.GetSummary(model.UsageFilters{}); err != nil {
		t.Fatalf("query after session summary failed: %v", err)
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
