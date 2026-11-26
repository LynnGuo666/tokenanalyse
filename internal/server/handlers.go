package server

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"tokenanalyse/internal/model"
	"tokenanalyse/internal/pricing"
	"tokenanalyse/internal/storage"
	"tokenanalyse/internal/sync"
)

type requestLogView struct {
	storage.RequestLogItem
	DeviceName            string   `json:"deviceName,omitempty"`
	InputTokens           uint64   `json:"inputTokens"`
	OutputTokens          uint64   `json:"outputTokens"`
	ReasoningTokens       uint64   `json:"reasoningTokens"`
	CacheReadTokens       uint64   `json:"cacheReadTokens"`
	CacheCreationTokens   uint64   `json:"cacheCreationTokens"`
	CacheCreation5MTokens uint64   `json:"cacheCreation5mTokens"`
	CacheCreation1HTokens uint64   `json:"cacheCreation1hTokens"`
	TotalCostUSD          string   `json:"totalCostUsd"`
	TotalCostCNY          string   `json:"totalCostCny"`
	SpeedTPS              *float64 `json:"speedTps,omitempty"`
}

type deviceView struct {
	model.Device
	Summary      model.UsageSummary `json:"summary"`
	TotalCostCNY string             `json:"totalCostCny"`
}

func parseFilters(r *http.Request) model.UsageFilters {
	query := r.URL.Query()
	return model.UsageFilters{
		From:         parseTimeParam(query.Get("from")),
		To:           parseTimeParam(query.Get("to")),
		DeviceID:     query.Get("deviceId"),
		AppType:      query.Get("appType"),
		ProviderID:   query.Get("providerId"),
		ProviderName: query.Get("providerName"),
		Model:        query.Get("model"),
		SessionID:    query.Get("sessionId"),
	}
}

func parseTimeParam(value string) *time.Time {
	if value == "" {
		return nil
	}
	if unix, err := strconv.ParseInt(value, 10, 64); err == nil {
		parsed := time.Unix(unix, 0).UTC()
		return &parsed
	}
	if parsed, err := time.Parse(time.RFC3339Nano, value); err == nil {
		parsed = parsed.UTC()
		return &parsed
	}
	return nil
}

func (s *AppState) handleSummary(w http.ResponseWriter, r *http.Request) {
	filters := parseFilters(r)
	summary, err := s.DB.GetSummary(filters)
	if err != nil {
		writeError(w, 500, "storage_error", err.Error())
		return
	}
	devices, _ := s.DB.ListDevices()
	writeJSON(w, 200, map[string]any{"summary": summary, "devices": devices})
}

func (s *AppState) handleTrends(w http.ResponseWriter, r *http.Request) {
	filters := parseFilters(r)
	bucket := r.URL.Query().Get("bucket")
	if bucket == "" {
		bucket = "day"
	}
	points, err := s.DB.GetDailyTrends(filters, bucket)
	if err != nil {
		writeError(w, 500, "storage_error", err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"bucket": bucket, "points": points})
}

func (s *AppState) handleProviders(w http.ResponseWriter, r *http.Request) {
	items, err := s.DB.GetProviderStats(parseFilters(r))
	if err != nil {
		writeError(w, 500, "storage_error", err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"items": items})
}

func (s *AppState) handleModels(w http.ResponseWriter, r *http.Request) {
	items, err := s.DB.GetModelStats(parseFilters(r))
	if err != nil {
		writeError(w, 500, "storage_error", err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"items": items})
}

func (s *AppState) handleRequests(w http.ResponseWriter, r *http.Request) {
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	pageSize, _ := strconv.Atoi(r.URL.Query().Get("pageSize"))
	result, err := s.DB.GetRequestLogs(parseFilters(r), page, pageSize)
	if err != nil {
		writeError(w, 500, "storage_error", err.Error())
		return
	}
	writeJSON(w, 200, requestLogResponse(result))
}

func (s *AppState) handleRequestDetail(w http.ResponseWriter, r *http.Request) {
	eventID := r.PathValue("event_id")
	item, err := s.DB.GetRequestDetail(eventID)
	if err != nil {
		writeError(w, 404, "not_found", err.Error())
		return
	}
	writeJSON(w, 200, toRequestLogView(item))
}

func (s *AppState) handleDevices(w http.ResponseWriter, r *http.Request) {
	stats, err := s.DB.GetDeviceStats(parseFilters(r))
	if err != nil {
		writeError(w, 500, "storage_error", err.Error())
		return
	}
	views := make([]deviceView, 0, len(stats))
	for _, stat := range stats {
		views = append(views, deviceView{Device: stat.Device, Summary: stat.Summary, TotalCostCNY: stat.TotalCostCNY})
	}
	writeJSON(w, 200, map[string]any{"devices": views})
}

func (s *AppState) handleHeartbeat(w http.ResponseWriter, r *http.Request) {
	var payload sync.HeartbeatRequest
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		writeError(w, 400, "bad_request", err.Error())
		return
	}
	if err := s.DB.Heartbeat(payload.Device); err != nil {
		writeError(w, 500, "storage_error", err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *AppState) handleIngestBatch(w http.ResponseWriter, r *http.Request) {
	var batch sync.EventBatch
	if err := json.NewDecoder(r.Body).Decode(&batch); err != nil {
		writeError(w, 400, "bad_request", err.Error())
		return
	}
	for _, event := range batch.Events {
		if event.DeviceID != batch.DeviceID {
			writeError(w, 400, "bad_request", "batch device_id mismatch")
			return
		}
	}
	result, err := s.DB.InsertEvents(batch.Events, true)
	if err != nil {
		writeError(w, 500, "storage_error", err.Error())
		return
	}
	writeJSON(w, 200, sync.BatchAck{BatchID: batch.BatchID, Accepted: result.Inserted,
		Duplicates: result.Skipped, Rejected: len(batch.Events) - result.Inserted - result.Skipped, ServerCursor: batch.Cursor})
}

func (s *AppState) handleEvents(w http.ResponseWriter, r *http.Request) {
	cursor, _ := strconv.ParseInt(r.URL.Query().Get("cursor"), 10, 64)
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	events, next, err := s.DB.GetEventsAfterRowID(cursor, limit)
	if err != nil {
		writeError(w, 500, "storage_error", err.Error())
		return
	}
	writeJSON(w, 200, sync.EventsResponse{Events: events, NextCursor: strconv.FormatInt(next, 10)})
}

func (s *AppState) handlePricing(w http.ResponseWriter, r *http.Request) {
	snapshot, err := s.DB.ActivePricingSnapshot()
	if err != nil {
		writeError(w, 500, "storage_error", err.Error())
		return
	}
	writeJSON(w, 200, snapshot)
}

func (s *AppState) handlePricingMerge(w http.ResponseWriter, r *http.Request) {
	var payload struct {
		Snapshot storage.PricingSnapshot `json:"snapshot"`
		Policy   string                  `json:"policy"`
	}
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		writeError(w, 400, "bad_request", err.Error())
		return
	}
	policy := pricing.MergePolicy(payload.Policy)
	if policy == "" {
		policy = pricing.MergePreferRemote
	}
	merged, conflicts, err := s.DB.MergePricingSnapshot(payload.Snapshot, policy)
	if err != nil {
		writeError(w, 500, "storage_error", err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"snapshot": merged, "conflicts": conflicts})
}

func (s *AppState) handleReprice(w http.ResponseWriter, r *http.Request) {
	count, err := s.DB.RepriceAll()
	if err != nil {
		writeError(w, 500, "storage_error", err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"repriced": count})
}

func (s *AppState) handleAliases(w http.ResponseWriter, r *http.Request) {
	aliases, err := s.DB.ListModelAliases()
	if err != nil {
		writeError(w, 500, "storage_error", err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"items": aliases})
}

func (s *AppState) handleUpsertAlias(w http.ResponseWriter, r *http.Request) {
	var alias model.ModelAlias
	if err := json.NewDecoder(r.Body).Decode(&alias); err != nil {
		writeError(w, 400, "bad_request", err.Error())
		return
	}
	if alias.AliasPattern == "" || alias.CanonicalModelID == "" {
		writeError(w, 400, "bad_request", "aliasPattern and canonicalModelId are required")
		return
	}
	if alias.MatchType == "" {
		alias.MatchType = "exact"
	}
	if err := s.DB.UpsertModelAlias(alias); err != nil {
		writeError(w, 500, "storage_error", err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *AppState) handleDeleteAlias(w http.ResponseWriter, r *http.Request) {
	if err := s.DB.DeleteModelAlias(r.PathValue("alias_id")); err != nil {
		writeError(w, 500, "storage_error", err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *AppState) handleSessions(w http.ResponseWriter, r *http.Request) {
	items, err := s.DB.GetSessionSummaries(parseFilters(r))
	if err != nil {
		writeError(w, 500, "storage_error", err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"items": items})
}

func (s *AppState) handleSessionDetail(w http.ResponseWriter, r *http.Request) {
	sessionID := r.PathValue("session_id")
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	pageSize, _ := strconv.Atoi(r.URL.Query().Get("pageSize"))
	result, err := s.DB.GetSessionRequests(parseFilters(r), sessionID, page, pageSize)
	if err != nil {
		writeError(w, 500, "storage_error", err.Error())
		return
	}
	writeJSON(w, 200, requestLogResponse(result))
}

func requestLogResponse(result storage.RequestLogPage) map[string]any {
	items := make([]requestLogView, 0, len(result.Items))
	for _, item := range result.Items {
		items = append(items, toRequestLogView(item))
	}
	return map[string]any{"items": items, "total": result.Total, "page": result.Page,
		"pageSize": result.PageSize, "totalPages": result.TotalPages}
}

func toRequestLogView(item storage.RequestLogItem) requestLogView {
	return requestLogView{RequestLogItem: item, DeviceName: item.DeviceName,
		InputTokens: item.Usage.InputTokens, OutputTokens: item.Usage.OutputTokens,
		ReasoningTokens: item.Usage.ReasoningTokens, CacheReadTokens: item.Usage.CacheReadTokens,
		CacheCreationTokens:   item.Usage.CacheCreationTokens,
		CacheCreation5MTokens: item.Usage.CacheCreation5MTokens,
		CacheCreation1HTokens: item.Usage.CacheCreation1HTokens,
		TotalCostUSD:          item.TotalCostUSD, TotalCostCNY: item.TotalCostCNY, SpeedTPS: item.SpeedTPS}
}
