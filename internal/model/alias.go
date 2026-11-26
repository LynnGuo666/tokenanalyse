package model

import "time"

type ModelAlias struct {
	AliasID          string    `json:"aliasId"`
	AliasPattern     string    `json:"aliasPattern"`
	CanonicalModelID string    `json:"canonicalModelId"`
	MatchType        string    `json:"matchType"`
	ScopeApp         string    `json:"scopeApp"`
	Priority         int       `json:"priority"`
	CreatedAt        time.Time `json:"createdAt"`
}
