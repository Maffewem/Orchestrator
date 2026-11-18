package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"
)

// Metric history. Samples are kept per worker at the interval and for the time its owner chose
// (see the server's metric preferences); columns for metrics that are switched off stay NULL.
const metricsSchema = `
CREATE TABLE IF NOT EXISTS worker_samples (
	device_id TEXT NOT NULL,
	container TEXT NOT NULL,
	ts        INTEGER NOT NULL,
	cpu       REAL,
	mem       INTEGER,
	mem_limit INTEGER,
	net_rx    REAL,
	net_tx    REAL,
	blk_read  REAL,
	blk_write REAL,
	pids      INTEGER,
	throttled REAL,
	restarts  INTEGER,
	health    TEXT,
	PRIMARY KEY (device_id, container, ts)
) WITHOUT ROWID;
CREATE TABLE IF NOT EXISTS proxy_samples (
	device_id TEXT NOT NULL,
	host      TEXT NOT NULL,
	ts        INTEGER NOT NULL,
	requests  REAL NOT NULL,
	c2xx      REAL NOT NULL DEFAULT 0,
	c3xx      REAL NOT NULL DEFAULT 0,
	c4xx      REAL NOT NULL DEFAULT 0,
	c5xx      REAL NOT NULL DEFAULT 0,
	bytes     REAL,
	hist      TEXT,
	PRIMARY KEY (device_id, host, ts)
) WITHOUT ROWID;
CREATE TABLE IF NOT EXISTS cf_samples (
	project_id  TEXT NOT NULL,
	ts          INTEGER NOT NULL,
	requests    INTEGER NOT NULL DEFAULT 0,
	errors      INTEGER NOT NULL DEFAULT 0,
	subrequests INTEGER NOT NULL DEFAULT 0,
	cpu_p50     REAL,
	cpu_p99     REAL,
	wall_p50    REAL,
	wall_p99    REAL,
	PRIMARY KEY (project_id, ts)
) WITHOUT ROWID;
CREATE TABLE IF NOT EXISTS cf_breakdown (
	project_id TEXT NOT NULL,
	ts         INTEGER NOT NULL,
	kind       TEXT NOT NULL,
	key        TEXT NOT NULL,
	requests   INTEGER NOT NULL,
	PRIMARY KEY (project_id, ts, kind, key)
) WITHOUT ROWID;
`

// WorkerSample is one stored resource sample of a container. Rates are per second over the
// interval since the previous stored sample. Nil means the metric was switched off.
type WorkerSample struct {
	Time      int64    `json:"t"`
	CPU       *float64 `json:"cpu,omitempty"`
	Mem       *int64   `json:"mem,omitempty"`
	MemLimit  *int64   `json:"mem_limit,omitempty"`
	NetRx     *float64 `json:"net_rx,omitempty"`
	NetTx     *float64 `json:"net_tx,omitempty"`
	BlkRead   *float64 `json:"blk_read,omitempty"`
	BlkWrite  *float64 `json:"blk_write,omitempty"`
	PIDs      *int64   `json:"pids,omitempty"`
	Throttled *float64 `json:"throttled,omitempty"` // percent of the interval held back by the CPU limit
	Restarts  *int64   `json:"restarts,omitempty"`
	Health    *string  `json:"health,omitempty"`
}

func (s *Store) AddWorkerSample(ctx context.Context, deviceID, container string, w WorkerSample) error {
	_, err := s.db.ExecContext(ctx, `INSERT OR REPLACE INTO worker_samples
		(device_id, container, ts, cpu, mem, mem_limit, net_rx, net_tx, blk_read, blk_write, pids, throttled, restarts, health)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		deviceID, container, w.Time, w.CPU, w.Mem, w.MemLimit, w.NetRx, w.NetTx, w.BlkRead, w.BlkWrite, w.PIDs, w.Throttled, w.Restarts, w.Health)
	return err
}

// WorkerSeries returns a container's samples since a time, averaged into buckets of step
// seconds (memory, processes and restarts take the bucket's highest value).
func (s *Store) WorkerSeries(ctx context.Context, deviceID, container string, since, step int64) ([]WorkerSample, error) {
	if step < 1 {
		step = 1
	}
	rows, err := s.db.QueryContext(ctx, `SELECT (ts / ?1) * ?1 AS b, AVG(cpu), MAX(mem), MAX(mem_limit), AVG(net_rx), AVG(net_tx),
		AVG(blk_read), AVG(blk_write), MAX(pids), AVG(throttled), MAX(restarts)
		FROM worker_samples WHERE device_id = ?2 AND container = ?3 AND ts >= ?4 GROUP BY b ORDER BY b`,
		step, deviceID, container, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []WorkerSample{}
	for rows.Next() {
		var w WorkerSample
		var cpu, rx, tx, br, bw, th sql.NullFloat64
		var mem, lim, pids, rs sql.NullInt64
		if err := rows.Scan(&w.Time, &cpu, &mem, &lim, &rx, &tx, &br, &bw, &pids, &th, &rs); err != nil {
			return nil, err
		}
		w.CPU, w.NetRx, w.NetTx, w.BlkRead, w.BlkWrite, w.Throttled = fptr(cpu), fptr(rx), fptr(tx), fptr(br), fptr(bw), fptr(th)
		w.Mem, w.MemLimit, w.PIDs, w.Restarts = iptr(mem), iptr(lim), iptr(pids), iptr(rs)
		out = append(out, w)
	}
	return out, rows.Err()
}

// WorkerRestartsSince is how many times the container restarted since a time: the growth of its
// restart counter, counting a counter that went back to zero (a new container) as a fresh start.
func (s *Store) WorkerRestartsSince(ctx context.Context, deviceID, container string, since int64) (int64, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT restarts FROM worker_samples
		WHERE device_id = ? AND container = ? AND ts >= ? AND restarts IS NOT NULL ORDER BY ts`, deviceID, container, since)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	var total, prev int64
	first := true
	for rows.Next() {
		var n int64
		if err := rows.Scan(&n); err != nil {
			return 0, err
		}
		if !first && n > prev {
			total += n - prev
		}
		prev, first = n, false
	}
	return total, rows.Err()
}

// ProxySample is one stored interval of a site's proxy traffic.
type ProxySample struct {
	Time     int64        `json:"t"`
	Requests float64      `json:"requests"`
	C2xx     float64      `json:"c2xx"`
	C3xx     float64      `json:"c3xx"`
	C4xx     float64      `json:"c4xx"`
	C5xx     float64      `json:"c5xx"`
	Bytes    *float64     `json:"bytes,omitempty"`
	Hist     [][2]float64 `json:"-"` // [upper bound in seconds, requests] per latency bucket in the interval; nil when latency is off
}

func (s *Store) AddProxySample(ctx context.Context, deviceID, host string, p ProxySample) error {
	var hist any
	if p.Hist != nil {
		b, _ := json.Marshal(p.Hist)
		hist = string(b)
	}
	_, err := s.db.ExecContext(ctx, `INSERT OR REPLACE INTO proxy_samples
		(device_id, host, ts, requests, c2xx, c3xx, c4xx, c5xx, bytes, hist) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		deviceID, host, p.Time, p.Requests, p.C2xx, p.C3xx, p.C4xx, p.C5xx, p.Bytes, hist)
	return err
}

// ProxySamples returns the stored intervals of some sites on a device since a time, oldest first.
func (s *Store) ProxySamples(ctx context.Context, deviceID string, hosts []string, since int64) ([]ProxySample, error) {
	out := []ProxySample{}
	for _, h := range hosts {
		rows, err := s.db.QueryContext(ctx, `SELECT ts, requests, c2xx, c3xx, c4xx, c5xx, bytes, hist FROM proxy_samples
			WHERE device_id = ? AND host = ? AND ts >= ? ORDER BY ts`, deviceID, h, since)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var p ProxySample
			var bytes sql.NullFloat64
			var hist sql.NullString
			if err := rows.Scan(&p.Time, &p.Requests, &p.C2xx, &p.C3xx, &p.C4xx, &p.C5xx, &bytes, &hist); err != nil {
				rows.Close()
				return nil, err
			}
			p.Bytes = fptr(bytes)
			if hist.Valid {
				json.Unmarshal([]byte(hist.String), &p.Hist)
			}
			out = append(out, p)
		}
		rows.Close()
	}
	return out, nil
}

// CFSample is one hour of a Cloudflare Worker's analytics.
type CFSample struct {
	Time        int64    `json:"t"`
	Requests    int64    `json:"requests"`
	Errors      int64    `json:"errors"`
	Subrequests int64    `json:"subrequests"`
	CPUP50      *float64 `json:"cpu_p50,omitempty"` // microseconds
	CPUP99      *float64 `json:"cpu_p99,omitempty"`
	WallP50     *float64 `json:"wall_p50,omitempty"` // microseconds
	WallP99     *float64 `json:"wall_p99,omitempty"`
}

func (s *Store) PutCFSample(ctx context.Context, projectID string, c CFSample) error {
	_, err := s.db.ExecContext(ctx, `INSERT OR REPLACE INTO cf_samples
		(project_id, ts, requests, errors, subrequests, cpu_p50, cpu_p99, wall_p50, wall_p99) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		projectID, c.Time, c.Requests, c.Errors, c.Subrequests, c.CPUP50, c.CPUP99, c.WallP50, c.WallP99)
	return err
}

func (s *Store) CFSamples(ctx context.Context, projectID string, since int64) ([]CFSample, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT ts, requests, errors, subrequests, cpu_p50, cpu_p99, wall_p50, wall_p99
		FROM cf_samples WHERE project_id = ? AND ts >= ? ORDER BY ts`, projectID, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []CFSample{}
	for rows.Next() {
		var c CFSample
		var a, b, d, e sql.NullFloat64
		if err := rows.Scan(&c.Time, &c.Requests, &c.Errors, &c.Subrequests, &a, &b, &d, &e); err != nil {
			return nil, err
		}
		c.CPUP50, c.CPUP99, c.WallP50, c.WallP99 = fptr(a), fptr(b), fptr(d), fptr(e)
		out = append(out, c)
	}
	return out, rows.Err()
}

// PutCFBreakdown replaces one hour's request counts split by kind ("status" or "version").
func (s *Store) PutCFBreakdown(ctx context.Context, projectID string, ts int64, kind string, counts map[string]int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM cf_breakdown WHERE project_id = ? AND ts = ? AND kind = ?`, projectID, ts, kind); err != nil {
		return err
	}
	for k, n := range counts {
		if _, err := tx.ExecContext(ctx, `INSERT INTO cf_breakdown (project_id, ts, kind, key, requests) VALUES (?, ?, ?, ?, ?)`, projectID, ts, kind, k, n); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// CFBreakdown sums request counts by key for one kind since a time.
func (s *Store) CFBreakdown(ctx context.Context, projectID, kind string, since int64) (map[string]int64, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT key, SUM(requests) FROM cf_breakdown
		WHERE project_id = ? AND kind = ? AND ts >= ? GROUP BY key`, projectID, kind, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var k string
		var n int64
		if err := rows.Scan(&k, &n); err != nil {
			return nil, err
		}
		out[k] = n
	}
	return out, rows.Err()
}

// HasCFSamples reports whether any hour is stored for a project, so the monitor knows to backfill.
func (s *Store) HasCFSamples(ctx context.Context, projectID string) bool {
	var n int
	s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM (SELECT 1 FROM cf_samples WHERE project_id = ? LIMIT 1)`, projectID).Scan(&n)
	return n > 0
}

// PruneWorker deletes a container's samples older than before.
func (s *Store) PruneWorker(ctx context.Context, deviceID, container string, before int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM worker_samples WHERE device_id = ? AND container = ? AND ts < ?`, deviceID, container, before)
	return err
}

// PruneProxy deletes a site's proxy samples older than before.
func (s *Store) PruneProxy(ctx context.Context, deviceID, host string, before int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM proxy_samples WHERE device_id = ? AND host = ? AND ts < ?`, deviceID, host, before)
	return err
}

// PruneCF deletes a Cloudflare project's hours older than before.
func (s *Store) PruneCF(ctx context.Context, projectID string, before int64) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM cf_samples WHERE project_id = ? AND ts < ?`, projectID, before); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `DELETE FROM cf_breakdown WHERE project_id = ? AND ts < ?`, projectID, before)
	return err
}

// PruneMetricsBefore deletes every metric sample older than before, whoever it belongs to. It
// is the backstop for containers and projects that no longer exist.
func (s *Store) PruneMetricsBefore(ctx context.Context, before time.Time) error {
	for _, t := range []string{"worker_samples", "proxy_samples", "cf_samples", "cf_breakdown"} {
		if _, err := s.db.ExecContext(ctx, `DELETE FROM `+t+` WHERE ts < ?`, before.Unix()); err != nil {
			return err
		}
	}
	return nil
}

func fptr(v sql.NullFloat64) *float64 {
	if !v.Valid {
		return nil
	}
	f := v.Float64
	return &f
}

func iptr(v sql.NullInt64) *int64 {
	if !v.Valid {
		return nil
	}
	n := v.Int64
	return &n
}
