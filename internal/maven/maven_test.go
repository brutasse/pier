package maven

import (
	"testing"
)

func TestParse(t *testing.T) {
	cases := []struct {
		raw      string
		kind     Kind
		group    string
		artifact string
		version  string
		ext      string
		algo     string
		snapshot bool
		wantErr  bool
	}{
		{"", KindUnknown, "", "", "", "", "", false, false},
		{"com", KindUnknown, "", "", "", "", "", false, false},
		{"com/example", KindUnknown, "", "", "", "", "", false, false},
		{"com/example/foo", KindUnknown, "", "", "", "", "", false, false},
		{
			"com/example/foo/1.0.0/foo-1.0.0.jar",
			KindArtifact, "com.example", "foo", "1.0.0", "jar", "", false, false,
		},
		{
			"com/example/foo/1.0.0/foo-1.0.0.pom",
			KindArtifact, "com.example", "foo", "1.0.0", "pom", "", false, false,
		},
		{
			"com/example/foo/1.0.0/foo-1.0.0-sources.jar",
			KindArtifact, "com.example", "foo", "1.0.0", "jar", "", false, false,
		},
		{
			"io/example/very/deep/art/2.5.1.RC1/art-2.5.1.RC1.war",
			KindArtifact, "io.example.very.deep", "art", "2.5.1.RC1", "war", "", false, false,
		},
		{
			"com/example/foo/1.0.0/foo-1.0.0.jar.sha1",
			KindChecksum, "com.example", "foo", "1.0.0", "jar", "sha1", false, false,
		},
		{
			"com/example/foo/1.0.0/foo-1.0.0.jar.md5",
			KindChecksum, "com.example", "foo", "1.0.0", "jar", "md5", false, false,
		},
		{
			"com/example/foo/1.0.0/foo-1.0.0-sources.jar.sha256",
			KindChecksum, "com.example", "foo", "1.0.0", "jar", "sha256", false, false,
		},
		{
			"com/example/foo/1.0.0/foo-1.0.0.jar.sha512",
			KindChecksum, "com.example", "foo", "1.0.0", "jar", "sha512", false, false,
		},
		{
			"com/example/foo/1.0.0-SNAPSHOT/foo-1.0.0-20260919.213000-1.jar",
			KindArtifact, "com.example", "foo", "1.0.0-SNAPSHOT", "jar", "", true, false,
		},
		{
			"com/example/foo/1.0.0-SNAPSHOT/foo-1.0.0-20260919.213000-1.pom",
			KindArtifact, "com.example", "foo", "1.0.0-SNAPSHOT", "pom", "", true, false,
		},
		{
			"com/example/foo/1.0.0-SNAPSHOT/foo-1.0.0-20260919.213000-1.jar.sha1",
			KindChecksum, "com.example", "foo", "1.0.0-SNAPSHOT", "jar", "sha1", true, false,
		},
		{
			"com/example/foo/maven-metadata.xml",
			KindMetadata, "com.example", "foo", "", "", "", false, false,
		},
		{
			"org/apache/maven/plugins/maven-deploy-plugin/maven-metadata.xml",
			KindMetadata, "org.apache.maven.plugins", "maven-deploy-plugin", "", "", "", false, false,
		},
		{
			"com/example/foo/maven-metadata.xml.sha1",
			KindMetadataChecksum, "com.example", "foo", "", "", "sha1", false, false,
		},
		{
			"a/b/1.0.0/b-1.0.0.jar", // single-segment group
			KindArtifact, "a", "b", "1.0.0", "jar", "", false, false,
		},
		// malformed
		{"com/example/foo/1.0.0/bar-2.0.0.jar", 0, "", "", "", "", "", false, true},
		{"com/example/foo/1.0.0/foo-9.9.9.jar", 0, "", "", "", "", "", false, true},
		{"com/example/foo/1.0.0/noext", 0, "", "", "", "", "", false, true},
		{"com/example/foo/1.0.0/foo-1.0.0.jar.notanalgo", 0, "", "", "", "", "", false, true},
		{"com/../etc/passwd", 0, "", "", "", "", "", false, true},
		{"com//example", 0, "", "", "", "", "", false, true},
		{"./leading", 0, "", "", "", "", "", false, true},
	}
	for _, c := range cases {
		t.Run(c.raw, func(t *testing.T) {
			got, err := Parse(c.raw)
			if c.wantErr {
				if err == nil {
					t.Fatalf("Parse(%q) = %+v, want error", c.raw, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("Parse(%q) error: %v", c.raw, err)
			}
			if got.Kind != c.kind {
				t.Errorf("kind = %v, want %v", got.Kind, c.kind)
			}
			if got.Group != c.group {
				t.Errorf("group = %q, want %q", got.Group, c.group)
			}
			if got.Artifact != c.artifact {
				t.Errorf("artifact = %q, want %q", got.Artifact, c.artifact)
			}
			if got.Version != c.version {
				t.Errorf("version = %q, want %q", got.Version, c.version)
			}
			if got.Ext != c.ext {
				t.Errorf("ext = %q, want %q", got.Ext, c.ext)
			}
			if got.Algo != c.algo {
				t.Errorf("algo = %q, want %q", got.Algo, c.algo)
			}
			if got.Snapshot != c.snapshot {
				t.Errorf("snapshot = %v, want %v", got.Snapshot, c.snapshot)
			}
		})
	}
}

func TestBasePath(t *testing.T) {
	cases := map[string]string{
		"com/example/foo/1.0.0/foo-1.0.0.jar":      "com/example/foo/1.0.0/foo-1.0.0.jar",
		"com/example/foo/1.0.0/foo-1.0.0.jar.sha1": "com/example/foo/1.0.0/foo-1.0.0.jar",
		"com/example/foo/maven-metadata.xml.md5":   "com/example/foo/maven-metadata.xml",
		"com/example/foo/maven-metadata.xml":       "com/example/foo/maven-metadata.xml",
	}
	for raw, want := range cases {
		p, err := Parse(raw)
		if err != nil {
			t.Fatalf("Parse(%q): %v", raw, err)
		}
		if got := p.BasePath(); got != want {
			t.Errorf("BasePath(%q) = %q, want %q", raw, got, want)
		}
	}
}

func TestNewHasher(t *testing.T) {
	for _, algo := range []string{"md5", "sha1", "sha256", "sha512"} {
		if h, ok := NewHasher(algo); !ok || h == nil {
			t.Errorf("NewHasher(%q) not available", algo)
		}
	}
	if _, ok := NewHasher("crc32"); ok {
		t.Error("NewHasher(crc32) should be unavailable")
	}
}

func TestContentType(t *testing.T) {
	cases := map[string]string{
		"jar":  "application/java-archive",
		"war":  "application/java-archive",
		"pom":  "application/xml",
		"asc":  "application/pgp-signature",
		"sha1": "text/plain; charset=utf-8",
		"bin":  "application/octet-stream",
	}
	for ext, want := range cases {
		if got := ContentType(ext); got != want {
			t.Errorf("ContentType(%q) = %q, want %q", ext, got, want)
		}
	}
}
