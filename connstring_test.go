package testdb

import "testing"

func TestWithDatabase(t *testing.T) {
	cases := []struct{ in, name, want string }{
		{"postgres://u:p@h:5432/asra?sslmode=disable", "x_1", "postgres://u:p@h:5432/x_1?sslmode=disable"},
		{"postgresql://u:p@h/", "x_1", "postgresql://u:p@h/x_1"},
		{"postgres://h", "x_1", "postgres://h/x_1"},
		{"host=h port=5432 dbname=asra user=u", "x_1", "host=h port=5432 user=u dbname=x_1"},
		{"host=h user=u", "x_1", "host=h user=u dbname=x_1"},
	}
	for _, c := range cases {
		got, err := withDatabase(c.in, c.name)
		if err != nil {
			t.Fatalf("%s: %v", c.in, err)
		}
		if got != c.want {
			t.Errorf("withDatabase(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestDatabaseOf(t *testing.T) {
	cases := []struct{ in, want string }{
		{"postgres://u:p@h:5432/asra?sslmode=disable", "asra"},
		{"postgres://u:p@h:5432/", ""},
		{"postgres://u:p@h:5432", ""},
		{"host=h dbname=asra", "asra"},
		{"host=h", ""},
	}
	for _, c := range cases {
		got, err := databaseOf(c.in)
		if err != nil {
			t.Fatalf("%s: %v", c.in, err)
		}
		if got != c.want {
			t.Errorf("databaseOf(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestConfigDefaults(t *testing.T) {
	t.Setenv(EnvURL, "")
	t.Setenv(EnvTemplate, "")

	c, err := Config{URL: "postgres://u:p@h/asra"}.withDefaults()
	if err != nil {
		t.Fatal(err)
	}
	if c.Template != "asra" || c.MaintenanceDB != "postgres" || c.Prefix != "testdb" || c.MaxConns != 4 {
		t.Errorf("unexpected defaults: %+v", c)
	}

	t.Setenv(EnvTemplate, "other")
	c, err = Config{URL: "postgres://u:p@h/asra"}.withDefaults()
	if err != nil || c.Template != "other" {
		t.Errorf("env template not honoured: %+v %v", c, err)
	}

	t.Setenv(EnvTemplate, "")
	if _, err := (Config{URL: "postgres://u:p@h/postgres"}).withDefaults(); err == nil {
		t.Error("expected error when template equals maintenance database")
	}
	if _, err := (Config{URL: "postgres://u:p@h/"}).withDefaults(); err == nil {
		t.Error("expected error when no template can be derived")
	}
	if _, err := (Config{URL: "postgres://u:p@h/asra", Prefix: "Bad-Prefix"}).withDefaults(); err == nil {
		t.Error("expected error for invalid prefix")
	}
}
