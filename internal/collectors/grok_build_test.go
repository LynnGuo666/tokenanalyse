package collectors

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"tokenanalyse/internal/model"
)

func TestGrokBuildTurnUsageIsIndependentPerTurn(t *testing.T) {
	home := t.TempDir()
	active := filepath.Join(home, "sessions", "enc-project", "session-active", "updates.jsonl")
	archived := filepath.Join(home, "archived_sessions", "enc-project", "session-archived", "updates.jsonl")
	grokBuildWriteFile(t, active, `not-json
{"timestamp":1767225600,"method":"_x.ai/session/update","params":{"update":{"sessionUpdate":"agent_message_chunk","usage":{"inputTokens":9999}}}}
{"timestamp":1767225601,"method":"_x.ai/session/update","params":{"update":{"sessionUpdate":"turn_completed","prompt_id":"p1","usage":{"modelUsage":{"grok-4.5-build":{"inputTokens":100,"outputTokens":10,"cachedReadTokens":20,"reasoningTokens":4,"apiDurationMs":1234,"modelCalls":2,"costUsdTicks":1234567890}}}}}}
{"timestamp":1767225602,"method":"_x.ai/session/update","params":{"update":{"sessionUpdate":"turn_completed","prompt_id":"p2","usage":{"modelUsage":{"grok-4.5-build":{"inputTokens":100,"outputTokens":10,"cachedReadTokens":20,"reasoningTokens":4,"apiDurationMs":1234,"modelCalls":2,"costUsdTicks":1234567890}}}}}}
{"timestamp":"2026-01-01T00:00:03Z","method":"_x.ai/session/update","params":{"update":{"sessionUpdate":"turn_completed","prompt_id":"p3","usage":{"modelUsage":{"grok-4.5-build":{"inputTokens":40,"outputTokens":4,"cachedReadTokens":0},"grok-4.3":{"inputTokens":30,"outputTokens":3,"cachedReadTokens":5}}}}}}
`)
	// Missing modelUsage falls back to the top-level per-turn counters. A
	// missing sessionUpdate is accepted for backward compatibility.
	grokBuildWriteFile(t, archived, `{"timestamp":1767225604,"method":"_x.ai/session/update","params":{"update":{"prompt_id":"archived-p1","usage":{"inputTokens":50,"outputTokens":5,"cachedReadTokens":10,"apiDurationMs":50}}}}
`)

	result, err := NewGrokBuildCollector().Collect(Context{DeviceID: "device-1", Paths: Paths{Grok: home}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.FilesScanned != 2 || len(result.Errors) != 0 || len(result.Events) != 5 {
		t.Fatalf("unexpected collection result: %+v", result)
	}
	byRecord := map[string]model.UsageEvent{}
	for _, event := range result.Events {
		byRecord[event.SourceRecordID] = event
	}
	p1 := byRecord["session-active:p1:grok-4.5-build"]
	p2 := byRecord["session-active:p2:grok-4.5-build"]
	if p1.EventID == "" || p1.EventID == p2.EventID {
		t.Fatalf("turn IDs are not stable and distinct: p1=%q p2=%q", p1.EventID, p2.EventID)
	}
	// Identical consecutive counters are two real turns, not a zero delta.
	if p1.Usage.InputTokens != 100 || p1.Usage.OutputTokens != 10 || p1.Usage.CacheReadTokens != 20 || p2.Usage.InputTokens != 100 {
		t.Fatalf("turns were differenced instead of imported at face value: p1=%+v p2=%+v", p1.Usage, p2.Usage)
	}
	if p1.InputTokenSemantics != model.SemanticsTotal || p1.Source != model.SourceSession || p1.SourceApp != "grokbuild" {
		t.Fatalf("incorrect source/semantics: %+v", p1)
	}
	if p1.RequestID != "grok_session:session-active:p1:grok-4.5-build" || p1.SessionID != "session-active" || p1.ProviderID != "_grok_session" || p1.ProviderName != "Grok Build" {
		t.Fatalf("incomplete event fields: %+v", p1)
	}
	if p1.RequestModel != "grok-4.5-build" || p1.PricingModel != "grok-4.5-build" || p1.Usage.Model != "grok-4.5-build" {
		t.Fatalf("incomplete model fields: %+v", p1)
	}
	if p1.LatencyMS == nil || *p1.LatencyMS != 1234 || p1.Metadata["modelCalls"] != uint64(2) {
		t.Fatalf("missing turn metadata: %+v", p1)
	}
	if p1.Cost.TotalCostUSD != "0.123456789" || p1.Metadata["costUsdTicks"] != "1234567890" {
		t.Fatalf("reported cost was not preserved: %+v", p1.Cost)
	}
	if p1.TimingQuality != model.TimingUnknown || !p1.IsStreaming || p1.StatusCode == nil || *p1.StatusCode != 200 || p1.CostMultiplier != "1" || p1.IngestedAt.IsZero() {
		t.Fatalf("incomplete event metadata: %+v", p1)
	}
	if !p1.OccurredAt.Equal(time.Unix(1767225601, 0).UTC()) {
		t.Fatalf("epoch timestamp parsed incorrectly: %s", p1.OccurredAt)
	}
	if event := byRecord["session-active:p3:grok-4.3"]; event.Usage.InputTokens != 30 || event.Usage.CacheReadTokens != 5 {
		t.Fatalf("multi-model usage missing: %+v", event)
	}
	if event := byRecord["session-archived:archived-p1:unknown"]; event.Usage.InputTokens != 50 || event.Usage.OutputTokens != 5 || event.Usage.CacheReadTokens != 10 {
		t.Fatalf("top-level usage fallback failed: %+v", event)
	}
}

func TestGrokBuildIncrementalCursorHandlesDamageAndPartialTail(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, "sessions", "enc-project", "session-incremental", "updates.jsonl")
	firstLine := `{"timestamp":1767225601,"method":"_x.ai/session/update","params":{"update":{"sessionUpdate":"turn_completed","prompt_id":"p1","usage":{"inputTokens":100,"outputTokens":10,"cachedReadTokens":20}}}}
`
	secondLine := `{"timestamp":1767225602,"method":"_x.ai/session/update","params":{"update":{"sessionUpdate":"turn_completed","prompt_id":"p2","usage":{"inputTokens":75,"outputTokens":7,"cachedReadTokens":0}}}}
`
	thirdLine := `{"timestamp":1767225603,"method":"_x.ai/session/update","params":{"update":{"sessionUpdate":"turn_completed","prompt_id":"p3","usage":{"inputTokens":25,"outputTokens":2,"cachedReadTokens":5}}}}
`
	oversized := `{"padding":"` + strings.Repeat("x", grokBuildMaxLine) + `"}` + "\n"
	split := len(secondLine) / 2
	initial := firstLine + "invalid json\n" + oversized + secondLine[:split]
	grokBuildWriteFile(t, path, initial)

	collector := NewGrokBuildCollector()
	ctx := Context{DeviceID: "device-1", Paths: Paths{Grok: home}}
	first, err := collector.Collect(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Errors) != 0 || len(first.Events) != 1 || first.Events[0].SourceRecordID != "session-incremental:p1:unknown" {
		t.Fatalf("damaged lines were not isolated: %+v", first)
	}
	if len(first.Cursors) != 1 || first.Cursors[0].Offset != int64(len(firstLine)+len("invalid json\n")+len(oversized)) {
		t.Fatalf("cursor advanced past partial tail: %+v", first.Cursors)
	}

	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString(secondLine[split:] + thirdLine); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := collector.Collect(ctx, first.Cursors)
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Errors) != 0 || len(second.Events) != 2 {
		t.Fatalf("append after partial tail failed: %+v", second)
	}
	if second.Events[0].SourceRecordID != "session-incremental:p2:unknown" || second.Events[1].SourceRecordID != "session-incremental:p3:unknown" {
		t.Fatalf("unexpected appended events: %+v", second.Events)
	}

	unchanged, err := collector.Collect(ctx, second.Cursors)
	if err != nil {
		t.Fatal(err)
	}
	if len(unchanged.Events) != 0 || unchanged.BytesRead != 0 {
		t.Fatalf("unchanged file was reread: %+v", unchanged)
	}

	fullReplay, err := collector.Collect(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(fullReplay.Events) != 3 {
		t.Fatalf("full replay got %d events, want 3", len(fullReplay.Events))
	}
	ids := map[string]bool{}
	for _, event := range fullReplay.Events {
		ids[event.EventID] = true
	}
	for _, event := range append(first.Events, second.Events...) {
		if !ids[event.EventID] {
			t.Fatalf("event ID changed between incremental and full collection: %s", event.EventID)
		}
	}
}

func grokBuildWriteFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
