package server

import (
	"embed"
	"encoding/json"
	"io/fs"
	"net/http"
	"strings"
	"time"
)

//go:embed web/*
var webFiles embed.FS

func WebFS() fs.FS { return webFiles }

func NewRouter(state *AppState) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/health", state.handleHealth)
	mux.HandleFunc("GET /api/v1/summary", state.handleSummary)
	mux.HandleFunc("GET /api/v1/trends", state.handleTrends)
	mux.HandleFunc("GET /api/v1/providers", state.handleProviders)
	mux.HandleFunc("GET /api/v1/models", state.handleModels)
	mux.HandleFunc("GET /api/v1/requests", state.handleRequests)
	mux.HandleFunc("GET /api/v1/requests/{event_id}", state.handleRequestDetail)
	mux.HandleFunc("GET /api/v1/devices", state.handleDevices)
	mux.HandleFunc("POST /api/v1/devices/heartbeat", state.handleHeartbeat)
	mux.HandleFunc("GET /api/v1/events", state.handleEvents)
	mux.HandleFunc("POST /api/v1/events/batch", state.handleIngestBatch)
	mux.HandleFunc("GET /api/v1/pricing", state.handlePricing)
	mux.HandleFunc("POST /api/v1/pricing/merge", state.handlePricingMerge)
	mux.HandleFunc("POST /api/v1/pricing/sync-models-dev", state.handleSyncModelsDev)
	mux.HandleFunc("POST /api/v1/pricing/recalculate", state.handleReprice)
	mux.HandleFunc("GET /api/v1/aliases", state.handleAliases)
	mux.HandleFunc("POST /api/v1/aliases", state.handleUpsertAlias)
	mux.HandleFunc("DELETE /api/v1/aliases/{alias_id}", state.handleDeleteAlias)
	mux.HandleFunc("GET /api/v1/sessions", state.handleSessions)
	mux.HandleFunc("GET /api/v1/sessions/{session_id}", state.handleSessionDetail)
	mux.HandleFunc("GET /api/v1/tasks", state.handleTasks)
	mux.HandleFunc("POST /api/v1/tasks/collect", state.handleCollectTask)
	mux.HandleFunc("POST /api/v1/tasks/reprice", state.handleRepriceTask)
	mux.HandleFunc("POST /api/v1/tasks/models-dev", state.handleModelsDevTask)
	mux.HandleFunc("POST /api/v1/tasks/import-cc-switch", state.handleImportCCSwitchTask)
	mux.HandleFunc("POST /api/v1/tasks/sync", state.handleSyncTask)
	mux.HandleFunc("POST /api/v1/tasks/pricing-import", state.handlePricingImportTask)
	mux.HandleFunc("POST /api/v1/tasks/pricing-merge", state.handlePricingMergeTask)
	mux.HandleFunc("POST /api/v1/tasks/aliases/upsert", state.handleAliasUpsertTask)
	mux.HandleFunc("POST /api/v1/tasks/aliases/delete", state.handleAliasDeleteTask)

	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.FS(webFiles))))
	mux.HandleFunc("GET /", state.handleIndex)
	return withMiddleware(state, mux)
}

func withMiddleware(state *AppState, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if state.Token != "" && strings.HasPrefix(r.URL.Path, "/api/") && r.URL.Path != "/api/v1/health" {
			provided := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			if provided != state.Token {
				writeError(w, http.StatusUnauthorized, "unauthorized", "invalid or missing API token")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func (s *AppState) handleIndex(w http.ResponseWriter, r *http.Request) {
	data, _ := webFiles.ReadFile("web/index.html")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(data)
}

func (s *AppState) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "version": s.Version, "now": time.Now().UTC()})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]string{"error": code, "message": message})
}
