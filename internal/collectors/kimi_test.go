package collectors

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"tokenanalyse/internal/model"
)

func writeKimiJSONL(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestKimiCollectorImportsCommittedStepsAndSubagents(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, "sessions", "project", "session-one", "agents", "main", "wire.jsonl")
	writeKimiJSONL(t, path, `{"type":"context.append_loop_event","time":1699999999990,"agentId":"main","event":{"type":"step.begin","uuid":"step-1","turnId":"0","step":1}}
{"type":"llm.request","time":1700000000000,"agentId":"main","kind":"loop","turnStep":"0.1","model":"kimi-k2"}
{"type":"usage.record","time":1700000000900,"agentId":"main","model":"kimi-k2","usage":{"inputOther":10,"output":50,"inputCacheRead":30,"inputCacheCreation":0}}
{"type":"context.append_loop_event","time":1700000001000,"agentId":"main","event":{"type":"step.end","uuid":"step-1","messageId":"message-1","usage":{"inputOther":10,"output":50,"inputCacheRead":30,"inputCacheCreation":0},"llmFirstTokenLatencyMs":200,"llmStreamDurationMs":800}}
`)
	child := filepath.Join(home, "sessions", "project", "session-one", "agents", "agent-0", "wire.jsonl")
	writeKimiJSONL(t, child, `{"type":"context.append_loop_event","time":1700000002000,"event":{"type":"step.begin","uuid":"step-2"}}
{"type":"context.append_loop_event","time":1700000003000,"event":{"type":"step.end","uuid":"step-2","messageId":"child-1","usage":{"inputOther":5,"output":8,"inputCacheRead":2,"inputCacheCreation":1}}}
`)

	collector := NewKimiCollector()
	result, err := collector.Collect(Context{DeviceID: "device", Paths: Paths{Kimi: home}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Events) != 2 || result.FilesScanned != 2 {
		t.Fatalf("events=%d files=%d errors=%v", len(result.Events), result.FilesScanned, result.Errors)
	}
	eventsByRequest := map[string]model.UsageEvent{}
	for _, event := range result.Events {
		eventsByRequest[event.RequestID] = event
	}
	main := eventsByRequest["kimi:agent-loop:message-1"]
	if main.Model != "kimi-k2" || main.RequestID != "kimi:agent-loop:message-1" {
		t.Fatalf("unexpected main event: %+v", main)
	}
	if main.Usage.InputTokens != 10 || main.Usage.OutputTokens != 50 || main.Usage.CacheReadTokens != 30 {
		t.Fatalf("unexpected usage: %+v", main.Usage)
	}
	if main.InputTokenSemantics != model.SemanticsFresh {
		t.Fatalf("semantics=%v", main.InputTokenSemantics)
	}
	if main.LatencyMS == nil || *main.LatencyMS != 1000 || main.FirstTokenMS == nil || *main.FirstTokenMS != 200 {
		t.Fatalf("timing: latency=%v first=%v", main.LatencyMS, main.FirstTokenMS)
	}
	if main.OccurredAt.UnixMilli() != 1700000001000 {
		t.Fatalf("occurredAt=%s", main.OccurredAt)
	}
	childEvent := eventsByRequest["kimi:subagent:child-1"]
	if childEvent.RequestID != "kimi:subagent:child-1" || childEvent.Model != "unknown" || childEvent.SessionID != "session-one" {
		t.Fatalf("unexpected subagent event: %+v", childEvent)
	}

	// An unchanged file is skipped. A damaged line is tolerated without losing
	// the valid record before it, and replay remains idempotent.
	again, err := collector.Collect(Context{DeviceID: "device", Paths: Paths{Kimi: home}}, result.Cursors)
	if err != nil || len(again.Events) != 0 || again.FilesScanned != 0 {
		t.Fatalf("unchanged scan: events=%d files=%d err=%v", len(again.Events), again.FilesScanned, err)
	}
	writeKimiJSONL(t, child, "{not-json}\n"+`{"type":"context.append_loop_event","time":1700000003000,"event":{"type":"step.end","uuid":"step-2","messageId":"child-1","usage":{"inputOther":5,"output":8,"inputCacheRead":2,"inputCacheCreation":1}}}`+"\n")
	time.Sleep(time.Millisecond)
	replayed, err := collector.Collect(Context{DeviceID: "device", Paths: Paths{Kimi: home}}, result.Cursors)
	if err != nil {
		t.Fatal(err)
	}
	if len(replayed.Events) != 1 || replayed.Events[0].EventID != childEvent.EventID {
		t.Fatalf("replay changed identity: %+v", replayed.Events)
	}
	if len(replayed.Errors) != 1 {
		t.Fatalf("expected damaged-record diagnostic, got %v", replayed.Errors)
	}
}

func TestKimiCollectorRetriesIncompleteTail(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, "sessions", "project", "session-one", "agents", "main", "wire.jsonl")
	writeKimiJSONL(t, path, `{"type":"agent.message.appended","time":1700000000000,"message":{"message":{"role":"assistant"},"meta":{"messageId":"one","model":{"provider":"custom","model":"vendor/model"},"usage":{"inputOther":10,"output":20,"inputCacheRead":30,"inputCacheCreation":40}}}}`+"\n"+`{"type":`)
	collector := NewKimiCollector()
	result, err := collector.Collect(Context{Paths: Paths{Kimi: home}}, nil)
	if err != nil || len(result.Events) != 1 {
		t.Fatalf("events=%d err=%v", len(result.Events), err)
	}
	// The cursor must stop before the incomplete record, not at EOF.
	completeRecord := `{"type":"agent.message.appended","time":1700000000000,"message":{"message":{"role":"assistant"},"meta":{"messageId":"one","model":{"provider":"custom","model":"vendor/model"},"usage":{"inputOther":10,"output":20,"inputCacheRead":30,"inputCacheCreation":40}}}}`
	if result.Cursors[0].Offset != int64(len(completeRecord)+1) {
		t.Fatalf("cursor=%d want=%d", result.Cursors[0].Offset, len(completeRecord)+1)
	}
}
