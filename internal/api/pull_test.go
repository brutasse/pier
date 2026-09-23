package api_test

import (
	"crypto/sha256"
	"encoding/hex"
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

// fakeUpstream serves fixed objects, counts hits per path, and can delay
// or stream responses slowly (for single-flight and abort tests).
type fakeUpstream struct {
	name       string
	ts         *httptest.Server
	mu         sync.Mutex
	hits       map[string]int
	objects    map[string]string
	status     map[string]int
	delay      time.Duration // one-shot delay before the response
	chunk      int           // stream the body in chunks of this size
	chunkDelay time.Duration // between chunk writes

	// gateAfter, when positive, makes the handler stop once it has
	// written that many body bytes, until release is called (for
	// streaming tests).
	gateAfter  int64
	gatedNow   bool
	releaseNow bool
	released   chan struct{}
}

func newFakeUpstream(t *testing.T, name string, objects map[string]string, statuses map[string]int) *fakeUpstream {
	t.Helper()
	if statuses == nil {
		statuses = map[string]int{}
	}
	fu := &fakeUpstream{
		name:     name,
		hits:     map[string]int{},
		objects:  objects,
		status:   statuses,
		released: make(chan struct{}),
	}
	fu.ts = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		fu.mu.Lock()
		fu.hits[p]++
		fu.mu.Unlock()

		st, data := http.StatusOK, fu.objects[p]
		if s, ok := fu.status[p]; ok {
			st, data = s, ""
		} else if _, ok := fu.objects[p]; !ok {
			st = http.StatusNotFound
		}
		if fu.delay > 0 {
			time.Sleep(fu.delay)
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", strconv.Itoa(len(data)))
		w.WriteHeader(st)
		if r.Method == http.MethodHead {
			return
		}
		b := []byte(data)
		var written int64
		for len(b) > 0 {
			n := fu.chunk
			if n <= 0 || n > len(b) {
				n = len(b)
			}
			if _, err := w.Write(b[:n]); err != nil {
				return // consumer went away
			}
			b = b[n:]
			written += int64(n)
			if fu.gateAfter > 0 && written >= fu.gateAfter {
				fu.mu.Lock()
				goGate := !fu.releaseNow
				if goGate {
					fu.gatedNow = true
				}
				fu.mu.Unlock()
				if goGate {
					<-fu.released
				}
			}
			if fu.chunkDelay > 0 {
				time.Sleep(fu.chunkDelay)
			}
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

// release unblocks a handler stopped at the gate. It is safe to call
// before the handler has reached the gate.
func (fu *fakeUpstream) release() {
	fu.mu.Lock()
	if fu.gatedNow {
		fu.mu.Unlock()
		close(fu.released)
		return
	}
	fu.releaseNow = true
	fu.mu.Unlock()
}

// pullEnv is a test environment with pull-through enabled and
// authentication disabled (auth is covered separately).
type pullEnv struct {
	te *testEnv
	st *memStore
}

func newPullEnv(t *testing.T, maxBytes int64, ups ...*fakeUpstream) *pullEnv {
	t.Helper()
	fi := newFakeIssuer(t)
	st := newMemStore()
	cfg := &config.Config{
		Listen:            "127.0.0.1:0",
		ImmutableReleases: &[]bool{true}[0],
		MaxUploadBytes:    maxBytes,
		ReservedGroups:    []string{"com.acme"},
		Auth:              config.AuthConfig{Disabled: true},
	}
	for _, u := range ups {
		cfg.Upstream = append(cfg.Upstream, config.Upstream{Name: u.name, URL: u.ts.URL})
	}
	pol, err := policy.New(nil, true)
	if err != nil {
		t.Fatal(err)
	}
	up := upstream.New(cfg.Upstream, maxBytes)
	srv := api.New(cfg, st, nil, pol, up, slog.New(slog.NewTextHandler(io.Discard, nil)))
	te := &testEnv{srv: srv, fi: fi, st: st}
	te.server = httptest.NewServer(srv.Handler())
	t.Cleanup(te.server.Close)
	return &pullEnv{te: te, st: st}
}

// newPullEnvTTL is like newPullEnv but sets metadata_ttl explicitly.
func newPullEnvTTL(t *testing.T, maxBytes int64, ttl *time.Duration, ups ...*fakeUpstream) *pullEnv {
	t.Helper()
	fi := newFakeIssuer(t)
	st := newMemStore()
	cfg := &config.Config{
		Listen:            "127.0.0.1:0",
		ImmutableReleases: &[]bool{true}[0],
		MaxUploadBytes:    maxBytes,
		ReservedGroups:    []string{"com.acme"},
		Auth:              config.AuthConfig{Disabled: true},
		MetadataTTL:       ttl,
	}
	for _, u := range ups {
		cfg.Upstream = append(cfg.Upstream, config.Upstream{Name: u.name, URL: u.ts.URL})
	}
	pol, err := policy.New(nil, true)
	if err != nil {
		t.Fatal(err)
	}
	up := upstream.New(cfg.Upstream, maxBytes)
	srv := api.New(cfg, st, nil, pol, up, slog.New(slog.NewTextHandler(io.Discard, nil)))
	te := &testEnv{srv: srv, fi: fi, st: st}
	te.server = httptest.NewServer(srv.Handler())
	t.Cleanup(te.server.Close)
	return &pullEnv{te: te, st: st}
}

// setPullPartSize shrinks the part size for a test and restores it
// afterwards.
func setPullPartSize(t *testing.T, size int) {
	t.Helper()
	old := api.PullPartSize
	api.PullPartSize = size
	t.Cleanup(func() { api.PullPartSize = old })
}

func TestPullThrough(t *testing.T) {
	const jar = "org/upstream/tool/1.0/tool-1.0.jar"
	fu := newFakeUpstream(t, "central", map[string]string{"/" + jar: "upstream-jar-bytes"}, nil)
	pe := newPullEnv(t, 1<<20, fu)

	resp, body := pe.te.do(t, http.MethodGet, "/"+jar, "", "")
	if resp.StatusCode != 200 || body != "upstream-jar-bytes" {
		t.Fatalf("pull GET: %d %q", resp.StatusCode, body)
	}
	if h := resp.Header.Get("Accept-Ranges"); h != "bytes" {
		t.Errorf("Accept-Ranges = %q", h)
	}
	if fu.hitCount("/"+jar) != 1 {
		t.Errorf("upstream hits = %d, want 1", fu.hitCount("/"+jar))
	}

	pe.te.srv.WaitIdle()
	// The object and its computed sidecars are now in the store.
	obj, _, _, _, err := pe.st.Get(t.Context(), jar)
	if err != nil {
		t.Fatalf("store get: %v", err)
	}
	b, _ := io.ReadAll(obj)
	if string(b) != "upstream-jar-bytes" {
		t.Errorf("stored object = %q", b)
	}
	cs, _, _, _, err := pe.st.Get(t.Context(), jar+".sha1")
	if err != nil {
		t.Fatalf("sidecar sha1 missing: %v", err)
	}
	cb, _ := io.ReadAll(cs)
	if want := sha1hex("upstream-jar-bytes"); strings.TrimSpace(string(cb)) != want {
		t.Errorf("sidecar sha1 = %q, want %q", cb, want)
	}

	// A second GET is served from the store without another upstream hit.
	resp, body = pe.te.do(t, http.MethodGet, "/"+jar, "", "")
	if resp.StatusCode != 200 || body != "upstream-jar-bytes" {
		t.Fatalf("warm GET: %d %q", resp.StatusCode, body)
	}
	if fu.hitCount("/"+jar) != 1 {
		t.Errorf("upstream hits after warm GET = %d, want 1", fu.hitCount("/"+jar))
	}
}

func TestPullUpstreamFallbackOrder(t *testing.T) {
	const jar = "org/upstream/tool/1.0/tool-1.0.jar"
	a := newFakeUpstream(t, "a", nil, map[string]int{"/" + jar: http.StatusNotFound})
	b := newFakeUpstream(t, "b", map[string]string{"/" + jar: "from-b"}, nil)
	pe := newPullEnv(t, 1<<20, a, b)

	resp, body := pe.te.do(t, http.MethodGet, "/"+jar, "", "")
	if resp.StatusCode != 200 || body != "from-b" {
		t.Fatalf("pull GET: %d %q", resp.StatusCode, body)
	}
	if a.hitCount("/"+jar) != 1 || b.hitCount("/"+jar) != 1 {
		t.Errorf("hits = %d / %d, want 1 / 1", a.hitCount("/"+jar), b.hitCount("/"+jar))
	}
}

func TestPullNotFound(t *testing.T) {
	const jar = "org/upstream/ghost/1.0/ghost-1.0.jar"
	pe := newPullEnv(t, 1<<20, newFakeUpstream(t, "central", nil, nil))
	resp, _ := pe.te.do(t, http.MethodGet, "/"+jar, "", "")
	if resp.StatusCode != 404 {
		t.Fatalf("missing everywhere: %d, want 404", resp.StatusCode)
	}
	if _, _, _, _, err := pe.st.Head(t.Context(), jar); err != store.ErrNotFound {
		t.Fatalf("store should be empty: %v", err)
	}
}

func TestPullUpstreamError(t *testing.T) {
	const jar = "org/upstream/tool/1.0/tool-1.0.jar"
	fu := newFakeUpstream(t, "central", map[string]string{"/" + jar: "x"}, map[string]int{"/" + jar: http.StatusInternalServerError})
	pe := newPullEnv(t, 1<<20, fu)
	resp, _ := pe.te.do(t, http.MethodGet, "/"+jar, "", "")
	if resp.StatusCode != 502 {
		t.Fatalf("upstream 500: %d, want 502", resp.StatusCode)
	}
}

func TestPullReservedNever(t *testing.T) {
	const jar = "com/acme/internal/1.0/internal-1.0.jar"
	fu := newFakeUpstream(t, "central", map[string]string{"/" + jar: "should-not-be-pulled"}, nil)
	pe := newPullEnv(t, 1<<20, fu)
	resp, _ := pe.te.do(t, http.MethodGet, "/"+jar, "", "")
	if resp.StatusCode != 404 {
		t.Fatalf("reserved miss: %d, want 404", resp.StatusCode)
	}
	if fu.hitCount("/"+jar) != 0 {
		t.Errorf("upstream was contacted %d times for a reserved path", fu.hitCount("/"+jar))
	}
}

func TestPullHeadPeek(t *testing.T) {
	const jar = "org/upstream/tool/1.0/tool-1.0.jar"
	fu := newFakeUpstream(t, "central", map[string]string{"/" + jar: "0123456789"}, nil)
	pe := newPullEnv(t, 1<<20, fu)

	resp, body := pe.te.do(t, http.MethodHead, "/"+jar, "", "")
	if resp.StatusCode != 200 {
		t.Fatalf("HEAD peek: %d", resp.StatusCode)
	}
	if cl := resp.Header.Get("Content-Length"); cl != "10" {
		t.Errorf("Content-Length = %q, want 10", cl)
	}
	if body != "" {
		t.Errorf("HEAD body = %q", body)
	}
	// Nothing was stored by the peek.
	if _, _, _, _, err := pe.st.Head(t.Context(), jar); err != store.ErrNotFound {
		t.Fatalf("peek must not store: %v", err)
	}
	// The following GET does the actual pull.
	resp, body = pe.te.do(t, http.MethodGet, "/"+jar, "", "")
	if resp.StatusCode != 200 || string(body) != "0123456789" {
		t.Fatalf("GET after peek: %d %q", resp.StatusCode, body)
	}
}

func TestPullHeadPeekNotFound(t *testing.T) {
	pe := newPullEnv(t, 1<<20, newFakeUpstream(t, "central", nil, nil))
	resp, _ := pe.te.do(t, http.MethodHead, "/org/upstream/ghost/1.0/ghost-1.0.jar", "", "")
	if resp.StatusCode != 404 {
		t.Fatalf("HEAD peek missing: %d, want 404", resp.StatusCode)
	}
}

func TestPullChecksumComputedFromBase(t *testing.T) {
	const jar = "org/upstream/tool/1.0/tool-1.0.jar"
	fu := newFakeUpstream(t, "central", map[string]string{"/" + jar + ".sha1": strings.Repeat("f", 40)}, nil)
	pe := newPullEnv(t, 1<<20, fu)

	// Seed the base object directly in the store.
	if err := pe.st.Put(t.Context(), jar, strings.NewReader("base-bytes"), 10, "application/java-archive", false); err != nil {
		t.Fatal(err)
	}

	resp, body := pe.te.do(t, http.MethodGet, "/"+jar+".sha1", "", "")
	if resp.StatusCode != 200 {
		t.Fatalf("checksum GET: %d", resp.StatusCode)
	}
	if want := sha1hex("base-bytes"); strings.TrimSpace(body) != want {
		t.Fatalf("computed sha1 = %q, want %q", body, want)
	}
	if fu.hitCount("/"+jar+".sha1") != 0 {
		t.Errorf("upstream was contacted for a computable checksum")
	}
	// The computed checksum is stored for future requests.
	if _, _, _, _, err := pe.st.Head(t.Context(), jar+".sha1"); err != nil {
		t.Fatalf("computed checksum not stored: %v", err)
	}
}

func TestPullChecksumBaseMissing(t *testing.T) {
	const jar = "org/upstream/tool/1.0/tool-1.0.jar"
	sum := sha1hex("whatever")
	fu := newFakeUpstream(t, "central", map[string]string{"/" + jar + ".sha1": sum + "\n"}, nil)
	pe := newPullEnv(t, 1<<20, fu)

	resp, body := pe.te.do(t, http.MethodGet, "/"+jar+".sha1", "", "")
	if resp.StatusCode != 200 {
		t.Fatalf("checksum GET: %d %s", resp.StatusCode, body)
	}
	if strings.TrimSpace(body) != sum {
		t.Fatalf("checksum = %q, want %q", body, sum)
	}
	if fu.hitCount("/"+jar+".sha1") != 1 {
		t.Errorf("upstream hits = %d, want 1", fu.hitCount("/"+jar+".sha1"))
	}
}

func TestPullRangeCold(t *testing.T) {
	const jar = "org/upstream/tool/1.0/tool-1.0.jar"
	fu := newFakeUpstream(t, "central", map[string]string{"/" + jar: "0123456789"}, nil)
	pe := newPullEnv(t, 1<<20, fu)

	req, err := http.NewRequest(http.MethodGet, pe.te.server.URL+"/"+jar, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Range", "bytes=2-5")
	resp, err := pe.te.server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 206 || string(body) != "2345" {
		t.Fatalf("cold range GET: %d %q", resp.StatusCode, body)
	}
	if cr := resp.Header.Get("Content-Range"); cr != "bytes 2-5/10" {
		t.Errorf("Content-Range = %q, want bytes 2-5/10", cr)
	}
	if cl := resp.Header.Get("Content-Length"); cl != "4" {
		t.Errorf("Content-Length = %q, want 4", cl)
	}
	// The full object was stored.
	pe.te.srv.WaitIdle()
	obj, size, _, _, err := pe.st.Get(t.Context(), jar)
	if err != nil || size != 10 {
		t.Fatalf("stored object: %v size=%d", err, size)
	}
	b, _ := io.ReadAll(obj)
	if string(b) != "0123456789" {
		t.Errorf("stored object = %q", b)
	}
}

func TestPullRangeStreamsIncrementally(t *testing.T) {
	setPullPartSize(t, 1<<20)

	const jar = "org/upstream/slow/1.0/slow-1.0.jar"
	// 4 MiB of deterministic content.
	var sb strings.Builder
	for i := 0; i < 4<<20; i++ {
		sb.WriteByte(byte((i*31 + 7) % 256))
	}
	content := sb.String()
	fu := newFakeUpstream(t, "central", map[string]string{"/" + jar: content}, nil)
	fu.chunk = 1024
	fu.gateAfter = 2 << 20 // stop once half the object is sent
	pe := newPullEnv(t, 64<<20, fu)

	req, err := http.NewRequest(http.MethodGet, pe.te.server.URL+"/"+jar, nil)
	if err != nil {
		t.Fatal(err)
	}
	// The range spans the server's 4 KiB response buffer, so the
	// client receives it in flight: the first flush lands as the
	// upstream stream starts, long before the pull completes.
	req.Header.Set("Range", "bytes=0-8191")
	resp, err := pe.te.server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body) // returns once the range is delivered
	resp.Body.Close()
	if resp.StatusCode != 206 || string(body) != content[:8192] {
		t.Fatalf("incremental range GET: %d %q", resp.StatusCode, body)
	}
	if cr := resp.Header.Get("Content-Range"); cr != "bytes 0-8191/"+strconv.Itoa(len(content)) {
		t.Errorf("Content-Range = %q", cr)
	}
	// The upstream is still mid-stream: the client already has its
	// range, but the object is not in the store yet.
	if _, _, _, _, err := pe.st.Head(t.Context(), jar); err != store.ErrNotFound {
		t.Fatalf("object in store before the upstream finished: %v", err)
	}

	fu.release()
	pe.te.srv.WaitIdle()
	// The pull completed in the background: full object and sidecars.
	obj, size, _, _, err := pe.st.Get(t.Context(), jar)
	if err != nil || size != int64(len(content)) {
		t.Fatalf("stored object: %v size=%d", err, size)
	}
	b, _ := io.ReadAll(obj)
	if string(b) != content {
		t.Fatal("stored bytes differ")
	}
	sum := sha256.Sum256([]byte(content))
	cs, _, _, _, err := pe.st.Get(t.Context(), jar+".sha256")
	if err != nil {
		t.Fatalf("sha256 sidecar missing: %v", err)
	}
	cb, _ := io.ReadAll(cs)
	if strings.TrimSpace(string(cb)) != hex.EncodeToString(sum[:]) {
		t.Fatalf("sha256 sidecar = %q", cb)
	}
}

func TestPullRangeNotSatisfiable(t *testing.T) {
	const jar = "org/upstream/tool/1.0/tool-1.0.jar"
	fu := newFakeUpstream(t, "central", map[string]string{"/" + jar: "0123456789"}, nil)
	pe := newPullEnv(t, 1<<20, fu)

	// The range is beyond the object: 416 without pulling the body.
	req, err := http.NewRequest(http.MethodGet, pe.te.server.URL+"/"+jar, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Range", "bytes=100-200")
	resp, err := pe.te.server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 416 {
		t.Fatalf("unsatisfiable range: %d, want 416", resp.StatusCode)
	}
	if cr := resp.Header.Get("Content-Range"); cr != "bytes */10" {
		t.Errorf("Content-Range = %q, want bytes */10", cr)
	}
	// Nothing was stored.
	if _, _, _, _, err := pe.st.Head(t.Context(), jar); err != store.ErrNotFound {
		t.Fatalf("store should be empty: %v", err)
	}

	// A satisfiable range still pulls, and the object lands.
	req.Header.Set("Range", "bytes=2-5")
	resp, err = pe.te.server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 206 || string(body) != "2345" {
		t.Fatalf("satisfiable range after 416: %d %q", resp.StatusCode, body)
	}
	pe.te.srv.WaitIdle()
	if _, _, _, _, err := pe.st.Head(t.Context(), jar); err != nil {
		t.Fatalf("object not stored after satisfiable range: %v", err)
	}
	if hits := fu.hitCount("/" + jar); hits != 2 {
		t.Errorf("upstream hits = %d, want 2", hits)
	}
}

func TestPullRangeSuffix(t *testing.T) {
	const jar = "org/upstream/tool/1.0/tool-1.0.jar"
	fu := newFakeUpstream(t, "central", map[string]string{"/" + jar: "0123456789"}, nil)
	pe := newPullEnv(t, 1<<20, fu)

	req, err := http.NewRequest(http.MethodGet, pe.te.server.URL+"/"+jar, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Range", "bytes=-4")
	resp, err := pe.te.server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 206 || string(body) != "6789" {
		t.Fatalf("suffix range GET: %d %q", resp.StatusCode, body)
	}
	if cr := resp.Header.Get("Content-Range"); cr != "bytes 6-9/10" {
		t.Errorf("Content-Range = %q, want bytes 6-9/10", cr)
	}
	pe.te.srv.WaitIdle()
	obj, size, _, _, err := pe.st.Get(t.Context(), jar)
	if err != nil || size != 10 {
		t.Fatalf("stored object: %v size=%d", err, size)
	}
	b, _ := io.ReadAll(obj)
	if string(b) != "0123456789" {
		t.Errorf("stored object = %q", b)
	}
}

func TestPullRangeMalformedServesFull(t *testing.T) {
	const jar = "org/upstream/tool/1.0/tool-1.0.jar"
	fu := newFakeUpstream(t, "central", map[string]string{"/" + jar: "0123456789"}, nil)
	pe := newPullEnv(t, 1<<20, fu)

	// Multiple ranges are unsupported: the full representation goes
	// out, and the object is cached.
	req, err := http.NewRequest(http.MethodGet, pe.te.server.URL+"/"+jar, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Range", "bytes=0-1,3-4")
	resp, err := pe.te.server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || string(body) != "0123456789" {
		t.Fatalf("malformed range GET: %d %q", resp.StatusCode, body)
	}
	pe.te.srv.WaitIdle()
	if _, _, _, _, err := pe.st.Head(t.Context(), jar); err != nil {
		t.Fatalf("object not stored: %v", err)
	}
}

func TestPullRangeFailureAfterCommit(t *testing.T) {
	setPullPartSize(t, 4)
	const jar = "org/upstream/fail/1.0/fail-1.0.jar"
	fu := newFakeUpstream(t, "central", map[string]string{"/" + jar: "0123456789"}, nil)
	pe := newPullEnv(t, 1<<20, fu)
	// Every part upload fails; the 206 is committed before the first
	// part goes out.
	pe.st.shouldFailPart = func() bool { return true }

	req, err := http.NewRequest(http.MethodGet, pe.te.server.URL+"/"+jar, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Range", "bytes=4-9")
	resp, err := pe.te.server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, rerr := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 206 {
		t.Fatalf("committed range: %d, want 206", resp.StatusCode)
	}
	if rerr == nil && len(body) == 6 {
		t.Fatalf("full range served despite failed pull: %q", body)
	}

	pe.te.srv.WaitIdle()
	// The failed pull left no object and no in-flight upload.
	if _, _, _, _, err := pe.st.Head(t.Context(), jar); err != store.ErrNotFound {
		t.Fatalf("store must be empty after failed pull: %v", err)
	}
	if n := pe.st.uploadCount(); n != 0 {
		t.Fatalf("in-flight uploads = %d, want 0", n)
	}
}

func TestPullSingleFlight(t *testing.T) {
	const jar = "org/upstream/tool/1.0/tool-1.0.jar"
	fu := newFakeUpstream(t, "central", map[string]string{"/" + jar: "coalesced"}, nil)
	fu.delay = 100 * time.Millisecond
	pe := newPullEnv(t, 1<<20, fu)

	const n = 5
	results := make([]int, n)
	wg := sync.WaitGroup{}
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			resp, err := http.Get(pe.te.server.URL + "/" + jar)
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
	if hits := fu.hitCount("/" + jar); hits != 1 {
		t.Fatalf("upstream hits = %d, want 1 (single-flight)", hits)
	}
}

func TestPullLargeStreaming(t *testing.T) {
	setPullPartSize(t, 1<<20) // 1 MiB parts to force several of them

	const jar = "org/upstream/big/1.0/big-1.0.jar"
	// 3.5 MiB of pseudo-random content.
	var sb strings.Builder
	for i := 0; i < 7*512*1024; i++ {
		sb.WriteByte(byte((i*7 + 13) % 256))
	}
	content := sb.String()
	fu := newFakeUpstream(t, "central", map[string]string{"/" + jar: content}, nil)
	pe := newPullEnv(t, 64<<20, fu)

	resp, body := pe.te.do(t, http.MethodGet, "/"+jar, "", "")
	if resp.StatusCode != 200 {
		t.Fatalf("large pull: %d", resp.StatusCode)
	}
	if len(body) != len(content) || body != content {
		t.Fatalf("client bytes = %d, want %d", len(body), len(content))
	}
	pe.te.srv.WaitIdle()
	obj, size, _, _, err := pe.st.Get(t.Context(), jar)
	if err != nil || size != int64(len(content)) {
		t.Fatalf("stored object: %v size=%d", err, size)
	}
	b, _ := io.ReadAll(obj)
	if string(b) != content {
		t.Fatal("stored bytes differ")
	}
	sum := sha256.Sum256([]byte(content))
	cs, _, _, _, err := pe.st.Get(t.Context(), jar+".sha256")
	if err != nil {
		t.Fatalf("sha256 sidecar missing: %v", err)
	}
	cb, _ := io.ReadAll(cs)
	if strings.TrimSpace(string(cb)) != hex.EncodeToString(sum[:]) {
		t.Fatalf("sha256 sidecar = %q", cb)
	}
}

func TestPullEmptyObject(t *testing.T) {
	const jar = "org/upstream/empty/1.0/empty-1.0.jar"
	fu := newFakeUpstream(t, "central", map[string]string{"/" + jar: ""}, nil)
	pe := newPullEnv(t, 1<<20, fu)
	resp, body := pe.te.do(t, http.MethodGet, "/"+jar, "", "")
	if resp.StatusCode != 200 || body != "" {
		t.Fatalf("empty pull: %d %q", resp.StatusCode, body)
	}
	pe.te.srv.WaitIdle()
	if _, size, _, _, err := pe.st.Get(t.Context(), jar); err != nil || size != 0 {
		t.Fatalf("stored empty object: %v size=%d", err, size)
	}
}

func TestPullCapExceeded(t *testing.T) {
	const jar = "org/upstream/big/1.0/big-1.0.jar"
	// 2 MiB object against a 1 MiB cap.
	fu := newFakeUpstream(t, "central", map[string]string{"/" + jar: strings.Repeat("x", 2<<20)}, nil)
	pe := newPullEnv(t, 1<<20, fu)
	resp, _ := pe.te.do(t, http.MethodGet, "/"+jar, "", "")
	if resp.StatusCode != 502 {
		t.Fatalf("over-cap pull: %d, want 502", resp.StatusCode)
	}
	if _, _, _, _, err := pe.st.Head(t.Context(), jar); err != store.ErrNotFound {
		t.Fatalf("store must be empty after over-cap pull: %v", err)
	}
}

func TestPullAbortOnClientDisconnect(t *testing.T) {
	const jar = "org/upstream/slow/1.0/slow-1.0.jar"
	// Stream 4 MiB slowly (2 ms per 1 KiB ≈ 8 s total) so the request
	// can be aborted mid-flight.
	fu := newFakeUpstream(t, "central", map[string]string{"/" + jar: strings.Repeat("x", 4<<20)}, nil)
	fu.chunk = 1024
	fu.chunkDelay = 2 * time.Millisecond
	pe := newPullEnv(t, 64<<20, fu)
	setPullPartSize(t, 1<<20) // parts upload while the stream is slow

	client := &http.Client{Timeout: 200 * time.Millisecond}
	resp, err := client.Get(pe.te.server.URL + "/" + jar)
	if err == nil {
		// Headers arrived; the deadline hits mid-body. Consume until
		// the deadline kills the read.
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}

	// The client connection is dead either way; pier must have aborted
	// the in-flight upload.
	pe.te.srv.WaitIdle()
	if _, _, _, _, err := pe.st.Head(t.Context(), jar); err != store.ErrNotFound {
		t.Fatalf("store must be empty after aborted pull: %v", err)
	}
	if n := pe.st.uploadCount(); n != 0 {
		t.Fatalf("in-flight uploads = %d, want 0", n)
	}
}

func TestPullLeaderFailure(t *testing.T) {
	const jar = "org/upstream/tool/1.0/tool-1.0.jar"
	fu := newFakeUpstream(t, "central", map[string]string{"/" + jar: "payload"}, nil)
	fu.delay = 50 * time.Millisecond
	pe := newPullEnv(t, 1<<20, fu)

	// The first S3 completion fails; the flag clears so the retry
	// after a leader failure succeeds.
	var fail bool
	var failMu sync.Mutex
	pe.st.shouldFailComplete = func() bool {
		failMu.Lock()
		defer failMu.Unlock()
		if fail {
			fail = false
			return true
		}
		return false
	}
	fail = true

	const n = 2
	results := make([][2]string, n) // status + body
	wg := sync.WaitGroup{}
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			resp, err := http.Get(pe.te.server.URL + "/" + jar)
			if err != nil {
				return
			}
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			results[i] = [2]string{resp.Status, string(b)}
		}(i)
	}
	wg.Wait()

	// Best-effort semantics: every client still got the upstream
	// bytes, and the object is cached by the retry.
	for i, r := range results {
		if !strings.HasPrefix(r[0], "200") || r[1] != "payload" {
			t.Fatalf("request %d: %q body=%q, want 200 payload", i, r[0], r[1])
		}
	}
	pe.te.srv.WaitIdle()
	obj, _, _, _, err := pe.st.Get(t.Context(), jar)
	if err != nil {
		t.Fatalf("object not cached after retry: %v", err)
	}
	io.Copy(io.Discard, obj)
	obj.Close()
	if hits := fu.hitCount("/" + jar); hits != 2 {
		t.Fatalf("upstream hits = %d, want 2 (one per attempt)", hits)
	}
}

func TestUploadNonReservedRejected(t *testing.T) {
	// Authentication is disabled in this environment; the 403 must
	// come from the reserved-group rule, not from auth.
	pe := newPullEnv(t, 1<<20, newFakeUpstream(t, "central", nil, nil))

	resp, body := pe.te.do(t, http.MethodPut, "/org/example/tool/1.0/tool-1.0.jar", "", "x")
	if resp.StatusCode != 403 {
		t.Fatalf("PUT non-reserved: %d %s, want 403", resp.StatusCode, body)
	}
	resp, _ = pe.te.do(t, http.MethodPost, "/org/example/tool/maven-metadata.xml", "", "<xml/>")
	if resp.StatusCode != 403 {
		t.Fatalf("POST non-reserved: %d, want 403", resp.StatusCode)
	}
	// A reserved upload still works.
	resp, _ = pe.te.do(t, http.MethodPut, "/com/acme/widget/1.0.0/widget-1.0.0.jar", "", "x")
	if resp.StatusCode != 201 {
		t.Fatalf("PUT reserved: %d, want 201", resp.StatusCode)
	}
	// DELETE is not restricted (operators purge cache entries).
	resp, _ = pe.te.do(t, http.MethodDelete, "/org/example/tool/1.0/tool-1.0.jar", "", "")
	if resp.StatusCode != 204 {
		t.Fatalf("DELETE non-reserved: %d, want 204", resp.StatusCode)
	}
}
