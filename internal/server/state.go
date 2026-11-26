package server

import (
	"sync"

	"tokenanalyse/internal/storage"
	"tokenanalyse/internal/task"
)

type AppState struct {
	DB      *storage.DB
	Token   string
	Version string

	taskOnce sync.Once
	TaskMgr  *task.Manager
}

func (s *AppState) TaskManager() *task.Manager {
	s.taskOnce.Do(func() { s.TaskMgr = task.NewManager(s.DB) })
	return s.TaskMgr
}
