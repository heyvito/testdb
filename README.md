# testdb

One PostgreSQL database per test, cloned from a template. 

Rails-style transactional tests break as soon as the code under test starts a
goroutine: that goroutine gets its own connection and cannot see the test's
uncommitted rows. `testdb` sidesteps this by giving each test a real database.
Any number of connections, pools and goroutines may touch it. When the test
ends the database is dropped, and any session still attached to it is
terminated.

```go
import "github.com/heyvito/testdb"

func TestOrders(t *testing.T) {
    db := testdb.New(t, testdb.Config{})

    pool, err := db.Pool(t.Context()) // *pgxpool.Pool, closed for you
    if err != nil {
        t.Fatal(err)
    }
    svc := orders.New(pool)
    // ...
}
```

## How it works

1. Migrate a database to the schema the tests expect. `testdb` never
   runs migrations; it only clones.
2. `New` runs `CREATE DATABASE testdb_<random> TEMPLATE <original>` from a
   maintenance connection. On a small schema this takes tens of milliseconds.
3. The test gets a connection string, a `*pgxpool.Config`, or a ready pool.
4. On clean-up, pools opened through `testdb` are closed, then
   `DROP DATABASE ... WITH (FORCE)` removes the clone and kills any leftover
   session. A pool that will not drain within `CloseTimeout` is reported and
   does not hang the test.

## Configuration

Everything has a default. The zero `Config` reads two environment variables:

| Variable          | Meaning                                                              |
|-------------------|----------------------------------------------------------------------|
| `TESTDB_URL`      | Server connection string. The database it names is the template.     |
| `TESTDB_TEMPLATE` | Overrides the template name.                                         |

Without them it uses `postgres://postgres:postgres@localhost:5432/postgres`,
which has no template and fails, so set at least `TESTDB_URL`, for example
`postgres://postgres:postgres@localhost:5432/myapp?sslmode=disable`.

Fields on `Config`:

- `URL`, `Template`: as above.
- `MaintenanceDB`: where admin sessions connect. Defaults to `postgres`. It
  must differ from the template, because Postgres refuses to clone a database
  that has other sessions attached.
- `Prefix`: start of generated names. Defaults to `testdb`.
- `MaxConns`: pool size for `Pool` and `PoolConfig`. Defaults to 4. Keep it
  small under `t.Parallel()`, since every test owns a pool.
- `KeepOnFailure`: leave the database in place when the test fails and log
  its URL.
- `SkipIfUnreachable`: skip instead of fail when the server is down.
- `CloseTimeout`: how long to wait for pools to drain before forcing.
  Defaults to 5s.

## Fitting an existing project

Most applications construct their pool from their own config type. Add a
constructor that accepts a `*pgxpool.Config` or `*pgxpool.Pool`, then in
tests:

```go
db := testdb.New(t, testdb.Config{})
cfg := db.PoolConfig()
cfg.ConnConfig.Tracer = myTracer
pool, _ := db.PoolWithConfig(t.Context(), cfg)
client := myapp.NewClientFromPool(pool)
```

Reader and writer pools can both point at the same clone.

## Requirements and caveats

- PostgreSQL 13 or newer, for `DROP DATABASE ... WITH (FORCE)`.
- The role in `URL` needs `CREATEDB` and must own the template or be a
  superuser.
- Nothing else may be connected to the template while tests run. A running
  app or an open `psql` against it makes `New` fail with a clear
  `object_in_use` error, after a five-second wait that Postgres imposes.
- Runs killed before clean-up leave `testdb_*` databases behind. Drop them
  with `testdb.Prune`, which skips databases that currently have sessions.
- `testdb.Create` and `DB.Close` are the non-`testing` variants for
  `TestMain` setups and tools.
