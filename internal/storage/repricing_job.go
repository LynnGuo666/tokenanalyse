package storage

import (
	"fmt"
	"strconv"
	"time"
)

const (
	RepriceJobRunning   = "running"
	RepriceJobCompleted = "completed"
	RepriceJobFailed    = "failed"
)

type RepriceJobStatus struct {
	Status     string     `json:"status"`
	Total      int        `json:"total"`
	Done       int        `json:"done"`
	StartedAt  *time.Time `json:"startedAt,omitempty"`
	FinishedAt *time.Time `json:"finishedAt,omitempty"`
	Error      string     `json:"error,omitempty"`
}

// StartRepriceJob records that a reprice job is running. The expensive work is
// still performed by RepriceAll; this API lets callers publish a running state
// before invoking it (for example from an asynchronous HTTP handler).
func (db *DB) StartRepriceJob(total ...int) error {
	count := 0
	if len(total) > 0 && total[0] > 0 {
		count = total[0]
	}
	now := time.Now().UTC()
	return db.writeRepriceJobStatus(RepriceJobStatus{
		Status: RepriceJobRunning, Total: count, Done: 0, StartedAt: &now,
	})
}

func (db *DB) GetRepriceJobStatus() (RepriceJobStatus, error) {
	status := RepriceJobStatus{}
	rows, err := db.conn.Query(`SELECT key, value FROM settings WHERE key IN (
		'reprice_job_status', 'reprice_job_total', 'reprice_job_done',
		'reprice_job_started_at', 'reprice_job_finished_at', 'reprice_job_error')`)
	if err != nil {
		return status, err
	}
	defer rows.Close()
	values := map[string]string{}
	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			return status, err
		}
		values[key] = value
	}
	if err := rows.Err(); err != nil {
		return status, err
	}
	status.Status = values["reprice_job_status"]
	status.Total = parseRepriceJobInt(values["reprice_job_total"])
	status.Done = parseRepriceJobInt(values["reprice_job_done"])
	status.Error = values["reprice_job_error"]
	if value := values["reprice_job_started_at"]; value != "" {
		parsed, err := parseTime(value)
		if err != nil {
			return status, fmt.Errorf("parse reprice job started_at: %w", err)
		}
		status.StartedAt = &parsed
	}
	if value := values["reprice_job_finished_at"]; value != "" {
		parsed, err := parseTime(value)
		if err != nil {
			return status, fmt.Errorf("parse reprice job finished_at: %w", err)
		}
		status.FinishedAt = &parsed
	}
	return status, nil
}

func (db *DB) beginRepriceJob(total int) error {
	current, err := db.GetRepriceJobStatus()
	if err == nil && current.Status == RepriceJobRunning {
		if total > 0 {
			current.Total = total
		}
		current.Done = 0
		current.FinishedAt = nil
		current.Error = ""
		return db.writeRepriceJobStatus(current)
	}
	return db.StartRepriceJob(total)
}

func (db *DB) setRepriceJobTotal(total int) error {
	status, err := db.GetRepriceJobStatus()
	if err != nil {
		return err
	}
	status.Total = total
	return db.writeRepriceJobStatus(status)
}

func (db *DB) updateRepriceJobProgress(done int) error {
	status, err := db.GetRepriceJobStatus()
	if err != nil {
		return err
	}
	status.Done = done
	return db.writeRepriceJobStatus(status)
}

func (db *DB) finishRepriceJob(state string, done int, jobErr error) error {
	status, err := db.GetRepriceJobStatus()
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	status.Status = state
	status.Done = done
	status.FinishedAt = &now
	status.Error = ""
	if jobErr != nil {
		status.Error = jobErr.Error()
	}
	return db.writeRepriceJobStatus(status)
}

func (db *DB) writeRepriceJobStatus(status RepriceJobStatus) error {
	tx, err := db.conn.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	values := map[string]string{
		"reprice_job_status":      status.Status,
		"reprice_job_total":       strconv.Itoa(status.Total),
		"reprice_job_done":        strconv.Itoa(status.Done),
		"reprice_job_started_at":  formatRepriceJobTime(status.StartedAt),
		"reprice_job_finished_at": formatRepriceJobTime(status.FinishedAt),
		"reprice_job_error":       status.Error,
	}
	for key, value := range values {
		if _, err := tx.Exec(`INSERT INTO settings(key, value) VALUES (?, ?)
			ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func parseRepriceJobInt(value string) int {
	parsed, _ := strconv.Atoi(value)
	return parsed
}

func formatRepriceJobTime(value *time.Time) string {
	if value == nil {
		return ""
	}
	return formatTime(*value)
}
