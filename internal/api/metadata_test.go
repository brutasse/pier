package api_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/brutasse/pier/internal/api"
	"github.com/brutasse/pier/internal/config"
	"github.com/brutasse/pier/internal/policy"
	"github.com/brutasse/pier/internal/store"
	"github.com/brutasse/pier/internal/upstream"
)

// ---- metadata TTL (pull-through revalidation) ----

func TestMetadataTTLRevalidation(t *testing.T) {
	const meta = "org/upstream/lib/maven-metadata.xml"
	v1 := "<metadata><versioning><versions><version>1.0</version></versions></versioning></metadata>"
	v2 := "<metadata><versioning><versions><version>1.0</version><version>2.0</version></versions></versioning></metadata>"
	fu := newFakeUpstream(t, "central", map[string]string{"/" + meta: v1}, nil)
	ttl := 100 * time.Millisecond
	pe := newPullEnvTTL(t, 1<<20, &ttl, fu)

	resp, body := pe.te.do(t, http.MethodGet, "/"+meta, "", "")
	if resp.StatusCode != 200 || body != v1 {
		t.Fatalf("cold GET: %d %q", resp.StatusCode, body)
	}
	if fu.hitCount("/"+meta) != 1 {
		t.Fatalf("upstream hits = %d, want 1", fu.hitCount("/"+meta))
	}

	// Fresh: served from the store without revalidation.
	resp, body = pe.te.do(t, http.MethodGet, "/"+meta, "", "")
	if resp.StatusCode != 200 || body != v1 {
		t.Fatalf("fresh GET: %d %q", resp.StatusCode, body)
	}
	if fu.hitCount("/"+meta) != 1 {
		t.Fatalf("fresh GET revalidated: hits = %d, want 1", fu.hitCount("/"+meta))
	}

	// Expired: the next GET revalidates from upstream.
	pe.st.ageOlderThan(meta, 200*time.Millisecond)
	fu.mu.Lock()
	fu.objects["/"+meta] = v2
	fu.mu.Unlock()
	resp, body = pe.te.do(t, http.MethodGet, "/"+meta, "", "")
	if resp.StatusCode != 200 || body != v2 {
		t.Fatalf("revalidation GET: %d %q, want 200 %q", resp.StatusCode, body, v2)
	}
	if fu.hitCount("/"+meta) != 2 {
		t.Fatalf("upstream hits = %d, want 2", fu.hitCount("/"+meta))
	}
	// The store holds the refreshed metadata and sidecars.
	obj, _, _, _, err := pe.st.Get(t.Context(), meta)
	if err != nil {
		t.Fatalf("store get: %v", err)
	}
	b, _ := io.ReadAll(obj)
	if string(b) != v2 {
		t.Fatalf("stored metadata = %q, want %q", b, v2)
	}
	cs, _, _, _, err := pe.st.Get(t.Context(), meta+".sha1")
	if err != nil {
		t.Fatalf("sidecar get: %v", err)
	}
	cb, _ := io.ReadAll(cs)
	if want := sha1hex(v2); strings.TrimSpace(string(cb)) != want {
		t.Fatalf("sidecar sha1 = %q, want %q", cb, want)
	}
	// The refreshed object is fresh again: no further revalidation.
	resp, _ = pe.te.do(t, http.MethodGet, "/"+meta, "", "")
	if resp.StatusCode != 200 || fu.hitCount("/"+meta) != 2 {
		t.Fatalf("warm GET after revalidation: %d hits=%d", resp.StatusCode, fu.hitCount("/"+meta))
	}
}

func TestMetadataTTLDisabled(t *testing.T) {
	const meta = "org/upstream/lib/maven-metadata.xml"
	fu := newFakeUpstream(t, "central", map[string]string{"/" + meta: "meta-v1"}, nil)
	zero := time.Duration(0)
	pe := newPullEnvTTL(t, 1<<20, &zero, fu)

	resp, body := pe.te.do(t, http.MethodGet, "/"+meta, "", "")
	if resp.StatusCode != 200 || body != "meta-v1" {
		t.Fatalf("cold GET: %d %q", resp.StatusCode, body)
	}
	pe.st.ageOlderThan(meta, time.Hour*24*30)
	fu.mu.Lock()
	fu.objects["/"+meta] = "meta-v2"
	fu.mu.Unlock()
	resp, body = pe.te.do(t, http.MethodGet, "/"+meta, "", "")
	if resp.StatusCode != 200 || body != "meta-v1" {
		t.Fatalf("TTL disabled must serve the cache: %d %q", resp.StatusCode, body)
	}
	if fu.hitCount("/"+meta) != 1 {
		t.Fatalf("upstream hits = %d, want 1", fu.hitCount("/"+meta))
	}
}

func TestMetadataTTLReservedNever(t *testing.T) {
	const meta = "com/acme/lib/maven-metadata.xml"
	fu := newFakeUpstream(t, "central", map[string]string{"/" + meta: "upstream-meta"}, nil)
	ttl := 100 * time.Millisecond
	pe := newPullEnvTTL(t, 1<<20, &ttl, fu)

	// A private (reserved) metadata object, backdated past the TTL.
	if err := pe.st.Put(t.Context(), meta, strings.NewReader("private-meta"), 12, "application/xml", false); err != nil {
		t.Fatal(err)
	}
	pe.st.ageOlderThan(meta, 200*time.Millisecond)

	resp, body := pe.te.do(t, http.MethodGet, "/"+meta, "", "")
	if resp.StatusCode != 200 || body != "private-meta" {
		t.Fatalf("reserved metadata GET: %d %q", resp.StatusCode, body)
	}
	if fu.hitCount("/"+meta) != 0 {
		t.Fatalf("upstream was contacted %d times for a reserved path", fu.hitCount("/"+meta))
	}
}

func TestMetadataTTLFallbackNotFound(t *testing.T) {
	const meta = "org/upstream/lib/maven-metadata.xml"
	fu := newFakeUpstream(t, "central", map[string]string{"/" + meta: "cached"}, nil)
	ttl := 100 * time.Millisecond
	pe := newPullEnvTTL(t, 1<<20, &ttl, fu)

	if resp, _ := pe.te.do(t, http.MethodGet, "/"+meta, "", ""); resp.StatusCode != 200 {
		t.Fatalf("cold GET: %d", resp.StatusCode)
	}
	// The upstream deletes the metadata; the revalidation must fall
	// back to the stale copy instead of a 404.
	fu.mu.Lock()
	delete(fu.objects, "/"+meta)
	fu.status["/"+meta] = http.StatusNotFound
	fu.mu.Unlock()
	pe.st.ageOlderThan(meta, 200*time.Millisecond)

	resp, body := pe.te.do(t, http.MethodGet, "/"+meta, "", "")
	if resp.StatusCode != 200 || body != "cached" {
		t.Fatalf("stale fallback GET: %d %q, want 200 cached", resp.StatusCode, body)
	}
	if fu.hitCount("/"+meta) != 2 {
		t.Fatalf("upstream hits = %d, want 2", fu.hitCount("/"+meta))
	}
}

func TestMetadataTTLFallbackError(t *testing.T) {
	const meta = "org/upstream/lib/maven-metadata.xml"
	fu := newFakeUpstream(t, "central", map[string]string{"/" + meta: "cached"}, nil)
	ttl := 100 * time.Millisecond
	pe := newPullEnvTTL(t, 1<<20, &ttl, fu)

	if resp, _ := pe.te.do(t, http.MethodGet, "/"+meta, "", ""); resp.StatusCode != 200 {
		t.Fatalf("cold GET: %d", resp.StatusCode)
	}
	fu.mu.Lock()
	fu.status["/"+meta] = http.StatusInternalServerError
	fu.mu.Unlock()
	pe.st.ageOlderThan(meta, 200*time.Millisecond)

	resp, body := pe.te.do(t, http.MethodGet, "/"+meta, "", "")
	if resp.StatusCode != 200 || body != "cached" {
		t.Fatalf("stale fallback GET: %d %q, want 200 cached", resp.StatusCode, body)
	}
}

func TestMetadataTTLHeadServesStale(t *testing.T) {
	const meta = "org/upstream/lib/maven-metadata.xml"
	fu := newFakeUpstream(t, "central", map[string]string{"/" + meta: "cached-meta"}, nil)
	ttl := 100 * time.Millisecond
	pe := newPullEnvTTL(t, 1<<20, &ttl, fu)

	if resp, _ := pe.te.do(t, http.MethodGet, "/"+meta, "", ""); resp.StatusCode != 200 {
		t.Fatalf("cold GET: %d", resp.StatusCode)
	}
	pe.st.ageOlderThan(meta, 200*time.Millisecond)

	resp, _ := pe.te.do(t, http.MethodHead, "/"+meta, "", "")
	if resp.StatusCode != 200 {
		t.Fatalf("stale HEAD: %d, want 200", resp.StatusCode)
	}
	if cl := resp.Header.Get("Content-Length"); cl != "11" {
		t.Fatalf("stale HEAD Content-Length = %q, want 11", cl)
	}
	if fu.hitCount("/"+meta) != 1 {
		t.Fatalf("upstream hits = %d, want 1 (HEAD must not revalidate)", fu.hitCount("/"+meta))
	}
}

func TestMetadataTTLSnapshotLevel(t *testing.T) {
	// Snapshot-level maven-metadata.xml is revalidated too: it is what
	// the resolver uses to map -SNAPSHOT to a timestamped build.
	const meta = "org/upstream/lib/1.0-SNAPSHOT/maven-metadata.xml"
	v1 := "<metadata><versioning><snapshot><timestamp>20260101.000001</timestamp><buildNumber>1</buildNumber></snapshot></versioning></metadata>"
	v2 := "<metadata><versioning><snapshot><timestamp>20260102.000001</timestamp><buildNumber>2</buildNumber></snapshot></versioning></metadata>"
	fu := newFakeUpstream(t, "central", map[string]string{"/" + meta: v1}, nil)
	ttl := 100 * time.Millisecond
	pe := newPullEnvTTL(t, 1<<20, &ttl, fu)

	resp, body := pe.te.do(t, http.MethodGet, "/"+meta, "", "")
	if resp.StatusCode != 200 || body != v1 {
		t.Fatalf("cold GET: %d %q", resp.StatusCode, body)
	}
	pe.st.ageOlderThan(meta, 200*time.Millisecond)
	fu.mu.Lock()
	fu.objects["/"+meta] = v2
	fu.mu.Unlock()
	resp, body = pe.te.do(t, http.MethodGet, "/"+meta, "", "")
	if resp.StatusCode != 200 || body != v2 {
		t.Fatalf("revalidation GET: %d %q, want 200 %q", resp.StatusCode, body, v2)
	}
	if fu.hitCount("/"+meta) != 2 {
		t.Fatalf("upstream hits = %d, want 2", fu.hitCount("/"+meta))
	}
}

func TestMetadataTTLArtifactsNotRevalidated(t *testing.T) {
	const jar = "org/upstream/tool/1.0/tool-1.0.jar"
	fu := newFakeUpstream(t, "central", map[string]string{"/" + jar: "v1-bytes"}, nil)
	ttl := 100 * time.Millisecond
	pe := newPullEnvTTL(t, 1<<20, &ttl, fu)

	resp, body := pe.te.do(t, http.MethodGet, "/"+jar, "", "")
	if resp.StatusCode != 200 || body != "v1-bytes" {
		t.Fatalf("cold GET: %d %q", resp.StatusCode, body)
	}
	pe.st.ageOlderThan(jar, 200*time.Millisecond)
	fu.mu.Lock()
	fu.objects["/"+jar] = "v2-bytes"
	fu.mu.Unlock()
	resp, body = pe.te.do(t, http.MethodGet, "/"+jar, "", "")
	if resp.StatusCode != 200 || body != "v1-bytes" {
		t.Fatalf("artifact must not revalidate: %d %q", resp.StatusCode, body)
	}
	if fu.hitCount("/"+jar) != 1 {
		t.Fatalf("upstream hits = %d, want 1", fu.hitCount("/"+jar))
	}
}

func TestMetadataTTLSingleFlight(t *testing.T) {
	const meta = "org/upstream/lib/maven-metadata.xml"
	fu := newFakeUpstream(t, "central", map[string]string{"/" + meta: "v1"}, nil)
	fu.delay = 100 * time.Millisecond
	ttl := 100 * time.Millisecond
	pe := newPullEnvTTL(t, 1<<20, &ttl, fu)

	if resp, _ := pe.te.do(t, http.MethodGet, "/"+meta, "", ""); resp.StatusCode != 200 {
		t.Fatalf("cold GET: %d", resp.StatusCode)
	}
	fu.mu.Lock()
	fu.objects["/"+meta] = "v2"
	fu.mu.Unlock()
	pe.st.ageOlderThan(meta, 200*time.Millisecond)

	const n = 5
	results := make([]int, n)
	wg := sync.WaitGroup{}
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			resp, err := http.Get(pe.te.server.URL + "/" + meta)
			if err != nil {
				results[i] = -1
				return
			}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			results[i] = resp.StatusCode
		}(i)
	}
	wg.Wait()
	for i, code := range results {
		if code != 200 {
			t.Fatalf("request %d: %d, want 200", i, code)
		}
	}
	if hits := fu.hitCount("/" + meta); hits != 2 {
		t.Fatalf("upstream hits = %d, want 2 (one cold pull, one revalidation)", hits)
	}
}

// ---- metadata synthesis (private GAVs) ----

func TestSynthesizeMetadata(t *testing.T) {
	pe := newPullEnv(t, 1<<20) // no upstream
	const meta = "com/acme/widget/maven-metadata.xml"

	if resp, _ := pe.te.do(t, http.MethodPut, "/com/acme/widget/1.0.0/widget-1.0.0.jar", "", "jar1"); resp.StatusCode != 201 {
		t.Fatalf("PUT 1.0.0: %d", resp.StatusCode)
	}
	if resp, _ := pe.te.do(t, http.MethodPut, "/com/acme/widget/2.0.0/widget-2.0.0.jar", "", "jar2"); resp.StatusCode != 201 {
		t.Fatalf("PUT 2.0.0: %d", resp.StatusCode)
	}

	resp, body := pe.te.do(t, http.MethodGet, "/"+meta, "", "")
	if resp.StatusCode != 200 {
		t.Fatalf("synthesized GET: %d %s", resp.StatusCode, body)
	}
	for _, frag := range []string{
		"<groupId>com.acme</groupId>",
		"<artifactId>widget</artifactId>",
		"<latest>2.0.0</latest>",
		"<release>2.0.0</release>",
		"<version>1.0.0</version>",
		"<version>2.0.0</version>",
	} {
		if !strings.Contains(body, frag) {
			t.Errorf("synthesized metadata missing %q:\n%s", frag, body)
		}
	}
	// The metadata and its sidecars are stored.
	obj, _, _, _, err := pe.st.Get(t.Context(), meta)
	if err != nil {
		t.Fatalf("store get: %v", err)
	}
	b, _ := io.ReadAll(obj)
	if string(b) != body {
		t.Fatalf("stored metadata = %q, want %q", b, body)
	}
	cs, _, _, _, err := pe.st.Get(t.Context(), meta+".sha1")
	if err != nil {
		t.Fatalf("sidecar get: %v", err)
	}
	cb, _ := io.ReadAll(cs)
	if want := sha1hex(body); strings.TrimSpace(string(cb)) != want {
		t.Fatalf("sidecar sha1 = %q, want %q", cb, want)
	}
	// A second GET is served from the store.
	resp, body2 := pe.te.do(t, http.MethodGet, "/"+meta, "", "")
	if resp.StatusCode != 200 || body2 != body {
		t.Fatalf("warm GET: %d %q", resp.StatusCode, body2)
	}
}

func TestSynthesizeMetadataInvalidation(t *testing.T) {
	pe := newPullEnv(t, 1<<20) // no upstream
	const meta = "com/acme/widget/maven-metadata.xml"

	if resp, _ := pe.te.do(t, http.MethodPut, "/com/acme/widget/1.0.0/widget-1.0.0.jar", "", "jar1"); resp.StatusCode != 201 {
		t.Fatalf("PUT 1.0.0: %d", resp.StatusCode)
	}
	resp, body := pe.te.do(t, http.MethodGet, "/"+meta, "", "")
	if resp.StatusCode != 200 || !strings.Contains(body, "<version>1.0.0</version>") || strings.Contains(body, "2.0.0") {
		t.Fatalf("first synthesis: %d %q", resp.StatusCode, body)
	}

	// A new version PUT invalidates the stored metadata.
	if resp, _ := pe.te.do(t, http.MethodPut, "/com/acme/widget/2.0.0/widget-2.0.0.jar", "", "jar2"); resp.StatusCode != 201 {
		t.Fatalf("PUT 2.0.0: %d", resp.StatusCode)
	}
	resp, body = pe.te.do(t, http.MethodGet, "/"+meta, "", "")
	if resp.StatusCode != 200 {
		t.Fatalf("re-synthesis GET: %d", resp.StatusCode)
	}
	if !strings.Contains(body, "<version>1.0.0</version>") || !strings.Contains(body, "<version>2.0.0</version>") {
		t.Fatalf("re-synthesized metadata missing versions:\n%s", body)
	}
}

func TestSynthesizeMetadataSnapshot(t *testing.T) {
	pe := newPullEnv(t, 1<<20) // no upstream
	const meta = "com/acme/widget/maven-metadata.xml"

	if resp, _ := pe.te.do(t, http.MethodPut, "/com/acme/widget/1.0.0/widget-1.0.0.jar", "", "jar1"); resp.StatusCode != 201 {
		t.Fatalf("PUT 1.0.0: %d", resp.StatusCode)
	}
	// A plugin-written snapshot-level metadata; the snapshot artifact PUT
	// below must invalidate it.
	const snapMeta = "com/acme/widget/1.1.0-SNAPSHOT/maven-metadata.xml"
	if err := pe.st.Put(t.Context(), snapMeta, strings.NewReader("<metadata/>"), 11, "application/xml", false); err != nil {
		t.Fatal(err)
	}
	const snap = "com/acme/widget/1.1.0-SNAPSHOT/widget-1.1.0-20260101.120000-1.jar"
	if resp, _ := pe.te.do(t, http.MethodPut, "/"+snap, "", "snap"); resp.StatusCode != 201 {
		t.Fatalf("PUT snapshot: %d", resp.StatusCode)
	}
	if _, _, _, _, err := pe.st.Head(t.Context(), snapMeta); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("snapshot-level metadata not invalidated: %v", err)
	}

	resp, body := pe.te.do(t, http.MethodGet, "/"+meta, "", "")
	if resp.StatusCode != 200 {
		t.Fatalf("synthesized GET: %d", resp.StatusCode)
	}
	for _, frag := range []string{
		"<latest>1.1.0-SNAPSHOT</latest>",
		"<release>1.0.0</release>",
		"<version>1.1.0-SNAPSHOT</version>",
	} {
		if !strings.Contains(body, frag) {
			t.Errorf("synthesized metadata missing %q:\n%s", frag, body)
		}
	}
}

func TestSynthesizeMetadataOrdering(t *testing.T) {
	pe := newPullEnv(t, 1<<20) // no upstream
	const meta = "com/acme/widget/maven-metadata.xml"

	for _, v := range []string{"1.0.10", "1.0.2", "1.0.0"} {
		if resp, _ := pe.te.do(t, http.MethodPut, "/com/acme/widget/"+v+"/widget-"+v+".jar", "", "x"); resp.StatusCode != 201 {
			t.Fatalf("PUT %s: %d", v, resp.StatusCode)
		}
	}
	resp, body := pe.te.do(t, http.MethodGet, "/"+meta, "", "")
	if resp.StatusCode != 200 {
		t.Fatalf("synthesized GET: %d", resp.StatusCode)
	}
	i10, i102, i1010 := strings.Index(body, "<version>1.0.0</version>"),
		strings.Index(body, "<version>1.0.2</version>"),
		strings.Index(body, "<version>1.0.10</version>")
	if !(i10 < i102 && i102 < i1010) {
		t.Fatalf("versions not sorted:\n%s", body)
	}
	if !strings.Contains(body, "<release>1.0.10</release>") {
		t.Fatalf("release = %q, want 1.0.10:\n%s", body, body)
	}
}

func TestSynthesizeMetadataEmptyGAV(t *testing.T) {
	pe := newPullEnv(t, 1<<20) // no upstream
	resp, _ := pe.te.do(t, http.MethodGet, "/com/acme/ghost/maven-metadata.xml", "", "")
	if resp.StatusCode != 404 {
		t.Fatalf("empty GAV metadata: %d, want 404", resp.StatusCode)
	}
}

func TestSynthesizeMetadataHead(t *testing.T) {
	pe := newPullEnv(t, 1<<20) // no upstream
	const meta = "com/acme/widget/maven-metadata.xml"
	if resp, _ := pe.te.do(t, http.MethodPut, "/com/acme/widget/1.0.0/widget-1.0.0.jar", "", "jar1"); resp.StatusCode != 201 {
		t.Fatalf("PUT: %d", resp.StatusCode)
	}

	resp, _ := pe.te.do(t, http.MethodHead, "/"+meta, "", "")
	if resp.StatusCode != 200 {
		t.Fatalf("synthesized HEAD: %d, want 200", resp.StatusCode)
	}
	if cl := resp.Header.Get("Content-Length"); cl == "" || cl == "0" {
		t.Fatalf("synthesized HEAD Content-Length = %q", cl)
	}
	// HEAD must not store.
	if _, _, _, _, err := pe.st.Head(t.Context(), meta); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("HEAD must not store: %v", err)
	}
}

func TestSynthesizeUpstreamWins(t *testing.T) {
	// A non-reserved GAV with stored objects is still served by
	// pull-through: the upstream's metadata is authoritative, and a
	// missing upstream metadata is a 404, not a synthesis.
	const meta = "org/up/lib/maven-metadata.xml"
	fu := newFakeUpstream(t, "central", nil, map[string]int{"/" + meta: http.StatusNotFound})
	pe := newPullEnv(t, 1<<20, fu)
	if err := pe.st.Put(t.Context(), "org/up/lib/1.0/lib-1.0.jar", strings.NewReader("x"), 1, "application/java-archive", false); err != nil {
		t.Fatal(err)
	}
	resp, _ := pe.te.do(t, http.MethodGet, "/"+meta, "", "")
	if resp.StatusCode != 404 {
		t.Fatalf("upstream 404 must not synthesize: %d, want 404", resp.StatusCode)
	}
}

func TestSynthesizeReservedWithUpstream(t *testing.T) {
	// A reserved GAV synthesizes even when an upstream is configured.
	const meta = "com/acme/lib/maven-metadata.xml"
	fu := newFakeUpstream(t, "central", map[string]string{"/" + meta: "upstream"}, nil)
	pe := newPullEnv(t, 1<<20, fu)
	if resp, _ := pe.te.do(t, http.MethodPut, "/com/acme/lib/1.0.0/lib-1.0.0.jar", "", "x"); resp.StatusCode != 201 {
		t.Fatalf("PUT: %d", resp.StatusCode)
	}
	resp, body := pe.te.do(t, http.MethodGet, "/"+meta, "", "")
	if resp.StatusCode != 200 || !strings.Contains(body, "<version>1.0.0</version>") {
		t.Fatalf("reserved synthesis: %d %q", resp.StatusCode, body)
	}
	if fu.hitCount("/"+meta) != 0 {
		t.Fatalf("upstream was contacted %d times for a reserved path", fu.hitCount("/"+meta))
	}
}

// ---- concurrency: per-GAV serialization of read-modify-write ----

// gatedStore wraps a memStore to gate List and observe or block
// specific Put calls, so a test can pin the per-GAV serialization
// ordering.
type gatedStore struct {
	*memStore
	// listBlock, when non-nil, makes List wait on it.
	listBlock chan struct{}
	// listEntered is closed once when List is entered.
	listEntered chan struct{}
	// putEntered[path], when set, is closed once when Put is entered
	// for path.
	putEntered map[string]chan struct{}
	// putBlock[path], when set, blocks Put for path until closed.
	putBlock map[string]chan struct{}
}

func (g *gatedStore) List(ctx context.Context, prefix string) ([]string, error) {
	if g.listEntered != nil {
		select {
		case <-g.listEntered:
		default:
			close(g.listEntered)
		}
	}
	if g.listBlock != nil {
		<-g.listBlock
	}
	return g.memStore.List(ctx, prefix)
}

func (g *gatedStore) Put(ctx context.Context, path string, r io.Reader, size int64, ct string, immutable bool) error {
	if ch, ok := g.putEntered[path]; ok {
		select {
		case <-ch:
		default:
			close(ch)
		}
	}
	if ch, ok := g.putBlock[path]; ok {
		<-ch
	}
	return g.memStore.Put(ctx, path, r, size, ct, immutable)
}

// newGatedEnv is like newPullEnv but serves gate as the store backend.
func newGatedEnv(t *testing.T, gate *gatedStore) *pullEnv {
	t.Helper()
	fi := newFakeIssuer(t)
	cfg := &config.Config{
		Listen:            "127.0.0.1:0",
		ImmutableReleases: &[]bool{true}[0],
		MaxUploadBytes:    1 << 20,
		ReservedGroups:    []string{"com.acme"},
		Auth:              config.AuthConfig{Disabled: true},
	}
	pol, err := policy.New(nil, true)
	if err != nil {
		t.Fatal(err)
	}
	srv := api.New(cfg, gate, nil, pol, upstream.New(cfg.Upstream, 1<<20), slog.New(slog.NewTextHandler(io.Discard, nil)))
	te := &testEnv{srv: srv, fi: fi, st: gate.memStore}
	te.server = httptest.NewServer(srv.Handler())
	t.Cleanup(te.server.Close)
	return &pullEnv{te: te, st: gate.memStore}
}

// TestSynthesizeGAVLock pins the per-GAV serialization: while a
// synthesis GET is in flight, holding the GAV lock inside its version
// list, an artifact PUT for the same GAV must wait — its store write
// starts only after the synthesis releases the lock, and its
// invalidation then removes the synthesized document.
func TestSynthesizeGAVLock(t *testing.T) {
	const meta = "com/acme/widget/maven-metadata.xml"
	const jar1 = "com/acme/widget/1.0.0/widget-1.0.0.jar"
	const jar2 = "com/acme/widget/2.0.0/widget-2.0.0.jar"

	gate := &gatedStore{
		memStore:    newMemStore(),
		listBlock:   make(chan struct{}),
		listEntered: make(chan struct{}),
		putEntered:  map[string]chan struct{}{jar2: make(chan struct{})},
		putBlock:    map[string]chan struct{}{jar2: make(chan struct{})},
	}
	pe := newGatedEnv(t, gate)

	if err := pe.st.Put(t.Context(), jar1, strings.NewReader("jar1"), 4, "application/java-archive", false); err != nil {
		t.Fatal(err)
	}

	// The metadata GET blocks inside its version list, holding the GAV lock.
	getCode := make(chan int, 1)
	go func() {
		resp, err := http.Get(pe.te.server.URL + "/" + meta)
		if err != nil {
			getCode <- -1
			return
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		getCode <- resp.StatusCode
	}()
	<-gate.listEntered

	// The artifact PUT must wait on the GAV lock: its store write has
	// not started while the synthesis is in flight.
	putCode := make(chan int, 1)
	go func() {
		req, err := http.NewRequest(http.MethodPut, pe.te.server.URL+"/"+jar2, strings.NewReader("jar2"))
		if err != nil {
			putCode <- -1
			return
		}
		resp, err := pe.te.server.Client().Do(req)
		if err != nil {
			putCode <- -1
			return
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		putCode <- resp.StatusCode
	}()
	var race string
	select {
	case <-gate.putEntered[jar2]:
		race = "artifact PUT reached the store while synthesis held the GAV lock"
	case <-time.After(100 * time.Millisecond):
	}

	// Release the synthesis: it stores a document listing only 1.0.0.
	// (Both gates are always released, so the test cannot hang on failure.)
	close(gate.listBlock)
	if code := <-getCode; code != 200 {
		t.Fatalf("synthesized GET: %d", code)
	}
	doc, _, _, _, err := pe.st.Get(t.Context(), meta)
	if err != nil {
		t.Fatalf("stored document: %v", err)
	}
	b, _ := io.ReadAll(doc)
	if !strings.Contains(string(b), "<version>1.0.0</version>") || strings.Contains(string(b), "2.0.0") {
		t.Fatalf("synthesized document = %q, want only 1.0.0", b)
	}
	// The frozen PUT has not committed.
	if _, _, _, _, err := pe.st.Head(t.Context(), jar2); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("frozen artifact PUT already committed: %v", err)
	}

	// Unfreeze the PUT: it stores 2.0.0 and invalidates the document.
	close(gate.putBlock[jar2])
	if code := <-putCode; code != 201 {
		t.Fatalf("artifact PUT: %d", code)
	}
	if race != "" {
		t.Fatal(race)
	}
	if _, _, _, _, err := pe.st.Head(t.Context(), meta); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("document not invalidated: %v", err)
	}

	// The next GET re-synthesizes with both versions.
	resp, body := pe.te.do(t, http.MethodGet, "/"+meta, "", "")
	if resp.StatusCode != 200 {
		t.Fatalf("re-synthesis GET: %d", resp.StatusCode)
	}
	if !strings.Contains(body, "<version>1.0.0</version>") || !strings.Contains(body, "<version>2.0.0</version>") {
		t.Fatalf("re-synthesized document missing versions:\n%s", body)
	}
}

// TestSynthesizeConcurrentPut hammers a GAV with concurrent artifact
// PUTs and metadata GETs: the stored synthesized document must never
// be present while missing a committed version.
func TestSynthesizeConcurrentPut(t *testing.T) {
	pe := newPullEnv(t, 1<<20) // no upstream
	const meta = "com/acme/widget/maven-metadata.xml"

	if resp, _ := pe.te.do(t, http.MethodPut, "/com/acme/widget/1.0.0/widget-1.0.0.jar", "", "jar1"); resp.StatusCode != 201 {
		t.Fatalf("PUT 1.0.0: %d", resp.StatusCode)
	}
	const versions = 12
	for i := 2; i <= versions; i++ {
		ver := strconv.Itoa(i) + ".0.0"
		putCode := make(chan int, 1)
		go func() {
			req, err := http.NewRequest(http.MethodPut, pe.te.server.URL+"/com/acme/widget/"+ver+"/widget-"+ver+".jar", strings.NewReader("jar"))
			if err != nil {
				putCode <- -1
				return
			}
			resp, err := pe.te.server.Client().Do(req)
			if err != nil {
				putCode <- -1
				return
			}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			putCode <- resp.StatusCode
		}()
		getCode := make(chan int, 1)
		go func() {
			resp, err := http.Get(pe.te.server.URL + "/" + meta)
			if err != nil {
				getCode <- -1
				return
			}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			getCode <- resp.StatusCode
		}()
		if code := <-putCode; code != 201 {
			t.Fatalf("PUT %s: %d", ver, code)
		}
		if code := <-getCode; code != 200 {
			t.Fatalf("GET metadata: %d", code)
		}

		// Invariant: a stored document lists every committed version.
		doc, _, _, _, err := pe.st.Get(t.Context(), meta)
		if err != nil {
			if !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("stored document: %v", err)
			}
			continue // absent: the PUT's invalidation removed it
		}
		b, _ := io.ReadAll(doc)
		for j := 1; j <= i; j++ {
			if !strings.Contains(string(b), "<version>"+strconv.Itoa(j)+".0.0</version>") {
				t.Fatalf("stored document missing %s:\n%s", strconv.Itoa(j)+".0.0", b)
			}
		}
	}

	resp, body := pe.te.do(t, http.MethodGet, "/"+meta, "", "")
	if resp.StatusCode != 200 {
		t.Fatalf("final GET: %d", resp.StatusCode)
	}
	for j := 1; j <= versions; j++ {
		if !strings.Contains(body, "<version>"+strconv.Itoa(j)+".0.0</version>") {
			t.Fatalf("final document missing %s:\n%s", strconv.Itoa(j)+".0.0", body)
		}
	}
}
