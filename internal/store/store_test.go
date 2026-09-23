package store

import "testing"

func TestKey(t *testing.T) {
	cases := []struct {
		prefix string
		path   string
		want   string
	}{
		{"", "com/example/foo-1.0.0.jar", "com/example/foo-1.0.0.jar"},
		{"maven", "com/example/foo-1.0.0.jar", "maven/com/example/foo-1.0.0.jar"},
		{"maven/sub", "a/b/c", "maven/sub/a/b/c"},
	}
	for _, c := range cases {
		s := &S3{prefix: c.prefix}
		if got := s.key(c.path); got != c.want {
			t.Errorf("key(%q) with prefix %q = %q, want %q", c.path, c.prefix, got, c.want)
		}
	}
}
