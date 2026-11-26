package sync

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"tokenanalyse/internal/model"
	"tokenanalyse/internal/pricing"
	"tokenanalyse/internal/storage"

	"github.com/shopspring/decimal"
)

func TestServerSyncRecordsPricingMetadataAndPreservesRemoteHashOnError(t *testing.T) {
	db, err := storage.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	remote := storage.PricingSnapshot{VersionID: "remote-v1", Source: "remote", CreatedAt: time.Now().UTC(),
		Models: []model.ModelPricing{{
			ModelID: "model", DisplayName: "Model", InputCostPerMillion: decimal.NewFromInt(2),
			OutputCostPerMillion: decimal.Zero, CacheReadCostPerMillion: decimal.Zero,
			CacheCreationCostPerMillion: decimal.Zero, Currency: model.CurrencyUSD, Source: "remote",
		}}}
	remote.ContentHash = pricing.HashModels(remote.Models)
	var failPricing atomic.Bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/events":
			_ = json.NewEncoder(w).Encode(EventsResponse{NextCursor: "0"})
		case "/api/v1/pricing":
			if failPricing.Load() {
				http.Error(w, "unavailable", http.StatusServiceUnavailable)
				return
			}
			_ = json.NewEncoder(w).Encode(remote)
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()
	runner := NewRunner(db, Config{Mode: ModeServer, Server: Server{BaseURL: upstream.URL}}, "device-local")
	if report, err := runner.RunOnce(context.Background()); err != nil || len(report.Errors) != 0 {
		t.Fatalf("first sync failed: report=%+v err=%v", report, err)
	}
	status, err := db.PricingSyncStatus()
	if err != nil {
		t.Fatal(err)
	}
	if status.RemoteHash != remote.ContentHash || status.LastCheckedAt == nil || status.LastSyncedAt == nil || status.LastError != "" {
		t.Fatalf("unexpected pricing metadata after success: %+v", status)
	}
	lastSynced := *status.LastSyncedAt
	failPricing.Store(true)
	report, err := runner.RunOnce(context.Background())
	status, statusErr := db.PricingSyncStatus()
	if err != nil || len(report.Errors) == 0 {
		t.Fatalf("pricing failure was not reported: report=%+v err=%v status=%+v statusErr=%v", report, err, status, statusErr)
	}
	status, err = db.PricingSyncStatus()
	if err != nil {
		t.Fatal(err)
	}
	if status.RemoteHash != remote.ContentHash || status.LastError == "" || status.LastSyncedAt == nil || !status.LastSyncedAt.Equal(lastSynced) {
		t.Fatalf("pricing failure metadata was not preserved correctly: %+v", status)
	}
}

func TestServerSyncUploadsAndDoesNotLoopPulledEvents(t *testing.T) {
	db, err := storage.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	remoteEvent := model.NewUsageEvent("device-remote", model.SourceSession, "codex", "remote:1", "model",
		model.TokenUsage{InputTokens: 100}, time.Now().UTC())
	var pulled bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/events/batch":
			_ = json.NewDecoder(r.Body).Decode(&EventBatch{})
			_ = json.NewEncoder(w).Encode(BatchAck{Accepted: 1})
		case "/api/v1/events":
			if pulled {
				_ = json.NewEncoder(w).Encode(EventsResponse{Events: []model.UsageEvent{remoteEvent}, NextCursor: "2"})
			} else {
				_ = json.NewEncoder(w).Encode(EventsResponse{NextCursor: "1"})
			}
		case "/api/v1/pricing":
			_ = json.NewEncoder(w).Encode(storage.PricingSnapshot{})
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()

	localEvent := model.NewUsageEvent("device-local", model.SourceSession, "claude", "local:1", "model",
		model.TokenUsage{InputTokens: 100}, time.Now().UTC())
	if _, err := db.InsertEvents([]model.UsageEvent{localEvent}, true); err != nil {
		t.Fatal(err)
	}
	runner := NewRunner(db, Config{Mode: ModeServer, Server: Server{BaseURL: upstream.URL}}, "device-local")
	report, err := runner.RunOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if report.Uploaded != 1 {
		t.Fatalf("expected upload 1, got %+v", report)
	}
	pending, _ := db.PendingOutbox(10)
	if len(pending) != 0 {
		t.Fatalf("outbox not drained: %d", len(pending))
	}

	pulled = true
	report, err = runner.RunOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if report.Downloaded != 1 {
		t.Fatalf("expected download 1, got %+v", report)
	}
	pending, _ = db.PendingOutbox(10)
	if len(pending) != 0 {
		t.Fatalf("pulled event must not loop upstream, outbox=%d", len(pending))
	}
}
