package main

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brutasse/pier/internal/api"
	"github.com/brutasse/pier/internal/auth"
	"github.com/brutasse/pier/internal/config"
	"github.com/brutasse/pier/internal/policy"
	"github.com/brutasse/pier/internal/store"
)

// newFakeS3 answers HEAD requests with 200 so store.Ping succeeds.
func newFakeS3(t *testing.T) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(ts.Close)
	return ts
}

func cfgYAML(endpoint, listen, rule string) string {
	return "listen: " + listen + "\n" +
		"s3:\n" +
		"  bucket: b\n" +
		"  region: us-east-1\n" +
		"  endpoint: " + endpoint + "\n" +
		"auth:\n" +
		"  disabled: true\n" +
		"policy:\n" +
		"  rules:\n" + rule
}

func writeCfg(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "pier.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func newServer(t *testing.T, path string) (*api.Server, string) {
	t.Helper()
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.NewS3(context.Background(), cfg.S3)
	if err != nil {
		t.Fatal(err)
	}
	pol, err := policy.New(cfg.Policy.Rules, *cfg.Policy.DefaultDeny)
	if err != nil {
		t.Fatal(err)
	}
	up := newUpstream(cfg)
	if up != nil {
		t.Fatal("newUpstream: nil expected with no upstream configured")
	}
	srv := api.New(cfg, st, auth.NewVerifier(nil), pol, up, slog.New(slog.NewTextHandler(io.Discard, nil)))
	return srv, cfg.Listen
}

func TestNewUpstream(t *testing.T) {
	cfg := &config.Config{
		MaxPullBytes: 42,
		Upstream:     []config.Upstream{{Name: "a", URL: "https://x"}},
	}
	if up := newUpstream(cfg); up == nil {
		t.Fatal("newUpstream: nil, want non-nil")
	}
}

func TestReload(t *testing.T) {
	// store.Ping signs with the default credential chain; use dummies so the
	// test does not depend on ambient AWS credentials.
	t.Setenv("AWS_ACCESS_KEY_ID", "test")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test")
	s3 := newFakeS3(t)
	ruleA := "    - name: r1\n      action: [download]\n      when: claims.sub == \"ops\"\n"
	ruleB := "    - name: r2\n      action: [download]\n      when: claims.sub == \"ops\"\n"
	path := writeCfg(t, cfgYAML(s3.URL, `"127.0.0.1:9"`, ruleA))
	srv, listen := newServer(t, path)

	// A valid change reloads.
	if err := os.WriteFile(path, []byte(cfgYAML(s3.URL, `"127.0.0.1:9"`, ruleB)), 0o600); err != nil {
		t.Fatal(err)
	}
	newCfg, err := reload(path, listen, srv)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if newCfg.Policy.Rules[0].Name != "r2" {
		t.Fatalf("rule = %q, want r2", newCfg.Policy.Rules[0].Name)
	}

	// A listen change is rejected.
	if err := os.WriteFile(path, []byte(cfgYAML(s3.URL, `"127.0.0.1:10"`, ruleA)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := reload(path, listen, srv); err == nil || !strings.Contains(err.Error(), "listen") {
		t.Fatalf("listen change: err = %v, want listen error", err)
	}

	// Bad CEL is rejected.
	bad := cfgYAML(s3.URL, `"127.0.0.1:9"`, "    - name: bad\n      action: [download]\n      when: claims.sub == \n")
	if err := os.WriteFile(path, []byte(bad), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := reload(path, listen, srv); err == nil {
		t.Fatal("bad CEL: want error, got nil")
	}

	// A missing file is rejected.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, err := reload(path, listen, srv); err == nil {
		t.Fatal("missing file: want error, got nil")
	}
}
