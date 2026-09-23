//go:build integration

package integration

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/brutasse/pier/internal/config"
)

// fakeUpstream serves fixed objects and counts hits per path. When
// authUser is set, every request must be basic-authenticated with
// authUser/authPass or gets a 401.
type fakeUpstream struct {
	name string
	ts   *httptest.Server

	mu       sync.Mutex
	hits     map[string]int
	objects  map[string]string
	status   map[string]int
	authUser string
	authPass string
}

func newFakeUpstream(t *testing.T, name string, objects map[string]string, statuses map[string]int) *fakeUpstream {
	t.Helper()
	if statuses == nil {
		statuses = map[string]int{}
	}
	fu := &fakeUpstream{
		name:    name,
		hits:    map[string]int{},
		objects: objects,
		status:  statuses,
	}
	fu.ts = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		fu.mu.Lock()
		fu.hits[p]++
		au, ap := fu.authUser, fu.authPass
		fu.mu.Unlock()
		if au != "" {
			u, pw, ok := r.BasicAuth()
			if !ok || u != au || pw != ap {
				w.Header().Set("Content-Type", "application/octet-stream")
				w.Header().Set("Content-Length", "0")
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
		}

		st, data := http.StatusOK, fu.objects[p]
		if s, ok := fu.status[p]; ok {
			st, data = s, ""
		} else if _, ok := fu.objects[p]; !ok {
			st = http.StatusNotFound
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", strconv.Itoa(len(data)))
		w.WriteHeader(st)
		if r.Method != http.MethodHead {
			w.Write([]byte(data))
		}
	}))
	t.Cleanup(fu.ts.Close)
	return fu
}

func (fu *fakeUpstream) hitCount(path string) int {
	fu.mu.Lock()
	defer fu.mu.Unlock()
	return fu.hits[path]
}

// setAuth makes the upstream require basic auth.
func (fu *fakeUpstream) setAuth(user, pass string) {
	fu.mu.Lock()
	fu.authUser, fu.authPass = user, pass
	fu.mu.Unlock()
}

// TestE2EPullThrough exercises the pull-through cache against a real S3
// backend: a cold GET pulls from the fake upstream, the object and its
// sidecar checksums land in the bucket, and a second GET is served from
// the cache.
func TestE2EPullThrough(t *testing.T) {
	const jar = "org/upstream/tool/1.0/tool-1.0.jar"
	const jarContent = "upstream jar bytes"
	const pom = "org/upstream/tool/1.0/tool-1.0.pom"
	const pomContent = "<project><modelVersion>4.0.0</modelVersion></project>"
	fu := newFakeUpstream(t, "central", map[string]string{
		"/" + jar: jarContent,
		"/" + pom: pomContent,
	}, nil)
	e := newEnvWith(t, envOpts{
		upstream: []config.Upstream{{Name: fu.name, URL: fu.ts.URL}},
	})
	ctx := context.Background()
	sc := e.s3Client(ctx)
	ops := e.opsToken(t)
	pub := e.publishToken(t)

	// Uploads outside the reserved groups are rejected, with or
	// without a publish-authorized token.
	if resp, body := e.do(t, http.MethodPut, "/"+jar, pub, "x"); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("PUT non-reserved: %d %s, want 403", resp.StatusCode, body)
	}
	// Uploads inside the reserved groups still work.
	if resp, body := e.do(t, http.MethodPut, "/com/acme/widget/1.0.0/widget-1.0.0.jar", pub, "internal"); resp.StatusCode != http.StatusCreated {
		t.Fatalf("PUT reserved: %d %s, want 201", resp.StatusCode, body)
	}

	// Cold pull: GET an upstream-only artifact.
	resp, body := e.do(t, http.MethodGet, "/"+jar, ops, "")
	if resp.StatusCode != http.StatusOK || body != jarContent {
		t.Fatalf("cold GET: %d %q", resp.StatusCode, body)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/java-archive" {
		t.Errorf("content-type = %q", ct)
	}
	if n := fu.hitCount("/" + jar); n != 1 {
		t.Fatalf("upstream hits = %d, want 1", n)
	}

	// The response can complete before the cache write does; wait the
	// pull out before asserting on the store.
	e.srv.WaitIdle()

	// The object and its sidecar checksums are now in S3.
	key := e.prefix + "/" + jar
	obj, err := sc.GetObject(ctx, &s3.GetObjectInput{Bucket: &e.bucket, Key: &key})
	if err != nil {
		t.Fatalf("s3 get: %v", err)
	}
	b, _ := io.ReadAll(obj.Body)
	obj.Body.Close()
	if !bytes.Equal(b, []byte(jarContent)) {
		t.Fatalf("stored object = %q", b)
	}
	jarSum := []byte(jarContent)
	for _, algo := range []string{"md5", "sha1", "sha256", "sha512"} {
		sum, ok := mavenHashHex(algo, jarSum)
		if !ok {
			t.Fatalf("no hasher for %s", algo)
		}
		cs, err := sc.GetObject(ctx, &s3.GetObjectInput{Bucket: &e.bucket, Key: aws.String(key + "." + algo)})
		if err != nil {
			t.Fatalf("s3 get %s: %v", algo, err)
		}
		csBody, _ := io.ReadAll(cs.Body)
		cs.Body.Close()
		if strings.TrimSpace(string(csBody)) != sum {
			t.Errorf("%s sidecar = %q, want %q", algo, csBody, sum)
		}
	}

	// A second GET is served from the cache: no new upstream hit.
	resp, body = e.do(t, http.MethodGet, "/"+jar, ops, "")
	if resp.StatusCode != http.StatusOK || body != jarContent {
		t.Fatalf("warm GET: %d %q", resp.StatusCode, body)
	}
	if n := fu.hitCount("/" + jar); n != 1 {
		t.Fatalf("upstream hits after warm GET = %d, want 1", n)
	}

	// Reserved groups are never pulled from upstream.
	if resp, _ := e.do(t, http.MethodGet, "/com/acme/ghost/1.0.0/ghost-1.0.0.jar", ops, ""); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("reserved miss: %d, want 404", resp.StatusCode)
	}
	if n := fu.hitCount("/com/acme/ghost/1.0.0/ghost-1.0.0.jar"); n != 0 {
		t.Fatalf("upstream contacted %d times for a reserved path", n)
	}

	// HEAD on a cold miss peeks upstream without storing.
	resp, _ = e.do(t, http.MethodHead, "/"+pom, ops, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("HEAD peek: %d", resp.StatusCode)
	}
	if cl := resp.Header.Get("Content-Length"); cl != strconv.Itoa(len(pomContent)) {
		t.Errorf("peek Content-Length = %q, want %d", cl, len(pomContent))
	}
	if _, err := sc.HeadObject(ctx, &s3.HeadObjectInput{Bucket: &e.bucket, Key: aws.String(e.prefix + "/" + pom)}); err == nil {
		t.Fatal("peek must not store the object")
	}

	// Range on a cold miss: the full object is pulled to S3, the
	// client gets the range.
	const big = "org/upstream/big/1.0/big-1.0.jar"
	fu.mu.Lock()
	fu.objects["/"+big] = "0123456789"
	fu.mu.Unlock()
	req, err := http.NewRequest(http.MethodGet, e.base+"/"+big, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+ops)
	req.Header.Set("Range", "bytes=2-5")
	rr, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	rb, _ := io.ReadAll(rr.Body)
	rr.Body.Close()
	if rr.StatusCode != http.StatusPartialContent || string(rb) != "2345" {
		t.Fatalf("cold range GET: %d %q", rr.StatusCode, rb)
	}
	if cr := rr.Header.Get("Content-Range"); cr != "bytes 2-5/10" {
		t.Errorf("cold range Content-Range = %q, want bytes 2-5/10", cr)
	}
	// The 206 returns before the full object is finalized in the
	// store; wait the pull out before asserting.
	e.srv.WaitIdle()
	bk := e.prefix + "/" + big
	obj, err = sc.GetObject(ctx, &s3.GetObjectInput{Bucket: &e.bucket, Key: &bk})
	if err != nil {
		t.Fatalf("s3 get big: %v", err)
	}
	b, _ = io.ReadAll(obj.Body)
	obj.Body.Close()
	if string(b) != "0123456789" {
		t.Fatalf("stored big object = %q", b)
	}
}

// TestE2EMetadataTTL verifies that pulled maven-metadata.xml is
// revalidated from upstream once its TTL expires, and served from the
// cache while fresh.
func TestE2EMetadataTTL(t *testing.T) {
	const meta = "org/upstream/lib/maven-metadata.xml"
	v1 := "<metadata><versioning><versions><version>1.0</version></versions></versioning></metadata>"
	v2 := "<metadata><versioning><versions><version>1.0</version><version>2.0</version></versions></versioning></metadata>"
	fu := newFakeUpstream(t, "central", map[string]string{"/" + meta: v1}, nil)
	// The TTL must exceed the LastModified skew of the test backend:
	// rclone serve s3 truncates it to second granularity.
	ttl := 2 * time.Second
	e := newEnvWith(t, envOpts{
		upstream:    []config.Upstream{{Name: fu.name, URL: fu.ts.URL}},
		metadataTTL: &ttl,
	})
	ops := e.opsToken(t)

	// Cold pull.
	resp, body := e.do(t, http.MethodGet, "/"+meta, ops, "")
	if resp.StatusCode != http.StatusOK || body != v1 {
		t.Fatalf("cold GET: %d %q", resp.StatusCode, body)
	}
	if n := fu.hitCount("/" + meta); n != 1 {
		t.Fatalf("upstream hits = %d, want 1", n)
	}
	// Fresh: served from the cache.
	resp, body = e.do(t, http.MethodGet, "/"+meta, ops, "")
	if resp.StatusCode != http.StatusOK || body != v1 {
		t.Fatalf("fresh GET: %d %q", resp.StatusCode, body)
	}
	if n := fu.hitCount("/" + meta); n != 1 {
		t.Fatalf("fresh GET revalidated: hits = %d, want 1", n)
	}

	// The upstream publishes a new version; after the TTL expires the
	// metadata is revalidated.
	fu.mu.Lock()
	fu.objects["/"+meta] = v2
	fu.mu.Unlock()
	time.Sleep(ttl + 300*time.Millisecond)
	resp, body = e.do(t, http.MethodGet, "/"+meta, ops, "")
	if resp.StatusCode != http.StatusOK || body != v2 {
		t.Fatalf("revalidation GET: %d %q, want 200 %q", resp.StatusCode, body, v2)
	}
	if n := fu.hitCount("/" + meta); n != 2 {
		t.Fatalf("upstream hits = %d, want 2", n)
	}
	// The refreshed object is in S3 (wait the revalidation pull out).
	e.srv.WaitIdle()
	key := e.prefix + "/" + meta
	ctx := context.Background()
	obj, err := e.s3Client(ctx).GetObject(ctx, &s3.GetObjectInput{Bucket: &e.bucket, Key: &key})
	if err != nil {
		t.Fatalf("s3 get: %v", err)
	}
	b, _ := io.ReadAll(obj.Body)
	obj.Body.Close()
	if string(b) != v2 {
		t.Fatalf("stored metadata = %q, want %q", b, v2)
	}
}

// TestE2ESynthesizeMetadata verifies that maven-metadata.xml is
// synthesized from the stored versions for a private GAV, and kept
// fresh by the write-side invalidation.
func TestE2ESynthesizeMetadata(t *testing.T) {
	e := newEnv(t) // no upstream
	pub := e.publishToken(t)
	ops := e.opsToken(t)
	const meta = "com/acme/synth/maven-metadata.xml"

	// A deploy-file-style publish: the artifact only, no metadata.
	if resp, body := e.do(t, http.MethodPut, "/com/acme/synth/1.0.0/synth-1.0.0.jar", pub, "jar1"); resp.StatusCode != http.StatusCreated {
		t.Fatalf("PUT 1.0.0: %d %s", resp.StatusCode, body)
	}
	resp, body := e.do(t, http.MethodGet, "/"+meta, ops, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("synthesized GET: %d %s", resp.StatusCode, body)
	}
	if !strings.Contains(body, "<version>1.0.0</version>") || !strings.Contains(body, "<release>1.0.0</release>") {
		t.Fatalf("synthesized metadata = %q", body)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/xml" {
		t.Errorf("content-type = %q", ct)
	}
	// The synthesized metadata and its checksums are in S3.
	key := e.prefix + "/" + meta
	sc := e.s3Client(context.Background())
	ctx := context.Background()
	cs, err := sc.GetObject(ctx, &s3.GetObjectInput{Bucket: &e.bucket, Key: aws.String(key + ".sha1")})
	if err != nil {
		t.Fatalf("s3 get sidecar: %v", err)
	}
	csBody, _ := io.ReadAll(cs.Body)
	cs.Body.Close()
	sum, ok := mavenHashHex("sha1", []byte(body))
	if !ok {
		t.Fatal("no sha1 hasher")
	}
	if strings.TrimSpace(string(csBody)) != sum {
		t.Errorf("sidecar sha1 = %q, want %q", csBody, sum)
	}

	// A second version publish invalidates the metadata: the next GET
	// lists both versions.
	if resp, body := e.do(t, http.MethodPut, "/com/acme/synth/2.0.0/synth-2.0.0.jar", pub, "jar2"); resp.StatusCode != http.StatusCreated {
		t.Fatalf("PUT 2.0.0: %d %s", resp.StatusCode, body)
	}
	resp, body = e.do(t, http.MethodGet, "/"+meta, ops, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("re-synthesis GET: %d %s", resp.StatusCode, body)
	}
	if !strings.Contains(body, "<version>1.0.0</version>") || !strings.Contains(body, "<version>2.0.0</version>") {
		t.Fatalf("re-synthesized metadata missing versions: %q", body)
	}
	if !strings.Contains(body, "<release>2.0.0</release>") {
		t.Fatalf("release = %q, want 2.0.0", body)
	}
	// HEAD synthesizes without storing over the GET-cached copy.
	if resp, _ := e.do(t, http.MethodHead, "/"+meta, ops, ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("synthesized HEAD: %d", resp.StatusCode)
	}
}

// TestE2EPullThroughBasicAuth verifies that pull-through works against an
// upstream that requires basic auth, and that wrong credentials fail
// loudly (502) rather than as a miss.
func TestE2EPullThroughBasicAuth(t *testing.T) {
	const jar = "org/upstream/auth/1.0/auth-1.0.jar"
	const jarContent = "authenticated jar bytes"
	fu := newFakeUpstream(t, "private", map[string]string{"/" + jar: jarContent}, nil)
	fu.setAuth("puller", "s3cret")
	e := newEnvWith(t, envOpts{
		upstream: []config.Upstream{{Name: fu.name, URL: fu.ts.URL, Username: "puller", Password: "s3cret"}},
	})
	ops := e.opsToken(t)

	resp, body := e.do(t, http.MethodGet, "/"+jar, ops, "")
	if resp.StatusCode != http.StatusOK || body != jarContent {
		t.Fatalf("cold GET: %d %q", resp.StatusCode, body)
	}
	if n := fu.hitCount("/" + jar); n != 1 {
		t.Fatalf("upstream hits = %d, want 1", n)
	}

	// Served from the cache on the second GET; no new upstream hit.
	resp, body = e.do(t, http.MethodGet, "/"+jar, ops, "")
	if resp.StatusCode != http.StatusOK || body != jarContent {
		t.Fatalf("warm GET: %d %q", resp.StatusCode, body)
	}
	if n := fu.hitCount("/" + jar); n != 1 {
		t.Fatalf("upstream hits after warm GET = %d, want 1", n)
	}

	// Wrong credentials: the pull fails with a 502, not a 404.
	e2 := newEnvWith(t, envOpts{
		upstream: []config.Upstream{{Name: fu.name, URL: fu.ts.URL, Username: "puller", Password: "wrong"}},
	})
	if resp, _ := e2.do(t, http.MethodGet, "/"+jar, e2.opsToken(t), ""); resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("wrong credentials: %d, want 502", resp.StatusCode)
	}
}
