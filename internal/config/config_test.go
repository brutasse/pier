package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const valid = `
listen: ":8080"
s3:
  bucket: my-bucket
  prefix: maven/
  region: ch-gva-2
auth:
  issuers:
    - name: github
      well_known: https://actions.githubusercontent.com/.well-known/openid-configuration
      expected_iss: https://token.actions.githubusercontent.com
      audiences:
        - https://maven.example.com
policy:
  rules:
    - name: publish
      action: [upload]
      when: claims.repository == "acme/widget"
    - name: read
      action: [download]
      when: claims.repository in ["acme/widget"]
`

func writeConfig(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadValid(t *testing.T) {
	c, err := Load(writeConfig(t, valid))
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != ":8080" {
		t.Errorf("listen = %q", c.Listen)
	}
	if c.S3.Prefix != "maven" {
		t.Errorf("prefix not trimmed: %q", c.S3.Prefix)
	}
	if c.ImmutableReleases == nil || !*c.ImmutableReleases {
		t.Errorf("immutable_releases default should be true")
	}
	if c.MaxUploadBytes != 512<<20 {
		t.Errorf("max_upload_bytes default = %d", c.MaxUploadBytes)
	}
	if c.MaxPullBytes != 512<<20 {
		t.Errorf("max_pull_bytes default = %d", c.MaxPullBytes)
	}
	if c.Policy.DefaultDeny == nil || !*c.Policy.DefaultDeny {
		t.Errorf("default_deny should default to true")
	}
}

func TestLoadErrors(t *testing.T) {
	cases := map[string]string{
		"no listen":  strings.Replace(valid, "listen: \":8080\"", "listen: \"\"", 1),
		"no bucket":  strings.Replace(valid, "bucket: my-bucket", "bucket: \"\"", 1),
		"no region":  "listen: \":8080\"\ns3:\n  bucket: b\nauth:\n  issuers:\n    - name: i\n      well_known: https://x.example/well-known\n      expected_iss: https://x.example\n      audiences: [a]\n",
		"bad issuer": strings.Replace(valid, "well_known: https://actions.githubusercontent.com/.well-known/openid-configuration", "well_known: not-a-url", 1),
		"no audiences": `
listen: ":8080"
s3:
  bucket: b
  region: r
auth:
  issuers:
    - name: i
      well_known: https://x.example/well-known
      expected_iss: https://x.example
policy:
  rules:
    - name: r
      when: "true"
`,
		"bad cel": strings.Replace(valid, `when: claims.repository == "acme/widget"`, "when: not( valid", 1),
		"no issuers": `
listen: ":8080"
s3:
  bucket: b
  region: r
`,
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Load(writeConfig(t, content)); err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func TestLoadDisabledAuth(t *testing.T) {
	// auth.disabled=true allows omitting issuers entirely (local development).
	c, err := Load(writeConfig(t, `
listen: ":8080"
s3:
  bucket: b
  region: r
auth:
  disabled: true
`))
	if err != nil {
		t.Fatal(err)
	}
	if !c.Auth.Disabled {
		t.Error("auth.disabled should be true")
	}
}

func TestLoadUpstreamAndReserved(t *testing.T) {
	c, err := Load(writeConfig(t, `
listen: ":8080"
s3:
  bucket: b
  region: r
auth:
  disabled: true
upstream:
  - name: maven-central
    url: https://repo1.maven.org/maven2/
  - name: clojars
    url: https://repo.clojars.org
reserved_groups:
  - org.example.internal
`))
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Upstream) != 2 {
		t.Fatalf("upstream len = %d", len(c.Upstream))
	}
	if c.Upstream[0].URL != "https://repo1.maven.org/maven2" {
		t.Errorf("trailing slash not trimmed: %q", c.Upstream[0].URL)
	}
	if c.Upstream[1].URL != "https://repo.clojars.org" {
		t.Errorf("url = %q", c.Upstream[1].URL)
	}
}

func TestLoadUpstreamErrors(t *testing.T) {
	base := `
listen: ":8080"
s3:
  bucket: b
  region: r
auth:
  disabled: true
upstream:
`
	cases := map[string]string{
		"no name":       base + "  - url: https://x.example\n",
		"bad url":       base + "  - name: a\n    url: not-a-url\n",
		"duplicate":     base + "  - name: a\n    url: https://x.example\n  - name: a\n    url: https://y.example\n",
		"user only":     base + "  - name: a\n    url: https://x.example\n    username: u\n",
		"password only": base + "  - name: a\n    url: https://x.example\n    password: PIER_TEST_UNSET_PASSWORD\n",
		"env unset":     base + "  - name: a\n    url: https://x.example\n    username: u\n    password: PIER_TEST_UNSET_PASSWORD\n",
		"bad group":     base + "  - name: a\n    url: https://x.example\nreserved_groups: [\".\"]\n",
		"bad group 2":   base + "  - name: a\n    url: https://x.example\nreserved_groups: [\"a..b\"]\n",
		"bad group 3":   base + "  - name: a\n    url: https://x.example\nreserved_groups: [\"a.\"]\n",
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			os.Unsetenv("PIER_TEST_UNSET_PASSWORD")
			if _, err := Load(writeConfig(t, content)); err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func TestLoadUpstreamBasicAuth(t *testing.T) {
	t.Setenv("PIER_TEST_UPSTREAM_PASSWORD", "s3cret")
	c, err := Load(writeConfig(t, `
listen: ":8080"
s3:
  bucket: b
  region: r
auth:
  disabled: true
upstream:
  - name: private
    url: https://artifactory.example.com/repo
    username: puller
    password: PIER_TEST_UPSTREAM_PASSWORD
`))
	if err != nil {
		t.Fatal(err)
	}
	if c.Upstream[0].Username != "puller" {
		t.Errorf("username = %q, want %q", c.Upstream[0].Username, "puller")
	}
	if c.Upstream[0].Password != "s3cret" {
		t.Errorf("password not resolved from env var: %q", c.Upstream[0].Password)
	}
}

func TestLoadMetadataTTL(t *testing.T) {
	// Unset: defaults to 24h.
	c, err := Load(writeConfig(t, `
listen: ":8080"
s3:
  bucket: b
  region: r
auth:
  disabled: true
`))
	if err != nil {
		t.Fatal(err)
	}
	if c.MetadataTTL != nil {
		t.Fatalf("metadata_ttl should be unset, got %v", *c.MetadataTTL)
	}
	if got := c.MetadataTTLOrDefault(); got != 24*time.Hour {
		t.Fatalf("default = %v, want 24h", got)
	}

	// Explicit value: honored.
	c, err = Load(writeConfig(t, `
listen: ":8080"
s3:
  bucket: b
  region: r
auth:
  disabled: true
metadata_ttl: 6h
`))
	if err != nil {
		t.Fatal(err)
	}
	if c.MetadataTTL == nil || *c.MetadataTTL != 6*time.Hour {
		t.Fatalf("metadata_ttl = %v, want 6h", c.MetadataTTL)
	}

	// Explicit zero: disables revalidation (not the default).
	c, err = Load(writeConfig(t, `
listen: ":8080"
s3:
  bucket: b
  region: r
auth:
  disabled: true
metadata_ttl: 0s
`))
	if err != nil {
		t.Fatal(err)
	}
	if c.MetadataTTL == nil || *c.MetadataTTL != 0 {
		t.Fatalf("metadata_ttl = %v, want 0", c.MetadataTTL)
	}
	if got := c.MetadataTTLOrDefault(); got != 0 {
		t.Fatalf("explicit zero must stay zero: %v", got)
	}

	// Negative: rejected.
	if _, err := Load(writeConfig(t, `
listen: ":8080"
s3:
  bucket: b
  region: r
auth:
  disabled: true
metadata_ttl: -1h
`)); err == nil {
		t.Fatal("expected error for negative metadata_ttl")
	}
}

func TestLoadMaxPullBytes(t *testing.T) {
	// Unset: defaults to 512 MiB, independently of max_upload_bytes.
	c, err := Load(writeConfig(t, `
listen: ":8080"
s3:
  bucket: b
  region: r
auth:
  disabled: true
max_upload_bytes: 1024
`))
	if err != nil {
		t.Fatal(err)
	}
	if c.MaxPullBytes != 512<<20 {
		t.Fatalf("max_pull_bytes default = %d, want 512 MiB", c.MaxPullBytes)
	}
	if c.MaxUploadBytes != 1024 {
		t.Fatalf("max_upload_bytes = %d, want 1024", c.MaxUploadBytes)
	}

	// Explicit value: honored.
	c, err = Load(writeConfig(t, `
listen: ":8080"
s3:
  bucket: b
  region: r
auth:
  disabled: true
max_pull_bytes: 1048576
`))
	if err != nil {
		t.Fatal(err)
	}
	if c.MaxPullBytes != 1048576 {
		t.Fatalf("max_pull_bytes = %d, want 1048576", c.MaxPullBytes)
	}
}

func TestLoadAdminPorts(t *testing.T) {
	// Unset: both disabled.
	c, err := Load(writeConfig(t, `
listen: ":8080"
s3:
  bucket: b
  region: r
auth:
  disabled: true
`))
	if err != nil {
		t.Fatal(err)
	}
	if c.MetricsPort != 0 || c.PprofPort != 0 {
		t.Fatalf("ports = %d, %d, want 0, 0", c.MetricsPort, c.PprofPort)
	}

	// Explicit values: honored.
	c, err = Load(writeConfig(t, `
listen: ":8080"
s3:
  bucket: b
  region: r
auth:
  disabled: true
metrics_port: 9090
pprof_port: 9091
`))
	if err != nil {
		t.Fatal(err)
	}
	if c.MetricsPort != 9090 || c.PprofPort != 9091 {
		t.Fatalf("ports = %d, %d, want 9090, 9091", c.MetricsPort, c.PprofPort)
	}

	// Out of range: rejected.
	for _, port := range []string{"metrics_port: -1", "metrics_port: 65536", "pprof_port: -1", "pprof_port: 65536"} {
		if _, err := Load(writeConfig(t, `
listen: ":8080"
s3:
  bucket: b
  region: r
auth:
  disabled: true
`+port+"\n")); err == nil {
			t.Fatalf("expected error for %s", port)
		}
	}

	// Boundaries: accepted.
	for _, port := range []string{"metrics_port: 65535", "pprof_port: 1"} {
		if _, err := Load(writeConfig(t, `
listen: ":8080"
s3:
  bucket: b
  region: r
auth:
  disabled: true
`+port+"\n")); err != nil {
			t.Fatalf("%s: %v", port, err)
		}
	}
}

func TestIsReserved(t *testing.T) {
	c := &Config{ReservedGroups: []string{"org.acme"}}
	cases := map[string]bool{
		"org.acme":          true,
		"org.acme.internal": true,
		"org.acme.a.b":      true,
		"org.acmeX":         false,
		"org":               false,
		"com.other":         false,
		"":                  false,
	}
	for g, want := range cases {
		if got := c.IsReserved(g); got != want {
			t.Errorf("IsReserved(%q) = %v, want %v", g, got, want)
		}
	}
}

func TestForcePathStyleDefault(t *testing.T) {
	c := S3Config{Endpoint: "http://s3mock:9090"}
	if !c.ForcePathStyleOrDefault() {
		t.Error("force_path_style should default to true with a custom endpoint")
	}
	c = S3Config{}
	if c.ForcePathStyleOrDefault() {
		t.Error("force_path_style should default to false without an endpoint")
	}
	c = S3Config{Endpoint: "http://s3mock:9090", ForcePathStyle: ptr(false)}
	if c.ForcePathStyleOrDefault() {
		t.Error("explicit false must win")
	}
}

func ptr(b bool) *bool { return &b }
