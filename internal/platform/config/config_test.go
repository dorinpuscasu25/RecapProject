package config

import (
	"strings"
	"testing"
)

func TestLoadRejectsOutOfRangeNumbers(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://x@localhost/x")
	t.Setenv("AUTH_ARGON2_PARALLELISM", "300")
	t.Setenv("DB_MAX_CONNS", "-1")
	_, err := Load()
	if err == nil {
		t.Fatal("expected an error for out-of-range values")
	}
	for _, k := range []string{"AUTH_ARGON2_PARALLELISM", "DB_MAX_CONNS"} {
		if !strings.Contains(err.Error(), k) {
			t.Errorf("error does not mention %s: %v", k, err)
		}
	}
}

func TestLoadAcceptsDefaults(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://x@localhost/x")
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.Auth.Argon2Parallelism != 2 || c.DBMaxConns != 20 {
		t.Fatalf("unexpected defaults: %d %d", c.Auth.Argon2Parallelism, c.DBMaxConns)
	}
}
