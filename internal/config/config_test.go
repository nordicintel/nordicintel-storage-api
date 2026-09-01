package config

import "testing"

func TestLoadRequiresSecrets(t *testing.T) {
	t.Setenv("DATABASE_URL", "")
	t.Setenv("API_BEARER_TOKEN", "")
	if _, err := Load(); err == nil {
		t.Fatal("expected missing DATABASE_URL to fail")
	}
	t.Setenv("DATABASE_URL", "postgres://example")
	if _, err := Load(); err == nil {
		t.Fatal("expected missing API_BEARER_TOKEN to fail")
	}
}

func TestLoadDefaults(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://example")
	t.Setenv("API_BEARER_TOKEN", "secret")
	t.Setenv("PORT", "")
	t.Setenv("MAX_REQUEST_BYTES", "")
	t.Setenv("MAX_CELLS", "")
	t.Setenv("DB_MAX_CONNS", "")
	t.Setenv("DB_TIMEOUT", "")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Port != "8080" || cfg.MaxCells != 1_000_000 || cfg.MaxRequestBytes != 128<<20 {
		t.Fatalf("unexpected defaults: %#v", cfg)
	}
}
