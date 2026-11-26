package collectors

import (
	"os"
	"path/filepath"
	"testing"

	"tokenanalyse/internal/model"
)

func writeCopilotSession(t *testing.T, home, content string) string {
	t.Helper()
	path := filepath.Join(home, "session-state", "session-1", "events.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestCopilotCollectorNormalizesUsageAndResumes(t *testing.T) {
	home := t.TempDir()
	path := writeCopilotSession(t, home, `{"type":"session.start","timestamp":"2026-10-10T09:08:00Z","data":{"selectedModel":"deepseek/deepseek-v4"}}
{"type":"session.usage_record","id":"u1","timestamp":"2026-10-10T09:08:03.054Z","data":{"usage":{"model":"gpt-5-mini","inputTokens":25301,"outputTokens":100,"cacheReadTokens":7680,"cacheWriteTokens":0,"duration":2988,"timeToFirstTokenMs":2906.8,"copilotUsage":{"tokenDetails":[{"tokenType":"input","tokenCount":17621},{"tokenType":"cache_read","tokenCount":7680},{"tokenType":"cache_write","tokenCount":0},{"tokenType":"output","tokenCount":100}]}}}}
`)
	collector := NewCopilotCollector()
	first, err := collector.Collect(Context{DeviceID: "device", Paths: Paths{Copilot: home}}, nil)
	if err != nil || len(first.Events) != 1 {
		t.Fatalf("events=%d err=%v", len(first.Events), err)
	}
	event := first.Events[0]
	if event.Usage.InputTokens != 17621 || event.Usage.CacheReadTokens != 7680 || event.Usage.OutputTokens != 100 {
		t.Fatalf("usage=%+v", event.Usage)
	}
	if event.InputTokenSemantics != model.SemanticsFresh || event.RequestID != "copilot:session-1:u1" {
		t.Fatalf("event=%+v", event)
	}
	if event.LatencyMS == nil || *event.LatencyMS != 2988 || event.FirstTokenMS == nil || *event.FirstTokenMS != 2907 {
		if event.LatencyMS == nil || event.FirstTokenMS == nil {
			t.Fatalf("timing pointers: latency=%v first=%v", event.LatencyMS, event.FirstTokenMS)
		}
		t.Fatalf("timing values: latency=%d first=%d", *event.LatencyMS, *event.FirstTokenMS)
	}
	if event.StatusCode == nil || *event.StatusCode != 200 {
		t.Fatalf("status=%v", event.StatusCode)
	}

	// Appended records use the selected model retained by the cursor. Older
	// records without tokenDetails subtract cache reads from the inclusive input.
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	_, err = file.WriteString(`{"type":"session.usage_record","timestamp":"2026-10-10T09:08:04Z","data":{"usage":{"model":"gpt-5-mini","inputTokens":100,"outputTokens":20,"cacheReadTokens":30,"cacheWriteTokens":10,"duration":100}}}` + "\n" + `{"type":"session.usage_record","id":"partial"`)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := collector.Collect(Context{DeviceID: "device", Paths: Paths{Copilot: home}}, first.Cursors)
	if err != nil || len(second.Events) != 1 {
		t.Fatalf("events=%d err=%v", len(second.Events), err)
	}
	if second.Events[0].Usage.InputTokens != 60 || second.Events[0].Usage.CacheCreationTokens != 10 {
		t.Fatalf("usage=%+v", second.Events[0].Usage)
	}
	stat, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if second.Cursors[0].Offset >= stat.Size() {
		t.Fatal("cursor consumed partial tail")
	}

	// A fresh collector restores model-selection state from persisted cursors.
	third, err := NewCopilotCollector().Collect(Context{DeviceID: "device", Paths: Paths{Copilot: home}}, second.Cursors)
	if err != nil || len(third.Events) != 0 {
		t.Fatalf("unchanged events=%d err=%v", len(third.Events), err)
	}
}

func TestCopilotCollectorSkipsDamagedAndEmptyRecords(t *testing.T) {
	home := t.TempDir()
	writeCopilotSession(t, home, `{bad json}
{"type":"session.usage_record","id":"zero","timestamp":"2026-10-10T09:08:03Z","data":{"usage":{"model":"m"}}}
`)
	result, err := NewCopilotCollector().Collect(Context{Paths: Paths{Copilot: home}}, nil)
	if err != nil || len(result.Events) != 0 {
		t.Fatalf("events=%d err=%v", len(result.Events), err)
	}
	if len(result.Errors) != 1 || len(result.Cursors) != 1 {
		t.Fatalf("errors=%v cursors=%v", result.Errors, result.Cursors)
	}
}
