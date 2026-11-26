package collectors

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"tokenanalyse/internal/model"
)

func TestCodexCollectorPrefersLastUsageAndFillsEvent(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "sessions", "2026", "01", "02")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "rollout-session.jsonl")
	lines := []string{
		`{"type":"session_meta","timestamp":"2026-01-02T03:04:00Z","payload":{"id":"thread-1"}}`,
		`{"type":"turn_context","timestamp":"2026-01-02T03:04:05Z","payload":{"model":"openai/gpt-5.4-2026-03-05"}}`,
		`{"type":"event_msg","timestamp":"2026-01-02T03:04:07Z","payload":{"type":"token_count","info":{"model":"openai/gpt-5.4-2026-03-05","last_token_usage":{"input_tokens":100,"cached_input_tokens":40,"cache_write_input_tokens":10,"output_tokens":20,"reasoning_output_tokens":5},"total_token_usage":{"input_tokens":1000,"cached_input_tokens":400,"cache_write_input_tokens":100,"output_tokens":200,"reasoning_output_tokens":50}}}}`,
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	collector := NewCodexCollector()
	result, err := collector.Collect(Context{DeviceID: "device-1", Home: root, Paths: Paths{Codex: root}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Events) != 1 {
		t.Fatalf("got %d events, want 1", len(result.Events))
	}
	event := result.Events[0]
	if event.Usage.InputTokens != 100 || event.Usage.OutputTokens != 25 || event.Usage.ReasoningTokens != 5 {
		t.Fatalf("last_token_usage was not preferred: %+v", event.Usage)
	}
	if event.Usage.CacheReadTokens != 40 || event.Usage.CacheCreationTokens != 10 {
		t.Fatalf("cache usage = %+v", event.Usage)
	}
	if event.InputTokenSemantics != model.SemanticsTotal {
		t.Fatalf("input semantics = %v, want total", event.InputTokenSemantics)
	}
	if event.Model != "gpt-5.4" || event.ModelRaw != "openai/gpt-5.4-2026-03-05" || event.CanonicalModel != "gpt-5.4" {
		t.Fatalf("unexpected model attribution: %+v", event)
	}
	if event.SessionID != "thread-1" || event.RequestID != "codex_session:"+path+":3" {
		t.Fatalf("missing session/request identity: %+v", event)
	}
	if event.SourceRecordID != path+":3" || event.Metadata["usageSource"] != "last_token_usage" {
		t.Fatalf("unstable source identity: %+v", event)
	}
	if event.ProviderID != "openai" || !event.IsStreaming || event.StatusCode == nil || *event.StatusCode != 200 {
		t.Fatalf("incomplete event metadata: %+v", event)
	}
	if event.TimingQuality != model.TimingEstimated || event.LatencyMS == nil || *event.LatencyMS != 2000 {
		t.Fatalf("unexpected timing: quality=%s latency=%v", event.TimingQuality, event.LatencyMS)
	}
	if len(result.Cursors) != 1 || result.Cursors[0].Offset == 0 {
		t.Fatalf("missing cursor: %+v", result.Cursors)
	}
	var state codexCursorState
	if err := json.Unmarshal([]byte(result.Cursors[0].State), &state); err != nil {
		t.Fatal(err)
	}
	if !state.HasBaseline || state.Baseline.Input != 1000 || state.Baseline.Output != 200 {
		t.Fatalf("cumulative baseline not persisted: %+v", state)
	}
}

func TestCodexCollectorIncrementalTotalDeltaSkipsDuplicatesAndPartialLines(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "sessions", "2026", "01", "02")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "rollout-session.jsonl")
	meta := `{"type":"session_meta","timestamp":"2026-01-02T03:04:00Z","payload":{"id":"thread-2"}}` + "\n"
	first := `{"type":"event_msg","timestamp":"2026-01-02T03:04:01Z","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":100,"cached_input_tokens":20,"cache_write_input_tokens":10,"output_tokens":30,"reasoning_output_tokens":5}}}}` + "\n"
	if err := os.WriteFile(path, []byte(meta+first+first), 0o644); err != nil {
		t.Fatal(err)
	}

	collector := NewCodexCollector()
	ctx := Context{DeviceID: "device-1", Home: root, Paths: Paths{Codex: root}}
	result, err := collector.Collect(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Events) != 1 || result.Events[0].Usage.OutputTokens != 35 {
		t.Fatalf("first cumulative snapshot/duplicate handling = %+v", result.Events)
	}
	if result.Events[0].Metadata["usageSource"] != "total_token_usage" {
		t.Fatalf("usage source = %v", result.Events[0].Metadata["usageSource"])
	}

	second := `{"type":"event_msg","timestamp":"2026-01-02T03:04:02Z","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":150,"cached_input_tokens":30,"cache_write_input_tokens":15,"output_tokens":50,"reasoning_output_tokens":10}}}}` + "\n"
	corrupt := "not-json\n"
	partial := `{"type":"event_msg","timestamp":"2026-01-02T03:04:03Z","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":200,"cached_input_tokens":40,"cache_write_input_tokens":20,"output_tokens":70,"reasoning_output_tokens":`
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString(second + corrupt + partial); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	incremental, err := collector.Collect(ctx, result.Cursors)
	if err != nil {
		t.Fatal(err)
	}
	if len(incremental.Events) != 1 {
		t.Fatalf("incremental events = %+v", incremental.Events)
	}
	event := incremental.Events[0]
	if event.Usage.InputTokens != 50 || event.Usage.CacheReadTokens != 10 || event.Usage.CacheCreationTokens != 5 || event.Usage.OutputTokens != 25 || event.Usage.ReasoningTokens != 5 {
		t.Fatalf("incorrect cumulative delta: %+v", event.Usage)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if int64(len(partial)) != info.Size()-incremental.Cursors[0].Offset {
		t.Fatalf("cursor consumed partial line: offset=%d size=%d", incremental.Cursors[0].Offset, info.Size())
	}

	file, err = os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString("15}}}}\n"); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	completed, err := collector.Collect(ctx, incremental.Cursors)
	if err != nil {
		t.Fatal(err)
	}
	if len(completed.Events) != 1 || completed.Events[0].Usage.InputTokens != 50 || completed.Events[0].Usage.OutputTokens != 25 {
		t.Fatalf("completed partial line = %+v", completed.Events)
	}
	if completed.Events[0].SourceRecordID != path+":6" {
		t.Fatalf("source record ID = %q, want physical line 6", completed.Events[0].SourceRecordID)
	}
	unchanged, err := collector.Collect(ctx, completed.Cursors)
	if err != nil {
		t.Fatal(err)
	}
	if len(unchanged.Events) != 0 {
		t.Fatalf("unchanged file replayed events: %+v", unchanged.Events)
	}
}
