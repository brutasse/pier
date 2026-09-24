package api_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/brutasse/pier/internal/api"
	"github.com/brutasse/pier/internal/auth"
	"github.com/brutasse/pier/internal/config"
	"github.com/brutasse/pier/internal/policy"
	"github.com/brutasse/pier/internal/store"
)

const iss = "https://fake-issuer.example"
const audience = "pier"

// fakeIssuer serves an OpenID discovery doc and JWKS.
type fakeIssuer struct {
	ts  *httptest.Server
	key *rsa.PrivateKey
}

func newFakeIssuer(t *testing.T) *fakeIssuer {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	fi := &fakeIssuer{key: key}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{"issuer": iss, "jwks_uri": fi.ts.URL + "/jwks"})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, r *http.Request) {
		n := base64.RawURLEncoding.EncodeToString(key.N.Bytes())
		e := base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes())
		json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{
			{"kty": "RSA", "kid": "k1", "n": n, "e": e, "alg": "RS256"},
		}})
	})
	fi.ts = httptest.NewServer(mux)
	t.Cleanup(fi.ts.Close)
	return fi
}

func (fi *fakeIssuer) token(t *testing.T, claims map[string]any) string {
	t.Helper()
	m := jwt.MapClaims{
		"iss": iss,
		"aud": []any{audience},
		"sub": "sub-1",
		"exp": time.Now().Add(time.Hour).Unix(),
		"iat": time.Now().Unix(),
	}
	for k, v := range claims {
		m[k] = v
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, m)
	tok.Header["kid"] = "k1"
	s, err := tok.SignedString(fi.key)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func sha1hex(s string) string {
	sum := sha1.Sum([]byte(s))
	return hex.EncodeToString(sum[:])
}

// memStore is an in-memory store.Backend.
type memStore struct {
	mu         sync.Mutex
	objs       map[string]memObj
	uploads    map[string]*memUpload // in-flight multipart uploads
	nextUpload int64
	// shouldFailComplete, when set, is consulted by CompleteMultipart
	// for fault injection in tests.
	shouldFailComplete func() bool
	// shouldFailPart, when set, is consulted by UploadPart for fault
	// injection in tests.
	shouldFailPart func() bool
}

type memObj struct {
	data []byte
	ct   string
	mod  time.Time
}

type memUpload struct {
	ct    string
	parts []memPart
}

type memPart struct {
	num  int32
	etag string
	data []byte
}

func newMemStore() *memStore {
	return &memStore{objs: map[string]memObj{}, uploads: map[string]*memUpload{}}
}

func (m *memStore) Get(ctx context.Context, path string) (io.ReadCloser, int64, string, string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	o, ok := m.objs[path]
	if !ok {
		return nil, 0, "", "", store.ErrNotFound
	}
	return io.NopCloser(bytes.NewReader(o.data)), int64(len(o.data)), o.ct, `"mem"`, nil
}

func (m *memStore) GetRange(ctx context.Context, path string, start, end int64) (io.ReadCloser, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	o, ok := m.objs[path]
	if !ok || start < 0 || end >= int64(len(o.data)) || start > end {
		return nil, store.ErrNotFound
	}
	return io.NopCloser(bytes.NewReader(o.data[start : end+1])), nil
}

func (m *memStore) Head(ctx context.Context, path string) (int64, string, string, time.Time, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	o, ok := m.objs[path]
	if !ok {
		return 0, "", "", time.Time{}, store.ErrNotFound
	}
	return int64(len(o.data)), o.ct, `"mem"`, o.mod, nil
}

func (m *memStore) List(ctx context.Context, prefix string) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p := prefix + "/"
	var out []string
	for k := range m.objs {
		if strings.HasPrefix(k, p) {
			out = append(out, strings.TrimPrefix(k, p))
		}
	}
	sort.Strings(out)
	return out, nil
}

// ageOlderThan backdates the modification time of path by d, to test
// TTL expiry.
func (m *memStore) ageOlderThan(path string, d time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if o, ok := m.objs[path]; ok {
		o.mod = time.Now().Add(-d)
		m.objs[path] = o
	}
}

func (m *memStore) Put(ctx context.Context, path string, r io.Reader, size int64, ct string, immutable bool) error {
	data, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if immutable {
		if _, ok := m.objs[path]; ok {
			return store.ErrConflict
		}
	}
	m.objs[path] = memObj{data: data, ct: ct, mod: time.Now()}
	return nil
}

func (m *memStore) Delete(ctx context.Context, path string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.objs, path)
	return nil
}

func (m *memStore) Ping(ctx context.Context) error { return nil }

func (m *memStore) CreateMultipart(ctx context.Context, path, contentType string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	id := fmt.Sprintf("mpu-%d", m.nextUpload)
	m.nextUpload++
	m.uploads[id] = &memUpload{ct: contentType}
	return id, nil
}

func (m *memStore) UploadPart(ctx context.Context, path, uploadID string, partNum int32, r io.Reader, size int64) (string, error) {
	if m.shouldFailPart != nil && m.shouldFailPart() {
		return "", fmt.Errorf("memstore: part failure injected")
	}
	data, err := io.ReadAll(io.LimitReader(r, size+1))
	if err != nil || int64(len(data)) != size {
		return "", fmt.Errorf("memstore: bad part")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	up, ok := m.uploads[uploadID]
	if !ok {
		return "", fmt.Errorf("memstore: unknown upload %q", uploadID)
	}
	up.parts = append(up.parts, memPart{num: partNum, etag: fmt.Sprintf("part-%d", partNum), data: data})
	return fmt.Sprintf("part-%d", partNum), nil
}

func (m *memStore) uploadCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.uploads)
}

func (m *memStore) CompleteMultipart(ctx context.Context, path, uploadID string, parts []store.Part) error {
	if m.shouldFailComplete != nil && m.shouldFailComplete() {
		return fmt.Errorf("memstore: complete failure injected")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	up, ok := m.uploads[uploadID]
	if !ok {
		return fmt.Errorf("memstore: unknown upload %q", uploadID)
	}
	var data []byte
	for _, p := range up.parts {
		data = append(data, p.data...)
	}
	m.objs[path] = memObj{data: data, ct: up.ct, mod: time.Now()}
	delete(m.uploads, uploadID)
	return nil
}

func (m *memStore) AbortMultipart(ctx context.Context, path, uploadID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.uploads, uploadID)
	return nil
}

type testEnv struct {
	srv    *api.Server
	server *httptest.Server
	fi     *fakeIssuer
	st     *memStore
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	fi := newFakeIssuer(t)
	st := newMemStore()
	te := &testEnv{fi: fi, st: st}

	cfg := &config.Config{
		Listen:            "127.0.0.1:0",
		ImmutableReleases: &[]bool{true}[0],
		MaxUploadBytes:    1 << 20, // 1 MiB for tests
		// com.acme is reserved for internal uploads in the tests.
		ReservedGroups: []string{"com.acme"},
	}
	pol, err := policy.New([]policy.Rule{
		{
			Name:   "publish",
			Action: []string{"upload"},
			When:   `claims.repository == "acme/widget" && claims.sub.startsWith("repo:acme/widget:ref:refs/tags/")`,
		},
		{
			Name:   "ci-read",
			Action: []string{"download"},
			When:   `claims.repository in ["acme/widget", "acme/other"]`,
		},
		{
			Name:   "ops",
			Action: []string{"download", "delete"},
			When:   `claims.sub == "ops@example.test"`,
		},
	}, true)
	if err != nil {
		t.Fatal(err)
	}
	verifier := auth.NewVerifier([]config.Issuer{{
		Name:        "fake",
		WellKnown:   fi.ts.URL + "/.well-known/openid-configuration",
		ExpectedISS: iss,
		Audiences:   []string{audience},
	}})
	srv := api.New(cfg, st, verifier, pol, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	te.srv = srv
	te.server = httptest.NewServer(srv.Handler())
	t.Cleanup(te.server.Close)
	return te
}

func (te *testEnv) do(t *testing.T, method, path, token, body string) (*http.Response, string) {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, te.server.URL+path, rd)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := te.server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

func (te *testEnv) publishToken(t *testing.T) string {
	return te.fi.token(t, map[string]any{
		"repository":       "acme/widget",
		"repository_owner": "acme",
		"sub":              "repo:acme/widget:ref:refs/tags/v1.0.0",
	})
}

func (te *testEnv) opsToken(t *testing.T) string {
	return te.fi.token(t, map[string]any{"sub": "ops@example.test"})
}

func TestHealthzAndAdminEndpoints(t *testing.T) {
	te := newTestEnv(t)
	resp, body := te.do(t, http.MethodGet, "/healthz", "", "")
	if resp.StatusCode != 200 || body != "ok\n" {
		t.Fatalf("healthz: %d %q", resp.StatusCode, body)
	}

	// The admin endpoints are not on the main listener.
	for _, path := range []string{"/metrics", "/debug/pprof/cmdline"} {
		resp, _ := te.do(t, http.MethodGet, path, "", "")
		if resp.StatusCode == 200 {
			t.Fatalf("%s served on the main listener", path)
		}
	}

	// They are served by the dedicated handlers.
	get := func(ts *httptest.Server, path string) (int, string) {
		t.Helper()
		resp, err := ts.Client().Get(ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	mts := httptest.NewServer(te.srv.AdminHandler(true, false))
	t.Cleanup(mts.Close)
	code, body := get(mts, "/metrics")
	if code != 200 {
		t.Fatalf("metrics: %d", code)
	}
	if !strings.Contains(body, "pier_upload_bytes_total") {
		t.Fatalf("metrics: body missing pier_ series:\n%s", body)
	}
	pts := httptest.NewServer(te.srv.AdminHandler(false, true))
	t.Cleanup(pts.Close)
	code, _ = get(pts, "/debug/pprof/cmdline")
	if code != 200 {
		t.Fatalf("pprof: %d", code)
	}
}

func TestDownloadAuth(t *testing.T) {
	te := newTestEnv(t)
	const jar = "com/acme/widget/1.0.0/widget-1.0.0.jar"

	resp, body := te.do(t, http.MethodPut, "/"+jar, te.publishToken(t), "jar-bytes")
	if resp.StatusCode != 201 {
		t.Fatalf("upload: %d %s", resp.StatusCode, body)
	}

	resp, _ = te.do(t, http.MethodGet, "/"+jar, "", "")
	if resp.StatusCode != 401 {
		t.Fatalf("no token: %d, want 401", resp.StatusCode)
	}
	resp, _ = te.do(t, http.MethodGet, "/"+jar, "garbage", "")
	if resp.StatusCode != 401 {
		t.Fatalf("bad token: %d, want 401", resp.StatusCode)
	}
	expired := te.fi.token(t, map[string]any{
		"repository": "acme/widget",
		"sub":        "repo:acme/widget:ref:refs/heads/main",
		"exp":        time.Now().Add(-time.Hour).Unix(),
	})
	resp, _ = te.do(t, http.MethodGet, "/"+jar, expired, "")
	if resp.StatusCode != 401 {
		t.Fatalf("expired: %d, want 401", resp.StatusCode)
	}
	other := te.fi.token(t, map[string]any{
		"repository": "acme/other-secret",
		"sub":        "repo:acme/other-secret:ref:refs/heads/main",
	})
	resp, _ = te.do(t, http.MethodGet, "/"+jar, other, "")
	if resp.StatusCode != 403 {
		t.Fatalf("other repo: %d, want 403", resp.StatusCode)
	}
	ci := te.fi.token(t, map[string]any{
		"repository": "acme/other",
		"sub":        "repo:acme/other:ref:refs/heads/main",
	})
	resp, body = te.do(t, http.MethodGet, "/"+jar, ci, "")
	if resp.StatusCode != 200 || body != "jar-bytes" {
		t.Fatalf("download: %d %q", resp.StatusCode, body)
	}
	resp, _ = te.do(t, http.MethodGet, "/"+jar, te.opsToken(t), "")
	if resp.StatusCode != 200 {
		t.Fatalf("ops download: %d", resp.StatusCode)
	}
}

func TestDownloadNotFoundAndHead(t *testing.T) {
	te := newTestEnv(t)
	ops := te.opsToken(t)

	resp, _ := te.do(t, http.MethodGet, "/com/acme/ghost/1.0.0/ghost-1.0.0.jar", ops, "")
	if resp.StatusCode != 404 {
		t.Fatalf("missing artifact: %d, want 404", resp.StatusCode)
	}
	resp, _ = te.do(t, http.MethodHead, "/com/acme/ghost/1.0.0/ghost-1.0.0.jar", ops, "")
	if resp.StatusCode != 404 {
		t.Fatalf("HEAD missing: %d, want 404", resp.StatusCode)
	}
	resp, _ = te.do(t, http.MethodGet, "/", ops, "")
	if resp.StatusCode != 404 {
		t.Fatalf("root: %d, want 404", resp.StatusCode)
	}
	resp, _ = te.do(t, http.MethodGet, "/com/acme", ops, "")
	if resp.StatusCode != 404 {
		t.Fatalf("prefix: %d, want 404", resp.StatusCode)
	}
}

func TestDownloadRange(t *testing.T) {
	te := newTestEnv(t)
	ops := te.opsToken(t)
	const jar = "com/acme/widget/1.0.0/widget-1.0.0.jar"
	content := "0123456789abcdef"
	if resp, _ := te.do(t, http.MethodPut, "/"+jar, te.publishToken(t), content); resp.StatusCode != 201 {
		t.Fatalf("upload: %d", resp.StatusCode)
	}

	get := func(t *testing.T, path, token, rangeHdr string) (*http.Response, string) {
		t.Helper()
		req, err := http.NewRequest(http.MethodGet, te.server.URL+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		if rangeHdr != "" {
			req.Header.Set("Range", rangeHdr)
		}
		resp, err := te.server.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp, string(b)
	}

	// No Range header: full representation, Accept-Ranges advertised.
	resp, body := get(t, "/"+jar, ops, "")
	if resp.StatusCode != 200 || body != content {
		t.Fatalf("full GET: %d %q", resp.StatusCode, body)
	}
	if h := resp.Header.Get("Accept-Ranges"); h != "bytes" {
		t.Fatalf("Accept-Ranges = %q, want %q", h, "bytes")
	}

	// Satisfiable ranges: 206 with Content-Range / Content-Length.
	cases := []struct {
		spec, wantBody, wantRange string
	}{
		{"bytes=0-4", "01234", "bytes 0-4/16"},
		{"bytes=5-", "56789abcdef", "bytes 5-15/16"},
		{"bytes=-5", "bcdef", "bytes 11-15/16"},
		{"bytes=10-100", "abcdef", "bytes 10-15/16"}, // end clamped to size-1
		{"bytes=0-15", content, "bytes 0-15/16"},
		{"BYTES=0-4", "01234", "bytes 0-4/16"}, // unit is case-insensitive
	}
	for _, c := range cases {
		resp, body := get(t, "/"+jar, ops, c.spec)
		if resp.StatusCode != 206 || body != c.wantBody {
			t.Fatalf("Range %q: %d %q, want 206 %q", c.spec, resp.StatusCode, body, c.wantBody)
		}
		if cr := resp.Header.Get("Content-Range"); cr != c.wantRange {
			t.Fatalf("Range %q: Content-Range = %q, want %q", c.spec, cr, c.wantRange)
		}
		if cl := resp.Header.Get("Content-Length"); cl != strconv.Itoa(len(c.wantBody)) {
			t.Fatalf("Range %q: Content-Length = %q", c.spec, cl)
		}
		if h := resp.Header.Get("Accept-Ranges"); h != "bytes" {
			t.Fatalf("Range %q: Accept-Ranges = %q", c.spec, h)
		}
	}

	// Valid but unsatisfiable: 416 with Content-Range: */size.
	for _, spec := range []string{"bytes=16-20", "bytes=5-3", "bytes=-0"} {
		resp, _ := get(t, "/"+jar, ops, spec)
		if resp.StatusCode != 416 {
			t.Fatalf("Range %q: %d, want 416", spec, resp.StatusCode)
		}
		if cr := resp.Header.Get("Content-Range"); cr != "bytes */16" {
			t.Fatalf("Range %q: Content-Range = %q", spec, cr)
		}
	}

	// Malformed or unsupported specs: fall back to the full 200.
	for _, spec := range []string{"bytes=0-2,4-6", "items=0-4", "bytes=abc-def", "bytes="} {
		resp, body := get(t, "/"+jar, ops, spec)
		if resp.StatusCode != 200 || body != content {
			t.Fatalf("Range %q: %d %q, want 200 full representation", spec, resp.StatusCode, body)
		}
	}

	// HEAD with a satisfiable range: headers only, no body.
	req, err := http.NewRequest(http.MethodHead, te.server.URL+"/"+jar, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+ops)
	req.Header.Set("Range", "bytes=0-4")
	resp, err = te.server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 206 || len(b) != 0 {
		t.Fatalf("HEAD range: %d body=%q", resp.StatusCode, b)
	}
	if cr := resp.Header.Get("Content-Range"); cr != "bytes 0-4/16" {
		t.Fatalf("HEAD range: Content-Range = %q", cr)
	}

	// Auth still applies to ranged requests.
	resp, _ = get(t, "/"+jar, "", "bytes=0-4")
	if resp.StatusCode != 401 {
		t.Fatalf("range without token: %d, want 401", resp.StatusCode)
	}
	// Missing object: 404.
	resp, _ = get(t, "/com/acme/ghost/1.0.0/ghost-1.0.0.jar", ops, "bytes=0-4")
	if resp.StatusCode != 404 {
		t.Fatalf("range on missing object: %d, want 404", resp.StatusCode)
	}
}

func TestUploadChecksumVerification(t *testing.T) {
	te := newTestEnv(t)
	const jar = "com/acme/widget/1.0.0/widget-1.0.0.jar"

	resp, _ := te.do(t, http.MethodPut, "/"+jar, te.publishToken(t), "hello")
	if resp.StatusCode != 201 {
		t.Fatalf("jar upload: %d", resp.StatusCode)
	}
	// Wrong checksum -> 409.
	resp, body := te.do(t, http.MethodPut, "/"+jar+".sha1", te.publishToken(t), strings.Repeat("0", 40))
	if resp.StatusCode != 409 {
		t.Fatalf("bad checksum: %d %s, want 409", resp.StatusCode, body)
	}
	// Checksum before base object -> 409.
	const pom = "com/acme/widget/2.0.0/widget-2.0.0.pom"
	resp, _ = te.do(t, http.MethodPut, "/"+pom+".sha1", te.publishToken(t), strings.Repeat("0", 40))
	if resp.StatusCode != 409 {
		t.Fatalf("checksum without base: %d, want 409", resp.StatusCode)
	}
	// Correct checksum -> 201, stored content served back.
	sum := sha1hex("hello")
	resp, _ = te.do(t, http.MethodPut, "/"+jar+".sha1", te.publishToken(t), sum)
	if resp.StatusCode != 201 {
		t.Fatalf("good checksum: %d", resp.StatusCode)
	}
	resp, body = te.do(t, http.MethodGet, "/"+jar+".sha1", te.opsToken(t), "")
	if resp.StatusCode != 200 || strings.TrimSpace(body) != sum {
		t.Fatalf("checksum download: %d %q", resp.StatusCode, body)
	}
}

func TestImmutableRelease(t *testing.T) {
	te := newTestEnv(t)
	const jar = "com/acme/widget/1.0.0/widget-1.0.0.jar"
	resp, _ := te.do(t, http.MethodPut, "/"+jar, te.publishToken(t), "v1")
	if resp.StatusCode != 201 {
		t.Fatalf("first upload: %d", resp.StatusCode)
	}
	resp, body := te.do(t, http.MethodPut, "/"+jar, te.publishToken(t), "v2")
	if resp.StatusCode != 409 {
		t.Fatalf("second upload: %d %s, want 409", resp.StatusCode, body)
	}
	// Snapshots may be overwritten.
	const snap = "com/acme/widget/1.0.0-SNAPSHOT/widget-1.0.0-20260919.213000-1.jar"
	resp, _ = te.do(t, http.MethodPut, "/"+snap, te.publishToken(t), "snap-1")
	if resp.StatusCode != 201 {
		t.Fatalf("snapshot first: %d", resp.StatusCode)
	}
	resp, _ = te.do(t, http.MethodPut, "/"+snap, te.publishToken(t), "snap-2")
	if resp.StatusCode != 201 {
		t.Fatalf("snapshot overwrite: %d, want 201", resp.StatusCode)
	}
}

func TestMetadataFlow(t *testing.T) {
	te := newTestEnv(t)
	const meta = "com/acme/widget/maven-metadata.xml"
	xml := `<metadata><groupId>com.acme</groupId><artifactId>widget</artifactId></metadata>`

	resp, body := te.do(t, http.MethodPost, "/"+meta, te.publishToken(t), xml)
	if resp.StatusCode != 201 {
		t.Fatalf("POST metadata: %d %s", resp.StatusCode, body)
	}
	// POST on an artifact path -> 405.
	resp, _ = te.do(t, http.MethodPost, "/com/acme/widget/1.0.0/widget-1.0.0.jar", te.publishToken(t), xml)
	if resp.StatusCode != 405 {
		t.Fatalf("POST artifact: %d, want 405", resp.StatusCode)
	}
	// Metadata checksum of the stored metadata.
	sum := sha1hex(xml)
	resp, _ = te.do(t, http.MethodPost, "/"+meta+".sha1", te.publishToken(t), sum)
	if resp.StatusCode != 201 {
		t.Fatalf("POST metadata checksum: %d", resp.StatusCode)
	}
	// Metadata is re-uploadable.
	resp, _ = te.do(t, http.MethodPost, "/"+meta, te.publishToken(t), xml+"<more/>")
	if resp.StatusCode != 201 {
		t.Fatalf("re-POST metadata: %d, want 201", resp.StatusCode)
	}
	resp, body = te.do(t, http.MethodGet, "/"+meta, te.opsToken(t), "")
	if resp.StatusCode != 200 || body != xml+"<more/>" {
		t.Fatalf("GET metadata: %d %q", resp.StatusCode, body)
	}
}

func TestDelete(t *testing.T) {
	te := newTestEnv(t)
	const jar = "com/acme/widget/1.0.0/widget-1.0.0.jar"
	ops := te.opsToken(t)
	ci := te.fi.token(t, map[string]any{
		"repository": "acme/widget",
		"sub":        "repo:acme/widget:ref:refs/heads/main",
	})

	resp, _ := te.do(t, http.MethodDelete, "/"+jar, ci, "")
	if resp.StatusCode != 403 {
		t.Fatalf("delete with ci token: %d, want 403", resp.StatusCode)
	}
	if _, body := te.do(t, http.MethodPut, "/"+jar, te.publishToken(t), "x"); body != "" {
		t.Fatalf("upload: %s", body)
	}
	resp, _ = te.do(t, http.MethodDelete, "/"+jar, ops, "")
	if resp.StatusCode != 204 {
		t.Fatalf("delete: %d, want 204", resp.StatusCode)
	}
	resp, _ = te.do(t, http.MethodGet, "/"+jar, ops, "")
	if resp.StatusCode != 404 {
		t.Fatalf("get after delete: %d, want 404", resp.StatusCode)
	}
}

func TestUploadErrors(t *testing.T) {
	te := newTestEnv(t)
	pub := te.publishToken(t)

	resp, _ := te.do(t, http.MethodPut, "/com/acme/widget/1.0.0/wrong-2.0.0.jar", pub, "x")
	if resp.StatusCode != 400 {
		t.Fatalf("malformed: %d, want 400", resp.StatusCode)
	}
	resp, _ = te.do(t, http.MethodPut, "/com/acme", pub, "x")
	if resp.StatusCode != 404 {
		t.Fatalf("unknown path: %d, want 404", resp.StatusCode)
	}
	resp, _ = te.do(t, http.MethodPatch, "/com/acme/widget/1.0.0/widget-1.0.0.jar", pub, "x")
	if resp.StatusCode != 405 {
		t.Fatalf("PATCH: %d, want 405", resp.StatusCode)
	}
	// Oversized upload (limit is 1 MiB in the test env).
	resp, _ = te.do(t, http.MethodPut, "/com/acme/widget/1.0.0/widget-1.0.0.jar", pub, strings.Repeat("a", 2<<20))
	if resp.StatusCode != 413 {
		t.Fatalf("oversized: %d, want 413", resp.StatusCode)
	}
	// Upload denied by policy (publish from branch).
	branch := te.fi.token(t, map[string]any{
		"repository": "acme/widget",
		"sub":        "repo:acme/widget:ref:refs/heads/main",
	})
	resp, _ = te.do(t, http.MethodPut, "/com/acme/widget/1.0.0/widget-1.0.0.pom", branch, "x")
	if resp.StatusCode != 403 {
		t.Fatalf("branch publish: %d, want 403", resp.StatusCode)
	}
}

func TestReloadSwapsRuntime(t *testing.T) {
	te := newTestEnv(t)
	const jar = "com/acme/widget/1.0.0/widget-1.0.0.jar"

	resp, body := te.do(t, http.MethodPut, "/"+jar, te.publishToken(t), "v1")
	if resp.StatusCode != 201 {
		t.Fatalf("upload under v1: %d %s", resp.StatusCode, body)
	}
	resp, _ = te.do(t, http.MethodGet, "/"+jar, te.publishToken(t), "")
	if resp.StatusCode != 200 {
		t.Fatalf("download under v1: %d", resp.StatusCode)
	}

	// v2: publishing and reading move to acme/other, the upload cap drops
	// to 16 bytes, and a fresh verifier is used.
	cfg2 := &config.Config{
		Listen:            "127.0.0.1:0",
		ImmutableReleases: &[]bool{true}[0],
		MaxUploadBytes:    16,
		ReservedGroups:    []string{"com.acme"},
	}
	pol2, err := policy.New([]policy.Rule{
		{Name: "publish", Action: []string{"upload"}, When: `claims.repository == "acme/other"`},
		{Name: "read", Action: []string{"download"}, When: `claims.repository == "acme/other"`},
	}, true)
	if err != nil {
		t.Fatal(err)
	}
	verifier2 := auth.NewVerifier([]config.Issuer{{
		Name:        "fake",
		WellKnown:   te.fi.ts.URL + "/.well-known/openid-configuration",
		ExpectedISS: iss,
		Audiences:   []string{audience},
	}})
	te.srv.Update(cfg2, te.st, verifier2, pol2, nil)

	other := te.fi.token(t, map[string]any{
		"repository": "acme/other",
		"sub":        "repo:acme/other:ref:refs/heads/main",
	})

	// The old token no longer matches any rule.
	resp, _ = te.do(t, http.MethodPut, "/com/acme/other/1.0.0/other-1.0.0.jar", te.publishToken(t), "ok")
	if resp.StatusCode != 403 {
		t.Fatalf("publish with old token: %d, want 403", resp.StatusCode)
	}
	resp, _ = te.do(t, http.MethodGet, "/"+jar, te.publishToken(t), "")
	if resp.StatusCode != 403 {
		t.Fatalf("read with old token: %d, want 403", resp.StatusCode)
	}
	// The new token matches, and the new upload cap applies.
	resp, _ = te.do(t, http.MethodPut, "/com/acme/other/1.0.0/other-1.0.0.jar", other, "ok")
	if resp.StatusCode != 201 {
		t.Fatalf("publish with new token: %d, want 201", resp.StatusCode)
	}
	resp, _ = te.do(t, http.MethodPut, "/com/acme/other/1.0.0/other-1.0.0.pom", other, strings.Repeat("a", 32))
	if resp.StatusCode != 413 {
		t.Fatalf("oversized under new cap: %d, want 413", resp.StatusCode)
	}
	resp, _ = te.do(t, http.MethodGet, "/"+jar, other, "")
	if resp.StatusCode != 200 {
		t.Fatalf("read with new token: %d, want 200", resp.StatusCode)
	}
}

func TestAuthDisabled(t *testing.T) {
	st := newMemStore()
	cfg := &config.Config{
		Listen:         "127.0.0.1:0",
		MaxUploadBytes: 1 << 20,
		Auth:           config.AuthConfig{Disabled: true},
		ReservedGroups: []string{"com.acme"},
	}
	pol, err := policy.New(nil, true)
	if err != nil {
		t.Fatal(err)
	}
	srv := api.New(cfg, st, auth.NewVerifier(nil), pol, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	resp, err := doPut(ts.URL+"/com/acme/widget/1.0.0/widget-1.0.0.jar", "x")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 201 {
		t.Fatalf("upload without auth: %d, want 201", resp.StatusCode)
	}
}

func doPut(url, body string) (*http.Response, error) {
	resp, err := http.NewRequest(http.MethodPut, url, strings.NewReader(body))
	if err != nil {
		return nil, err
	}
	return http.DefaultClient.Do(resp)
}
