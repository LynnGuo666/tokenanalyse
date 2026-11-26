package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"tokenanalyse/internal/model"
	"tokenanalyse/internal/storage"
	"tokenanalyse/internal/sync"

	"github.com/shopspring/decimal"
)

func TestAPIIngestSummaryAndDuplicate(t *testing.T) {
	db, err := storage.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	prices := []model.ModelPricing{{
		ModelID: "model", DisplayName: "Model",
		InputCostPerMillion: decimal.NewFromInt(1), OutputCostPerMillion: decimal.NewFromInt(2),
		CacheReadCostPerMillion: decimal.Zero, CacheCreationCostPerMillion: decimal.Zero,
		Currency: model.CurrencyUSD, Source: "test",
	}}
	if err := db.CreatePricingVersion("v1", "test", prices); err != nil {
		t.Fatal(err)
	}
	if err := db.SetActivePricingVersion("v1"); err != nil {
		t.Fatal(err)
	}

	state := &AppState{DB: db, Version: "test"}
	httpServer := httptest.NewServer(NewRouter(state))
	defer httpServer.Close()

	event := model.NewUsageEvent("device-a", model.SourceSession, "claude", "s1:m1", "model",
		model.TokenUsage{InputTokens: 1_000_000, OutputTokens: 1_000_000}, time.Now().UTC())
	batch := sync.EventBatch{BatchID: "b1", DeviceID: "device-a", Cursor: 1, Events: []model.UsageEvent{event}}
	payload, _ := json.Marshal(batch)
	response, err := http.Post(httpServer.URL+"/api/v1/events/batch", "application/json", bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("ingest status: %d", response.StatusCode)
	}

	response, err = http.Get(httpServer.URL + "/api/v1/summary")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var summary struct {
		Summary model.UsageSummary `json:"summary"`
	}
	if err := json.NewDecoder(response.Body).Decode(&summary); err != nil {
		t.Fatal(err)
	}
	if summary.Summary.TotalRequests != 1 {
		t.Fatalf("unexpected requests: %d", summary.Summary.TotalRequests)
	}
	if summary.Summary.TotalCost != "3" {
		t.Fatalf("unexpected cost: %s", summary.Summary.TotalCost)
	}

	response, err = http.Post(httpServer.URL+"/api/v1/events/batch", "application/json", bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	var ack sync.BatchAck
	_ = ack
}
