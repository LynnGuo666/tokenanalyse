package collectors

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"tokenanalyse/internal/model"
)

func TestGeminiCLILegacyJSONUsageAndIdempotentCursor(t *testing.T) {
	home := t.TempDir()
	chats := filepath.Join(home, "tmp", "project-hash", "chats")
	if err := os.MkdirAll(chats, 0o755); err != nil {
		t.Fatal(err)
	}
	content := `{
		"sessionId":"session-legacy",
		"messages":[
			{"id":"user-1","type":"user","tokens":{"input":999}},
			{"id":"m1","type":"gemini","model":"gemini-2.5-pro","timestamp":"2026-01-02T03:04:05Z","tokens":{"input":100,"output":20,"cached":30,"thoughts":7,"tool":0}},
			{"id":"empty","type":"gemini","model":"gemini-2.5-pro","tokens":{"input":0,"output":0,"cached":0,"thoughts":0}}
		]
	}`
	path := filepath.Join(chats, "session-legacy.json")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	collector := NewGeminiCliCollector()
	result, err := collector.Collect(Context{DeviceID: "device-1", Paths: Paths{Gemini: home}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.FilesScanned != 1 || result.BytesRead == 0 || len(result.Errors) != 0 {
		t.Fatalf("unexpected result: %+v", result)
	}
	if len(result.Events) != 1 {
		t.Fatalf("got %d events, want 1", len(result.Events))
	}
	event := result.Events[0]
	if event.EventID == "" || event.EventID != model.DeterministicEventID("device-1", "session", "gemini", "session-legacy:m1") {
		t.Fatalf("unstable event id: %q", event.EventID)
	}
	if event.Source != model.SourceSession || event.SourceApp != "gemini" || event.SourceRecordID != "session-legacy:m1" {
		t.Fatalf("incomplete source fields: %+v", event)
	}
	if event.RequestID != "gemini_session:session-legacy:m1" || event.SessionID != "session-legacy" {
		t.Fatalf("incomplete request/session fields: %+v", event)
	}
	if event.ProviderID != "_gemini_session" || event.ProviderName != "Gemini CLI" || event.RequestModel != "gemini-2.5-pro" || event.PricingModel != "gemini-2.5-pro" {
		t.Fatalf("incomplete provider/model fields: %+v", event)
	}
	if event.Usage.InputTokens != 100 || event.Usage.OutputTokens != 27 || event.Usage.CacheReadTokens != 30 || event.Usage.CacheCreationTokens != 0 {
		t.Fatalf("incorrect token mapping: %+v", event.Usage)
	}
	if event.InputTokenSemantics != model.SemanticsTotal {
		t.Fatalf("input semantics = %v, want total", event.InputTokenSemantics)
	}
	wantTime := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	if !event.OccurredAt.Equal(wantTime) || event.IngestedAt.IsZero() {
		t.Fatalf("incorrect timestamps: occurred=%s ingested=%s", event.OccurredAt, event.IngestedAt)
	}
	if event.TimingQuality != model.TimingUnknown || !event.IsStreaming || event.StatusCode == nil || *event.StatusCode != 200 || event.CostMultiplier != "1" {
		t.Fatalf("incomplete event metadata: %+v", event)
	}
	if len(result.Cursors) != 1 || result.Cursors[0].Offset <= 0 || result.Cursors[0].ModifiedNanos <= 0 || result.Cursors[0].State == "" {
		t.Fatalf("invalid cursor: %+v", result.Cursors)
	}

	second, err := collector.Collect(Context{DeviceID: "device-1", Paths: Paths{Gemini: home}}, result.Cursors)
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Events) != 0 || second.BytesRead != 0 || len(second.Cursors) != 1 {
		t.Fatalf("unchanged file was not skipped incrementally: %+v", second)
	}
}

func TestGeminiCLIJSONLReplayAndIncrementalAppend(t *testing.T) {
	home := t.TempDir()
	chats := filepath.Join(home, "tmp", "project-hash", "chats")
	if err := os.MkdirAll(chats, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(chats, "session-replay.jsonl")
	// The legacy file is stale after migration and must not be read. Invalid
	// JSONL records are ignored without discarding valid replay records.
	if err := os.WriteFile(filepath.Join(chats, "session-replay.json"), []byte(`{"sessionId":"stale","messages":[{"id":"stale","type":"gemini","tokens":{"input":999}}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	initial := `{"sessionId":"session-replay"}
{"id":"m1","type":"gemini","model":"gemini-2.5-pro","timestamp":"2026-01-02T03:04:05Z","tokens":{"input":100,"output":10,"cached":20,"thoughts":1}}
not-json
{"id":"m2","type":"gemini","model":"gemini-2.5-pro","timestamp":"2026-01-02T03:04:06Z","tokens":{"input":200,"output":20,"cached":0,"thoughts":2}}
{"$set":{"messages":[{"id":"m1","type":"gemini","model":"gemini-2.5-pro","timestamp":"2026-01-02T03:04:05Z","tokens":{"input":100,"output":10,"cached":20,"thoughts":1}},{"id":"m2","type":"gemini","model":"gemini-2.5-pro","timestamp":"2026-01-02T03:04:06Z","tokens":{"input":200,"output":20,"cached":0,"thoughts":2}}]}}
{"$rewindTo":"m2"}
`
	if err := os.WriteFile(path, []byte(initial), 0o644); err != nil {
		t.Fatal(err)
	}

	collector := NewGeminiCliCollector()
	ctx := Context{DeviceID: "device-1", Paths: Paths{Gemini: home}}
	first, err := collector.Collect(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if first.FilesScanned != 1 || len(first.Errors) != 0 || len(first.Events) != 1 || first.Events[0].SourceRecordID != "session-replay:m1" {
		t.Fatalf("replay/rewind or stale JSON handling failed: %+v", first)
	}

	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, err = file.WriteString(`{"id":"m2","type":"gemini","model":"gemini-2.5-pro","timestamp":"2026-01-02T03:05:06Z","tokens":{"input":250,"output":25,"cached":50,"thoughts":5}}
{"id":"m3","type":"gemini","model":"gemini-2.5-flash","timestamp":"2026-01-02T03:06:07Z","tokens":{"input":40,"output":4,"cached":0,"thoughts":0}}
`)
	if closeErr := file.Close(); err != nil || closeErr != nil {
		t.Fatalf("append: write=%v close=%v", err, closeErr)
	}

	second, err := collector.Collect(ctx, first.Cursors)
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Errors) != 0 || len(second.Events) != 2 {
		t.Fatalf("append produced %+v", second)
	}
	byID := map[string]model.UsageEvent{}
	for _, event := range second.Events {
		byID[event.SourceRecordID] = event
	}
	if event := byID["session-replay:m2"]; event.Usage.InputTokens != 250 || event.Usage.OutputTokens != 30 || event.Usage.CacheReadTokens != 50 {
		t.Fatalf("upsert message not mapped correctly: %+v", event)
	}
	if event := byID["session-replay:m3"]; event.Model != "gemini-2.5-flash" || event.Usage.OutputTokens != 4 {
		t.Fatalf("new message missing: %+v", event)
	}

	// Forcing a full replay must retain the same IDs, even though the cursor
	// state suppresses messages whose fingerprints have not changed.
	third, err := collector.Collect(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(third.Events) != 3 {
		t.Fatalf("full replay got %d events, want 3", len(third.Events))
	}
	ids := map[string]bool{}
	for _, event := range third.Events {
		ids[event.EventID] = true
	}
	for _, event := range append(first.Events, second.Events...) {
		if !ids[event.EventID] {
			t.Fatalf("event id changed across replay: %s", event.EventID)
		}
	}
}

func TestGeminiCLICorruptFileDoesNotBlockOtherSessions(t *testing.T) {
	home := t.TempDir()
	chats := filepath.Join(home, "tmp", "project-hash", "chats")
	if err := os.MkdirAll(chats, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(chats, "session-broken.json"), []byte(`{"sessionId":`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(chats, "session-valid.jsonl"), []byte(`{"sessionId":"valid"}
{"id":"m1","type":"gemini","model":"gemini-2.5-pro","timestamp":"2026-01-02T03:04:05Z","tokens":{"input":10,"output":2,"cached":3,"thoughts":4}}
`), 0o644); err != nil {
		t.Fatal(err)
	}

	result, err := NewGeminiCliCollector().Collect(Context{DeviceID: "device-1", Paths: Paths{Gemini: home}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.FilesScanned != 2 || len(result.Errors) != 1 || len(result.Events) != 1 || len(result.Cursors) != 1 {
		t.Fatalf("corruption was not isolated: %+v", result)
	}
	if result.Cursors[0].FilePath != filepath.Join(chats, "session-valid.jsonl") {
		t.Fatalf("cursor advanced for corrupt file: %+v", result.Cursors)
	}
}
