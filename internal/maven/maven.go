// Package maven models the path structure of a Maven repository.
//
// A repository is a flat set of objects laid out as
//
//	group-path/artifactId/version/artifactId-version[-classifier].ext
//	group-path/artifactId/maven-metadata.xml
//
// where group-path is the groupId with dots replaced by slashes. Every
// artifact is accompanied by .md5/.sha1/.sha256/.sha512 checksum objects.
package maven

import (
	"crypto/md5"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"fmt"
	"hash"
	"strings"
)

// Kind classifies a repository object.
type Kind int

const (
	// KindUnknown is a path that is not a resolvable repository object.
	KindUnknown Kind = iota
	// KindArtifact is a file such as foo-1.0.0.jar.
	KindArtifact
	// KindChecksum is a checksum of an artifact.
	KindChecksum
	// KindMetadata is maven-metadata.xml.
	KindMetadata
	// KindMetadataChecksum is a checksum of maven-metadata.xml.
	KindMetadataChecksum
)

// Path is a parsed, repository-relative Maven path.
type Path struct {
	Raw      string // original path
	Group    string // dot-separated groupId ("" for non-versioned paths)
	Artifact string
	Version  string // "" when not inside a version directory
	Ext      string // artifact extension, e.g. "jar"
	Algo     string // checksum algorithm, e.g. "sha1"
	Kind     Kind
	Snapshot bool
}

// BasePath returns the path of the object this object is a checksum of.
// For non-checksum kinds it returns Raw.
func (p Path) BasePath() string {
	if p.Kind == KindChecksum || p.Kind == KindMetadataChecksum {
		return strings.TrimSuffix(p.Raw, "."+p.Algo)
	}
	return p.Raw
}

var checksumAlgos = map[string]bool{"md5": true, "sha1": true, "sha256": true, "sha512": true}

// Parse parses a repository-relative path. It returns a Path with
// KindUnknown (and no error) for paths that are simply not repository
// objects (e.g. directory prefixes), and an error for malformed paths.
func Parse(raw string) (Path, error) {
	p := Path{Raw: raw, Kind: KindUnknown}
	if raw == "" {
		return p, nil
	}
	parts := strings.Split(raw, "/")
	for _, seg := range parts {
		if seg == "" || seg == "." || seg == ".." {
			return Path{}, fmt.Errorf("invalid path %q", raw)
		}
	}
	n := len(parts)
	last := parts[n-1]

	if last == "maven-metadata.xml" {
		if n < 2 {
			return p, nil
		}
		return Path{Raw: raw, Group: strings.Join(parts[:n-2], "."), Artifact: parts[n-2], Kind: KindMetadata}, nil
	}
	if algo, ok := algoSuffix(last); ok && strings.TrimSuffix(last, "."+algo) == "maven-metadata.xml" {
		if n < 2 {
			return p, nil
		}
		return Path{Raw: raw, Group: strings.Join(parts[:n-2], "."), Artifact: parts[n-2], Algo: algo, Kind: KindMetadataChecksum}, nil
	}
	if n < 4 {
		return p, nil
	}

	art := parts[n-3]
	ver := parts[n-2]
	grp := strings.Join(parts[:n-3], ".")
	snapshot := strings.HasSuffix(ver, "-SNAPSHOT")
	out := Path{Raw: raw, Group: grp, Artifact: art, Version: ver, Snapshot: snapshot}

	name := last
	if algo, ok := algoSuffix(name); ok {
		out.Kind = KindChecksum
		out.Algo = algo
		name = strings.TrimSuffix(name, "."+algo)
	} else {
		out.Kind = KindArtifact
	}
	dot := strings.LastIndex(name, ".")
	if dot <= 0 {
		return Path{}, fmt.Errorf("artifact %q has no extension", last)
	}
	out.Ext = name[dot+1:]
	base := name[:dot]
	// Non-snapshot artifacts are named {artifact}-{version}[-{classifier}].ext.
	// Snapshot timestamps replace the version in the file name, so only the
	// artifact prefix can be checked.
	if out.Snapshot {
		if !strings.HasPrefix(base, out.Artifact+"-") {
			return Path{}, fmt.Errorf("%q does not match artifact %q", last, out.Artifact)
		}
		return out, nil
	}
	prefix := out.Artifact + "-" + out.Version
	if !strings.HasPrefix(base, prefix) {
		return Path{}, fmt.Errorf("%q does not match artifact %q version %q", last, out.Artifact, out.Version)
	}
	if len(base) > len(prefix) && base[len(prefix)] != '-' {
		return Path{}, fmt.Errorf("%q does not match artifact %q version %q", last, out.Artifact, out.Version)
	}
	return out, nil
}

func algoSuffix(name string) (string, bool) {
	i := strings.LastIndex(name, ".")
	if i <= 0 {
		return "", false
	}
	a := name[i+1:]
	if checksumAlgos[a] {
		return a, true
	}
	return "", false
}

// NewHasher returns a hasher for a Maven checksum algorithm.
func NewHasher(algo string) (hash.Hash, bool) {
	switch algo {
	case "md5":
		return md5.New(), true
	case "sha1":
		return sha1.New(), true
	case "sha256":
		return sha256.New(), true
	case "sha512":
		return sha512.New(), true
	}
	return nil, false
}

// HashHex returns hex-encoded digest content for a Maven checksum file.
func HashHex(algo string, h hash.Hash) (string, bool) {
	if _, ok := NewHasher(algo); !ok {
		return "", false
	}
	return hex.EncodeToString(h.Sum(nil)), true
}

// ContentType picks the Content-Type for an object by extension.
func ContentType(ext string) string {
	switch ext {
	case "jar", "war", "ear", "sjar", "class":
		return "application/java-archive"
	case "pom", "xml":
		return "application/xml"
	case "properties":
		return "text/plain; charset=utf-8"
	case "asc":
		return "application/pgp-signature"
	case "md5", "sha1", "sha256", "sha512":
		return "text/plain; charset=utf-8"
	default:
		return "application/octet-stream"
	}
}
