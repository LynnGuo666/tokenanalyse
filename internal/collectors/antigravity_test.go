package collectors

import (
	"bytes"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"tokenanalyse/internal/model"
)

func TestAntigravityCollectorNativeModelsAndIncrementalSteps(t *testing.T) {
	root := t.TempDir()
	brain := filepath.Join(root, "brain")
	session := filepath.Join(brain, "session-one")
	logs := filepath.Join(session, ".system_generated", "logs")
	if err := os.MkdirAll(logs, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "conversations"), 0o755); err != nil {
		t.Fatal(err)
	}
	transcript := filepath.Join(logs, "transcript_full.jsonl")
	lines := "" +
		`{"step_index":0,"type":"USER_INPUT","created_at":"2026-11-21T10:00:00Z","content":"changed setting Model Selection from None to Orchestrator (Carbon). No need to comment"}` + "\n" +
		`{"step_index":1,"type":"PLANNER_RESPONSE","status":"DONE","created_at":"2026-11-21T10:00:05Z","input_tokens":100,"output_tokens":10,"cache_read_tokens":20}` + "\n" +
		"not-json\n" +
		`{"step_index":3,"type":"PLANNER_RESPONSE","status":"DONE","created_at":"2026-11-21T10:00:09Z","model":"gemini-flash-worker-agy","input_tokens":50,"output_tokens":5,"cache_write_tokens":2}` + "\n"
	if err := os.WriteFile(transcript, []byte(lines), 0o644); err != nil {
		t.Fatal(err)
	}

	nativePath := filepath.Join(root, "conversations", "session-one.db")
	native, err := sql.Open("sqlite", nativePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := native.Exec(`CREATE TABLE steps (idx INTEGER PRIMARY KEY, step_type INTEGER, metadata BLOB); CREATE TABLE gen_metadata (idx INTEGER PRIMARY KEY, data BLOB)`); err != nil {
		native.Close()
		t.Fatal(err)
	}
	insertGeneration := func(step int, generation int, base, tier string, firstToken, streaming uint64) {
		t.Helper()
		metadata := agyTestBytesField(20, agyTestVarintField(3, uint64(generation)))
		response := agyTestBytesField(19, []byte(base))
		response = append(response, agyTestBytesField(21, []byte(tier))...)
		response = append(response, agyTestBytesField(11, agyTestDuration(firstToken))...)
		response = append(response, agyTestBytesField(12, agyTestDuration(streaming))...)
		data := agyTestBytesField(1, response)
		if _, err := native.Exec(`INSERT INTO steps VALUES (?,15,?)`, step, metadata); err != nil {
			t.Fatal(err)
		}
		if _, err := native.Exec(`INSERT INTO gen_metadata VALUES (?,?)`, generation, data); err != nil {
			t.Fatal(err)
		}
	}
	insertGeneration(1, 0, "gemini-pro-orchestrator", "Orchestrator (Carbon)", 1200, 800)
	insertGeneration(3, 7, "gemini-flash-worker-agy", "gemini-4-hydrogen-high", 300, 700)
	if err := native.Close(); err != nil {
		t.Fatal(err)
	}
	nativeBefore, err := os.ReadFile(nativePath)
	if err != nil {
		t.Fatal(err)
	}

	collector := NewAntigravityCollector()
	ctx := Context{DeviceID: "device-1", Paths: Paths{Antigravity: root}}
	first, err := collector.Collect(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Errors) != 0 {
		t.Fatalf("collector errors: %v", first.Errors)
	}
	if len(first.Events) != 2 {
		t.Fatalf("expected 2 events, got %d: %+v", len(first.Events), first.Events)
	}
	planner := first.Events[0]
	if planner.Model != "gemini-4-carbon" || planner.RequestModel != "Orchestrator (Carbon)" {
		t.Fatalf("planner provenance not preserved: %+v", planner)
	}
	if planner.Metadata["role"] != "orchestrator" || planner.Metadata["orchestrator"] != "carbon" {
		t.Fatalf("planner role not preserved: %+v", planner.Metadata)
	}
	if planner.InputTokenSemantics != model.SemanticsFresh || planner.Usage.InputTokens != 100 || planner.Usage.CacheReadTokens != 20 {
		t.Fatalf("planner usage/semantics incorrect: %+v", planner)
	}
	if planner.LatencyMS == nil || *planner.LatencyMS != 2000 || planner.FirstTokenMS == nil || *planner.FirstTokenMS != 1200 {
		t.Fatalf("native timing not recovered: %+v", planner)
	}
	worker := first.Events[1]
	if worker.Model != "gemini-4-hydrogen-high" || worker.RequestModel != "gemini-flash-worker-agy" || worker.Metadata["role"] != "orchestrator-worker" {
		t.Fatalf("worker provenance not preserved: %+v", worker)
	}
	if len(first.Cursors) != 1 || first.Cursors[0].Offset != int64(len(lines)) {
		t.Fatalf("unexpected cursor: %+v", first.Cursors)
	}

	nativeAfter, err := os.ReadFile(nativePath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(nativeBefore, nativeAfter) {
		t.Fatal("collector modified the native Antigravity database")
	}

	appended := `{"step_index":5,"type":"PLANNER_RESPONSE","status":"DONE","created_at":"2026-11-21T10:00:15Z","model":"Gemini 3.8 Flash (High)","input_tokens":70,"output_tokens":9}` + "\n"
	file, err := os.OpenFile(transcript, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString(appended); err != nil {
		file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := collector.Collect(ctx, first.Cursors)
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Events) != 1 || second.Events[0].Model != "gemini-3.8-flash-high" {
		t.Fatalf("appended step not imported incrementally: %+v", second.Events)
	}
	third, err := collector.Collect(ctx, second.Cursors)
	if err != nil {
		t.Fatal(err)
	}
	if len(third.Events) != 0 {
		t.Fatalf("unchanged transcript was reimported: %+v", third.Events)
	}

	replayed, err := collector.Collect(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(replayed.Events) != 3 {
		t.Fatalf("replay returned %d events, want 3", len(replayed.Events))
	}
	ids := map[string]bool{}
	for _, event := range replayed.Events {
		ids[event.EventID] = true
	}
	for _, event := range append(first.Events, second.Events...) {
		if !ids[event.EventID] {
			t.Fatalf("event ID changed across replay: %s", event.EventID)
		}
	}
}

func agyTestVarint(value uint64) []byte {
	var output []byte
	for value >= 0x80 {
		output = append(output, byte(value)|0x80)
		value >>= 7
	}
	return append(output, byte(value))
}

func agyTestVarintField(number int, value uint64) []byte {
	output := agyTestVarint(uint64(number) << 3)
	return append(output, agyTestVarint(value)...)
}

func agyTestBytesField(number int, value []byte) []byte {
	output := agyTestVarint(uint64(number)<<3 | 2)
	output = append(output, agyTestVarint(uint64(len(value)))...)
	return append(output, value...)
}

func agyTestDuration(milliseconds uint64) []byte {
	output := agyTestVarintField(1, milliseconds/1000)
	return append(output, agyTestVarintField(2, (milliseconds%1000)*1_000_000)...)
}
