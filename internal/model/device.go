package model

import "time"

type Platform string

const (
	PlatformLinux   Platform = "linux"
	PlatformMacOS   Platform = "macos"
	PlatformWindows Platform = "windows"
	PlatformOther   Platform = "other"
)

type Device struct {
	DeviceID     string         `json:"deviceId"`
	Name         string         `json:"name"`
	Platform     Platform       `json:"platform"`
	Arch         string         `json:"arch"`
	AgentVersion string         `json:"agentVersion"`
	FirstSeenAt  time.Time      `json:"firstSeenAt"`
	LastSeenAt   time.Time      `json:"lastSeenAt"`
	Metadata     map[string]any `json:"metadata,omitempty"`
}

type DeviceHeartbeat struct {
	DeviceID     string    `json:"deviceId"`
	Name         string    `json:"name"`
	Platform     Platform  `json:"platform"`
	Arch         string    `json:"arch"`
	AgentVersion string    `json:"agentVersion"`
	SentAt       time.Time `json:"sentAt"`
}
