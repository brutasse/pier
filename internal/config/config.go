package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/brutasse/pier/internal/policy"
)

// Config is the pier configuration.
type Config struct {
	Listen            string   `yaml:"listen"`
	S3                S3Config `yaml:"s3"`
	ImmutableReleases *bool    `yaml:"immutable_releases"`
	MaxUploadBytes    int64    `yaml:"max_upload_bytes"`
	// MaxPullBytes caps the size of objects pulled from upstream.
	// Independent from MaxUploadBytes.
	MaxPullBytes int64        `yaml:"max_pull_bytes"`
	Auth         AuthConfig   `yaml:"auth"`
	Policy       PolicyConfig `yaml:"policy"`
	// Upstream is the ordered list of repositories the pull-through
	// cache fetches from on a local miss. Empty disables pull-through.
	Upstream []Upstream `yaml:"upstream"`
	// ReservedGroups are the groupIds reserved for internal uploads.
	// Paths under them are never pulled from upstream, and uploads
	// outside them are rejected.
	ReservedGroups []string `yaml:"reserved_groups"`
	// MetadataTTL is the freshness window for maven-metadata.xml objects
	// pulled from upstream. When unset it defaults to 24h; an explicit
	// zero disables revalidation (cached metadata is served until it is
	// deleted).
	MetadataTTL *time.Duration `yaml:"metadata_ttl"`
}

// Upstream is a pull-through source repository.
type Upstream struct {
	Name string `yaml:"name"`
	URL  string `yaml:"url"`
	// Username is the basic-auth username for this repository. Together
	// with Password it is empty (unauthenticated fetches) or set.
	Username string `yaml:"username"`
	// Password is the name of an environment variable holding the
	// basic-auth password. Load resolves it; the struct holds the
	// resolved value.
	Password string `yaml:"password"`
}

// IsReserved reports whether group (or any of its subgroups) is reserved
// for internal uploads. Matching is by dot-boundary prefix: reserving
// "org.acme" covers "org.acme" and "org.acme.internal" but not "org.acmeX".
func (c *Config) IsReserved(group string) bool {
	for _, g := range c.ReservedGroups {
		if group == g || strings.HasPrefix(group, g+".") {
			return true
		}
	}
	return false
}

// defaultMetadataTTL is the freshness window for pulled
// maven-metadata.xml when metadata_ttl is unset.
const defaultMetadataTTL = 24 * time.Hour

// MetadataTTLOrDefault returns the configured metadata freshness window,
// defaulting to 24h when unset.
func (c *Config) MetadataTTLOrDefault() time.Duration {
	if c.MetadataTTL != nil {
		return *c.MetadataTTL
	}
	return defaultMetadataTTL
}

// S3Config configures the object storage backend.
type S3Config struct {
	Bucket         string `yaml:"bucket"`
	Prefix         string `yaml:"prefix"`
	Region         string `yaml:"region"`
	Endpoint       string `yaml:"endpoint"`
	ForcePathStyle *bool  `yaml:"force_path_style"`
}

// ForcePathStyleOrDefault returns the path-style flag, defaulting to true
// when a custom endpoint is configured (needed for path-style backends
// such as S3Mock).
func (s *S3Config) ForcePathStyleOrDefault() bool {
	if s.ForcePathStyle != nil {
		return *s.ForcePathStyle
	}
	return s.Endpoint != ""
}

// AuthConfig configures token verification.
type AuthConfig struct {
	// Disabled bypasses authentication entirely. For local development
	// only; never use in production.
	Disabled bool     `yaml:"disabled"`
	Issuers  []Issuer `yaml:"issuers"`
}

// Issuer is a trusted token issuer.
type Issuer struct {
	Name        string   `yaml:"name"`
	WellKnown   string   `yaml:"well_known"`
	ExpectedISS string   `yaml:"expected_iss"`
	Audiences   []string `yaml:"audiences"`
}

// PolicyConfig holds the authorization rules.
type PolicyConfig struct {
	DefaultDeny *bool         `yaml:"default_deny"`
	Rules       []policy.Rule `yaml:"rules"`
}

const (
	defaultMaxUploadBytes = 512 << 20 // 512 MiB
	defaultMaxPullBytes   = 512 << 20 // 512 MiB
)

// Load reads and validates the YAML config at path.
func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Config
	if err := yaml.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if err := c.validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

func (c *Config) validate() error {
	var errs []error
	add := func(format string, args ...any) {
		errs = append(errs, fmt.Errorf(format, args...))
	}

	if c.Listen == "" {
		add("listen is required")
	}
	if c.S3.Bucket == "" {
		add("s3.bucket is required")
	}
	if c.S3.Region == "" {
		add("s3.region is required")
	}
	c.S3.Prefix = strings.Trim(c.S3.Prefix, "/")

	if c.ImmutableReleases == nil {
		t := true
		c.ImmutableReleases = &t
	}
	if c.MaxUploadBytes <= 0 {
		c.MaxUploadBytes = defaultMaxUploadBytes
	}
	if c.MaxPullBytes <= 0 {
		c.MaxPullBytes = defaultMaxPullBytes
	}
	if c.Policy.DefaultDeny == nil {
		t := true
		c.Policy.DefaultDeny = &t
	}

	if !c.Auth.Disabled {
		if len(c.Auth.Issuers) == 0 {
			add("auth.issuers: at least one issuer is required (or set auth.disabled for local development)")
		}
		seen := map[string]bool{}
		for i, is := range c.Auth.Issuers {
			if is.Name == "" {
				add("auth.issuers[%d]: name is required", i)
			} else if seen[is.Name] {
				add("auth.issuers[%d]: duplicate name %q", i, is.Name)
			}
			seen[is.Name] = true
			u, err := url.Parse(is.WellKnown)
			if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
				add("auth.issuers[%d] (%s): well_known must be an http(s) URL", i, is.Name)
			}
			if is.ExpectedISS == "" {
				add("auth.issuers[%d] (%s): expected_iss is required", i, is.Name)
			}
			if len(is.Audiences) == 0 {
				add("auth.issuers[%d] (%s): at least one audience is required", i, is.Name)
			}
		}
	}

	if len(c.Policy.Rules) > 0 {
		if _, err := policy.New(c.Policy.Rules, *c.Policy.DefaultDeny); err != nil {
			errs = append(errs, fmt.Errorf("policy: %w", err))
		}
	}

	seenUp := map[string]bool{}
	for i, up := range c.Upstream {
		if up.Name == "" {
			add("upstream[%d]: name is required", i)
		} else if seenUp[up.Name] {
			add("upstream[%d]: duplicate name %q", i, up.Name)
		}
		seenUp[up.Name] = true
		u, err := url.Parse(up.URL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			add("upstream[%d] (%s): url must be an http(s) URL", i, up.Name)
			continue
		}
		c.Upstream[i].URL = strings.TrimRight(u.String(), "/")
		switch {
		case up.Username == "" && up.Password == "":
			// Unauthenticated upstream.
		case up.Username == "" || up.Password == "":
			add("upstream[%d] (%s): username and password must be set together", i, up.Name)
		default:
			pw, ok := os.LookupEnv(up.Password)
			if !ok {
				add("upstream[%d] (%s): password env var %q is not set", i, up.Name, up.Password)
			} else {
				c.Upstream[i].Password = pw
			}
		}
	}

	for i, g := range c.ReservedGroups {
		if !validGroupID(g) {
			add("reserved_groups[%d]: %q is not a valid groupId", i, g)
		}
	}

	if c.MetadataTTL != nil && *c.MetadataTTL < 0 {
		add("metadata_ttl must be >= 0 (0 disables revalidation)")
	}

	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	return nil
}

// validGroupID reports whether s looks like a Maven groupId: non-empty
// dot-separated segments with no leading, trailing or double dots.
func validGroupID(s string) bool {
	if s == "" || strings.HasPrefix(s, ".") || strings.HasSuffix(s, ".") {
		return false
	}
	for _, seg := range strings.Split(s, ".") {
		if seg == "" {
			return false
		}
	}
	return true
}
