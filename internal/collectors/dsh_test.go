package collectors

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"

	"tokenanalyse/internal/model"
)

func TestDSHCollectorJSONLIncrementalAndFresh(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "sessions", "project", "session-one", "session.v4.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	firstLines := "" +
		`{"type":"step/start","time":1700000000000,"data":{"turn":1,"step":1}}` + "\n" +
		`{"type":"assistant/message","time":1700000003000,"data":{"turn":1,"step":1,"message":{"id":"m1","source":{"provider":"deepseek-account","model":"global:deepseek-flash"}},"usage":{"inputTokens":100,"outputTokens":50,"cacheReadTokens":900,"cacheWriteTokens":0},"stream":[{"time":1700000000200}]}}` + "\n" +
		`this is a damaged line` + "\n"
	if err := os.WriteFile(path, []byte(firstLines), 0o644); err != nil {
		t.Fatal(err)
	}

	collector := NewDSHCollector()
	ctx := Context{DeviceID: "device-1", Paths: Paths{DSH: root}}
	first, err := collector.Collect(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Events) != 1 {
		t.Fatalf("expected one event, got %d (errors: %v)", len(first.Events), first.Errors)
	}
	event := first.Events[0]
	if event.InputTokenSemantics != model.SemanticsFresh {
		t.Fatalf("input semantics = %v, want fresh", event.InputTokenSemantics)
	}
	if event.Usage.InputTokens != 100 || event.Usage.OutputTokens != 50 || event.Usage.CacheReadTokens != 900 {
		t.Fatalf("unexpected usage: %+v", event.Usage)
	}
	if event.ProviderID != "_dsh_account_session" || event.PricingModel != "deepseek-flash" {
		t.Fatalf("provider/pricing model not preserved: %+v", event)
	}
	if event.LatencyMS == nil || *event.LatencyMS != 3000 || event.FirstTokenMS == nil || *event.FirstTokenMS != 200 {
		t.Fatalf("timing not recovered: latency=%v first=%v", event.LatencyMS, event.FirstTokenMS)
	}
	if len(first.Cursors) != 1 || first.Cursors[0].Offset != int64(len(firstLines)) {
		t.Fatalf("unexpected cursors: %+v", first.Cursors)
	}

	unchanged, err := collector.Collect(ctx, first.Cursors)
	if err != nil {
		t.Fatal(err)
	}
	if len(unchanged.Events) != 0 || unchanged.FilesScanned != 0 {
		t.Fatalf("unchanged file was re-read: %+v", unchanged)
	}

	secondLines := "" +
		`{"type":"step/start","time":1700000010000,"data":{"turn":1,"step":2}}` + "\n" +
		`{"type":"assistant/message","time":1700000012500,"data":{"turn":1,"step":2,"message":{"id":"m2","source":{"provider":"gateway","model":"deepseek-pro"}},"usage":{"inputTokens":20,"outputTokens":10,"cacheReadTokens":1000,"cacheWriteTokens":0}}}` + "\n"
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString(secondLines); err != nil {
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
	if len(second.Events) != 1 || second.Events[0].RequestID != "dsh:m2" {
		t.Fatalf("expected only appended event, got %+v", second.Events)
	}
	if second.Events[0].EventID == event.EventID {
		t.Fatal("different messages received the same event ID")
	}

	replayed, err := collector.Collect(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(replayed.Events) != 2 {
		t.Fatalf("replay returned %d events, want 2", len(replayed.Events))
	}
	ids := map[string]bool{}
	for _, replayedEvent := range replayed.Events {
		ids[replayedEvent.EventID] = true
	}
	if !ids[event.EventID] || !ids[second.Events[0].EventID] {
		t.Fatal("event IDs are not stable across replay")
	}
}

func TestDSHCollectorZstdFramesWaitForTornTail(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "sessions", "project", "session-two", "session.v4.jsonl.zstd")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}

	firstText := "" +
		`{"type":"step/start","time":1700000000000,"data":{"turn":1,"step":1}}` + "\n" +
		`{"type":"assistant/message","time":1700000003000,"data":{"turn":1,"step":1,"message":{"id":"z1","source":{"provider":"gateway","model":"deepseek-flash"}},"usage":{"inputTokens":10,"outputTokens":5,"cacheReadTokens":20,"cacheWriteTokens":0}}}` + "\n" +
		`{"type":"step/start","time":1700000010000,"data":{"turn":1,"step":2}}` + "\n"
	secondText := `{"type":"assistant/message","time":1700000012000,"data":{"turn":1,"step":2,"message":{"id":"z2","source":{"provider":"gateway","model":"deepseek-pro"}},"usage":{"inputTokens":30,"outputTokens":15,"cacheReadTokens":40,"cacheWriteTokens":0}}}` + "\n"
	firstFrame := dshTestZstdFrame(t, []byte(firstText))
	secondFrame := dshTestZstdFrame(t, []byte(secondText))
	split := len(secondFrame) / 2
	initial := append(append([]byte(nil), firstFrame...), secondFrame[:split]...)
	if err := os.WriteFile(path, initial, 0o644); err != nil {
		t.Fatal(err)
	}

	collector := NewDSHCollector()
	ctx := Context{DeviceID: "device-1", Paths: Paths{DSH: root}}
	partial, err := collector.Collect(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(partial.Events) != 1 || partial.Events[0].RequestID != "dsh:z1" {
		t.Fatalf("expected only complete-frame event, got %+v", partial.Events)
	}
	if len(partial.Cursors) != 1 || partial.Cursors[0].Offset != int64(len(firstFrame)) {
		t.Fatalf("cursor crossed torn frame: %+v", partial.Cursors)
	}

	completed := append(append([]byte(nil), firstFrame...), secondFrame...)
	if err := os.WriteFile(path, completed, 0o644); err != nil {
		t.Fatal(err)
	}
	complete, err := collector.Collect(ctx, partial.Cursors)
	if err != nil {
		t.Fatal(err)
	}
	if len(complete.Events) != 1 || complete.Events[0].RequestID != "dsh:z2" {
		t.Fatalf("expected completed-frame event, got %+v", complete.Events)
	}
	if complete.Events[0].LatencyMS == nil || *complete.Events[0].LatencyMS != 2000 {
		t.Fatalf("step timing was not preserved across frames: %+v", complete.Events[0])
	}
}

func dshTestZstdFrame(t *testing.T, plain []byte) []byte {
	t.Helper()
	if len(plain) == 0 {
		return nil
	}
	if len(plain) > 0x7ffff0 {
		t.Fatal("test frame is too large for one raw block")
	}
	frame := make([]byte, 0, len(plain)+18)
	frame = append(frame, 0x28, 0xb5, 0x2f, 0xfd)
	frame = append(frame, 0xa0) // single segment, four-byte content size, no checksum
	var contentSize [4]byte
	binary.LittleEndian.PutUint32(contentSize[:], uint32(len(plain)))
	frame = append(frame, contentSize[:]...)
	header := uint32(len(plain))<<3 | 1 // last raw block
	frame = append(frame, byte(header), byte(header>>8), byte(header>>16))
	frame = append(frame, plain...)
	return frame
}
