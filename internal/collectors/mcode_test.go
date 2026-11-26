package collectors

import (
	"bytes"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"tokenanalyse/internal/model"
)

func TestMCodeCollectorReadOnlyIncrementalProjection(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "v2", "sqlite", "runtime-state.sqlite")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	writeDB, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = writeDB.Exec(`
CREATE TABLE local_runtime_token_usage (
 id INTEGER PRIMARY KEY, session_id TEXT, turn_id TEXT, model TEXT, ts INTEGER,
 input_tokens INTEGER, output_tokens INTEGER, reasoning_tokens INTEGER,
 cache_read_tokens INTEGER, cache_write_tokens INTEGER, cost_usd REAL
);
CREATE TABLE local_runtime_pi_history_rows (
 id INTEGER PRIMARY KEY, session_id TEXT, role TEXT, created_at_ms INTEGER, data_json TEXT
);
CREATE TABLE local_runtime_messages (
 session_id TEXT PRIMARY KEY, display_messages_json TEXT, pi_history_json TEXT
);
CREATE TABLE local_runtime_message_rows (
 id INTEGER PRIMARY KEY, session_id TEXT, role TEXT, turn_id TEXT, created_at_ms INTEGER, data_json TEXT
);
INSERT INTO local_runtime_token_usage VALUES
 (1,'s1','t1','custom_provider:router/vendor/model',100000,10,20,3,40,5,0.25),
 (2,'s1','t2',NULL,101000,1,2,0,0,0,NULL),
 (3,'s2',NULL,NULL,200000,4,5,0,6,0,NULL);
INSERT INTO local_runtime_pi_history_rows VALUES
 (1,'s1','assistant',101000,'{"role":"assistant","model":"MiniMax-M3","timestamp":101000}');
INSERT INTO local_runtime_messages VALUES
 ('s2','[]','[{"role":"assistant","model":"vendor/legacy-model","timestamp":200000}]');
INSERT INTO local_runtime_message_rows VALUES
 (1,'s1','assistant','t1',116610,'{"usage":{"input_tokens":10,"output_tokens":20,"request_duration_ms":16610,"decode_duration_ms":14595}}');
`)
	if err != nil {
		writeDB.Close()
		t.Fatal(err)
	}
	if err := writeDB.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	collector := NewMCodeCollector()
	ctx := Context{DeviceID: "device-1", Paths: Paths{MCode: root}}
	first, err := collector.Collect(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Errors) != 0 {
		t.Fatalf("collector errors: %v", first.Errors)
	}
	if len(first.Events) != 3 {
		t.Fatalf("expected 3 events, got %d", len(first.Events))
	}
	event := first.Events[0]
	if event.Model != "vendor/model" || event.ModelRaw != "custom_provider:router/vendor/model" {
		t.Fatalf("model namespace not preserved: %+v", event)
	}
	if event.Usage.OutputTokens != 23 || event.Usage.ReasoningTokens != 3 || event.Usage.CacheCreationTokens != 5 {
		t.Fatalf("unexpected normalized usage: %+v", event.Usage)
	}
	if event.InputTokenSemantics != model.SemanticsFresh || event.Cost.TotalCostUSD != "0.25" {
		t.Fatalf("semantics/cost not preserved: %+v", event)
	}
	if event.LatencyMS == nil || *event.LatencyMS != 16610 || event.FirstTokenMS == nil || *event.FirstTokenMS != 2015 {
		t.Fatalf("message timing not recovered: %+v", event)
	}
	if first.Events[1].Model != "MiniMax-M3" || first.Events[2].Model != "vendor/legacy-model" {
		t.Fatalf("history model fallback failed: %q, %q", first.Events[1].Model, first.Events[2].Model)
	}
	if len(first.Cursors) != 1 || first.Cursors[0].Offset != 3 {
		t.Fatalf("unexpected watermark: %+v", first.Cursors)
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("collector modified the source SQLite database")
	}

	writeDB, err = sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writeDB.Exec(`INSERT INTO local_runtime_token_usage VALUES (4,'s3',NULL,'provider/zero-cost',300000,7,8,0,9,0,0)`); err != nil {
		writeDB.Close()
		t.Fatal(err)
	}
	if err := writeDB.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := collector.Collect(ctx, first.Cursors)
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Events) != 1 || second.Events[0].RequestID != "mcode:s3:4" {
		t.Fatalf("incremental collection returned %+v", second.Events)
	}
	if second.Events[0].Cost.TotalCostUSD != "0" {
		t.Fatalf("native zero cost was not preserved: %+v", second.Events[0].Cost)
	}
	third, err := collector.Collect(ctx, second.Cursors)
	if err != nil {
		t.Fatal(err)
	}
	if len(third.Events) != 0 {
		t.Fatalf("unchanged projection was reimported: %+v", third.Events)
	}
}
