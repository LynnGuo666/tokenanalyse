package collectors

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"tokenanalyse/internal/model"
)

func TestWorkBuddyCollectorUsesFreshInputAndIgnoresCredit(t *testing.T) {
	home := t.TempDir()
	main := filepath.Join(home, "projects", "project", "s1.jsonl")
	writeBuddyFile(t, main, `{"id":"u1","type":"message","role":"user","sessionId":"s1","timestamp":1792764357348}
{"id":"a1","type":"message","role":"assistant","sessionId":"s1","timestamp":1792764379226,"providerData":{"model":"glm-test","requestModelId":"fast-model","rawUsage":{"prompt_tokens":1200,"completion_tokens":34,"prompt_cache_hit_tokens":1000,"cache_creation_input_tokens":100,"completion_thinking_tokens":3,"credit":0.5}}}
{"id":"c1","type":"function_call","sessionId":"s1","timestamp":1792764380000,"providerData":{"model":"glm-test","rawUsage":{"prompt_tokens":1000,"completion_tokens":20,"prompt_cache_hit_tokens":900}}}
`)
	subagent := filepath.Join(home, "projects", "project", "s1", "subagents", "agent-x.jsonl")
	writeBuddyFile(t, subagent, `{"id":"c2","type":"function_call","sessionId":"agent-own-id","timestamp":1792764380000,"providerData":{"model":"glm-test","rawUsage":{"prompt_tokens":1000,"completion_tokens":20,"prompt_cache_hit_tokens":900}}}
`)

	collector := NewWorkBuddyCollector()
	result, err := collector.Collect(Context{DeviceID: "device", Paths: Paths{WorkBuddy: home}}, nil)
	if err != nil || len(result.Events) != 3 {
		t.Fatalf("events=%d err=%v errors=%v", len(result.Events), err, result.Errors)
	}
	event := result.Events[0]
	if event.Usage.InputTokens != 100 || event.Usage.CacheReadTokens != 1000 || event.Usage.CacheCreationTokens != 100 || event.Usage.OutputTokens != 34 || event.Usage.ReasoningTokens != 3 {
		t.Fatalf("usage=%+v", event.Usage)
	}
	if event.InputTokenSemantics != model.SemanticsFresh || event.Cost.TotalCostUSD != "0" || event.Metadata["credit"] != json.Number("0.5") {
		t.Fatalf("semantics/cost/credit=%v/%s/%v", event.InputTokenSemantics, event.Cost.TotalCostUSD, event.Metadata["credit"])
	}
	if result.Events[1].RequestID != "workbuddy:s1:c1" || result.Events[2].SessionID != "s1" || result.Events[2].RequestID != "workbuddy:agent-own-id:c2" {
		eventsByRequest := map[string]model.UsageEvent{}
		for _, event := range result.Events {
			eventsByRequest[event.RequestID] = event
		}
		if eventsByRequest["workbuddy:s1:c1"].RequestID == "" || eventsByRequest["workbuddy:s1:c2"].SessionID != "s1" || eventsByRequest["workbuddy:s1:c2"].RequestID == "" {
			t.Fatalf("tool/subagent identities=%v", eventsByRequest)
		}
	}

	// Same-process incremental resumes retain usage identity across appends.
	appendBuddyFile(t, subagent, `{"id":"c3","type":"function_call","sessionId":"agent-own-id","timestamp":1792764381000,"providerData":{"model":"glm-test","rawUsage":{"prompt_tokens":10,"completion_tokens":1}}}`+"\n"+`{"id":`)
	incremental, err := collector.Collect(Context{DeviceID: "device", Paths: Paths{WorkBuddy: home}}, result.Cursors)
	if err != nil || len(incremental.Events) != 1 || incremental.Events[0].RequestID != "workbuddy:s1:c3" {
		t.Fatalf("incremental events=%+v err=%v", incremental.Events, err)
	}

	// Rewrites replay safely through stable event IDs and reset byte cursors.
	writeBuddyFile(t, main, `{"id":"a1","type":"message","role":"assistant","sessionId":"s1","timestamp":1792764379226,"providerData":{"model":"glm-test","rawUsage":{"prompt_tokens":1200,"completion_tokens":34,"prompt_cache_hit_tokens":1000,"cache_creation_input_tokens":100,"completion_thinking_tokens":3,"credit":0.5}}}`+"\n")
	replayed, err := collector.Collect(Context{DeviceID: "device", Paths: Paths{WorkBuddy: home}}, incremental.Cursors)
	if err != nil || len(replayed.Events) != 1 {
		t.Fatalf("replayed events=%d err=%v", len(replayed.Events), err)
	}
	if replayed.Events[0].EventID != event.EventID {
		t.Fatal("event identity changed after rewrite")
	}
	stat, err := os.Stat(main)
	if err != nil {
		t.Fatal(err)
	}
	if replayed.Cursors[0].Offset != stat.Size() {
		t.Fatalf("cursor=%d size=%d", replayed.Cursors[0].Offset, stat.Size())
	}
}

func TestBuddyCollectorSkipsMissingDirectory(t *testing.T) {
	result, err := NewWorkBuddyCollector().Collect(Context{Paths: Paths{WorkBuddy: filepath.Join(t.TempDir(), "missing")}}, nil)
	if err != nil || len(result.Events) != 0 || len(result.Errors) != 0 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}
