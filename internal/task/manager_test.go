package task

import (
	"testing"
	"time"

	"tokenanalyse/internal/model"
	"tokenanalyse/internal/storage"

	"github.com/shopspring/decimal"
)

func TestTaskManagerTracksForegroundAndBackgroundTasks(t *testing.T) {
	db, err := storage.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	manager := NewManager(db)

	snapshot := storage.PricingSnapshot{VersionID: "v1", Source: "test", CreatedAt: time.Now().UTC(),
		ContentHash: "hash", Models: []model.ModelPricing{{ModelID: "m", DisplayName: "M",
			InputCostPerMillion: decimal.NewFromInt(1), OutputCostPerMillion: decimal.NewFromInt(1),
			CacheReadCostPerMillion: decimal.Zero, CacheCreationCostPerMillion: decimal.Zero,
			Currency: model.CurrencyUSD, Source: "test"}}}
	foreground, err := manager.StartPricingImport(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if foreground.Status != StatusCompleted {
		t.Fatalf("foreground task not completed: %+v", foreground)
	}

	background, err := manager.StartReprice()
	if err != nil {
		t.Fatal(err)
	}
	if background.Mode != ModeBackground {
		t.Fatalf("expected background task: %+v", background)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		current, _ := manager.Get(background.ID)
		if current.Status != StatusRunning {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("background task did not finish")
		}
		time.Sleep(10 * time.Millisecond)
	}
	current, _ := manager.Get(background.ID)
	if current.Status != StatusCompleted {
		t.Fatalf("background task failed: %+v", current)
	}
	if len(manager.List()) != 2 {
		t.Fatalf("expected 2 tasks, got %d", len(manager.List()))
	}
}
