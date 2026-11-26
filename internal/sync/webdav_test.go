package sync

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"tokenanalyse/internal/model"
	"tokenanalyse/internal/pricing"
	"tokenanalyse/internal/storage"

	"github.com/shopspring/decimal"
)

type fakeWebDAV struct {
	mu      sync.Mutex
	dirs    map[string]bool
	objects map[string][]byte
}

func newFakeWebDAV() *fakeWebDAV {
	return &fakeWebDAV{dirs: map[string]bool{"/": true}, objects: map[string][]byte{}}
}

func (f *fakeWebDAV) addObject(t *testing.T, objectPath string, data []byte) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	f.objects[objectPath] = append([]byte(nil), data...)
	dir := objectPath
	for {
		dir = strings.TrimSuffix(dir, "/")
		idx := strings.LastIndex(dir, "/")
		if idx <= 0 {
			break
		}
		dir = dir[:idx+1]
		f.dirs[dir] = true
	}
}

func (f *fakeWebDAV) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := r.URL.Path
	switch r.Method {
	case http.MethodPut:
		data, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		f.objects[key] = data
		w.WriteHeader(http.StatusCreated)
	case http.MethodGet:
		data, ok := f.objects[key]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(data)
	case "MKCOL":
		if f.dirs[key] {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		f.dirs[key] = true
		w.WriteHeader(http.StatusCreated)
	case "PROPFIND":
		if !f.dirs[key] {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Depth") != "1" {
			http.Error(w, "missing Depth header", http.StatusBadRequest)
			return
		}
		children := map[string]bool{}
		prefix := key
		if !strings.HasSuffix(prefix, "/") {
			prefix += "/"
		}
		for candidate := range f.dirs {
			if strings.HasPrefix(candidate, prefix) {
				rest := strings.TrimSuffix(strings.TrimPrefix(candidate, prefix), "/")
				if rest != "" && !strings.Contains(rest, "/") {
					children[prefix+rest+"/"] = true
				}
			}
		}
		for candidate := range f.objects {
			if strings.HasPrefix(candidate, prefix) {
				rest := strings.TrimPrefix(candidate, prefix)
				if rest != "" && !strings.Contains(rest, "/") {
					children[prefix+rest] = true
				}
			}
		}
		responses := []string{fmt.Sprintf(`<d:response><d:href>%s</d:href></d:response>`, xmlEscape(escapedURLPath(key)))}
		for child := range children {
			responses = append(responses, fmt.Sprintf(`<d:response><d:href>%s</d:href></d:response>`, xmlEscape(escapedURLPath(child))))
		}
		payload := `<?xml version="1.0" encoding="utf-8"?><d:multistatus xmlns:d="DAV:">` + strings.Join(responses, "") + `</d:multistatus>`
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusMultiStatus)
		_, _ = w.Write([]byte(payload))
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func xmlEscape(value string) string {
	var escaped strings.Builder
	_ = xml.EscapeText(&escaped, []byte(value))
	return escaped.String()
}

func escapedURLPath(value string) string {
	return (&url.URL{Path: value}).EscapedPath()
}

func TestWebDAVSyncUploadDownloadIdempotencyAndPricingMerge(t *testing.T) {
	db, err := storage.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	localPrices := []model.ModelPricing{
		testPricing("local-only", 2),
		testPricing("shared", 1),
	}
	if err := db.CreatePricingVersion("local-v1", "test", localPrices); err != nil {
		t.Fatal(err)
	}
	if err := db.SetActivePricingVersion("local-v1"); err != nil {
		t.Fatal(err)
	}
	localEvent := model.NewUsageEvent("device-local", model.SourceSession, "claude", "local:1", "shared",
		model.TokenUsage{InputTokens: 1_000_000}, time.Now().UTC())
	if _, err := db.InsertEvents([]model.UsageEvent{localEvent}, true); err != nil {
		t.Fatal(err)
	}

	remoteEvent := model.NewUsageEvent("device remote", model.SourceSession, "codex", "remote:1", "vendor/remote-model",
		model.TokenUsage{InputTokens: 1_000_000}, time.Now().UTC())
	remotePayload, err := json.Marshal(remoteEvent)
	if err != nil {
		t.Fatal(err)
	}
	remoteSnapshot := storage.PricingSnapshot{
		VersionID: "remote-v1", Source: "remote", CreatedAt: time.Now().UTC(),
		Models:  []model.ModelPricing{testPricing("remote-model", 3), testPricing("shared", 5)},
		Aliases: []model.ModelAlias{{AliasPattern: "vendor/remote-model", CanonicalModelID: "remote-model", MatchType: "exact", Priority: 100}},
	}
	remoteSnapshot.ContentHash = pricing.HashModels(remoteSnapshot.Models)
	remotePricingPayload, err := json.Marshal(remoteSnapshot)
	if err != nil {
		t.Fatal(err)
	}

	dav := newFakeWebDAV()
	devicesPath := "/tokenanalyse/usage-v1/default/devices/"
	dav.addObject(t, devicesPath+"device remote/batches/00000000000000000001-remote.json", remotePayload)
	dav.addObject(t, devicesPath+"device remote/pricing.json", remotePricingPayload)

	server := httptest.NewServer(dav)
	defer server.Close()
	runner := NewRunner(db, Config{Mode: ModeWebDAV, WebDAV: WebDAV{
		BaseURL: server.URL, RemoteRoot: "tokenanalyse", Profile: "default",
	}}, "device-local")

	report, err := runner.RunOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Errors) != 0 {
		t.Fatalf("unexpected sync errors: %+v", report.Errors)
	}
	if report.Uploaded != 1 || report.Downloaded != 1 || report.Duplicates != 0 || report.Conflicts != 1 {
		t.Fatalf("unexpected first report: %+v", report)
	}
	if pending, err := db.PendingOutbox(10); err != nil || len(pending) != 0 {
		t.Fatalf("outbox not drained: pending=%d err=%v", len(pending), err)
	}

	dav.mu.Lock()
	uploaded := false
	for objectPath, data := range dav.objects {
		if strings.HasPrefix(objectPath, devicesPath+"device-local/batches/") {
			uploaded = true
			var event model.UsageEvent
			if err := json.Unmarshal(data, &event); err != nil {
				dav.mu.Unlock()
				t.Fatal(err)
			}
			if event.Cost.TotalCostUSD != "" || event.PricingVersion != "" {
				dav.mu.Unlock()
				t.Fatalf("outbox payload must leave pricing to the receiver: %+v", event)
			}
		}
	}
	dav.mu.Unlock()
	if !uploaded {
		t.Fatal("local event was not uploaded")
	}

	summary, err := db.GetSummary(model.UsageFilters{})
	if err != nil {
		t.Fatal(err)
	}
	if summary.TotalRequests != 2 || summary.TotalCost != "8" {
		t.Fatalf("expected two events repriced with the merged table, got %+v", summary)
	}
	snapshot, err := db.ActivePricingSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	prices := map[string]model.ModelPricing{}
	for _, entry := range snapshot.Models {
		prices[entry.ModelID] = entry
	}
	for modelID, want := range map[string]int64{"local-only": 2, "remote-model": 3, "shared": 5} {
		entry, ok := prices[modelID]
		if !ok || !entry.InputCostPerMillion.Equal(decimal.NewFromInt(want)) {
			t.Fatalf("unexpected merged price for %s: %+v", modelID, entry)
		}
	}
	resolved, err := db.ResolveModel("codex", "vendor/remote-model")
	if err != nil || resolved != "remote-model" {
		t.Fatalf("remote alias was not merged: resolved=%q err=%v", resolved, err)
	}
	status, err := db.PricingSyncStatus()
	if err != nil {
		t.Fatal(err)
	}
	if status.LastCheckedAt == nil || status.LastSyncedAt == nil || status.RemoteHash != remoteSnapshot.ContentHash || status.LastError != "" {
		t.Fatalf("unexpected pricing sync metadata: %+v", status)
	}

	report, err = runner.RunOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Errors) != 0 {
		t.Fatalf("unexpected second sync errors: %+v", report.Errors)
	}
	if report.Uploaded != 0 || report.Downloaded != 0 || report.Duplicates != 1 {
		t.Fatalf("duplicate WebDAV event was not idempotent: %+v", report)
	}
	if pending, err := db.PendingOutbox(10); err != nil || len(pending) != 0 {
		t.Fatalf("duplicate download must not enqueue: pending=%d err=%v", len(pending), err)
	}
}

func testPricing(modelID string, input int64) model.ModelPricing {
	return model.ModelPricing{
		ModelID: modelID, DisplayName: modelID,
		InputCostPerMillion: decimal.NewFromInt(input), OutputCostPerMillion: decimal.Zero,
		CacheReadCostPerMillion: decimal.Zero, CacheCreationCostPerMillion: decimal.Zero,
		Currency: model.CurrencyUSD, Source: "test",
	}
}
