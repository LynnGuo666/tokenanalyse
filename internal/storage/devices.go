package storage

import (
	"database/sql"
	"encoding/json"
	"fmt"

	"token-mirror/internal/model"
)

func (db *DB) UpsertDevice(device model.Device) error {
	metadata, err := json.Marshal(device.Metadata)
	if err != nil {
		return err
	}
	if len(metadata) == 0 || string(metadata) == "null" {
		metadata = []byte("{}")
	}
	_, err = db.conn.Exec(`
		INSERT INTO devices(device_id, name, platform, arch, agent_version, first_seen_at, last_seen_at, metadata)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(device_id) DO UPDATE SET
			name=excluded.name,
			platform=excluded.platform,
			arch=excluded.arch,
			agent_version=excluded.agent_version,
			first_seen_at=MIN(devices.first_seen_at, excluded.first_seen_at),
			last_seen_at=MAX(devices.last_seen_at, excluded.last_seen_at),
			metadata=CASE WHEN excluded.metadata='{}' THEN devices.metadata ELSE excluded.metadata END
	`, device.DeviceID, device.Name, device.Platform, device.Arch, device.AgentVersion,
		formatTime(device.FirstSeenAt), formatTime(device.LastSeenAt), string(metadata))
	if err != nil {
		return fmt.Errorf("upsert device: %w", err)
	}
	return nil
}

func (db *DB) Heartbeat(heartbeat model.DeviceHeartbeat) error {
	_, err := db.conn.Exec(`
		INSERT INTO devices(device_id, name, platform, arch, agent_version, first_seen_at, last_seen_at, metadata)
		VALUES (?, ?, ?, ?, ?, ?, ?, '{}')
		ON CONFLICT(device_id) DO UPDATE SET
			name=excluded.name,
			platform=excluded.platform,
			arch=excluded.arch,
			agent_version=excluded.agent_version,
			last_seen_at=MAX(devices.last_seen_at, excluded.last_seen_at)
	`, heartbeat.DeviceID, heartbeat.Name, heartbeat.Platform, heartbeat.Arch,
		heartbeat.AgentVersion, formatTime(heartbeat.SentAt), formatTime(heartbeat.SentAt))
	if err != nil {
		return fmt.Errorf("heartbeat: %w", err)
	}
	return nil
}

func (db *DB) ListDevices() ([]model.Device, error) {
	rows, err := db.conn.Query(`
		SELECT device_id, name, platform, arch, agent_version, first_seen_at, last_seen_at, metadata
		FROM devices ORDER BY device_id
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var devices []model.Device
	for rows.Next() {
		device, err := scanDevice(rows)
		if err != nil {
			return nil, err
		}
		devices = append(devices, device)
	}
	return devices, rows.Err()
}

func (db *DB) GetDevice(deviceID string) (model.Device, error) {
	row := db.conn.QueryRow(`
		SELECT device_id, name, platform, arch, agent_version, first_seen_at, last_seen_at, metadata
		FROM devices WHERE device_id=?`, deviceID)
	device, err := scanDevice(row)
	if err == sql.ErrNoRows {
		return model.Device{}, fmt.Errorf("device not found: %s", deviceID)
	}
	return device, err
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanDevice(row rowScanner) (model.Device, error) {
	var device model.Device
	var firstSeen, lastSeen, metadata string
	if err := row.Scan(&device.DeviceID, &device.Name, &device.Platform, &device.Arch,
		&device.AgentVersion, &firstSeen, &lastSeen, &metadata); err != nil {
		return device, err
	}
	var err error
	device.FirstSeenAt, err = parseTime(firstSeen)
	if err != nil {
		return device, err
	}
	device.LastSeenAt, err = parseTime(lastSeen)
	if err != nil {
		return device, err
	}
	_ = json.Unmarshal([]byte(metadata), &device.Metadata)
	return device, nil
}
