package collectors

import (
	"database/sql"
	"fmt"
	"net/url"
	"time"

	"tokenanalyse/internal/model"

	_ "modernc.org/sqlite"
)

type CCSwitchImporter struct{}

func ImportCCSwitch(sourcePath, deviceID string, since int64) (Result, error) {
	dsn := "file:" + url.PathEscape(sourcePath) + "?mode=ro"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return Result{}, err
	}
	defer db.Close()
	rows, err := db.Query(`
		SELECT request_id, COALESCE(provider_id,''), app_type, COALESCE(model,'unknown'),
			COALESCE(request_model,''), COALESCE(pricing_model,''),
			COALESCE(input_tokens,0), COALESCE(output_tokens,0), COALESCE(cache_read_tokens,0),
			COALESCE(cache_creation_tokens,0), COALESCE(input_token_semantics,0),
			COALESCE(latency_ms,0), COALESCE(first_token_ms,0), COALESCE(status_code,0),
			COALESCE(session_id,''), COALESCE(is_streaming,0), COALESCE(cost_multiplier,'1'),
			COALESCE(created_at,0), COALESCE(data_source,'proxy')
		FROM proxy_request_logs WHERE created_at > ? ORDER BY created_at`, since)
	if err != nil {
		return Result{}, fmt.Errorf("read CC Switch database: %w", err)
	}
	defer rows.Close()
	result := Result{Metadata: map[string]any{"source": "cc-switch", "sourcePath": sourcePath}}
	maxTimestamp := since
	for rows.Next() {
		var requestID, providerID, appType, rawModel, requestModel, pricingModel, sessionID, multiplier, dataSource string
		var input, output, cacheRead, cacheCreation, semantics, latency, firstToken, status, streaming, createdAt int64
		if err := rows.Scan(&requestID, &providerID, &appType, &rawModel, &requestModel, &pricingModel,
			&input, &output, &cacheRead, &cacheCreation, &semantics, &latency, &firstToken,
			&status, &sessionID, &streaming, &multiplier, &createdAt, &dataSource); err != nil {
			return result, err
		}
		modelName := rawModel
		if modelName == "" {
			modelName = requestModel
		}
		if modelName == "" {
			modelName = pricingModel
		}
		if modelName == "" {
			modelName = "unknown"
		}
		usage := model.TokenUsage{InputTokens: uint64(maxInt64(input, 0)), OutputTokens: uint64(maxInt64(output, 0)),
			CacheReadTokens: uint64(maxInt64(cacheRead, 0)), CacheCreationTokens: uint64(maxInt64(cacheCreation, 0))}
		event := model.NewUsageEvent(deviceID, model.SourceImport, appType, requestID, modelName, usage, time.Unix(createdAt, 0).UTC())
		event.RequestID = requestID
		event.SessionID = sessionID
		event.ProviderID = providerID
		event.RequestModel = requestModel
		event.PricingModel = pricingModel
		event.InputTokenSemantics = model.InputSemantics(semantics)
		event.CostMultiplier = multiplier
		event.IsStreaming = streaming != 0
		if latency > 0 {
			value := uint64(latency)
			event.LatencyMS = &value
			event.DurationMS = &value
			event.TimingQuality = model.TimingExact
		}
		if firstToken > 0 {
			value := uint64(firstToken)
			event.FirstTokenMS = &value
		}
		if status > 0 {
			value := int(status)
			event.StatusCode = &value
		}
		event.Metadata["ccSwitchDataSource"] = dataSource
		result.Events = append(result.Events, event)
		if createdAt > maxTimestamp {
			maxTimestamp = createdAt
		}
	}
	if err := rows.Err(); err != nil {
		return result, err
	}
	result.FilesScanned = 1
	result.Metadata["maxTimestamp"] = maxTimestamp
	return result, nil
}

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
