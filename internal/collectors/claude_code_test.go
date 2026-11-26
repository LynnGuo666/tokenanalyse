package collectors

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"tokenanalyse/internal/model"
)

func TestClaudeCodeCollectorParsesDeduplicatesAndEstimatesTiming(t *testing.T) {
	root := t.TempDir()
	projects := filepath.Join(root, "projects", "project")
	if err := os.MkdirAll(projects, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(projects, "session.jsonl")
	lines := []string{
		`{"type":"user","uuid":"u1","parentUuid":null,"timestamp":"2026-01-02T03:04:05Z","sessionId":"session-1","message":{"role":"user","content":"hello"}}`,
		`{"type":"assistant","uuid":"a1","parentUuid":"u1","timestamp":"2026-01-02T03:04:05.500Z","sessionId":"session-1","message":{"id":"msg-1","model":"claude-sonnet-4","stop_reason":null,"content":[{"type":"text","text":"partial"}],"usage":{"input_tokens":10,"output_tokens":5,"cache_read_input_tokens":20,"cache_creation_input_tokens":30}}}`,
		`{"type":"assistant","uuid":"a2","parentUuid":"u1","timestamp":"2026-01-02T03:04:07Z","sessionId":"session-1","thinkingDurationMs":500,"message":{"id":"msg-1","model":"claude-sonnet-4","stop_reason":"end_turn","content":[{"type":"thinking","thinking":"done"}],"usage":{"input_tokens":11,"output_tokens":100,"reasoning_tokens":7,"cache_read_input_tokens":21,"cache_creation_input_tokens":30,"cache_creation":{"ephemeral_1h_input_tokens":10,"ephemeral_5m_input_tokens":20}}}}`,
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	collector := NewClaudeCodeCollector()
	result, err := collector.Collect(Context{DeviceID: "device-1", Home: root, Paths: Paths{ClaudeCode: root}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Events) != 1 {
		t.Fatalf("got %d events, want 1 deduplicated event", len(result.Events))
	}
	event := result.Events[0]
	if event.Usage.InputTokens != 11 || event.Usage.OutputTokens != 100 || event.Usage.ReasoningTokens != 7 {
		t.Fatalf("unexpected usage: %+v", event.Usage)
	}
	if event.Usage.CacheReadTokens != 21 || event.Usage.CacheCreationTokens != 30 || event.Usage.CacheCreation1HTokens != 10 || event.Usage.CacheCreation5MTokens != 20 {
		t.Fatalf("unexpected cache usage: %+v", event.Usage)
	}
	if event.InputTokenSemantics != model.SemanticsFresh {
		t.Fatalf("input semantics = %v, want fresh", event.InputTokenSemantics)
	}
	if event.RequestID != "claude_session:msg-1" || event.SessionID != "session-1" || event.Usage.MessageID != "msg-1" {
		t.Fatalf("missing request/session/message identity: %+v", event)
	}
	if event.ProviderID != "anthropic" || !event.IsStreaming || event.StatusCode == nil || *event.StatusCode != 200 {
		t.Fatalf("incomplete event metadata: %+v", event)
	}
	if event.LatencyMS == nil || *event.LatencyMS != 2000 || event.FirstTokenMS == nil || *event.FirstTokenMS != 1500 {
		t.Fatalf("unexpected timing: latency=%v first=%v", event.LatencyMS, event.FirstTokenMS)
	}
	if event.GenerationDurationMS == nil || *event.GenerationDurationMS != 500 || event.SpeedTPS == nil || *event.SpeedTPS != 200 {
		t.Fatalf("unexpected generation timing: generation=%v speed=%v", event.GenerationDurationMS, event.SpeedTPS)
	}
	if event.TimingQuality != "estimated" {
		t.Fatalf("timing quality = %q, want estimated", event.TimingQuality)
	}
	if event.OccurredAt.Format(time.RFC3339Nano) != "2026-01-02T03:04:07Z" {
		t.Fatalf("occurredAt = %s", event.OccurredAt)
	}

	// Event identity is stable even if a caller replays the file without a cursor.
	replayed, err := collector.Collect(Context{DeviceID: "device-1", Home: root, Paths: Paths{ClaudeCode: root}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(replayed.Events) != 1 || replayed.Events[0].EventID != event.EventID {
		t.Fatalf("event ID is not stable: %q vs %q", event.EventID, replayed.Events[0].EventID)
	}
}

func TestClaudeCodeCollectorIncrementalToleratesCorruptAndPartialLines(t *testing.T) {
	root := t.TempDir()
	projects := filepath.Join(root, "projects", "project")
	if err := os.MkdirAll(projects, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(projects, "session.jsonl")
	first := `{"type":"assistant","timestamp":"2026-01-02T03:04:05Z","sessionId":"s1","message":{"id":"m1","model":"claude-test","stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":2}}}` + "\n"
	corrupt := "{this is not json}\n"
	partial := `{"type":"assistant","timestamp":"2026-01-02T03:04:06Z","sessionId":"s1","message":{"id":"m2","model":"claude-test","stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":`
	if err := os.WriteFile(path, []byte(first+corrupt+partial), 0o644); err != nil {
		t.Fatal(err)
	}

	collector := NewClaudeCodeCollector()
	ctx := Context{DeviceID: "device-1", Home: root, Paths: Paths{ClaudeCode: root}}
	result, err := collector.Collect(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Events) != 1 || result.Events[0].Usage.MessageID != "m1" {
		t.Fatalf("first collect events = %+v", result.Events)
	}
	if len(result.Cursors) != 1 || result.Cursors[0].Offset != int64(len(first+corrupt)) {
		t.Fatalf("cursor advanced into partial line: %+v", result.Cursors)
	}

	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString("4}}}\n"); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := collector.Collect(ctx, result.Cursors)
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Events) != 1 || second.Events[0].Usage.MessageID != "m2" || second.Events[0].Usage.OutputTokens != 4 {
		t.Fatalf("completed partial line was not imported exactly once: %+v", second.Events)
	}
	third, err := collector.Collect(ctx, second.Cursors)
	if err != nil {
		t.Fatal(err)
	}
	if len(third.Events) != 0 {
		t.Fatalf("unchanged file replayed events: %+v", third.Events)
	}
}
