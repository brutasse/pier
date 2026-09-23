package upstream

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/brutasse/pier/internal/config"
)

// fakeRepo serves fixed bodies per path and counts hits.
type fakeRepo struct {
	ts     *httptest.Server
	hits   map[string]int
	bodies map[string]string
	status map[string]int
}

func newFakeRepo(t *testing.T, objects map[string]string, statuses map[string]int) *fakeRepo {
	t.Helper()
	fr := &fakeRepo{
		hits:   map[string]int{},
		bodies: objects,
		status: statuses,
	}
	fr.ts = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		fr.hits[p]++
		if st, ok := fr.status[p]; ok && st != http.StatusOK {
			w.WriteHeader(st)
			return
		}
		b, ok := fr.bodies[p]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Write([]byte(b))
	}))
	t.Cleanup(fr.ts.Close)
	return fr
}

func repos(rs ...*fakeRepo) []config.Upstream {
	var out []config.Upstream
	for _, r := range rs {
		out = append(out, config.Upstream{Name: r.ts.URL, URL: r.ts.URL})
	}
	return out
}

func TestFetchOrderedFallback(t *testing.T) {
	a := newFakeRepo(t, nil, map[string]int{"/x/1": http.StatusNotFound})
	b := newFakeRepo(t, map[string]string{"/x/1": "hello"}, nil)
	u := New(repos(a, b), 0)

	res, err := u.Fetch(context.Background(), "x/1")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if res.Repo != b.ts.URL {
		t.Errorf("Repo = %q, want %q", res.Repo, b.ts.URL)
	}
	body, err := io.ReadAll(res.Body)
	res.Body.Close()
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if string(body) != "hello" {
		t.Errorf("body = %q", body)
	}
	if res.Size != 5 {
		t.Errorf("Size = %d, want 5", res.Size)
	}
	if res.ContentType != "application/octet-stream" {
		t.Errorf("ContentType = %q", res.ContentType)
	}
}

func TestFetchAllNotFound(t *testing.T) {
	a := newFakeRepo(t, nil, nil)
	b := newFakeRepo(t, nil, nil)
	u := New(repos(a, b), 0)
	if _, err := u.Fetch(context.Background(), "x/1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestFetchRecoverAfterError(t *testing.T) {
	a := newFakeRepo(t, map[string]string{"/x/1": "boom"}, map[string]int{"/x/1": http.StatusInternalServerError})
	b := newFakeRepo(t, map[string]string{"/x/1": "fine"}, nil)
	u := New(repos(a, b), 0)
	res, err := u.Fetch(context.Background(), "x/1")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if res.Repo != b.ts.URL {
		t.Errorf("Repo = %q", res.Repo)
	}
}

func TestFetchErrorBeatsNotFound(t *testing.T) {
	a := newFakeRepo(t, map[string]string{"/x/1": "boom"}, map[string]int{"/x/1": http.StatusInternalServerError})
	b := newFakeRepo(t, nil, nil)
	u := New(repos(a, b), 0)
	_, err := u.Fetch(context.Background(), "x/1")
	if err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want hard error", err)
	}
}

func TestFetchCapKnownSize(t *testing.T) {
	a := newFakeRepo(t, map[string]string{"/x/1": "123456"}, nil)
	u := New(repos(a), 4)
	_, err := u.Fetch(context.Background(), "x/1")
	if err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want size error", err)
	}
}

func TestFetchCapUnknownSize(t *testing.T) {
	// Serve without Content-Length so the cap trips mid-stream.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Transfer-Encoding", "chunked")
		w.Write([]byte("123456"))
	}))
	t.Cleanup(ts.Close)
	u := New([]config.Upstream{{Name: ts.URL, URL: ts.URL}}, 4)
	res, err := u.Fetch(context.Background(), "x/1")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if _, err := io.ReadAll(res.Body); !errors.Is(err, errTooLarge) {
		t.Fatalf("read err = %v, want errTooLarge", err)
	}
}

func TestFetchNoRepos(t *testing.T) {
	u := New(nil, 0)
	if _, err := u.Fetch(context.Background(), "x/1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestPeek(t *testing.T) {
	a := newFakeRepo(t, map[string]string{"/x/1": "hello"}, map[string]int{"/x/2": http.StatusNotFound})
	b := newFakeRepo(t, nil, nil)
	u := New(repos(a, b), 0)

	ct, size, err := u.Peek(context.Background(), "x/1")
	if err != nil {
		t.Fatalf("Peek: %v", err)
	}
	if size != 5 || ct != "application/octet-stream" {
		t.Errorf("Peek = (%q, %d)", ct, size)
	}

	if _, _, err := u.Peek(context.Background(), "x/2"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Peek missing = %v, want ErrNotFound", err)
	}

	// No object was fetched as a side effect.
	if a.hits["/x/1"] != 1 || b.hits["/x/1"] != 0 {
		t.Errorf("hits = %v / %v, want 1 / 0", a.hits, b.hits)
	}
}

func TestPeekError(t *testing.T) {
	a := newFakeRepo(t, map[string]string{"/x/1": "boom"}, map[string]int{"/x/1": http.StatusInternalServerError})
	u := New(repos(a), 0)
	if _, _, err := u.Peek(context.Background(), "x/1"); err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want hard error", err)
	}
}

// TestBasicAuth verifies that repositories configured with basic-auth
// credentials send the Authorization header on both GET (Fetch) and
// HEAD (Peek), and that an unauthenticated repository sends none.
func TestBasicAuth(t *testing.T) {
	const user, pass = "puller", "s3cret"
	var mu sync.Mutex
	var gotAuth bool
	var gotUser, gotPass string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, p, ok := r.BasicAuth()
		mu.Lock()
		if ok {
			gotAuth, gotUser, gotPass = true, u, p
		}
		mu.Unlock()
		if !ok || u != user || p != pass {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Write([]byte("hello"))
	}))
	t.Cleanup(ts.Close)

	u := New([]config.Upstream{{Name: ts.URL, URL: ts.URL, Username: user, Password: pass}}, 0)

	// GET (Fetch) sends the credentials.
	res, err := u.Fetch(context.Background(), "x/1")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if string(body) != "hello" {
		t.Fatalf("body = %q", body)
	}
	mu.Lock()
	saw, su, sp := gotAuth, gotUser, gotPass
	mu.Unlock()
	if !saw || su != user || sp != pass {
		t.Errorf("GET auth = (%v, %q, %q), want (%v, %q, %q)", saw, su, sp, true, user, pass)
	}

	// HEAD (Peek) sends the credentials too.
	mu.Lock()
	gotAuth = false
	mu.Unlock()
	if _, _, err := u.Peek(context.Background(), "x/1"); err != nil {
		t.Fatalf("Peek: %v", err)
	}
	mu.Lock()
	saw, su, sp = gotAuth, gotUser, gotPass
	mu.Unlock()
	if !saw || su != user || sp != pass {
		t.Errorf("HEAD auth = (%v, %q, %q), want (%v, %q, %q)", saw, su, sp, true, user, pass)
	}

	// A repository without credentials must send no Authorization header;
	// the 401 surfaces as a hard error, not a miss.
	mu.Lock()
	gotAuth = false
	mu.Unlock()
	unauth := New([]config.Upstream{{Name: ts.URL, URL: ts.URL}}, 0)
	if _, err := unauth.Fetch(context.Background(), "x/1"); err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("Fetch without creds: err = %v, want hard error", err)
	}
	mu.Lock()
	saw = gotAuth
	mu.Unlock()
	if saw {
		t.Error("unauthenticated request carried an Authorization header")
	}
}
