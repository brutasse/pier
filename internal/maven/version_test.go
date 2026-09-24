package maven

import (
	"testing"
)

func TestCompareVersions(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		// numeric ordering
		{"1.0.0", "1.0.0", 0},
		{"1.0", "1.0.0", 0},
		{"1.0.0.0", "1.0", 0},
		{"1.0.1", "1.0", 1},
		{"1.0", "1.0.1", -1},
		{"2", "10", -1},
		{"1.0.0.1", "1.0.0.2", -1},
		{"1.0.2", "1.0.10", -1},
		// snapshot and qualifiers
		{"1.0-SNAPSHOT", "1.0", -1},
		{"1.0.1", "1.0-SNAPSHOT", 1},
		{"1.0-alpha1", "1.0-beta1", -1},
		{"1.0-beta1", "1.0-rc1", -1},
		{"1.0-rc1", "1.0", -1},
		{"1.0-sp1", "1.0", 1},
		{"1.0-alpha", "1.0-alpha2", -1},
		{"1.0-alpha", "1.0-a", 0},
		{"1.0-rc1", "1.0-cr1", 0},
		{"1.0-ga", "1.0", 0},
		{"1.0-SNAPSHOT", "1.0-alpha1", -1},
		// separator weight
		{"1.0-1", "1.0.1", 0},
		{"1_0", "1.0", 0},
		// trailing qualifier vs release
		{"1.0-foo", "1.0", -1},
		{"1.0-release", "1.0", 0},
		// mixed
		{"1.0.1-rc1", "1.0.1-rc2", -1},
		{"2.5.1.RC1", "2.5.1", -1},
		{"2.5.1.RC1", "2.5.2", -1},
	}
	for _, c := range cases {
		if got := CompareVersions(c.a, c.b); got != c.want {
			t.Errorf("CompareVersions(%q, %q) = %d, want %d", c.a, c.b, got, c.want)
		} else if got := CompareVersions(c.b, c.a); got != -c.want {
			t.Errorf("CompareVersions(%q, %q) = %d, want %d (antisymmetry)", c.b, c.a, got, -c.want)
		}
	}
}

func TestSortVersions(t *testing.T) {
	in := []string{"1.0.10", "2.0.0-SNAPSHOT", "1.0.2", "1.0", "2.0.0"}
	SortVersions(in)
	want := []string{"1.0", "1.0.2", "1.0.10", "2.0.0-SNAPSHOT", "2.0.0"}
	for i, v := range in {
		if v != want[i] {
			t.Fatalf("sorted = %v, want %v", in, want)
		}
	}
}
