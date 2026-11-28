package store

import "context"

// Device resource history: one row per device, resolution (seconds per point) and bucket start.
// data is the server's JSON for the averaged sample.

// PutDeviceSample stores (or replaces) one averaged sample.
func (s *Store) PutDeviceSample(ctx context.Context, deviceID string, res int, ts int64, data []byte) error {
	_, err := s.db.ExecContext(ctx, `INSERT OR REPLACE INTO device_samples (device_id, res, ts, data) VALUES (?, ?, ?, ?)`, deviceID, res, ts, string(data))
	return err
}

// DeviceSamples returns a device's samples at one resolution from since, oldest first, as raw JSON.
func (s *Store) DeviceSamples(ctx context.Context, deviceID string, res int, since int64) ([][]byte, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT data FROM device_samples WHERE device_id = ? AND res = ? AND ts >= ? ORDER BY ts`, deviceID, res, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out [][]byte
	for rows.Next() {
		var d string
		if err := rows.Scan(&d); err != nil {
			return nil, err
		}
		out = append(out, []byte(d))
	}
	return out, rows.Err()
}

// PruneDeviceSamples deletes samples at one resolution older than before, and every sample of a
// device that no longer exists.
func (s *Store) PruneDeviceSamples(ctx context.Context, res int, before int64) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM device_samples WHERE res = ? AND ts < ?`, res, before); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `DELETE FROM device_samples WHERE device_id NOT IN (SELECT id FROM devices)`)
	return err
}

// DeviceSampleStats counts the stored device samples and the bytes their data takes.
func (s *Store) DeviceSampleStats(ctx context.Context) (rows, bytes int64, err error) {
	err = s.db.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(SUM(LENGTH(data) + 40), 0) FROM device_samples`).Scan(&rows, &bytes)
	return
}
