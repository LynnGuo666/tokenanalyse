package collectors

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"tokenanalyse/internal/model"
)

func writePiTestSession(t *testing.T, path string, lines ...string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestPiCollectorParsesUsageAndIncrementalCursor(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "sessions", "project", "session.jsonl")
	writePiTestSession(t, path,
		`{"type":"session","version":3,"id":"session-a","timestamp":"2023-11-14T22:13:20Z","cwd":"/work"}`,
		`{damaged-json`,
		`{"type":"message","id":"assistant","parentId":"user","timestamp":"2023-11-14T22:13:21Z","message":{"role":"assistant","provider":"custom-pi","model":"requested-model","responseModel":"actual-model","timestamp":1700000000000,"usage":{"input":10,"output":7,"cacheRead":5,"cacheWrite":2,"reasoning":3,"totalTokens":23,"cost":{"input":0.00001,"output":0.000014,"cacheRead":0.0000005,"cacheWrite":0.000001,"total":0.0000255}},"stopReason":"stop"}}`,
	)
	ctx := Context{DeviceID: "device-pi", Paths: Paths{Pi: root}}
	collector := PiCollector{}

	first, err := collector.Collect(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Events) != 1 {
		t.Fatalf("expected one event, got %d (errors: %v)", len(first.Events), first.Errors)
	}
	event := first.Events[0]
	if event.SourceApp != "pi" || event.SessionID != "session-a" || event.SourceRecordID != "session-a:assistant" {
		t.Fatalf("unexpected identity: %+v", event)
	}
	if event.Model != "actual-model" || event.RequestModel != "requested-model" || event.ProviderID != "custom-pi" {
		t.Fatalf("unexpected attribution: %+v", event)
	}
	if event.Usage.InputTokens != 10 || event.Usage.OutputTokens != 10 || event.Usage.ReasoningTokens != 3 {
		t.Fatalf("unexpected usage: %+v", event.Usage)
	}
	if event.Usage.CacheReadTokens != 5 || event.Usage.CacheCreationTokens != 2 || event.InputTokenSemantics != model.SemanticsFresh {
		t.Fatalf("unexpected cache/semantics: %+v", event)
	}
	if event.LatencyMS == nil || *event.LatencyMS != 1000 || event.TimingQuality != model.TimingEstimated {
		t.Fatalf("unexpected timing: latency=%v quality=%v", event.LatencyMS, event.TimingQuality)
	}
	if event.Cost.TotalCostUSD != "0.0000255" {
		t.Fatalf("reported cost was not preserved: %+v", event.Cost)
	}
	if len(first.Cursors) != 1 || first.Cursors[0].Offset <= 0 {
		t.Fatalf("missing incremental cursor: %+v", first.Cursors)
	}

	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	secondLine := `{"type":"message","id":"assistant-2","timestamp":"2023-11-14T22:13:22Z","message":{"role":"assistant","provider":"custom-pi","model":"m2","usage":{"input":4,"output":2,"cacheRead":1,"cacheWrite":0},"stopReason":"stop"}}`
	if _, err := file.WriteString("\n" + secondLine + "\n"); err != nil {
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
	if len(second.Events) != 1 || second.Events[0].SourceRecordID != "session-a:assistant-2" {
		t.Fatalf("incremental scan expected only appended event, got %+v", second.Events)
	}
	third, err := collector.Collect(ctx, second.Cursors)
	if err != nil {
		t.Fatal(err)
	}
	if len(third.Events) != 0 {
		t.Fatalf("unchanged session emitted duplicates: %+v", third.Events)
	}
}

func TestPiCollectorDefersPartialTailAndKeepsFailedUsage(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "session.jsonl")
	header := `{"type":"session","id":"session-b","timestamp":"2023-11-14T22:13:20Z"}`
	failed := `{"type":"message","id":"failed","timestamp":"2023-11-14T22:13:21Z","message":{"role":"assistant","provider":"p","model":"m","usage":{"input":0,"output":0,"cacheRead":0,"cacheWrite":0},"stopReason":"error","errorMessage":"boom"}}`
	complete := `{"type":"message","id":"later","timestamp":"2023-11-14T22:13:22Z","message":{"role":"assistant","provider":"p","model":"m","usage":{"input":2,"output":1,"cacheRead":0,"cacheWrite":0},"stopReason":"stop"}}`
	split := len(complete) - 12
	if err := os.WriteFile(path, []byte(header+"\n"+failed+"\n"+complete[:split]), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx := Context{DeviceID: "device-pi", Paths: Paths{Pi: root}}
	collector := PiCollector{}

	first, err := collector.Collect(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Events) != 1 || first.Events[0].StatusCode == nil || *first.Events[0].StatusCode != 500 || first.Events[0].ErrorMessage != "boom" {
		t.Fatalf("failed usage was not retained: %+v", first.Events)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Cursors) != 1 || first.Cursors[0].Offset >= info.Size() {
		t.Fatalf("partial tail should not advance the cursor: cursor=%+v size=%d", first.Cursors, info.Size())
	}

	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString(complete[split:] + "\n"); err != nil {
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
	if len(second.Events) != 1 || second.Events[0].SourceRecordID != "session-b:later" {
		t.Fatalf("completed tail was not imported: %+v", second.Events)
	}
	if !second.Events[0].OccurredAt.Equal(time.Date(2023, 11, 14, 22, 13, 22, 0, time.UTC)) {
		t.Fatalf("unexpected timestamp: %v", second.Events[0].OccurredAt)
	}
}
