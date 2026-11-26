package collectors

import (
	"os"
	"path/filepath"
	"testing"

	"tokenanalyse/internal/model"
)

func writeBuddyFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func appendBuddyFile(t *testing.T, path, content string) {
	t.Helper()
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString(content); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestCodeBuddyCollectorReadsMainAndSubagentUsage(t *testing.T) {
	home := t.TempDir()
	main := filepath.Join(home, "projects", "project", "s1.jsonl")
	writeBuddyFile(t, main, `{"id":"reasoning","type":"reasoning","sessionId":"s1","timestamp":1791900000000,"providerData":{"messageId":"m1","stepSeq":1}}
{"id":"c1","type":"function_call","sessionId":"s1","timestamp":1791900000000,"providerData":{"messageId":"m1","stepSeq":1,"requestModelId":"custom-local:glm-5.3","rawUsage":{"prompt_tokens":1200,"completion_tokens":40,"prompt_cache_hit_tokens":1000,"prompt_cache_write_tokens":50,"completion_thinking_tokens":3}}}
{"id":"a1","type":"message","role":"assistant","sessionId":"s1","timestamp":1791900001000,"providerData":{"messageId":"m1","stepSeq":2,"model":"glm-5.3","usage":{"inputTokens":500,"outputTokens":7,"inputTokensDetails":[{"cached_tokens":400}],"outputTokensDetails":[{"reasoning_tokens":2}]}}}
`)
	subagent := filepath.Join(home, "projects", "project", "s1", "subagents", "agent-1.jsonl")
	writeBuddyFile(t, subagent, `{"id":"c9","type":"function_call","sessionId":"agent-session","timestamp":1791900002000,"providerData":{"messageId":"m9","stepSeq":1,"model":"hy4-preview","rawUsage":{"prompt_tokens":100,"completion_tokens":20,"prompt_cache_hit_tokens":90}}}
`)

	collector := NewCodeBuddyCollector()
	result, err := collector.Collect(Context{DeviceID: "device", Paths: Paths{CodeBuddy: home}}, nil)
	if err != nil || len(result.Events) != 3 || result.FilesScanned != 2 {
		t.Fatalf("events=%d files=%d err=%v errors=%v events=%+v", len(result.Events), result.FilesScanned, err, result.Errors, result.Events)
	}
	eventsByRequest := map[string]model.UsageEvent{}
	for _, event := range result.Events {
		eventsByRequest[event.RequestID] = event
	}
	event := eventsByRequest["codebuddy:s1:m1:1"]
	if event.Model != "glm-5.3" || event.Usage.InputTokens != 150 || event.Usage.CacheReadTokens != 1000 || event.Usage.CacheCreationTokens != 50 || event.Usage.ReasoningTokens != 3 {
		t.Fatalf("event=%+v", event)
	}
	if event.InputTokenSemantics != model.SemanticsFresh || event.RequestID != "codebuddy:s1:m1:1" {
		t.Fatalf("identity=%q semantics=%v", event.RequestID, event.InputTokenSemantics)
	}
	if eventsByRequest["codebuddy:s1:m9:1"].SessionID != "s1" {
		t.Fatalf("subagent=%+v", eventsByRequest["codebuddy:s1:m9:1"])
	}

	appendBuddyFile(t, main, `{"id":"c2","type":"function_call","sessionId":"s1","timestamp":1791900003000,"providerData":{"messageId":"m2","stepSeq":1,"model":"glm-5.3","rawUsage":{"prompt_tokens":10,"completion_tokens":2}}}`+"\n"+`{"id":`)
	appended, err := collector.Collect(Context{DeviceID: "device", Paths: Paths{CodeBuddy: home}}, result.Cursors)
	if err != nil || len(appended.Events) != 1 {
		t.Fatalf("events=%d err=%v", len(appended.Events), err)
	}
	stat, err := os.Stat(main)
	if err != nil {
		t.Fatal(err)
	}
	var cursor Cursor
	for _, candidate := range appended.Cursors {
		if candidate.FilePath == main {
			cursor = candidate
		}
	}
	if cursor.Offset >= stat.Size() {
		t.Fatal("partial tail was consumed")
	}
}
