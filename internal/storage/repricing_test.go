package storage

import (
	"fmt"
	"testing"
	"time"

	"tokenanalyse/internal/model"

	"github.com/shopspring/decimal"
)

func TestRepriceAllParallelMatchesSerialAndIsIdempotent(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.CreatePricingVersion("v1", "test", repriceTestPrices(1)); err != nil {
		t.Fatal(err)
	}
	if err := db.SetActivePricingVersion("v1"); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertModelAlias(model.ModelAlias{AliasPattern: "vendor/model-a", CanonicalModelID: "model-a", MatchType: "exact", Priority: 100}); err != nil {
		t.Fatal(err)
	}

	events := make([]model.UsageEvent, 0, 600)
	for i := 0; i < 600; i++ {
		app := "claude"
		if i%2 == 0 {
			app = "codex"
		}
		rawModel := "model-b"
		if i%3 == 0 {
			rawModel = "vendor/model-a"
		}
		event := model.NewUsageEvent("device-a", model.SourceSession, app, fmt.Sprintf("s1:m%d", i), rawModel,
			model.TokenUsage{InputTokens: 1000, OutputTokens: 100, CacheReadTokens: 50}, time.Now().UTC())
		if i%5 == 0 {
			event.CostMultiplier = "2"
		}
		events = append(events, event)
	}
	if _, err := db.InsertEvents(events, false); err != nil {
		t.Fatal(err)
	}

	if err := db.UpsertModelAlias(model.ModelAlias{AliasPattern: "vendor/model-a", CanonicalModelID: "model-b", MatchType: "exact", Priority: 100}); err != nil {
		t.Fatal(err)
	}
	if err := db.CreatePricingVersion("v2", "test", repriceTestPrices(10)); err != nil {
		t.Fatal(err)
	}
	if err := db.SetActivePricingVersion("v2"); err != nil {
		t.Fatal(err)
	}

	readTx, err := db.conn.Begin()
	if err != nil {
		t.Fatal(err)
	}
	records, err := db.loadRepriceEventRecordsTx(readTx)
	if err != nil {
		_ = readTx.Rollback()
		t.Fatal(err)
	}
	catalog, err := loadPricingCatalogTx(readTx, "v2")
	if err != nil {
		_ = readTx.Rollback()
		t.Fatal(err)
	}
	aliases, err := db.listModelAliasesTx(readTx)
	_ = readTx.Rollback()
	if err != nil {
		t.Fatal(err)
	}
	rate, err := db.CNYExchangeRate()
	if err != nil {
		t.Fatal(err)
	}
	rulesByApp := buildRepriceRulesByApp(records, aliases)
	serial := make([]repriceComputeResult, 0, len(records))
	for _, record := range records {
		serial = append(serial, calculateRepriceResult(record, catalog, rulesByApp[record.appType], rate, "v2"))
	}
	parallel, err := runRepriceWorkers(records, catalog, rulesByApp, rate, "v2", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(serial) != len(parallel) {
		t.Fatalf("serial/parallel result count mismatch: %d != %d", len(serial), len(parallel))
	}
	serialByID := make(map[string]repriceComputeResult, len(serial))
	for _, result := range serial {
		serialByID[result.record.id] = result
	}
	for _, parallelResult := range parallel {
		serialResult, ok := serialByID[parallelResult.record.id]
		if !ok || serialResult.canonical != parallelResult.canonical ||
			serialResult.pricingModel != parallelResult.pricingModel || serialResult.cost != parallelResult.cost ||
			serialResult.changed != parallelResult.changed || serialResult.err != nil || parallelResult.err != nil {
			t.Fatalf("serial/parallel mismatch: serial=%+v parallel=%+v", serialResult, parallelResult)
		}
	}

	count, err := db.RepriceAll()
	if err != nil || count != len(events) {
		t.Fatalf("RepriceAll failed: count=%d err=%v", count, err)
	}
	firstLogs, err := db.GetRequestLogs(model.UsageFilters{}, 1, 1000)
	if err != nil {
		t.Fatal(err)
	}
	count, err = db.RepriceAll()
	if err != nil || count != len(events) {
		t.Fatalf("second RepriceAll failed: count=%d err=%v", count, err)
	}
	secondLogs, err := db.GetRequestLogs(model.UsageFilters{}, 1, 1000)
	if err != nil {
		t.Fatal(err)
	}
	for i := range firstLogs.Items {
		first := firstLogs.Items[i]
		second := secondLogs.Items[i]
		if first.EventID != second.EventID || first.TotalCostUSD != second.TotalCostUSD ||
			first.TotalCostCNY != second.TotalCostCNY || first.CanonicalModel != second.CanonicalModel ||
			first.PricingModel != second.PricingModel {
			t.Fatalf("reprice is not idempotent at %d: first=%+v second=%+v", i, first, second)
		}
		if first.Metadata["repricedAt"] != second.Metadata["repricedAt"] {
			t.Fatalf("unchanged reprice updated metadata at %d: %v != %v", i, first.Metadata["repricedAt"], second.Metadata["repricedAt"])
		}
	}
}

func TestRepriceJobStatusTransitionsAndFailure(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.StartRepriceJob(1); err != nil {
		t.Fatal(err)
	}
	running, err := db.GetRepriceJobStatus()
	if err != nil {
		t.Fatal(err)
	}
	if running.Status != RepriceJobRunning || running.Total != 1 || running.Done != 0 || running.StartedAt == nil {
		t.Fatalf("unexpected running status: %+v", running)
	}
	startedAt := *running.StartedAt

	if err := db.CreatePricingVersion("v1", "test", repriceTestPrices(1)); err != nil {
		t.Fatal(err)
	}
	if err := db.SetActivePricingVersion("v1"); err != nil {
		t.Fatal(err)
	}
	event := model.NewUsageEvent("device-a", model.SourceSession, "claude", "s1:m1", "model-a",
		model.TokenUsage{InputTokens: 1_000_000}, time.Now().UTC())
	if _, err := db.InsertEvents([]model.UsageEvent{event}, false); err != nil {
		t.Fatal(err)
	}
	if _, err := db.RepriceAll(); err != nil {
		t.Fatal(err)
	}
	completed, err := db.GetRepriceJobStatus()
	if err != nil {
		t.Fatal(err)
	}
	if completed.Status != RepriceJobCompleted || completed.Total != 1 || completed.Done != 1 ||
		completed.FinishedAt == nil || completed.Error != "" || completed.StartedAt == nil || !completed.StartedAt.Equal(startedAt) {
		t.Fatalf("unexpected completed status: %+v", completed)
	}

	emptyDB, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer emptyDB.Close()
	if _, err := emptyDB.RepriceAll(); err == nil {
		t.Fatal("expected reprice without active pricing to fail")
	}
	failed, err := emptyDB.GetRepriceJobStatus()
	if err != nil {
		t.Fatal(err)
	}
	if failed.Status != RepriceJobFailed || failed.Error == "" || failed.FinishedAt == nil {
		t.Fatalf("unexpected failed status: %+v", failed)
	}
}

func TestRealtimePricingDoesNotRequireRepriceJob(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.CreatePricingVersion("v1", "test", repriceTestPrices(1)); err != nil {
		t.Fatal(err)
	}
	if err := db.SetActivePricingVersion("v1"); err != nil {
		t.Fatal(err)
	}
	event := model.NewUsageEvent("device-a", model.SourceSession, "claude", "s1:m1", "model-a",
		model.TokenUsage{InputTokens: 1_000_000}, time.Now().UTC())
	if _, err := db.InsertEvents([]model.UsageEvent{event}, false); err != nil {
		t.Fatal(err)
	}
	if err := db.CreatePricingVersion("v2", "test", repriceTestPrices(10)); err != nil {
		t.Fatal(err)
	}
	if err := db.SetActivePricingVersion("v2"); err != nil {
		t.Fatal(err)
	}
	summary, err := db.GetSummary(model.UsageFilters{})
	if err != nil {
		t.Fatal(err)
	}
	if summary.TotalCost != "10" {
		t.Fatalf("realtime query did not use new pricing: %+v", summary)
	}
	status, err := db.GetRepriceJobStatus()
	if err != nil {
		t.Fatal(err)
	}
	if status.Status != "" {
		t.Fatalf("realtime query should not start a reprice job: %+v", status)
	}
}

func repriceTestPrices(multiplier int64) []model.ModelPricing {
	return []model.ModelPricing{
		{
			ModelID: "model-a", DisplayName: "Model A", InputCostPerMillion: decimal.NewFromInt(multiplier),
			OutputCostPerMillion: decimal.NewFromInt(multiplier * 2), CacheReadCostPerMillion: decimal.NewFromInt(multiplier / 10),
			CacheCreationCostPerMillion: decimal.NewFromInt(multiplier), Currency: model.CurrencyUSD, Source: "test",
		},
		{
			ModelID: "model-b", DisplayName: "Model B", InputCostPerMillion: decimal.NewFromInt(multiplier * 3),
			OutputCostPerMillion: decimal.NewFromInt(multiplier * 4), CacheReadCostPerMillion: decimal.NewFromInt(multiplier / 5),
			CacheCreationCostPerMillion: decimal.NewFromInt(multiplier * 2), Currency: model.CurrencyUSD, Source: "test",
		},
	}
}
