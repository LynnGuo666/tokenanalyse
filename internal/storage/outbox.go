package storage

import (
	"database/sql"
	"encoding/json"
	"time"

	"tokenanalyse/internal/model"
)

type OutboxEntry struct {
	OutboxID  int64      `json:"outboxId"`
	EventID   string     `json:"eventId"`
	DeviceID  string     `json:"deviceId"`
	EventType string     `json:"eventType"`
	Target    string     `json:"target"`
	Payload   string     `json:"payload"`
	Status    string     `json:"status"`
	Attempts  int64      `json:"attempts"`
	LastError string     `json:"lastError"`
	CreatedAt time.Time  `json:"createdAt"`
	SentAt    *time.Time `json:"sentAt,omitempty"`
}

func (db *DB) PendingOutbox(limit int) ([]OutboxEntry, error) {
	if limit <= 0 {
		limit = 500
	}
	rows, err := db.conn.Query(`SELECT outbox_id, event_id, device_id, event_type, target, payload,
		status, attempts, last_error, created_at, sent_at FROM outbox
		WHERE status='pending' ORDER BY created_at, outbox_id LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var entries []OutboxEntry
	for rows.Next() {
		var entry OutboxEntry
		var createdAt string
		var sentAt sql.NullString
		if err := rows.Scan(&entry.OutboxID, &entry.EventID, &entry.DeviceID, &entry.EventType,
			&entry.Target, &entry.Payload, &entry.Status, &entry.Attempts, &entry.LastError,
			&createdAt, &sentAt); err != nil {
			return nil, err
		}
		entry.CreatedAt, _ = parseTime(createdAt)
		if sentAt.Valid {
			parsed, _ := parseTime(sentAt.String)
			entry.SentAt = &parsed
		}
		entries = append(entries, entry)
	}
	return entries, rows.Err()
}

func (db *DB) MarkOutboxSent(outboxID int64) error {
	_, err := db.conn.Exec(`UPDATE outbox SET status='sent', sent_at=?, last_error='' WHERE outbox_id=?`,
		formatTime(time.Now().UTC()), outboxID)
	return err
}

func (db *DB) MarkOutboxFailed(outboxID int64, message string) error {
	_, err := db.conn.Exec(`UPDATE outbox SET status='failed', attempts=attempts+1, last_error=? WHERE outbox_id=?`,
		message, outboxID)
	return err
}

func (db *DB) RetryFailedOutbox() error {
	_, err := db.conn.Exec(`UPDATE outbox SET status='pending' WHERE status='failed'`)
	return err
}

func (entry OutboxEntry) Event() (model.UsageEvent, error) {
	var event model.UsageEvent
	err := json.Unmarshal([]byte(entry.Payload), &event)
	return event, err
}
