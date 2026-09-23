package maven

import (
	"strings"
)

// releaseRank is the rank of the absence of a trailing qualifier
// (release), the point a version with no qualifier is judged against.
const releaseRank = 5

// versionToken is one component of a version string: either a run of
// digits or a run of letters. Separators (".", "-", "_") carry no weight.
type versionToken struct {
	num      int64
	qual     string
	isNumber bool
}

// tokenizeVersion splits a version into its numeric and qualifier
// components.
func tokenizeVersion(v string) []versionToken {
	var toks []versionToken
	var (
		num    int64
		hasNum bool
		qual   strings.Builder
	)
	flushQual := func() {
		if qual.Len() == 0 {
			return
		}
		q := qual.String()
		if strings.EqualFold(q, "ga") {
			q = "release"
		}
		toks = append(toks, versionToken{qual: q})
		qual.Reset()
	}
	flushNum := func() {
		if !hasNum {
			return
		}
		toks = append(toks, versionToken{num: num, isNumber: true})
		hasNum, num = false, 0
	}
	flush := func() {
		flushNum()
		flushQual()
	}
	for _, r := range v {
		switch {
		case r >= '0' && r <= '9':
			flushQual()
			if num < 1<<62 { // saturate, not wrap
				num = num*10 + int64(r-'0')
			}
			hasNum = true
		case (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z'):
			flushNum()
			qual.WriteRune(r)
		default:
			flush()
		}
	}
	flush()
	return toks
}

// qualifierRank orders qualifiers on the documented Maven ladder:
// snapshot < alpha < beta < milestone < rc < release < sp. Qualifiers
// outside the ladder rank with release and tie-break by name.
func qualifierRank(q string) int {
	switch strings.ToLower(q) {
	case "snapshot":
		return 0
	case "alpha", "a":
		return 1
	case "beta", "b":
		return 2
	case "milestone", "m":
		return 3
	case "rc", "cr":
		return 4
	case "sp":
		return 6
	}
	return releaseRank
}

// compareToken orders two components at the same position. A number
// outranks a qualifier; qualifiers compare on the rank ladder, then by
// name when the ranks tie.
func compareToken(x, y versionToken) int {
	switch {
	case x.isNumber && y.isNumber:
		switch {
		case x.num < y.num:
			return -1
		case x.num > y.num:
			return 1
		}
		return 0
	case x.isNumber:
		return 1
	case y.isNumber:
		return -1
	}
	rx, ry := qualifierRank(x.qual), qualifierRank(y.qual)
	if rx != ry {
		if rx < ry {
			return -1
		}
		return 1
	}
	if rx != releaseRank {
		return 0
	}
	return strings.Compare(strings.ToLower(x.qual), strings.ToLower(y.qual))
}

// CompareVersions orders two Maven version strings the way the resolver
// does: numeric components compare as numbers, non-numeric components as
// qualifiers, and a trailing qualifier makes a version pre-release
// (lower) unless it outranks release, as in "1.0-sp1" > "1.0". It
// returns -1, 0 or 1.
func CompareVersions(a, b string) int {
	ta, tb := tokenizeVersion(a), tokenizeVersion(b)
	for i := 0; i < len(ta) && i < len(tb); i++ {
		if c := compareToken(ta[i], tb[i]); c != 0 {
			return c
		}
	}
	if len(ta) == len(tb) {
		return 0
	}
	var longer, shorter []versionToken
	if len(ta) > len(tb) {
		longer, shorter = ta, tb
	} else {
		longer, shorter = tb, ta
	}
	// The first extra component of the longer version decides against
	// release: a zero is insignificant ("1.0" == "1.0.0"), a number
	// makes it higher, a qualifier makes it lower.
	first := longer[len(shorter)]
	if first.isNumber && first.num == 0 {
		return 0
	}
	c := compareToken(first, versionToken{qual: "release"})
	if c == 0 {
		return 0
	}
	if len(ta) > len(tb) {
		return c
	}
	return -c
}

// SortVersions orders versions ascending with CompareVersions.
func SortVersions(versions []string) {
	for i := 1; i < len(versions); i++ {
		for j := i; j > 0 && CompareVersions(versions[j], versions[j-1]) < 0; j-- {
			versions[j], versions[j-1] = versions[j-1], versions[j]
		}
	}
}
