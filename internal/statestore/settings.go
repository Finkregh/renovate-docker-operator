package statestore

import (
	"context"
	"database/sql"
	"errors"
)

// GetSetting reads a value from the settings table by key.
// Returns ("", false, nil) if the key does not exist.
func (s *SQLiteStore) GetSetting(ctx context.Context, key string) (string, bool, error) {
	var value string
	err := s.readDB.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, key).Scan(&value)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", false, nil
		}
		return "", false, err
	}
	return value, true, nil
}

// SetSetting upserts a value in the settings table.
func (s *SQLiteStore) SetSetting(ctx context.Context, key, value string) error {
	_, err := s.writeDB.ExecContext(ctx,
		`INSERT INTO settings (key, value, updated_at) VALUES (?, ?, datetime('now'))
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		key, value,
	)
	return err
}
