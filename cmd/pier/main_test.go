package main

import (
	"bytes"
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

func TestAdminServers(t *testing.T) {
	s3 := newFakeS3(t)
	rule := "    - name: r1\n      action: [download]\n      when: claims.sub == \"ops\"\n"
	path := writeCfg(t, cfgYAML(s3.URL, `"127.0.0.1:9"`, rule))
	srv, _ := newServer(t, path)

	// No flags: no admin servers.
	if got := adminServers(0, 0, srv); len(got) != 0 {
		t.Fatalf("no ports: %d servers, want 0", len(got))
	}

	// One port per endpoint.
	got := adminServers(9090, 9091, srv)
	if len(got) != 2 || got[0].Addr != ":9090" || got[1].Addr != ":9091" {
		t.Fatalf("two ports: %+v", got)
	}

	// Same port: one shared server.
	got = adminServers(9090, 9090, srv)
	if len(got) != 1 || got[0].Addr != ":9090" {
		t.Fatalf("same port: %+v", got)
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

func TestVersionOutput(t *testing.T) {
	var out bytes.Buffer
	rootCmd.SetOut(&out)
	rootCmd.SetErr(&out)
	rootCmd.SetArgs([]string{"--version"})
	if err := rootCmd.Execute(); err != nil {
		t.Fatalf("execute --version: %v", err)
	}
	if got := out.String(); got != "pier dev\n" {
		t.Fatalf("version output = %q, want %q", got, "pier dev\n")
	}
	// The version flag is parsed on the shared rootCmd; reset it so later
	// tests do not inherit the parsed value.
	if err := rootCmd.Flags().Set("version", "false"); err != nil {
		t.Fatal(err)
	}
}

func TestBareRootShowsHelp(t *testing.T) {
	var out bytes.Buffer
	rootCmd.SetOut(&out)
	rootCmd.SetErr(&out)
	rootCmd.SetArgs(nil)
	if err := rootCmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !strings.Contains(out.String(), "serve") {
		t.Fatalf("help does not list serve:\n%s", out.String())
	}
	if f := serveCmd.Flags().Lookup("config"); f == nil {
		t.Fatal("serve has no -config flag")
	}
}
