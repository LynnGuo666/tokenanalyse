package sync

import (
	"time"

	"tokenanalyse/internal/model"
)

type EventBatch struct {
	BatchID   string             `json:"batchId"`
	DeviceID  string             `json:"deviceId"`
	CreatedAt time.Time          `json:"createdAt"`
	Cursor    int64              `json:"cursor"`
	Events    []model.UsageEvent `json:"events"`
}

type BatchAck struct {
	BatchID      string `json:"batchId"`
	Accepted     int    `json:"accepted"`
	Duplicates   int    `json:"duplicates"`
	Rejected     int    `json:"rejected"`
	ServerCursor int64  `json:"serverCursor"`
}

type HeartbeatRequest struct {
	Device model.DeviceHeartbeat `json:"device"`
}

type EventsResponse struct {
	Events     []model.UsageEvent `json:"events"`
	NextCursor string             `json:"nextCursor"`
}

type Report struct {
	Uploaded   int      `json:"uploaded"`
	Downloaded int      `json:"downloaded"`
	Duplicates int      `json:"duplicates"`
	Conflicts  int      `json:"conflicts"`
	Errors     []string `json:"errors"`
}
