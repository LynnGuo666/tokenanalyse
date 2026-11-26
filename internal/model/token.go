package model

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// InputSemantics describes whether a provider's input token count includes
// cache reads/writes.
type InputSemantics int

const (
	SemanticsLegacy InputSemantics = iota
	SemanticsTotal
	SemanticsFresh
)

func (s InputSemantics) String() string {
	switch s {
	case SemanticsTotal:
		return "total"
	case SemanticsFresh:
		return "fresh"
	default:
		return "legacy"
	}
}

func ParseInputSemantics(value string) InputSemantics {
	switch strings.ToLower(value) {
	case "total", "inclusive":
		return SemanticsTotal
	case "fresh":
		return SemanticsFresh
	default:
		return SemanticsLegacy
	}
}

func DefaultSemanticsForApp(appType string) InputSemantics {
	switch appType {
	case "codex", "gemini", "grokbuild":
		return SemanticsTotal
	default:
		return SemanticsFresh
	}
}

func (s InputSemantics) FreshInput(appType string, input, cacheRead, cacheWrite uint64) uint64 {
	switch s {
	case SemanticsFresh:
		return input
	case SemanticsTotal:
		if appType == "codex" || appType == "gemini" || appType == "grokbuild" {
			return subtract(subtract(input, cacheRead), cacheWrite)
		}
		return subtract(input, cacheRead)
	default:
		return subtract(input, cacheRead)
	}
}

// TokenUsage is the normalized usage for one model request.
type TokenUsage struct {
	InputTokens           uint64 `json:"inputTokens"`
	OutputTokens          uint64 `json:"outputTokens"`
	ReasoningTokens       uint64 `json:"reasoningTokens"`
	CacheReadTokens       uint64 `json:"cacheReadTokens"`
	CacheCreationTokens   uint64 `json:"cacheCreationTokens"`
	CacheCreation5MTokens uint64 `json:"cacheCreation5mTokens"`
	CacheCreation1HTokens uint64 `json:"cacheCreation1hTokens"`
	Model                 string `json:"model,omitempty"`
	MessageID             string `json:"messageId,omitempty"`
}

func (u TokenUsage) HasBillableTokens() bool {
	return u.InputTokens > 0 || u.OutputTokens > 0 || u.CacheReadTokens > 0 || u.CacheCreationTokens > 0
}

func (u TokenUsage) FreshInput(appType string, semantics InputSemantics) uint64 {
	return semantics.FreshInput(appType, u.InputTokens, u.CacheReadTokens, u.CacheCreationTokens)
}

func (u TokenUsage) RealTotal(appType string, semantics InputSemantics) uint64 {
	return u.FreshInput(appType, semantics) + u.OutputTokens + u.CacheCreationTokens + u.CacheReadTokens
}

func (u TokenUsage) CacheHitRate(appType string, semantics InputSemantics) float64 {
	denominator := u.FreshInput(appType, semantics) + u.CacheCreationTokens + u.CacheReadTokens
	if denominator == 0 {
		return 0
	}
	return float64(u.CacheReadTokens) / float64(denominator)
}

func FromClaudeResponse(body map[string]any) (TokenUsage, bool) {
	usage, ok := body["usage"].(map[string]any)
	if !ok {
		return TokenUsage{}, false
	}
	input, ok := asUint64(usage["input_tokens"])
	if !ok {
		return TokenUsage{}, false
	}
	output, ok := asUint64(usage["output_tokens"])
	if !ok {
		return TokenUsage{}, false
	}
	cacheCreation1h := uint64(0)
	if creation, ok := usage["cache_creation"].(map[string]any); ok {
		cacheCreation1h, _ = asUint64(creation["ephemeral_1h_input_tokens"])
	}
	return TokenUsage{
		InputTokens:           input,
		OutputTokens:          output,
		CacheReadTokens:       valueUint64(usage["cache_read_input_tokens"]),
		CacheCreationTokens:   valueUint64(usage["cache_creation_input_tokens"]),
		CacheCreation1HTokens: cacheCreation1h,
		Model:                 valueString(body["model"]),
		MessageID:             valueString(body["id"]),
	}, true
}

func FromOpenAIResponse(body map[string]any) (TokenUsage, bool) {
	body = unwrapChatCompletionEnvelope(body)
	usage, ok := body["usage"].(map[string]any)
	if !ok {
		return TokenUsage{}, false
	}
	input, ok := asUint64(usage["prompt_tokens"])
	if !ok {
		return TokenUsage{}, false
	}
	output, ok := asUint64(usage["completion_tokens"])
	if !ok {
		return TokenUsage{}, false
	}
	return TokenUsage{
		InputTokens:         input,
		OutputTokens:        output,
		CacheReadTokens:     openAICacheReadTokens(usage),
		CacheCreationTokens: openAICacheWriteTokens(usage),
		Model:               valueString(body["model"]),
		MessageID:           valueString(body["id"]),
	}, true
}

func FromCodexResponse(body map[string]any) (TokenUsage, bool) {
	usage, ok := body["usage"].(map[string]any)
	if !ok {
		return TokenUsage{}, false
	}
	input, ok := asUint64(usage["input_tokens"])
	if !ok {
		return TokenUsage{}, false
	}
	output, ok := asUint64(usage["output_tokens"])
	if !ok {
		return TokenUsage{}, false
	}
	return TokenUsage{
		InputTokens:         input,
		OutputTokens:        output,
		CacheReadTokens:     openAICacheReadTokens(usage),
		CacheCreationTokens: openAICacheWriteTokens(usage),
		Model:               valueString(body["model"]),
		MessageID:           valueString(body["id"]),
	}, true
}

func FromGeminiResponse(body map[string]any) (TokenUsage, bool) {
	usage, ok := body["usageMetadata"].(map[string]any)
	if !ok {
		return TokenUsage{}, false
	}
	input, ok := asUint64(usage["promptTokenCount"])
	if !ok {
		return TokenUsage{}, false
	}
	return TokenUsage{
		InputTokens:     input,
		OutputTokens:    valueUint64(usage["candidatesTokenCount"]),
		CacheReadTokens: valueUint64(usage["cachedContentTokenCount"]),
		Model:           valueString(body["modelVersion"]),
		MessageID:       valueString(body["responseId"]),
	}, true
}

func asUint64(value any) (uint64, bool) {
	switch typed := value.(type) {
	case float64:
		if typed < 0 {
			return 0, false
		}
		return uint64(typed), true
	case json.Number:
		parsed, err := strconv.ParseUint(typed.String(), 10, 64)
		return parsed, err == nil
	case int:
		if typed < 0 {
			return 0, false
		}
		return uint64(typed), true
	case int64:
		if typed < 0 {
			return 0, false
		}
		return uint64(typed), true
	case uint64:
		return typed, true
	case string:
		parsed, err := strconv.ParseUint(strings.TrimSpace(typed), 10, 64)
		return parsed, err == nil
	default:
		return 0, false
	}
}

func valueUint64(value any) uint64 {
	parsed, _ := asUint64(value)
	return parsed
}

func valueString(value any) string {
	if value == nil {
		return ""
	}
	if typed, ok := value.(string); ok {
		return typed
	}
	return fmt.Sprint(value)
}

func subtract(a, b uint64) uint64 {
	if b >= a {
		return 0
	}
	return a - b
}

func openAICacheReadTokens(usage map[string]any) uint64 {
	candidates := []any{
		usage["cache_read_input_tokens"],
		nestedValue(usage, "input_tokens_details", "cached_tokens"),
		nestedValue(usage, "prompt_tokens_details", "cached_tokens"),
		usage["prompt_cache_hit_tokens"],
	}
	for _, candidate := range candidates {
		if tokens := valueUint64(candidate); tokens > 0 {
			return tokens
		}
	}
	return 0
}

func openAICacheWriteTokens(usage map[string]any) uint64 {
	candidates := []any{
		usage["cache_creation_input_tokens"],
		nestedValue(usage, "input_tokens_details", "cache_write_tokens"),
		nestedValue(usage, "prompt_tokens_details", "cache_write_tokens"),
	}
	for _, candidate := range candidates {
		if tokens := valueUint64(candidate); tokens > 0 {
			return tokens
		}
	}
	return 0
}

func nestedValue(value map[string]any, path ...string) any {
	var current any = value
	for _, key := range path {
		object, ok := current.(map[string]any)
		if !ok {
			return nil
		}
		current = object[key]
	}
	return current
}

func unwrapChatCompletionEnvelope(body map[string]any) map[string]any {
	data, ok := body["data"].(map[string]any)
	if !ok {
		return body
	}
	if _, hasUsage := data["usage"]; !hasUsage {
		if _, hasChoices := data["choices"]; !hasChoices {
			return body
		}
	}
	result := make(map[string]any, len(data)+len(body))
	for key, value := range data {
		result[key] = value
	}
	for key, value := range body {
		if key == "data" || key == "choices" || value == nil {
			continue
		}
		result[key] = value
	}
	return result
}
