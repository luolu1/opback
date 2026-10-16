package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadMissingFileIsOK(t *testing.T) {
	cfg, err := Load(filepath.Join(t.TempDir(), "nope.json"), Config{})
	if err != nil {
		t.Fatalf("missing file should not error: %v", err)
	}
	if cfg.Web.Port != 0 || cfg.Agent.Port != 0 {
		t.Fatalf("unexpected config: %+v", cfg)
	}
}

func TestLoadEmptyFileIsOK(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(p, []byte("  \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(p, Config{}); err != nil {
		t.Fatalf("empty file should not error: %v", err)
	}
}

func TestLoadParsesAndRejectsUnknown(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.json")
	raw := `{"web":{"host":"0.0.0.0","port":9000},"agent":{"port":9001},"enroll_token":"s3cr3t"}`
	if err := os.WriteFile(p, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(p, Config{})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Web.Addr() != "0.0.0.0:9000" {
		t.Fatalf("web addr = %q", cfg.Web.Addr())
	}
	if cfg.Agent.Addr() != ":9001" {
		t.Fatalf("agent addr = %q", cfg.Agent.Addr())
	}
	if cfg.EnrollToken != "s3cr3t" {
		t.Fatalf("enroll token = %q", cfg.EnrollToken)
	}

	if err := os.WriteFile(p, []byte(`{"nope":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(p, Config{}); err == nil {
		t.Fatal("expected unknown-field error")
	}
}

func TestExampleConfigLoads(t *testing.T) {
	// The shipped example must round-trip through Load (DisallowUnknownFields)
	// so a typo in the sample can't silently break real deployments.
	cfg, err := Load("../../deploy/config.example.json", Config{})
	if err != nil {
		t.Fatalf("deploy/config.example.json does not load: %v", err)
	}
	if cfg.Web.Port != 8080 || cfg.Agent.Port != 8081 {
		t.Fatalf("unexpected example ports: web=%d agent=%d", cfg.Web.Port, cfg.Agent.Port)
	}
	if cfg.OpenList.URL == "" {
		t.Fatal("example OpenList URL should be populated")
	}
}

func TestListenAddr(t *testing.T) {
	cases := []struct {
		in   Listen
		want string
	}{
		{Listen{}, ""},
		{Listen{Port: 8080}, ":8080"},
		{Listen{Host: "127.0.0.1", Port: 8081}, "127.0.0.1:8081"},
		{Listen{Host: "127.0.0.1"}, "127.0.0.1:0"},
		{Listen{Port: 70000}, ""},
	}
	for _, c := range cases {
		if got := c.in.Addr(); got != c.want {
			t.Errorf("Listen%+v.Addr() = %q, want %q", c.in, got, c.want)
		}
	}
}
