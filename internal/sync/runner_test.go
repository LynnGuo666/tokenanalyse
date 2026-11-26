package sync

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"tokenanalyse/internal/model"
	"tokenanalyse/internal/storage"
)

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
