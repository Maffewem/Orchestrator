package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

// KeyPrefix starts every API key, so a key is recognised before any lookup.
const KeyPrefix = "orch_"

// APIKey is a long-lived credential for scripts and integrations. It acts with one role.
type APIKey struct {
	ID        string     `json:"id"`
	Name      string     `json:"name"`
	Prefix    string     `json:"prefix"` // the first characters of the key, to tell keys apart
	Role      string     `json:"role"`
	CreatedBy string     `json:"created_by"`
	CreatedAt time.Time  `json:"created_at"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
	LastUsed  *time.Time `json:"last_used,omitempty"`
}

const apiKeyCols = `id, name, prefix, role, created_by, created_at, expires_at, last_used`

func scanKey(r interface{ Scan(...any) error }) (*APIKey, error) {
	var k APIKey
	var created, exp, used int64
	if err := r.Scan(&k.ID, &k.Name, &k.Prefix, &k.Role, &k.CreatedBy, &created, &exp, &used); err != nil {
		return nil, err
	}
	k.CreatedAt = time.Unix(created, 0).UTC()
	if exp > 0 {
		t := time.Unix(exp, 0).UTC()
		k.ExpiresAt = &t
	}
	if used > 0 {
		t := time.Unix(used, 0).UTC()
		k.LastUsed = &t
	}
	return &k, nil
}

// CreateAPIKey stores a key by its hash and returns it with the one-time plaintext.
func (s *Store) CreateAPIKey(ctx context.Context, name, role, createdBy string, expires time.Time) (*APIKey, string, error) {
	plain := KeyPrefix + NewToken()
	k := &APIKey{ID: newID("key_"), Name: name, Prefix: plain[:len(KeyPrefix)+6], Role: role, CreatedBy: createdBy, CreatedAt: time.Now().UTC().Truncate(time.Second)}
	var exp int64
	if !expires.IsZero() {
		exp = expires.Unix()
		t := time.Unix(exp, 0).UTC()
		k.ExpiresAt = &t
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO api_keys(id, name, prefix, role, created_by, created_at, expires_at, last_used, token_hash) VALUES(?, ?, ?, ?, ?, ?, ?, 0, ?)`,
		k.ID, k.Name, k.Prefix, k.Role, k.CreatedBy, k.CreatedAt.Unix(), exp, HashToken(plain))
	if err != nil {
		return nil, "", err
	}
	return k, plain, nil
}

// APIKeyByHash returns the unexpired key with this hash and records that it was used.
func (s *Store) APIKeyByHash(ctx context.Context, hash string) (*APIKey, error) {
	now := time.Now().Unix()
	k, err := scanKey(s.db.QueryRowContext(ctx, `SELECT `+apiKeyCols+` FROM api_keys WHERE token_hash = ? AND (expires_at = 0 OR expires_at > ?)`, hash, now))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if k.LastUsed == nil || now-k.LastUsed.Unix() >= 60 { // one write a minute at most
		s.db.ExecContext(ctx, `UPDATE api_keys SET last_used = ? WHERE id = ?`, now, k.ID)
	}
	return k, nil
}

func (s *Store) ListAPIKeys(ctx context.Context) ([]*APIKey, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+apiKeyCols+` FROM api_keys ORDER BY created_at, rowid`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*APIKey{}
	for rows.Next() {
		k, err := scanKey(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

func (s *Store) GetAPIKey(ctx context.Context, id string) (*APIKey, error) {
	k, err := scanKey(s.db.QueryRowContext(ctx, `SELECT `+apiKeyCols+` FROM api_keys WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return k, err
}

func (s *Store) DeleteAPIKey(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM api_keys WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// --- webhooks ---

// Webhook is an address that receives signed events.
type Webhook struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	URL       string    `json:"url"`
	Events    []string  `json:"events"` // "*", "deployment.*" or an exact kind
	Enabled   bool      `json:"enabled"`
	CreatedAt time.Time `json:"created_at"`
	Secret    string    `json:"-"`
}

// Delivery is one attempt series to send an event to a webhook.
type Delivery struct {
	ID         int64     `json:"id"`
	WebhookID  string    `json:"webhook_id"`
	Event      string    `json:"event"`
	OK         bool      `json:"ok"`
	HTTPStatus int       `json:"http_status"`
	Error      string    `json:"error,omitempty"`
	Attempts   int       `json:"attempts"`
	DurationMS int64     `json:"duration_ms"`
	Payload    string    `json:"payload,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
}

const webhookCols = `id, name, url, events, enabled, created_at, secret`

func (s *Store) scanWebhook(r interface{ Scan(...any) error }) (*Webhook, error) {
	var w Webhook
	var ev, sec string
	var en, created int64
	if err := r.Scan(&w.ID, &w.Name, &w.URL, &ev, &en, &created, &sec); err != nil {
		return nil, err
	}
	w.Enabled = en != 0
	w.CreatedAt = time.Unix(created, 0).UTC()
	w.Events = []string{}
	for _, e := range strings.Split(ev, ",") {
		if e = strings.TrimSpace(e); e != "" {
			w.Events = append(w.Events, e)
		}
	}
	w.Secret = sec
	if s.box != nil {
		if p, ok := s.box.open(sec); ok {
			w.Secret = p
		}
	}
	return &w, nil
}

func (s *Store) sealSecret(v string) string {
	if s.box == nil {
		return v
	}
	return s.box.seal(v)
}

func (s *Store) CreateWebhook(ctx context.Context, name, url, secret string, events []string) (*Webhook, error) {
	w := &Webhook{ID: newID("whk_"), Name: name, URL: url, Events: events, Enabled: true, Secret: secret, CreatedAt: time.Now().UTC().Truncate(time.Second)}
	_, err := s.db.ExecContext(ctx, `INSERT INTO webhooks(id, name, url, events, enabled, created_at, secret) VALUES(?, ?, ?, ?, 1, ?, ?)`,
		w.ID, w.Name, w.URL, strings.Join(events, ","), w.CreatedAt.Unix(), s.sealSecret(secret))
	return w, err
}

func (s *Store) UpdateWebhook(ctx context.Context, w *Webhook) error {
	en := 0
	if w.Enabled {
		en = 1
	}
	res, err := s.db.ExecContext(ctx, `UPDATE webhooks SET name = ?, url = ?, events = ?, enabled = ?, secret = ? WHERE id = ?`,
		w.Name, w.URL, strings.Join(w.Events, ","), en, s.sealSecret(w.Secret), w.ID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) GetWebhook(ctx context.Context, id string) (*Webhook, error) {
	w, err := s.scanWebhook(s.db.QueryRowContext(ctx, `SELECT `+webhookCols+` FROM webhooks WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return w, err
}

func (s *Store) ListWebhooks(ctx context.Context) ([]*Webhook, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+webhookCols+` FROM webhooks ORDER BY created_at, rowid`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*Webhook{}
	for rows.Next() {
		w, err := s.scanWebhook(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

func (s *Store) DeleteWebhook(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM webhooks WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

const keepDeliveries = 50

// AddDelivery records a delivery and keeps only the newest ones for the webhook.
func (s *Store) AddDelivery(ctx context.Context, d *Delivery) error {
	ok := 0
	if d.OK {
		ok = 1
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO webhook_deliveries(webhook_id, event, ok, http_status, error, attempts, duration_ms, payload, created_at) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		d.WebhookID, d.Event, ok, d.HTTPStatus, d.Error, d.Attempts, d.DurationMS, d.Payload, time.Now().Unix())
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `DELETE FROM webhook_deliveries WHERE webhook_id = ? AND id NOT IN (SELECT id FROM webhook_deliveries WHERE webhook_id = ? ORDER BY id DESC LIMIT ?)`,
		d.WebhookID, d.WebhookID, keepDeliveries)
	return err
}

func (s *Store) ListDeliveries(ctx context.Context, webhookID string) ([]*Delivery, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, webhook_id, event, ok, http_status, error, attempts, duration_ms, payload, created_at FROM webhook_deliveries WHERE webhook_id = ? ORDER BY id DESC`, webhookID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*Delivery{}
	for rows.Next() {
		var d Delivery
		var ok, created int64
		if err := rows.Scan(&d.ID, &d.WebhookID, &d.Event, &ok, &d.HTTPStatus, &d.Error, &d.Attempts, &d.DurationMS, &d.Payload, &created); err != nil {
			return nil, err
		}
		d.OK = ok != 0
		d.CreatedAt = time.Unix(created, 0).UTC()
		out = append(out, &d)
	}
	return out, rows.Err()
}
