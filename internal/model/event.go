package model

import (
	"crypto/sha256"
	"encoding/hex"
	"time"
)

type EventSource string

const (
	SourceProxy   EventSource = "proxy"
	SourceSession EventSource = "session"
	SourceImport  EventSource = "import"
	SourceAPI     EventSource = "api"
)

type TimingQuality string

const (
	TimingExact     TimingQuality = "exact"
	TimingEstimated TimingQuality = "estimated"
	TimingUnknown   TimingQuality = "unknown"
)

type CostSnapshot struct {
	InputCostUSD         string `json:"inputCostUsd"`
	OutputCostUSD        string `json:"outputCostUsd"`
	CacheReadCostUSD     string `json:"cacheReadCostUsd"`
	CacheCreationCostUSD string `json:"cacheCreationCostUsd"`
	TotalCostUSD         string `json:"totalCostUsd"`
	InputCostCNY         string `json:"inputCostCny"`
	OutputCostCNY        string `json:"outputCostCny"`
	CacheReadCostCNY     string `json:"cacheReadCostCny"`
	CacheCreationCostCNY string `json:"cacheCreationCostCny"`
	TotalCostCNY         string `json:"totalCostCny"`
	PricingVersion       string `json:"pricingVersion,omitempty"`
}

type UsageEvent struct {
	EventID              string         `json:"eventId"`
	DeviceID             string         `json:"deviceId"`
	Source               EventSource    `json:"source"`
	SourceApp            string         `json:"sourceApp"`
	SourceRecordID       string         `json:"sourceRecordId"`
	RequestID            string         `json:"requestId,omitempty"`
	SessionID            string         `json:"sessionId,omitempty"`
	ProviderID           string         `json:"providerId,omitempty"`
	ProviderName         string         `json:"providerName,omitempty"`
	Model                string         `json:"model"`
	ModelRaw             string         `json:"modelRaw,omitempty"`
	CanonicalModel       string         `json:"canonicalModel,omitempty"`
	RequestModel         string         `json:"requestModel,omitempty"`
	PricingModel         string         `json:"pricingModel,omitempty"`
	OccurredAt           time.Time      `json:"occurredAt"`
	IngestedAt           time.Time      `json:"ingestedAt"`
	Usage                TokenUsage     `json:"usage"`
	InputTokenSemantics  InputSemantics `json:"inputTokenSemantics"`
	LatencyMS            *uint64        `json:"latencyMs,omitempty"`
	FirstTokenMS         *uint64        `json:"firstTokenMs,omitempty"`
	DurationMS           *uint64        `json:"durationMs,omitempty"`
	GenerationDurationMS *uint64        `json:"generationDurationMs,omitempty"`
	SpeedTPS             *float64       `json:"speedTps,omitempty"`
	TimingQuality        TimingQuality  `json:"timingQuality"`
	StatusCode           *int           `json:"statusCode,omitempty"`
	ErrorMessage         string         `json:"errorMessage,omitempty"`
	IsStreaming          bool           `json:"isStreaming"`
	CostMultiplier       string         `json:"costMultiplier"`
	PricingVersion       string         `json:"pricingVersion,omitempty"`
	Cost                 CostSnapshot   `json:"cost"`
	Metadata             map[string]any `json:"metadata,omitempty"`
}

func NewUsageEvent(deviceID string, source EventSource, sourceApp, sourceRecordID, model string, usage TokenUsage, occurredAt time.Time) UsageEvent {
	now := time.Now().UTC()
	return UsageEvent{
		EventID:             DeterministicEventID(deviceID, string(source), sourceApp, sourceRecordID),
		DeviceID:            deviceID,
		Source:              source,
		SourceApp:           sourceApp,
		SourceRecordID:      sourceRecordID,
		Model:               model,
		ModelRaw:            model,
		CanonicalModel:      model,
		OccurredAt:          occurredAt.UTC(),
		IngestedAt:          now,
		Usage:               usage,
		InputTokenSemantics: DefaultSemanticsForApp(sourceApp),
		TimingQuality:       TimingUnknown,
		CostMultiplier:      "1",
		Cost: CostSnapshot{
			InputCostUSD: "0", OutputCostUSD: "0", CacheReadCostUSD: "0",
			CacheCreationCostUSD: "0", TotalCostUSD: "0",
			InputCostCNY: "0", OutputCostCNY: "0", CacheReadCostCNY: "0",
			CacheCreationCostCNY: "0", TotalCostCNY: "0",
		},
		Metadata: map[string]any{},
	}
}

func DeterministicEventID(parts ...string) string {
	hash := sha256.New()
	for _, part := range parts {
		hash.Write([]byte("token-mirror\x00"))
		hash.Write([]byte(part))
	}
	return hex.EncodeToString(hash.Sum(nil))
}
