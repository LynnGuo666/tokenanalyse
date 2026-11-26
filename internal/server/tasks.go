package server

import (
	"encoding/json"
	"net/http"

	"tokenanalyse/internal/model"
	"tokenanalyse/internal/pricing"
	"tokenanalyse/internal/storage"
	syncpkg "tokenanalyse/internal/sync"
	"tokenanalyse/internal/task"
)

func (s *AppState) handleTasks(w http.ResponseWriter, r *http.Request) {
	manager := s.TaskManager()
	writeJSON(w, http.StatusOK, map[string]any{
		"items":       manager.List(),
		"activeCount": manager.ActiveCount(),
	})
}

func (s *AppState) handleCollectTask(w http.ResponseWriter, r *http.Request) {
	started, err := s.TaskManager().StartCollect()
	writeTaskResult(w, started, err)
}

func (s *AppState) handleRepriceTask(w http.ResponseWriter, r *http.Request) {
	started, err := s.TaskManager().StartReprice()
	writeTaskResult(w, started, err)
}

func (s *AppState) handleModelsDevTask(w http.ResponseWriter, r *http.Request) {
	started, err := s.TaskManager().StartModelsDev()
	writeTaskResult(w, started, err)
}

func (s *AppState) handleImportCCSwitchTask(w http.ResponseWriter, r *http.Request) {
	var payload struct {
		SourcePath string `json:"sourcePath"`
	}
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	if payload.SourcePath == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "sourcePath is required")
		return
	}
	started, err := s.TaskManager().StartImportCCSwitch(payload.SourcePath)
	writeTaskResult(w, started, err)
}

func (s *AppState) handleSyncTask(w http.ResponseWriter, r *http.Request) {
	var config syncpkg.Config
	if err := json.NewDecoder(r.Body).Decode(&config); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	if config.Mode == syncpkg.ModeDisabled {
		writeError(w, http.StatusBadRequest, "bad_request", "sync mode is disabled")
		return
	}
	started, err := s.TaskManager().StartSync(config)
	writeTaskResult(w, started, err)
}

func (s *AppState) handlePricingImportTask(w http.ResponseWriter, r *http.Request) {
	var snapshot storage.PricingSnapshot
	if err := json.NewDecoder(r.Body).Decode(&snapshot); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	started, err := s.TaskManager().StartPricingImport(snapshot)
	writeTaskResult(w, started, err)
}

func (s *AppState) handlePricingMergeTask(w http.ResponseWriter, r *http.Request) {
	var payload struct {
		Snapshot storage.PricingSnapshot `json:"snapshot"`
		Policy   string                  `json:"policy"`
	}
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	started, err := s.TaskManager().StartPricingMerge(payload.Snapshot, pricing.MergePolicy(payload.Policy))
	writeTaskResult(w, started, err)
}

func (s *AppState) handleAliasUpsertTask(w http.ResponseWriter, r *http.Request) {
	var alias model.ModelAlias
	if err := json.NewDecoder(r.Body).Decode(&alias); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	started, err := s.TaskManager().StartAliasUpsert(alias)
	writeTaskResult(w, started, err)
}

func (s *AppState) handleAliasDeleteTask(w http.ResponseWriter, r *http.Request) {
	var payload struct {
		AliasID string `json:"aliasId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	if payload.AliasID == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "aliasId is required")
		return
	}
	started, err := s.TaskManager().StartAliasDelete(payload.AliasID)
	writeTaskResult(w, started, err)
}

func writeTaskResult(w http.ResponseWriter, started task.Task, err error) {
	if err != nil {
		writeError(w, http.StatusInternalServerError, "task_start_failed", err.Error())
		return
	}
	status := http.StatusAccepted
	if started.Mode == task.ModeForeground {
		status = http.StatusOK
	}
	writeJSON(w, status, started)
}
