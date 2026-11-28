package main

import "testing"

func TestLoadConfig(t *testing.T) {
	t.Setenv("DB_DSN", "postgres://collector@example/db")
	t.Setenv("DATABASE_URL", "")
	t.Setenv("RAWPROXY_TOKEN", "token")
	t.Setenv("RAWPROXY_ADDR", "")
	t.Setenv("RAWPROXY_ALLOWED_ORIGINS", "")

	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.DSN != "postgres://collector@example/db" || cfg.Token != "token" ||
		cfg.Addr != defaultAddr || cfg.Origins != defaultOrigins {
		t.Errorf("cfg = %+v", cfg)
	}
}

func TestLoadConfigFallback(t *testing.T) {
	t.Setenv("DB_DSN", "")
	t.Setenv("DATABASE_URL", "postgres://collector@fallback/db")
	t.Setenv("RAWPROXY_TOKEN", "other")
	t.Setenv("RAWPROXY_ADDR", ":9092")
	t.Setenv("RAWPROXY_ALLOWED_ORIGINS", "http://localhost:8080")

	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.DSN != "postgres://collector@fallback/db" || cfg.Addr != ":9092" ||
		cfg.Origins != "http://localhost:8080" {
		t.Errorf("cfg = %+v", cfg)
	}
}

func TestLoadConfigRequiresDSNAndToken(t *testing.T) {
	t.Setenv("DB_DSN", "")
	t.Setenv("DATABASE_URL", "")
	t.Setenv("RAWPROXY_TOKEN", "token")
	if _, err := loadConfig(); err == nil {
		t.Error("accepted missing DSN")
	}

	t.Setenv("DB_DSN", "postgres://collector@example/db")
	t.Setenv("RAWPROXY_TOKEN", "")
	if _, err := loadConfig(); err == nil {
		t.Error("accepted missing token")
	}
}
