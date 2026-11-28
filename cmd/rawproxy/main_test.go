package main

import "testing"

func TestLoadConfig(t *testing.T) {
	t.Setenv("DB_DSN", "postgres://collector@example/db")
	t.Setenv("DATABASE_URL", "")
	t.Setenv("RAWPROXY_ADDR", "")

	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.DSN != "postgres://collector@example/db" || cfg.Addr != defaultAddr {
		t.Errorf("cfg = %+v", cfg)
	}
}

func TestLoadConfigFallback(t *testing.T) {
	t.Setenv("DB_DSN", "")
	t.Setenv("DATABASE_URL", "postgres://collector@fallback/db")
	t.Setenv("RAWPROXY_ADDR", ":9092")

	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.DSN != "postgres://collector@fallback/db" || cfg.Addr != ":9092" {
		t.Errorf("cfg = %+v", cfg)
	}
}

func TestLoadConfigRequiresDSN(t *testing.T) {
	t.Setenv("DB_DSN", "")
	t.Setenv("DATABASE_URL", "")
	if _, err := loadConfig(); err == nil {
		t.Error("accepted missing DSN")
	}
}
