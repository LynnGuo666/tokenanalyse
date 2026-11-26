package storage

import (
	"token-mirror/internal/collectors"
)

func (db *DB) SaveCursors(cursors []collectors.Cursor) error {
	tx, err := db.conn.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, cursor := range cursors {
		if _, err := tx.Exec(`INSERT INTO collector_cursors(source_id, file_path, offset, modified_nanos, state, updated_at)
			VALUES (?, ?, ?, ?, ?, ?)
			ON CONFLICT(source_id, file_path) DO UPDATE SET offset=excluded.offset,
				modified_nanos=excluded.modified_nanos, state=excluded.state, updated_at=excluded.updated_at`,
			cursor.SourceID, cursor.FilePath, cursor.Offset, cursor.ModifiedNanos, cursor.State,
			formatTime(cursor.UpdatedAt)); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (db *DB) LoadCursors() ([]collectors.Cursor, error) {
	rows, err := db.conn.Query(`SELECT source_id, file_path, offset, modified_nanos, state, updated_at FROM collector_cursors`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var cursors []collectors.Cursor
	for rows.Next() {
		var cursor collectors.Cursor
		var updatedAt string
		if err := rows.Scan(&cursor.SourceID, &cursor.FilePath, &cursor.Offset, &cursor.ModifiedNanos, &cursor.State, &updatedAt); err != nil {
			return nil, err
		}
		cursor.UpdatedAt, _ = parseTime(updatedAt)
		cursors = append(cursors, cursor)
	}
	return cursors, rows.Err()
}
