// Package testdb gives every test its own PostgreSQL database.
//
// It clones an already-migrated database (the template) with
// CREATE DATABASE ... TEMPLATE, hands the test a connection string or a
// pgxpool, and drops the clone when the test finishes. Because each test
// gets a real, separate database, code under test may open as many
// connections and goroutines as it likes without leaking state into other
// tests, something a transaction-per-test approach cannot offer.
//
// The caller is responsible for keeping the template migrated to the schema
// the tests expect. testdb never touches the template beyond cloning it.
//
// Requirements: PostgreSQL 13 or newer (DROP DATABASE ... WITH (FORCE)) and a
// role allowed to CREATE DATABASE from the template (its owner or a
// superuser). No session may be connected to the template while a clone is
// created, so point URL at a maintenance database, or let MaintenanceDB
// default to "postgres".
package testdb

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"os"
	"regexp"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Environment variables consulted when the matching Config field is empty.
const (
	// EnvURL holds the server connection string (URL or keyword/value form).
	// The database it names is used as the template unless EnvTemplate or
	// Config.Template says otherwise.
	EnvURL = "TESTDB_URL"
	// EnvTemplate names the template database to clone.
	EnvTemplate = "TESTDB_TEMPLATE"
)

const (
	defaultURL           = "postgres://postgres:postgres@localhost:5432/postgres?sslmode=disable"
	defaultPrefix        = "testdb"
	defaultMaintenanceDB = "postgres"
	defaultMaxConns      = 4
	defaultCloseTimeout  = 5 * time.Second
	randomSuffixBytes    = 8
	maxIdentifierLen     = 63
)

var prefixRe = regexp.MustCompile(`^[a-z_][a-z0-9_]*$`)

// Config controls how databases are created. The zero value is usable: it
// reads TESTDB_URL and TESTDB_TEMPLATE and falls back to a local
// postgres/postgres server.
type Config struct {
	// URL is the server connection string, in URL or keyword/value form.
	// Defaults to $TESTDB_URL, then a local postgres:postgres server.
	URL string

	// Template is the already-migrated database to clone. Defaults to
	// $TESTDB_TEMPLATE, then the database named in URL.
	Template string

	// MaintenanceDB is the database admin sessions connect to for CREATE and
	// DROP DATABASE. It must not be the template. Defaults to "postgres".
	MaintenanceDB string

	// Prefix starts every generated database name; lowercase letters, digits
	// and underscores only. Defaults to "testdb".
	Prefix string

	// MaxConns caps pools returned by DB.Pool and DB.PoolConfig. Keep it low
	// when tests run in parallel: every test holds its own pool. Defaults to 4.
	MaxConns int32

	// KeepOnFailure leaves the database in place when the test fails and logs
	// its connection string, so it can be inspected. Only honoured by New.
	KeepOnFailure bool

	// SkipIfUnreachable makes New skip the test instead of failing it when the
	// server cannot be reached. Only honoured by New.
	SkipIfUnreachable bool

	// CloseTimeout bounds how long Close waits for pools to drain before
	// terminating their backends. Defaults to 5s.
	CloseTimeout time.Duration

	// Logf receives warnings. Defaults to testing.TB.Logf under New, and to
	// the standard logger otherwise.
	Logf func(format string, args ...any)
}

func (c Config) withDefaults() (Config, error) {
	if c.URL == "" {
		c.URL = os.Getenv(EnvURL)
	}
	if c.URL == "" {
		c.URL = defaultURL
	}
	if _, err := pgconn.ParseConfig(c.URL); err != nil {
		return c, fmt.Errorf("testdb: invalid URL: %w", err)
	}

	if c.Template == "" {
		c.Template = os.Getenv(EnvTemplate)
	}
	if c.Template == "" {
		name, err := databaseOf(c.URL)
		if err != nil {
			return c, fmt.Errorf("testdb: invalid URL: %w", err)
		}
		c.Template = name
	}
	if c.Template == "" {
		return c, errors.New("testdb: no template database: set Config.Template, $" + EnvTemplate + ", or name a database in the URL")
	}

	if c.MaintenanceDB == "" {
		c.MaintenanceDB = defaultMaintenanceDB
	}
	if c.MaintenanceDB == c.Template {
		return c, fmt.Errorf("testdb: maintenance database %q must differ from the template", c.Template)
	}

	if c.Prefix == "" {
		c.Prefix = defaultPrefix
	}
	if !prefixRe.MatchString(c.Prefix) {
		return c, fmt.Errorf("testdb: prefix %q must match %s", c.Prefix, prefixRe)
	}
	if len(c.Prefix)+1+randomSuffixBytes*2 > maxIdentifierLen {
		return c, fmt.Errorf("testdb: prefix %q is too long", c.Prefix)
	}

	if c.MaxConns <= 0 {
		c.MaxConns = defaultMaxConns
	}
	if c.CloseTimeout <= 0 {
		c.CloseTimeout = defaultCloseTimeout
	}
	if c.Logf == nil {
		c.Logf = log.Printf
	}
	return c, nil
}

// DB is a database cloned from the template. It is safe for concurrent use.
type DB struct {
	cfg  Config
	name string
	url  string

	mu     sync.Mutex
	pools  []*pgxpool.Pool
	closed bool
}

// New creates a database for tb and drops it when tb finishes. It fails the
// test if the database cannot be created, or skips it when
// cfg.SkipIfUnreachable is set and the server is down.
func New(tb testing.TB, cfg Config) *DB {
	tb.Helper()
	if cfg.Logf == nil {
		cfg.Logf = tb.Logf
	}

	db, err := Create(tb.Context(), cfg)
	if err != nil {
		if cfg.SkipIfUnreachable && isUnreachable(err) {
			tb.Skipf("testdb: %v", err)
		}
		tb.Fatalf("testdb: %v", err)
	}

	tb.Cleanup(func() {
		if cfg.KeepOnFailure && tb.Failed() {
			db.forget()
			tb.Logf("testdb: keeping database %s for inspection: %s", db.name, db.url)
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := db.Close(ctx); err != nil {
			tb.Errorf("testdb: %v", err)
		}
	})
	return db
}

// Create clones the template into a fresh database. Callers must Close it.
// Prefer New inside tests; Create exists for TestMain-style setups and tools.
func Create(ctx context.Context, cfg Config) (*DB, error) {
	cfg, err := cfg.withDefaults()
	if err != nil {
		return nil, err
	}

	admin, err := connectAdmin(ctx, cfg)
	if err != nil {
		return nil, err
	}
	defer func() { _ = admin.Close(ctx) }()

	name := cfg.Prefix + "_" + randomHex(randomSuffixBytes)
	stmt := fmt.Sprintf("CREATE DATABASE %s TEMPLATE %s",
		pgx.Identifier{name}.Sanitize(), pgx.Identifier{cfg.Template}.Sanitize())
	if _, err := admin.Exec(ctx, stmt); err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "55006" { // object_in_use
			return nil, fmt.Errorf("create %s from template %s: %w (disconnect other sessions from the template, e.g. a running app or psql)", name, cfg.Template, err)
		}
		return nil, fmt.Errorf("create %s from template %s: %w", name, cfg.Template, err)
	}

	url, err := withDatabase(cfg.URL, name)
	if err != nil {
		return nil, err
	}
	return &DB{cfg: cfg, name: name, url: url}, nil
}

// Name is the database name.
func (d *DB) Name() string { return d.name }

// URL is a connection string for the database, in the same form as Config.URL.
func (d *DB) URL() string { return d.url }

// PoolConfig returns a fresh pgxpool configuration for the database with
// MaxConns applied. Adjust it (tracers, hooks) and build the pool yourself
// when DB.Pool is not flexible enough; Close will still terminate any
// connections your pool leaves open.
func (d *DB) PoolConfig() *pgxpool.Config {
	cfg, err := pgxpool.ParseConfig(d.url)
	if err != nil {
		// d.url was derived from a string that already parsed.
		panic("testdb: " + err.Error())
	}
	cfg.MaxConns = d.cfg.MaxConns
	return cfg
}

// Pool opens a pgxpool on the database. Pools created here are closed by
// Close, so callers need not close them.
func (d *DB) Pool(ctx context.Context) (*pgxpool.Pool, error) {
	return d.PoolWithConfig(ctx, d.PoolConfig())
}

// PoolWithConfig is Pool with a caller-adjusted configuration, typically one
// obtained from PoolConfig.
func (d *DB) PoolWithConfig(ctx context.Context, cfg *pgxpool.Config) (*pgxpool.Pool, error) {
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("testdb: open pool on %s: %w", d.name, err)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		pool.Close()
		return nil, fmt.Errorf("testdb: database %s is closed", d.name)
	}
	d.pools = append(d.pools, pool)
	return pool, nil
}

// Connect opens a single connection to the database. The caller closes it;
// Close will terminate it if they do not.
func (d *DB) Connect(ctx context.Context) (*pgx.Conn, error) {
	conn, err := pgx.Connect(ctx, d.url)
	if err != nil {
		return nil, fmt.Errorf("testdb: connect to %s: %w", d.name, err)
	}
	return conn, nil
}

// Close closes pools opened through Pool and drops the database. Remaining
// sessions, including ones held by leaked goroutines, are terminated with
// DROP DATABASE ... WITH (FORCE) after CloseTimeout. Close is idempotent.
func (d *DB) Close(ctx context.Context) error {
	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		return nil
	}
	d.closed = true
	pools := d.pools
	d.pools = nil
	d.mu.Unlock()

	drained := make(chan struct{})
	go func() {
		defer close(drained)
		for _, p := range pools {
			p.Close()
		}
	}()

	select {
	case <-drained:
	case <-time.After(d.cfg.CloseTimeout):
		d.cfg.Logf("testdb: connections to %s still in use after %s (leaked goroutine?); terminating them", d.name, d.cfg.CloseTimeout)
	}

	if err := dropDatabase(ctx, d.cfg, d.name, true); err != nil {
		return err
	}

	select {
	case <-drained:
	case <-time.After(d.cfg.CloseTimeout):
		d.cfg.Logf("testdb: a pool on %s never drained; a goroutine is still holding a connection", d.name)
	}
	return nil
}

// forget marks the DB closed without dropping it or closing pools.
func (d *DB) forget() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.closed = true
	d.pools = nil
}

// Prune drops databases left behind by earlier runs that were killed before
// clean-up, identified by cfg.Prefix and the generated-name shape. Databases
// with live sessions are skipped, so it is safe to run while other test
// processes are using the server. It returns the names it dropped.
func Prune(ctx context.Context, cfg Config) ([]string, error) {
	cfg, err := cfg.withDefaults()
	if err != nil {
		return nil, err
	}

	admin, err := connectAdmin(ctx, cfg)
	if err != nil {
		return nil, err
	}
	defer func() { _ = admin.Close(ctx) }()

	// Databases with sessions are filtered out up front: asking DROP DATABASE
	// to find out costs a five-second wait inside Postgres per database.
	rows, err := admin.Query(ctx,
		`SELECT d.datname FROM pg_database d
		 WHERE d.datname ~ $1 AND NOT d.datistemplate
		   AND NOT EXISTS (SELECT 1 FROM pg_stat_activity a WHERE a.datid = d.oid)
		 ORDER BY d.datname`,
		"^"+regexp.QuoteMeta(cfg.Prefix)+"_[0-9a-f]{"+fmt.Sprint(randomSuffixBytes*2)+"}$")
	if err != nil {
		return nil, fmt.Errorf("testdb: list databases: %w", err)
	}
	names, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("testdb: list databases: %w", err)
	}

	var dropped []string
	for _, name := range names {
		err := dropDatabase(ctx, cfg, name, false)
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "55006" { // object_in_use: a session appeared meanwhile
			continue
		}
		if err != nil {
			return dropped, err
		}
		dropped = append(dropped, name)
	}
	return dropped, nil
}

func connectAdmin(ctx context.Context, cfg Config) (*pgx.Conn, error) {
	url, err := withDatabase(cfg.URL, cfg.MaintenanceDB)
	if err != nil {
		return nil, err
	}
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		return nil, fmt.Errorf("testdb: connect to maintenance database %s: %w", cfg.MaintenanceDB, err)
	}
	return conn, nil
}

func dropDatabase(ctx context.Context, cfg Config, name string, force bool) error {
	admin, err := connectAdmin(ctx, cfg)
	if err != nil {
		return err
	}
	defer func() { _ = admin.Close(ctx) }()

	stmt := "DROP DATABASE IF EXISTS " + pgx.Identifier{name}.Sanitize()
	if force {
		stmt += " WITH (FORCE)"
	}
	if _, err := admin.Exec(ctx, stmt); err != nil {
		return fmt.Errorf("testdb: drop %s: %w", name, err)
	}
	return nil
}

func isUnreachable(err error) bool {
	var connErr *pgconn.ConnectError
	return errors.As(err, &connErr)
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic("testdb: crypto/rand: " + err.Error())
	}
	return hex.EncodeToString(b)
}
