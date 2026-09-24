package maven

import (
	"strings"
	"testing"
	"time"
)

func TestIsArtifactMetadata(t *testing.T) {
	cases := []struct {
		raw  string
		want bool
	}{
		{"com/example/foo/maven-metadata.xml", true},
		{"a/b/maven-metadata.xml", true},
		{"com/example/foo/1.0-SNAPSHOT/maven-metadata.xml", false},
		{"com/example/foo/maven-metadata.xml.sha1", false},
		{"com/example/foo/1.0.0/foo-1.0.0.jar", false},
	}
	for _, c := range cases {
		p, err := Parse(c.raw)
		if err != nil {
			t.Fatalf("Parse(%q): %v", c.raw, err)
		}
		if got := p.IsArtifactMetadata(); got != c.want {
			t.Errorf("IsArtifactMetadata(%q) = %v, want %v", c.raw, got, c.want)
		}
	}
}

func TestSynthesizeMetadata(t *testing.T) {
	now := time.Date(2026, 9, 20, 12, 30, 45, 0, time.UTC)
	body := SynthesizeMetadata("com.acme", "widget", []string{"1.0.0", "1.1.0-SNAPSHOT", "2.0.0"}, now)

	wantFragments := []string{
		"<?xml version=\"1.0\" encoding=\"UTF-8\"?>",
		"<groupId>com.acme</groupId>",
		"<artifactId>widget</artifactId>",
		"<latest>2.0.0</latest>",
		"<release>2.0.0</release>",
		"<version>1.0.0</version>",
		"<version>1.1.0-SNAPSHOT</version>",
		"<version>2.0.0</version>",
		"<lastUpdated>20260920123045</lastUpdated>",
	}
	for _, f := range wantFragments {
		if !strings.Contains(body, f) {
			t.Errorf("synthesized metadata missing %q:\n%s", f, body)
		}
	}
	// Versions are sorted, not input-ordered.
	i10, i11, i20 := strings.Index(body, "<version>1.0.0</version>"),
		strings.Index(body, "<version>1.1.0-SNAPSHOT</version>"),
		strings.Index(body, "<version>2.0.0</version>")
	if !(i10 < i11 && i11 < i20) {
		t.Errorf("versions not sorted:\n%s", body)
	}

	// All-snapshot GAVs have no <release>.
	body = SynthesizeMetadata("com.acme", "widget", []string{"1.0.0-SNAPSHOT"}, now)
	if strings.Contains(body, "<release>") {
		t.Errorf("release element must be omitted:\n%s", body)
	}
}
