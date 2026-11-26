package model

import (
	"regexp"
	"strings"

	"github.com/shopspring/decimal"
)

type Currency string

const (
	CurrencyUSD Currency = "USD"
	CurrencyCNY Currency = "CNY"
)

type ModelPricing struct {
	ModelID                     string          `json:"modelId"`
	DisplayName                 string          `json:"displayName"`
	InputCostPerMillion         decimal.Decimal `json:"inputCostPerMillion"`
	OutputCostPerMillion        decimal.Decimal `json:"outputCostPerMillion"`
	CacheReadCostPerMillion     decimal.Decimal `json:"cacheReadCostPerMillion"`
	CacheCreationCostPerMillion decimal.Decimal `json:"cacheCreationCostPerMillion"`
	Currency                    Currency        `json:"currency"`
	Source                      string          `json:"source"`
}

func NewModelPricing(modelID, displayName, input, output, cacheRead, cacheCreation string) (ModelPricing, error) {
	return ModelPricing{
		ModelID:                     modelID,
		DisplayName:                 displayName,
		InputCostPerMillion:         decimal.RequireFromString(input),
		OutputCostPerMillion:        decimal.RequireFromString(output),
		CacheReadCostPerMillion:     decimal.RequireFromString(cacheRead),
		CacheCreationCostPerMillion: decimal.RequireFromString(cacheCreation),
		Currency:                    CurrencyUSD,
		Source:                      "custom",
	}, nil
}

var (
	providerPrefix  = regexp.MustCompile(`^(?:[A-Za-z0-9_-]+/)+`)
	dateSuffix      = regexp.MustCompile(`(?:[-_]\d{4}[-_]\d{2}[-_]\d{2}|[-_]\d{8})$`)
	reasoningSuffix = regexp.MustCompile(`(?:@(?:low|medium|high|xhigh)|[-_](?:low|medium|high|xhigh))$`)
)

func NormalizeModelID(modelID string) string {
	value := strings.ToLower(strings.TrimSpace(modelID))
	value = strings.ReplaceAll(value, "@", "-")
	value = strings.ReplaceAll(value, "[", "")
	value = strings.ReplaceAll(value, "]", "")
	for {
		before := value
		value = providerPrefix.ReplaceAllString(value, "")
		value = reasoningSuffix.ReplaceAllString(value, "")
		value = dateSuffix.ReplaceAllString(value, "")
		for _, prefix := range []string{"global.", "anthropic.", "openai.", "google."} {
			value = strings.TrimPrefix(value, prefix)
		}
		if before == value {
			break
		}
	}
	value = strings.TrimSuffix(value, "-thinking")
	value = strings.TrimSuffix(value, "-tiered")
	return value
}
