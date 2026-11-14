// Package store persists the domain dataset in Postgres + PostGIS and runs the geo search.
package store

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mrfroglab/sewain-api/internal/domain"
)

//go:embed migrations/*.sql
var migrations embed.FS

type Store struct{ pool *pgxpool.Pool }

func Open(ctx context.Context, url string) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, fmt.Errorf("parse DATABASE_URL: %w", err)
	}
	cfg.ConnConfig.RuntimeParams["timezone"] = "UTC"
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}
	return &Store{pool: pool}, nil
}

func (s *Store) Close()                         { s.pool.Close() }
func (s *Store) Ping(ctx context.Context) error { return s.pool.Ping(ctx) }

// Migrate applies embedded migrations in order, once each.
func (s *Store) Migrate(ctx context.Context) error {
	if _, err := s.pool.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (version text PRIMARY KEY, applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		return err
	}
	entries, err := migrations.ReadDir("migrations")
	if err != nil {
		return err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	for _, name := range names {
		var exists bool
		if err := s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE version=$1)`, name).Scan(&exists); err != nil {
			return err
		}
		if exists {
			continue
		}
		sql, _ := migrations.ReadFile("migrations/" + name)
		err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, string(sql)); err != nil {
				return fmt.Errorf("%s: %w", name, err)
			}
			_, err := tx.Exec(ctx, `INSERT INTO schema_migrations(version) VALUES ($1)`, name)
			return err
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// collections maps the dataset fields to tables and their load order (matching how the web app lists them).
var collections = []struct {
	table string
	order string
}{
	{"users", "ts ASC"}, {"verifications", "ts DESC"}, {"listings", "ts DESC"}, {"bookings", "ts DESC"},
	{"photos", "ts ASC"}, {"messages", "ts ASC"}, {"reviews", "ts ASC"}, {"reports", "ts DESC"},
	{"disputes", "ts DESC"}, {"strikes", "ts DESC"}, {"audit", "ts DESC"}, {"wanted", "ts DESC"},
	{"notifications", "ts DESC"},
}

type meta struct {
	Version        int64 `json:"version"`
	Seeded         bool  `json:"seeded"`
	SeedBase       int64 `json:"seedBase"`
	ClockOffset    int64 `json:"clockOffset"`
	BookingCounter int   `json:"bookingCounter"`
}

// Load reads the whole dataset. Returns (nil, nil) when the database is empty (needs seeding).
func (s *Store) Load(ctx context.Context) (*domain.Data, error) {
	settings := map[string]json.RawMessage{}
	rows, err := s.pool.Query(ctx, `SELECT key, value FROM settings`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var k string
		var v json.RawMessage
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		settings[k] = v
	}
	rows.Close()
	if len(settings["meta"]) == 0 {
		return nil, nil
	}
	// Assemble the DataState-shaped JSON from the tables, then decode once.
	doc := map[string]json.RawMessage{}
	for _, c := range collections {
		var arr json.RawMessage
		q := fmt.Sprintf(`SELECT COALESCE(jsonb_agg(data ORDER BY %s), '[]'::jsonb) FROM %s`, c.order, c.table)
		if err := s.pool.QueryRow(ctx, q).Scan(&arr); err != nil {
			return nil, fmt.Errorf("load %s: %w", c.table, err)
		}
		doc[c.table] = arr
	}
	for _, k := range []string{"commission", "collateralRules", "bannedKeywords", "favorites", "chatReads"} {
		if v, ok := settings[k]; ok {
			doc[k] = v
		}
	}
	raw, _ := json.Marshal(doc)
	var d domain.Data
	if err := json.Unmarshal(raw, &d); err != nil {
		return nil, err
	}
	var m meta
	_ = json.Unmarshal(settings["meta"], &m)
	d.Version, d.Seeded, d.SeedBase, d.ClockOffset, d.BookingCounter = m.Version, m.Seeded, m.SeedBase, m.ClockOffset, m.BookingCounter
	return &d, nil
}

func upsertSQL(table string) string {
	return fmt.Sprintf(`INSERT INTO %s (id, data) VALUES ($1, $2) ON CONFLICT (id) DO UPDATE SET data = EXCLUDED.data, updated_at = now()`, table)
}

func (s *Store) writeSettings(ctx context.Context, tx pgx.Tx, d *domain.Data) error {
	vals := map[string]any{
		"meta":            meta{Version: d.Version, Seeded: d.Seeded, SeedBase: d.SeedBase, ClockOffset: d.ClockOffset, BookingCounter: d.BookingCounter},
		"commission":      d.Commission,
		"collateralRules": d.CollateralRules,
		"bannedKeywords":  d.BannedKeywords,
		"favorites":       d.Favorites,
		"chatReads":       d.ChatReads,
	}
	for k, v := range vals {
		b, _ := json.Marshal(v)
		if _, err := tx.Exec(ctx, `INSERT INTO settings (key, value) VALUES ($1, $2) ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, updated_at = now()`, k, b); err != nil {
			return err
		}
	}
	return nil
}

// Persist writes the changed rows (and settings) in one transaction.
func (s *Store) Persist(ctx context.Context, d *domain.Data, ch domain.Changes) error {
	if ch.Empty() {
		return nil
	}
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		batch := &pgx.Batch{}
		for table, items := range ch.Rows {
			for _, it := range items {
				b, err := json.Marshal(it)
				if err != nil {
					return err
				}
				batch.Queue(upsertSQL(table), idOf(it), b)
			}
		}
		if batch.Len() > 0 {
			if err := tx.SendBatch(ctx, batch).Close(); err != nil {
				return err
			}
		}
		if ch.Settings {
			return s.writeSettings(ctx, tx, d)
		}
		return nil
	})
}

func idOf(v any) string {
	var x struct {
		ID string `json:"id"`
	}
	b, _ := json.Marshal(v)
	_ = json.Unmarshal(b, &x)
	return x.ID
}

// ReplaceAll wipes every table and writes the whole dataset (demo reset / first boot). Sessions are kept.
func (s *Store) ReplaceAll(ctx context.Context, d *domain.Data) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		tables := make([]string, 0, len(collections))
		for _, c := range collections {
			tables = append(tables, c.table)
		}
		if _, err := tx.Exec(ctx, `TRUNCATE `+strings.Join(tables, ", ")); err != nil {
			return err
		}
		batch := &pgx.Batch{}
		add := func(table string, items any) {
			b, _ := json.Marshal(items)
			var arr []json.RawMessage
			_ = json.Unmarshal(b, &arr)
			for _, it := range arr {
				batch.Queue(upsertSQL(table), idOf(it), []byte(it))
			}
		}
		add("users", d.Users)
		add("verifications", d.Verifications)
		add("listings", d.Listings)
		add("bookings", d.Bookings)
		add("photos", d.Photos)
		add("messages", d.Messages)
		add("reviews", d.Reviews)
		add("reports", d.Reports)
		add("disputes", d.Disputes)
		add("strikes", d.Strikes)
		add("audit", d.Audit)
		add("wanted", d.Wanted)
		add("notifications", d.Notifications)
		if err := tx.SendBatch(ctx, batch).Close(); err != nil {
			return err
		}
		return s.writeSettings(ctx, tx, d)
	})
}

/* ---------------------------------------------------------------- sessions */

func (s *Store) SaveSession(ctx context.Context, token, userID string) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO sessions (token, user_id) VALUES ($1, $2) ON CONFLICT (token) DO UPDATE SET user_id = EXCLUDED.user_id`, token, userID)
	return err
}

func (s *Store) SessionUser(ctx context.Context, token string) (string, error) {
	var uid string
	err := s.pool.QueryRow(ctx, `SELECT user_id FROM sessions WHERE token = $1`, token).Scan(&uid)
	if err == pgx.ErrNoRows {
		return "", nil
	}
	return uid, err
}

/* ------------------------------------------------------------------ search */

type SearchParams struct {
	Lat, Lng     float64
	RadiusKm     float64
	Category     string
	MinPrice     int64
	MaxPrice     int64
	Handover     string
	Collateral   string // identitas | deposit | barang | custom | cukup-identitas
	ProviderType string // individu | toko
	Q            string
}

type SearchHit struct {
	ID         string  `json:"id"`
	DistanceKm float64 `json:"distanceKm"`
}

type SearchResult struct {
	Hits                  []SearchHit `json:"hits"`
	HiddenByProviderReach int         `json:"hiddenByProviderReach"`
	Query                 string      `json:"query"`
}

// Search implements the two-way radius rule with PostGIS (FR-S3):
// a listing shows when distance ≤ renter radius AND distance ≤ provider reach, i.e.
// ST_DWithin(l.location, :point, LEAST(:renter_radius, l.radius_m)). Ordered by distance (FR-S4).
func (s *Store) Search(ctx context.Context, p SearchParams) (SearchResult, error) {
	where := []string{`l.status = 'aktif'`, `u.status = 'aktif'`, `ST_DWithin(l.location, pt.g, $3)`}
	args := []any{p.Lng, p.Lat, p.RadiusKm * 1000}
	add := func(cond string, v any) {
		args = append(args, v)
		where = append(where, strings.ReplaceAll(cond, "?", fmt.Sprintf("$%d", len(args))))
	}
	if p.Category != "" {
		add(`l.category = ?`, p.Category)
	}
	if p.MinPrice > 0 {
		add(`l.price_per_day >= ?`, p.MinPrice)
	}
	if p.MaxPrice > 0 {
		add(`l.price_per_day <= ?`, p.MaxPrice)
	}
	if p.Handover != "" {
		add(`l.data->'handover' ? ?`, p.Handover)
	}
	switch p.Collateral {
	case "":
	case "cukup-identitas":
		where = append(where, `EXISTS (SELECT 1 FROM jsonb_array_elements(l.data->'collateralOptions') o WHERE o->>'kind' = 'identitas')`)
	default:
		add(`EXISTS (SELECT 1 FROM jsonb_array_elements(l.data->'collateralOptions') o WHERE o->>'kind' = ?)`, p.Collateral)
	}
	if p.ProviderType != "" {
		add(`u.data->>'accountType' = ?`, p.ProviderType)
	}
	for _, w := range strings.Fields(strings.ToLower(p.Q)) {
		add(`lower(l.data->>'title' || ' ' || (l.data->>'description')) LIKE '%' || ? || '%'`, w)
	}
	q := `
WITH pt AS (SELECT ST_SetSRID(ST_MakePoint($1, $2), 4326)::geography AS g)
SELECT l.id, ST_Distance(l.location, pt.g) / 1000 AS km, ST_DWithin(l.location, pt.g, LEAST($3, l.radius_m)) AS visible
FROM listings l JOIN users u ON u.id = l.owner_id, pt
WHERE ` + strings.Join(where, " AND ") + `
ORDER BY km`
	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return SearchResult{}, err
	}
	defer rows.Close()
	res := SearchResult{Hits: []SearchHit{}, Query: "ST_DWithin(location, point, LEAST(radius_penyewa, radius_penyedia))"}
	for rows.Next() {
		var h SearchHit
		var visible bool
		if err := rows.Scan(&h.ID, &h.DistanceKm, &visible); err != nil {
			return res, err
		}
		if visible {
			res.Hits = append(res.Hits, h)
		} else {
			res.HiddenByProviderReach++
		}
	}
	return res, rows.Err()
}
