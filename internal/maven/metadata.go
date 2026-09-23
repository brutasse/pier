package maven

import (
	"encoding/xml"
	"strings"
	"time"
)

// IsArtifactMetadata reports whether p is the artifact-level
// maven-metadata.xml of a GAV (group-path/artifact/maven-metadata.xml),
// as opposed to the snapshot-level metadata inside a -SNAPSHOT version
// directory. Parse reports the version segment as the "artifact" for the
// snapshot-level shape, so the suffix distinguishes the two.
func (p Path) IsArtifactMetadata() bool {
	return p.Kind == KindMetadata && !strings.HasSuffix(p.Artifact, "-SNAPSHOT")
}

// SynthesizeMetadata renders the artifact-level maven-metadata.xml for a
// GAV from its published versions. versions must be non-empty; it is
// sorted before rendering. <release> is the highest non-snapshot
// version (omitted when there are none) and <latest> the highest
// version overall.
func SynthesizeMetadata(group, artifact string, versions []string, now time.Time) string {
	vs := append([]string(nil), versions...)
	SortVersions(vs)
	doc := metadataDoc{
		GroupId:    group,
		ArtifactId: artifact,
		Versioning: versioning{
			Latest:      vs[len(vs)-1],
			LastUpdated: now.Format("20060102150405"),
		},
	}
	for _, v := range vs {
		doc.Versioning.Versions = append(doc.Versioning.Versions, version{V: v})
		if !strings.HasSuffix(v, "-SNAPSHOT") {
			doc.Versioning.Release = v
		}
	}
	b, err := xml.Marshal(doc)
	if err != nil {
		panic(err) // cannot happen for this static structure
	}
	return xml.Header + string(b)
}

type metadataDoc struct {
	XMLName    xml.Name   `xml:"metadata"`
	GroupId    string     `xml:"groupId"`
	ArtifactId string     `xml:"artifactId"`
	Versioning versioning `xml:"versioning"`
}

type versioning struct {
	Latest      string    `xml:"latest"`
	Release     string    `xml:"release,omitempty"`
	Versions    []version `xml:"versions>version"`
	LastUpdated string    `xml:"lastUpdated"`
}

type version struct {
	V string `xml:",chardata"`
}
