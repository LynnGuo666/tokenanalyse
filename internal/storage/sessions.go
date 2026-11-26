package storage

import (
	"sort"
	"time"

	"tokenanalyse/internal/model"

	"github.com/shopspring/decimal"
)

type SessionSummary struct {
	DeviceID       string    `json:"deviceId"`
	DeviceName     string    `json:"deviceName,omitempty"`
	SourceApp      string    `json:"sourceApp"`
	SessionID      string    `json:"sessionId"`
	RequestCount   uint64    `json:"requestCount"`
	TotalTokens    uint64    `json:"totalTokens"`
	TotalCostUSD   string    `json:"totalCostUsd"`
	TotalCostCNY   string    `json:"totalCostCny"`
	FirstRequestAt time.Time `json:"firstRequestAt"`
	LastRequestAt  time.Time `json:"lastRequestAt"`
}

func (db *DB) GetSessionSummaries(filters model.UsageFilters) ([]SessionSummary, error) {
	where, args := buildFilters(filters)
	activeVersion, _ := db.ActivePricingVersion()
	exchangeRate, _ := db.CNYExchangeRate()
	pricingCatalog := db.loadPricingCatalog(activeVersion)
	rows, err := db.conn.Query(`SELECT `+eventColumns+` FROM usage_events`+where+` AND session_id<>'' ORDER BY occurred_at`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type key struct{ device, app, session string }
	groups := map[key][]eventFact{}
	for rows.Next() {
		fact, err := scanEventFact(rows)
		if err != nil {
			return nil, err
		}
		fact = db.repriceFact(fact, activeVersion, exchangeRate, pricingCatalog)
		k := key{fact.event.DeviceID, fact.event.SourceApp, fact.event.SessionID}
		groups[k] = append(groups[k], fact)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	devices, _ := db.ListDevices()
	names := map[string]string{}
	for _, device := range devices {
		names[device.DeviceID] = device.Name
	}
	keys := make([]key, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].device == keys[j].device && keys[i].app == keys[j].app {
			return keys[i].session < keys[j].session
		}
		if keys[i].device == keys[j].device {
			return keys[i].app < keys[j].app
		}
		return keys[i].device < keys[j].device
	})
	result := []SessionSummary{}
	for _, k := range keys {
		facts := groups[k]
		summary := summarizeFacts(facts)
		cny := decimal.Zero
		for _, fact := range facts {
			cny = cny.Add(fact.costCNY)
		}
		result = append(result, SessionSummary{
			DeviceID: k.device, DeviceName: names[k.device], SourceApp: k.app, SessionID: k.session,
			RequestCount: summary.TotalRequests, TotalTokens: summary.RealTotalTokens,
			TotalCostUSD: summary.TotalCost, TotalCostCNY: cny.String(),
			FirstRequestAt: facts[0].event.OccurredAt, LastRequestAt: facts[len(facts)-1].event.OccurredAt,
		})
	}
	return result, nil
}

func (db *DB) GetSessionRequests(filters model.UsageFilters, sessionID string, page, pageSize int) (RequestLogPage, error) {
	filters.Model = ""
	where, args := buildFilters(filters)
	where += " AND session_id=?"
	args = append(args, sessionID)
	if page < 1 {
		page = 1
	}
	if pageSize < 1 {
		pageSize = 50
	}
	if pageSize > 1000 {
		pageSize = 1000
	}
	var total int64
	if err := db.conn.QueryRow(`SELECT COUNT(*) FROM usage_events`+where, args...).Scan(&total); err != nil {
		return RequestLogPage{}, err
	}
	queryArgs := append([]any{}, args...)
	queryArgs = append(queryArgs, pageSize, (page-1)*pageSize)
	activeVersion, _ := db.ActivePricingVersion()
	exchangeRate, _ := db.CNYExchangeRate()
	pricingCatalog := db.loadPricingCatalog(activeVersion)
	rows, err := db.conn.Query(`SELECT `+eventColumns+` FROM usage_events`+where+` ORDER BY occurred_at LIMIT ? OFFSET ?`, queryArgs...)
	if err != nil {
		return RequestLogPage{}, err
	}
	defer rows.Close()
	items := []RequestLogItem{}
	for rows.Next() {
		fact, err := scanEventFact(rows)
		if err != nil {
			return RequestLogPage{}, err
		}
		fact = db.repriceFact(fact, activeVersion, exchangeRate, pricingCatalog)
		items = append(items, RequestLogItem{UsageEvent: fact.event,
			RealTotalTokens: fact.event.Usage.RealTotal(fact.event.SourceApp, fact.event.InputTokenSemantics),
			TotalCostUSD:    fact.costUSD.String(), TotalCostCNY: fact.costCNY.String()})
	}
	if err := rows.Err(); err != nil {
		return RequestLogPage{}, err
	}
	totalPages := int((total + int64(pageSize) - 1) / int64(pageSize))
	return RequestLogPage{Items: items, Total: total, Page: page, PageSize: pageSize, TotalPages: totalPages}, nil
}
