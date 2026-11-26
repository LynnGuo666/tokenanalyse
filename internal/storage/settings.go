package storage

import (
	"database/sql"
	"time"
)

type SyncState struct {
	DeviceID      string     `json:"deviceId"`
	LastEventAt   *time.Time `json:"lastEventAt,omitempty"`
	LastSuccessAt *time.Time `json:"lastSuccessAt,omitempty"`
	Cursor        string     `json:"cursor,omitempty"`
	UpdatedAt     time.Time  `json:"updatedAt"`
}

func (db *DB) GetSetting(key string) (string, error) {
	var value string
	err := db.conn.QueryRow(`SELECT value FROM settings WHERE key=?`, key).Scan(&value)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return value, err
}

func (db *DB) SetSetting(key, value string) error {
	_, err := db.conn.Exec(`INSERT INTO settings(key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value)
	return err
}

func (db *DB) GetSyncState(deviceID string) (SyncState, error) {
	var state SyncState
	var lastEvent, lastSuccess, cursor, updatedAt string
	err := db.conn.QueryRow(`SELECT device_id, COALESCE(last_event_at,''), COALESCE(last_success_at,''),
		COALESCE(cursor,''), updated_at FROM sync_state WHERE device_id=?`, deviceID).
		Scan(&state.DeviceID, &lastEvent, &lastSuccess, &cursor, &updatedAt)
	if err != nil {
		return state, err
	}
	state.Cursor = cursor
	state.UpdatedAt, _ = parseTime(updatedAt)
	if lastEvent != "" {
		parsed, _ := parseTime(lastEvent)
		state.LastEventAt = &parsed
	}
	if lastSuccess != "" {
		parsed, _ := parseTime(lastSuccess)
		state.LastSuccessAt = &parsed
	}
	return state, nil
}

func (db *DB) SetSyncState(state SyncState) error {
	_, err := db.conn.Exec(`INSERT INTO sync_state(device_id, last_event_at, last_success_at, cursor, updated_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(device_id) DO UPDATE SET last_event_at=excluded.last_event_at,
			last_success_at=excluded.last_success_at, cursor=excluded.cursor, updated_at=excluded.updated_at`,
		state.DeviceID, nullableTime(state.LastEventAt), nullableTime(state.LastSuccessAt),
		state.Cursor, formatTime(state.UpdatedAt))
	return err
}

func nullableTime(value *time.Time) any {
	if value == nil {
		return nil
	}
	return formatTime(*value)
}
