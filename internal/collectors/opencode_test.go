package collectors

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	"tokenanalyse/internal/model"
)

func openCodeTestContext(t *testing.T) Context {
	t.Helper()
	return Context{DeviceID: "device-opencode", Paths: Paths{OpenCode: t.TempDir()}}
}

func createOpenCodeTestDB(t *testing.T, path, schema string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		t.Fatal(err)
	}
	return db
}

func TestOpenCodeCollectorV1ParsesUsageAndCachesCursor(t *testing.T) {
	ctx := openCodeTestContext(t)
	path := filepath.Join(ctx.Paths.OpenCode, "opencode.db")
	db := createOpenCodeTestDB(t, path, `
		CREATE TABLE session (id TEXT PRIMARY KEY, time_updated INTEGER);
		CREATE TABLE message (
			id TEXT PRIMARY KEY, session_id TEXT, time_created INTEGER,
			time_updated INTEGER, data TEXT
		);
		INSERT INTO session VALUES ('s1', 2000);
	`)
	data := `{"role":"assistant","modelID":"claude-test","providerID":"anthropic","cost":0.01,"tokens":{"input":100,"output":20,"reasoning":5,"cache":{"read":30,"write":4}},"time":{"created":1000,"completed":1500}}`
	if _, err := db.Exec(`INSERT INTO message VALUES ('m1', 's1', 1000, 2000, ?)`, data); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	collector := OpenCodeCollector{}
	first, err := collector.Collect(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Events) != 1 {
		t.Fatalf("expected one event, got %d (errors: %v)", len(first.Events), first.Errors)
	}
	event := first.Events[0]
	if event.SourceApp != "opencode" || event.SessionID != "s1" || event.SourceRecordID != "s1:m1" {
		t.Fatalf("unexpected identity: %+v", event)
	}
	if event.Model != "claude-test" || event.ProviderID != "anthropic" {
		t.Fatalf("unexpected model/provider: %+v", event)
	}
	if event.Usage.InputTokens != 100 || event.Usage.OutputTokens != 25 || event.Usage.ReasoningTokens != 5 {
		t.Fatalf("unexpected token usage: %+v", event.Usage)
	}
	if event.Usage.CacheReadTokens != 30 || event.Usage.CacheCreationTokens != 4 {
		t.Fatalf("unexpected cache usage: %+v", event.Usage)
	}
	if event.InputTokenSemantics != model.SemanticsFresh {
		t.Fatalf("expected fresh input semantics, got %v", event.InputTokenSemantics)
	}
	if event.LatencyMS == nil || *event.LatencyMS != 500 || event.TimingQuality != model.TimingExact {
		t.Fatalf("unexpected timing: latency=%v quality=%v", event.LatencyMS, event.TimingQuality)
	}
	if event.EventID == "" || event.RequestID == "" || event.StatusCode == nil || *event.StatusCode != 200 {
		t.Fatalf("event fields were not completed: %+v", event)
	}

	second, err := collector.Collect(ctx, first.Cursors)
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Events) != 0 {
		t.Fatalf("unchanged database emitted %d duplicate events", len(second.Events))
	}
}

func TestOpenCodeCollectorV2UsesModelObjectAndStreamedLatency(t *testing.T) {
	ctx := openCodeTestContext(t)
	path := filepath.Join(ctx.Paths.OpenCode, "opencode.db")
	db := createOpenCodeTestDB(t, path, `
		CREATE TABLE session_v2 (id TEXT PRIMARY KEY, time_updated INTEGER);
		CREATE TABLE session_message (
			id TEXT PRIMARY KEY, session_id TEXT, type TEXT,
			time_created INTEGER, time_updated INTEGER, data TEXT
		);
		INSERT INTO session_v2 VALUES ('s2', 3000);
	`)
	data := `{"tokens":{"input":50,"output":10,"reasoning":2,"cache":{"read":7}},"model":{"id":"gpt-test","providerID":"openai"},"time":{"created":1000,"streamed":1800,"completed":5000}}`
	if _, err := db.Exec(`INSERT INTO session_message VALUES ('m2', 's2', 'assistant', 1000, 3000, ?)`, data); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO session_message VALUES ('user', 's2', 'user', 1000, 3000, ?)`, `{"tokens":{"input":999}}`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	result, err := OpenCodeCollector{}.Collect(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Events) != 1 {
		t.Fatalf("expected one assistant event, got %d", len(result.Events))
	}
	event := result.Events[0]
	if event.Model != "gpt-test" || event.ProviderID != "openai" || event.Usage.OutputTokens != 12 {
		t.Fatalf("unexpected V2 event: %+v", event)
	}
	if event.LatencyMS == nil || *event.LatencyMS != 800 {
		t.Fatalf("expected streamed latency 800, got %v", event.LatencyMS)
	}
}

func TestOpenCodeCollectorMixedLayoutDeduplicatesAndToleratesCorruption(t *testing.T) {
	ctx := openCodeTestContext(t)
	path := filepath.Join(ctx.Paths.OpenCode, "opencode.db")
	db := createOpenCodeTestDB(t, path, `
		CREATE TABLE session (id TEXT PRIMARY KEY, time_updated INTEGER);
		CREATE TABLE message (
			id TEXT PRIMARY KEY, session_id TEXT, time_created INTEGER,
			time_updated INTEGER, data TEXT
		);
		CREATE TABLE session_v2 (id TEXT PRIMARY KEY, time_updated INTEGER);
		CREATE TABLE session_message (
			id TEXT PRIMARY KEY, session_id TEXT, type TEXT,
			time_created INTEGER, time_updated INTEGER, data TEXT
		);
		INSERT INTO session VALUES ('shared', 100);
		INSERT INTO session_v2 VALUES ('shared', 500);
	`)
	old := `{"role":"assistant","modelID":"old-model","tokens":{"input":1,"output":1},"time":{"created":1,"completed":2}}`
	newer := `{"model":{"id":"new-model"},"tokens":{"input":9,"output":3},"time":{"created":10,"completed":20}}`
	if _, err := db.Exec(`INSERT INTO message VALUES ('same', 'shared', 1, 100, ?)`, old); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO session_message VALUES ('same', 'shared', 'assistant', 10, 500, ?)`, newer); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO session_message VALUES ('broken', 'shared', 'assistant', 10, 500, ?)`, `{not-json`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	collector := OpenCodeCollector{}
	first, err := collector.Collect(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Events) != 1 || first.Events[0].Model != "new-model" || first.Events[0].Usage.InputTokens != 9 {
		t.Fatalf("mixed layout did not keep newest snapshot: %+v", first.Events)
	}

	db = createOpenCodeTestDB(t, path, ``)
	if _, err := db.Exec(`UPDATE session_v2 SET time_updated = 600 WHERE id = 'shared'`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO session_message VALUES ('later', 'shared', 'assistant', 30, 600, ?)`, `{"model":"later-model","tokens":{"input":4,"output":2},"time":{"created":30,"completed":40}}`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(path, future, future); err != nil {
		t.Fatal(err)
	}

	second, err := collector.Collect(ctx, first.Cursors)
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Events) != 1 || second.Events[0].SourceRecordID != "shared:later" {
		t.Fatalf("incremental scan expected only later event, got %+v", second.Events)
	}
}
