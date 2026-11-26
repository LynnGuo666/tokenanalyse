package task

import (
	"context"
	"fmt"
	"sync"
	"time"

	"tokenanalyse/internal/collectors"
	"tokenanalyse/internal/model"
	"tokenanalyse/internal/pricing"
	"tokenanalyse/internal/storage"
	syncpkg "tokenanalyse/internal/sync"
)

type Status string

const (
	StatusRunning   Status = "running"
	StatusCompleted Status = "completed"
	StatusFailed    Status = "failed"
)

type Mode string

const (
	ModeForeground Mode = "foreground"
	ModeBackground Mode = "background"
)

type Task struct {
	ID         string     `json:"id"`
	Type       string     `json:"type"`
	Title      string     `json:"title"`
	Mode       Mode       `json:"mode"`
	Status     Status     `json:"status"`
	Progress   int        `json:"progress"`
	Message    string     `json:"message,omitempty"`
	Error      string     `json:"error,omitempty"`
	CreatedAt  time.Time  `json:"createdAt"`
	StartedAt  *time.Time `json:"startedAt,omitempty"`
	FinishedAt *time.Time `json:"finishedAt,omitempty"`
}

type Manager struct {
	db    *storage.DB
	mu    sync.RWMutex
	tasks map[string]*Task
	order []string
}

func NewManager(db *storage.DB) *Manager {
	return &Manager{db: db, tasks: map[string]*Task{}}
}

func (m *Manager) StartReprice() (Task, error) {
	return m.start("reprice", "重算费用快照", ModeBackground, func() error {
		_, err := m.db.RepriceAll()
		return err
	})
}

func (m *Manager) StartModelsDev() (Task, error) {
	return m.start("models_dev", "同步 models.dev 价格", ModeBackground, func() error {
		snapshot, err := pricing.FetchModelsDev(context.Background())
		if err != nil {
			return err
		}
		return m.db.ApplyPricingSnapshot(storage.PricingSnapshot{VersionID: snapshot.VersionID, Source: snapshot.Source,
			CreatedAt: snapshot.CreatedAt, Models: snapshot.Models, Aliases: snapshot.Aliases, ContentHash: snapshot.ContentHash}, true)
	})
}

func (m *Manager) StartCollect() (Task, error) {
	return m.start("collect", "采集本机 CLI 用量", ModeBackground, func() error {
		deviceID, err := m.firstDeviceID()
		if err != nil {
			return err
		}
		ctx := collectors.DetectContext(deviceID)
		cursors, _ := m.db.LoadCursors()
		result := collectors.BuiltinRegistry().CollectAll(ctx, cursors)
		if len(result.Events) > 0 {
			if _, err := m.db.InsertEvents(result.Events, true); err != nil {
				return err
			}
		}
		if len(result.Cursors) > 0 {
			return m.db.SaveCursors(result.Cursors)
		}
		return nil
	})
}

func (m *Manager) StartSync(config syncpkg.Config) (Task, error) {
	return m.start("sync", "同步设备数据", ModeBackground, func() error {
		deviceID, err := m.firstDeviceID()
		if err != nil {
			return err
		}
		_, err = syncpkg.NewRunner(m.db, config, deviceID).RunOnce(context.Background())
		return err
	})
}

func (m *Manager) StartImportCCSwitch(sourcePath string) (Task, error) {
	return m.start("import_cc_switch", "导入 CC Switch 数据", ModeBackground, func() error {
		deviceID, err := m.firstDeviceID()
		if err != nil {
			return err
		}
		result, err := collectors.ImportCCSwitch(sourcePath, deviceID, 0)
		if err != nil {
			return err
		}
		if len(result.Events) > 0 {
			_, err = m.db.InsertEvents(result.Events, false)
		}
		return err
	})
}

func (m *Manager) StartPricingImport(snapshot storage.PricingSnapshot) (Task, error) {
	return m.start("pricing_import", "导入自定义价格", ModeForeground, func() error {
		return m.db.ApplyPricingSnapshot(snapshot, true)
	})
}

func (m *Manager) StartPricingMerge(snapshot storage.PricingSnapshot, policy pricing.MergePolicy) (Task, error) {
	return m.start("pricing_merge", "合并价格表", ModeForeground, func() error {
		_, _, err := m.db.MergePricingSnapshot(snapshot, policy)
		return err
	})
}

func (m *Manager) StartAliasUpsert(alias model.ModelAlias) (Task, error) {
	return m.start("alias_upsert", "保存模型别名", ModeForeground, func() error {
		return m.db.UpsertModelAlias(alias)
	})
}

func (m *Manager) StartAliasDelete(aliasID string) (Task, error) {
	return m.start("alias_delete", "删除模型别名", ModeForeground, func() error {
		return m.db.DeleteModelAlias(aliasID)
	})
}

func (m *Manager) start(kind, title string, mode Mode, run func() error) (Task, error) {
	m.mu.Lock()
	for _, id := range m.order {
		task := m.tasks[id]
		if task.Type == kind && task.Status == StatusRunning {
			m.mu.Unlock()
			return *task, nil
		}
	}
	now := time.Now().UTC()
	id := fmt.Sprintf("%s-%d", kind, now.UnixNano())
	task := &Task{ID: id, Type: kind, Title: title, Mode: mode, Status: StatusRunning, CreatedAt: now, StartedAt: &now}
	m.tasks[id] = task
	m.order = append([]string{id}, m.order...)
	if mode == ModeForeground {
		m.mu.Unlock()
		m.execute(id, run)
		m.mu.RLock()
		defer m.mu.RUnlock()
		return *m.tasks[id], nil
	}
	m.mu.Unlock()
	go m.execute(id, run)
	m.mu.RLock()
	defer m.mu.RUnlock()
	return *m.tasks[id], nil
}

func (m *Manager) execute(id string, run func() error) {
	err := run()
	finished := time.Now().UTC()
	m.mu.Lock()
	defer m.mu.Unlock()
	task := m.tasks[id]
	if task == nil {
		return
	}
	task.FinishedAt = &finished
	if err != nil {
		task.Status = StatusFailed
		task.Error = err.Error()
		task.Message = "任务失败"
	} else {
		task.Status = StatusCompleted
		task.Progress = 100
		task.Message = "任务完成"
	}
}

func (m *Manager) List() []Task {
	m.mu.RLock()
	defer m.mu.RUnlock()
	result := make([]Task, 0, len(m.order))
	for _, id := range m.order {
		if task := m.tasks[id]; task != nil {
			result = append(result, *task)
		}
	}
	return result
}

func (m *Manager) Get(id string) (Task, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	task, ok := m.tasks[id]
	if !ok {
		return Task{}, false
	}
	return *task, true
}

func (m *Manager) ActiveCount() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	count := 0
	for _, task := range m.tasks {
		if task.Status == StatusRunning {
			count++
		}
	}
	return count
}

func (m *Manager) firstDeviceID() (string, error) {
	devices, err := m.db.ListDevices()
	if err != nil {
		return "", err
	}
	if len(devices) == 0 {
		return "", fmt.Errorf("no registered device")
	}
	return devices[0].DeviceID, nil
}
