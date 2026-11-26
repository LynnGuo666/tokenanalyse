package model

import "time"

type UsageSummary struct {
	TotalRequests            uint64  `json:"totalRequests"`
	TotalCost                string  `json:"totalCost"`
	TotalInputTokens         uint64  `json:"totalInputTokens"`
	TotalOutputTokens        uint64  `json:"totalOutputTokens"`
	TotalCacheCreationTokens uint64  `json:"totalCacheCreationTokens"`
	TotalCacheReadTokens     uint64  `json:"totalCacheReadTokens"`
	SuccessRate              float64 `json:"successRate"`
	RealTotalTokens          uint64  `json:"realTotalTokens"`
	CacheHitRate             float64 `json:"cacheHitRate"`
}

type DailyStats struct {
	Date                     string `json:"date"`
	RequestCount             uint64 `json:"requestCount"`
	TotalCost                string `json:"totalCost"`
	TotalTokens              uint64 `json:"totalTokens"`
	TotalInputTokens         uint64 `json:"totalInputTokens"`
	TotalOutputTokens        uint64 `json:"totalOutputTokens"`
	TotalCacheCreationTokens uint64 `json:"totalCacheCreationTokens"`
	TotalCacheReadTokens     uint64 `json:"totalCacheReadTokens"`
}

type ProviderStats struct {
	ProviderID   string  `json:"providerId"`
	ProviderName string  `json:"providerName,omitempty"`
	AppType      string  `json:"appType"`
	RequestCount uint64  `json:"requestCount"`
	TotalTokens  uint64  `json:"totalTokens"`
	TotalCost    string  `json:"totalCost"`
	SuccessRate  float64 `json:"successRate"`
	AvgLatencyMS uint64  `json:"avgLatencyMs"`
}

type ModelStats struct {
	Model        string  `json:"model"`
	RequestCount uint64  `json:"requestCount"`
	TotalTokens  uint64  `json:"totalTokens"`
	TotalCost    string  `json:"totalCost"`
	SuccessRate  float64 `json:"successRate"`
	AvgLatencyMS uint64  `json:"avgLatencyMs"`
}

type UsageFilters struct {
	From         *time.Time
	To           *time.Time
	DeviceID     string
	AppType      string
	ProviderID   string
	ProviderName string
	Model        string
}
