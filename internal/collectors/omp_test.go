package collectors

import (
	"path/filepath"
	"testing"
)

func TestOMPCollectorParsesTitleAssistantAndModelUsage(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "sessions", "project", "omp-session.jsonl")
	writePiTestSession(t, path,
		`{"type":"title","v":1,"title":"Fix the parser","updatedAt":"2026-10-29T13:17:48.750Z","pad":""}`,
		`{"type":"session","version":3,"id":"omp-session","timestamp":"2026-10-29T13:16:32.926Z","cwd":"/work"}`,
		`{"type":"message","id":"call-1","timestamp":"2026-10-29T13:17:42.984Z","message":{"role":"assistant","provider":"workbuddy","model":"deepseek-v4.1-flash","duration":6744.172099999996,"ttft":6192.651100000003,"usage":{"input":282,"output":194,"cacheRead":12800,"cacheWrite":0,"reasoningTokens":41,"cost":{"total":0.0003942}},"stopReason":"toolUse"}}`,
		`{"type":"model_usage","id":"judge-1","timestamp":"2026-10-29T13:26:53.160Z","provider":"workbuddy","model":"deepseek-v4.1-flash","usage":{"input":312,"output":110,"cacheRead":2432,"cacheWrite":0,"totalTokens":2854,"cost":{"total":0.000240192}},"stopReason":"toolUse"}`,
	)
	ctx := Context{DeviceID: "device-omp", Paths: Paths{OMP: root}}

	result, err := OMPCollector{}.Collect(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Events) != 2 {
		t.Fatalf("expected assistant and model_usage events, got %d (errors: %v)", len(result.Events), result.Errors)
	}
	assistant := result.Events[0]
	if assistant.SourceApp != "omp" || assistant.SourceRecordID != "omp-session:call-1" {
		t.Fatalf("unexpected OMP identity: %+v", assistant)
	}
	if assistant.Usage.InputTokens != 282 || assistant.Usage.OutputTokens != 235 || assistant.Usage.ReasoningTokens != 41 {
		t.Fatalf("reasoning was not included in output: %+v", assistant.Usage)
	}
	if assistant.LatencyMS == nil || *assistant.LatencyMS != 6744 || assistant.FirstTokenMS == nil || *assistant.FirstTokenMS != 6193 {
		t.Fatalf("unexpected OMP timing: %+v", assistant)
	}
	if assistant.GenerationDurationMS == nil || *assistant.GenerationDurationMS != 551 {
		t.Fatalf("unexpected generation duration: %v", assistant.GenerationDurationMS)
	}
	modelUsage := result.Events[1]
	if modelUsage.SourceRecordID != "omp-session:judge-1" || modelUsage.Usage.InputTokens != 312 || modelUsage.ProviderID != "workbuddy" {
		t.Fatalf("model_usage carrier was not attributed: %+v", modelUsage)
	}
}

func TestOMPCollectorSharedRootStaysSeparateFromPi(t *testing.T) {
	root := t.TempDir()
	writePiTestSession(t, filepath.Join(root, "pi.jsonl"),
		`{"type":"session","id":"pi-session","timestamp":"2023-11-14T22:13:20Z"}`,
		`{"type":"message","id":"pi-call","timestamp":"2023-11-14T22:13:21Z","message":{"role":"assistant","provider":"pi-provider","model":"pi-model","usage":{"input":10,"output":2,"cacheRead":0,"cacheWrite":0},"stopReason":"stop"}}`,
	)
	writePiTestSession(t, filepath.Join(root, "omp.jsonl"),
		`{"type":"title","v":1,"title":"OMP","pad":""}`,
		`{"type":"session","id":"omp-session","timestamp":"2023-11-14T22:13:20Z"}`,
		`{"type":"message","id":"omp-call","timestamp":"2023-11-14T22:13:21Z","message":{"role":"assistant","provider":"omp-provider","model":"omp-model","usage":{"input":20,"output":3,"cacheRead":0,"cacheWrite":0},"stopReason":"stop"}}`,
	)
	ctx := Context{DeviceID: "device-shared", Paths: Paths{Pi: root, OMP: root}}

	piResult, err := PiCollector{}.Collect(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(piResult.Events) != 1 || piResult.Events[0].SourceApp != "pi" || piResult.Events[0].SourceRecordID != "pi-session:pi-call" {
		t.Fatalf("Pi absorbed an OMP session: %+v", piResult.Events)
	}
	ompResult, err := OMPCollector{}.Collect(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(ompResult.Events) != 1 || ompResult.Events[0].SourceApp != "omp" || ompResult.Events[0].SourceRecordID != "omp-session:omp-call" {
		t.Fatalf("OMP absorbed a Pi session: %+v", ompResult.Events)
	}
}
