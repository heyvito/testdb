package testdb

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"
)

// withDatabase returns connString pointing at database name instead of
// whatever database it currently names. Both URL ("postgres://...") and
// keyword/value ("host=... dbname=...") forms are supported.
func withDatabase(connString, name string) (string, error) {
	if isURL(connString) {
		u, err := url.Parse(connString)
		if err != nil {
			return "", fmt.Errorf("parse connection url: %w", err)
		}
		u.Path = "/" + name
		return u.String(), nil
	}

	fields := strings.Fields(connString)
	out := make([]string, 0, len(fields)+1)
	for _, f := range fields {
		if strings.HasPrefix(f, "dbname=") {
			continue
		}
		out = append(out, f)
	}
	out = append(out, "dbname="+quoteKV(name))
	return strings.Join(out, " "), nil
}

// databaseOf returns the database named by connString, or "" when it names none.
func databaseOf(connString string) (string, error) {
	cfg, err := pgconn.ParseConfig(connString)
	if err != nil {
		return "", err
	}
	if isURL(connString) {
		u, err := url.Parse(connString)
		if err != nil {
			return "", err
		}
		if strings.Trim(u.Path, "/") == "" {
			return "", nil
		}
		return cfg.Database, nil
	}
	for f := range strings.FieldsSeq(connString) {
		if strings.HasPrefix(f, "dbname=") {
			return cfg.Database, nil
		}
	}
	return "", nil
}

func isURL(s string) bool {
	return strings.HasPrefix(s, "postgres://") || strings.HasPrefix(s, "postgresql://")
}

func quoteKV(v string) string {
	if v == "" || strings.ContainsAny(v, " '\\") {
		v = strings.NewReplacer(`\`, `\\`, `'`, `\'`).Replace(v)
		return "'" + v + "'"
	}
	return v
}
