package model

import "github.com/shopspring/decimal"

type CostBreakdown struct {
	InputCost         decimal.Decimal
	OutputCost        decimal.Decimal
	CacheReadCost     decimal.Decimal
	CacheCreationCost decimal.Decimal
	TotalCost         decimal.Decimal
}

var million = decimal.NewFromInt(1_000_000)

func CalculateCost(appType string, usage TokenUsage, pricing ModelPricing, semantics InputSemantics, multiplier decimal.Decimal) CostBreakdown {
	freshInput := usage.FreshInput(appType, semantics)
	inputCost := decimal.NewFromInt(int64(freshInput)).Mul(pricing.InputCostPerMillion).Div(million)
	outputCost := decimal.NewFromInt(int64(usage.OutputTokens)).Mul(pricing.OutputCostPerMillion).Div(million)
	cacheReadCost := decimal.NewFromInt(int64(usage.CacheReadTokens)).Mul(pricing.CacheReadCostPerMillion).Div(million)

	cacheCreation1h := usage.CacheCreation1HTokens
	if cacheCreation1h > usage.CacheCreationTokens {
		cacheCreation1h = usage.CacheCreationTokens
	}
	weightedCacheCreation := decimal.NewFromInt(int64(usage.CacheCreationTokens - cacheCreation1h)).
		Add(decimal.NewFromInt(int64(cacheCreation1h)).Mul(decimal.NewFromFloat(1.6)))
	cacheCreationCost := weightedCacheCreation.Mul(pricing.CacheCreationCostPerMillion).Div(million)

	total := inputCost.Add(outputCost).Add(cacheReadCost).Add(cacheCreationCost).Mul(multiplier)
	return CostBreakdown{
		InputCost:         inputCost,
		OutputCost:        outputCost,
		CacheReadCost:     cacheReadCost,
		CacheCreationCost: cacheCreationCost,
		TotalCost:         total,
	}
}
