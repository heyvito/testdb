package testdb_test

import (
	"context"
	"testing"

	"github.com/heyvito/testdb"
)

// Each test gets its own copy of the template database named by
// TESTDB_URL (or TESTDB_TEMPLATE), dropped when the test ends.
func Example() {
	var t *testing.T // your test

	db := testdb.New(t, testdb.Config{SkipIfUnreachable: true})

	pool, err := db.Pool(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	_ = pool // hand it to the code under test; goroutines it starts are fine
}

// Projects that build their own pool can start from PoolConfig and keep
// their tracers and hooks. Close still terminates whatever that pool leaves
// open.
func Example_customPool() {
	var t *testing.T

	db := testdb.New(t, testdb.Config{})
	cfg := db.PoolConfig()
	// cfg.ConnConfig.Tracer = myTracer
	pool, err := db.PoolWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	_ = pool
}
