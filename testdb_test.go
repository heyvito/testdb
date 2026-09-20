package testdb

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// serverURL is the maintenance connection used to build the fixture template.
var serverURL = func() string {
	if v := os.Getenv(EnvURL); v != "" {
		return v
	}
	return defaultURL
}()

// fixtureTemplate is created once per test binary and dropped afterwards.
var fixtureTemplate string

func TestMain(m *testing.M) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	admin, err := pgx.Connect(ctx, serverURL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "postgres not reachable at %s (%v); running only unit tests\n", serverURL, err)
		os.Exit(m.Run())
	}

	fixtureTemplate = "testdb_fixture_" + randomHex(4)
	ident := pgx.Identifier{fixtureTemplate}.Sanitize()
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+ident); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	tmplURL, _ := withDatabase(serverURL, fixtureTemplate)
	tmpl, err := pgx.Connect(ctx, tmplURL)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	_, err = tmpl.Exec(ctx, `CREATE TABLE things (id serial PRIMARY KEY, name text NOT NULL); INSERT INTO things (name) VALUES ('seed')`)
	tmpl.Close(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	code := m.Run()

	if _, err := admin.Exec(ctx, "DROP DATABASE IF EXISTS "+ident+" WITH (FORCE)"); err != nil {
		fmt.Fprintln(os.Stderr, err)
	}
	admin.Close(ctx)
	os.Exit(code)
}

func requireServer(t *testing.T) Config {
	t.Helper()
	if fixtureTemplate == "" {
		t.Skip("postgres not reachable")
	}
	return Config{URL: serverURL, Template: fixtureTemplate, Prefix: "testdb_t"}
}

func databaseExists(t *testing.T, name string) bool {
	t.Helper()
	conn, err := pgx.Connect(t.Context(), serverURL)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(t.Context())
	var exists bool
	if err := conn.QueryRow(t.Context(), `SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = $1)`, name).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	return exists
}

func TestNewIsolatesAndDrops(t *testing.T) {
	cfg := requireServer(t)

	var name string
	t.Run("inner", func(t *testing.T) {
		t.Parallel()
		a := New(t, cfg)
		b := New(t, cfg)
		name = a.Name()

		if a.Name() == b.Name() {
			t.Fatal("two databases share a name")
		}
		if !databaseExists(t, a.Name()) {
			t.Fatalf("%s not created", a.Name())
		}

		pa, err := a.Pool(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		pb, err := b.Pool(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pa.Exec(t.Context(), `INSERT INTO things (name) VALUES ('only in a')`); err != nil {
			t.Fatal(err)
		}

		count := func(q interface {
			QueryRow(context.Context, string, ...any) pgx.Row
		}) int {
			var n int
			if err := q.QueryRow(t.Context(), `SELECT count(*) FROM things`).Scan(&n); err != nil {
				t.Fatal(err)
			}
			return n
		}
		if got := count(pa); got != 2 {
			t.Errorf("a has %d rows, want 2", got)
		}
		if got := count(pb); got != 1 {
			t.Errorf("b has %d rows, want 1 (template seed only)", got)
		}
	})

	if databaseExists(t, name) {
		t.Errorf("%s still exists after test cleanup", name)
	}
}

func TestCloseTerminatesLeakedConnections(t *testing.T) {
	cfg := requireServer(t)
	cfg.CloseTimeout = 200 * time.Millisecond
	var warnings []string
	cfg.Logf = func(f string, a ...any) { warnings = append(warnings, fmt.Sprintf(f, a...)) }

	db, err := Create(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	pool, err := db.Pool(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	// A "goroutine" that acquired a connection and never gives it back.
	leaked, err := pool.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	// And a raw connection outside any pool.
	raw, err := db.Connect(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	if err := db.Close(t.Context()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("Close took %s; should not hang on leaked connections", took)
	}
	if databaseExists(t, db.Name()) {
		t.Errorf("%s still exists after Close", db.Name())
	}
	if len(warnings) == 0 {
		t.Error("expected a leaked-connection warning")
	}

	// Both sessions were terminated server-side.
	if _, err := raw.Exec(t.Context(), `SELECT 1`); err == nil {
		t.Error("raw connection survived DROP DATABASE WITH (FORCE)")
	}
	if _, err := leaked.Exec(t.Context(), `SELECT 1`); err == nil {
		t.Error("leaked pooled connection survived DROP DATABASE WITH (FORCE)")
	}
	leaked.Release()

	if err := db.Close(t.Context()); err != nil {
		t.Errorf("second Close should be a no-op, got %v", err)
	}
	if _, err := db.Pool(t.Context()); err == nil {
		t.Error("Pool after Close should fail")
	}
}

func TestPrune(t *testing.T) {
	cfg := requireServer(t)
	cfg.Prefix = "testdb_prune_" + randomHex(2)

	orphan, err := Create(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	live, err := Create(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = live.Close(context.Background()) })
	conn, err := live.Connect(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.Background())

	dropped, err := Prune(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(dropped) != 1 || dropped[0] != orphan.Name() {
		t.Errorf("Prune dropped %v, want [%s]", dropped, orphan.Name())
	}
	if databaseExists(t, orphan.Name()) {
		t.Error("orphan survived Prune")
	}
	if !databaseExists(t, live.Name()) {
		t.Error("Prune dropped a database with a live session")
	}
}

func TestCreateReportsTemplateInUse(t *testing.T) {
	cfg := requireServer(t)

	tmplURL, _ := withDatabase(serverURL, fixtureTemplate)
	hold, err := pgx.Connect(t.Context(), tmplURL)
	if err != nil {
		t.Fatal(err)
	}
	defer hold.Close(context.Background())

	_, err = Create(t.Context(), cfg)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "55006" {
		t.Fatalf("expected object_in_use error, got %v", err)
	}
}

func TestNewSkipsWhenUnreachable(t *testing.T) {
	cfg := Config{URL: "postgres://nobody:nothing@127.0.0.1:1/asra?sslmode=disable", SkipIfUnreachable: true}
	fake := &recordingTB{TB: t}
	done := make(chan struct{})
	go func() {
		defer close(done)
		New(fake, cfg) // Skipf ends this goroutine via runtime.Goexit, like the real thing.
	}()
	<-done
	if !fake.skipped {
		t.Error("expected New to skip when the server is unreachable")
	}
}

type recordingTB struct {
	testing.TB
	skipped bool
}

func (r *recordingTB) Helper() {}
func (r *recordingTB) Skipf(string, ...any) {
	r.skipped = true
	runtimeGoexit() // trust me
}
func (r *recordingTB) Fatalf(f string, a ...any) {
	r.TB.Fatalf("New called Fatalf instead of Skipf: "+f, a...)
}
