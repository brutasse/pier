// Package api implements the HTTP surface of pier: the Maven
// repository protocol plus /healthz and /metrics.
package api

import (
	"bytes"
	"context"
	"crypto/hmac"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/pprof"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/brutasse/pier/internal/auth"
	"github.com/brutasse/pier/internal/config"
	"github.com/brutasse/pier/internal/maven"
	"github.com/brutasse/pier/internal/policy"
	"github.com/brutasse/pier/internal/store"
	"github.com/brutasse/pier/internal/upstream"
)

const (
	maxMetadataBytes = 1 << 20 // maven-metadata.xml is small
	maxChecksumBytes = 512
	maxUploadBytes   = 512 << 20
)

// Runtime bundles the components a request runs against: configuration,
// storage, token verification, policy and the pull-through upstream. The
// server holds the active runtime behind an atomic pointer so a config
// reload can swap the whole bundle in place; requests in flight keep the
// bundle they started with.
type Runtime struct {
	Cfg    *config.Config
	Store  store.Backend
	Auth   *auth.Verifier
	Policy *policy.Policy
	// Upstream fetches objects for the pull-through cache. It is nil
	// when pull-through is not configured.
	Upstream *upstream.Upstream
}

// MaxUpload returns the configured per-request upload limit.
func (rt *Runtime) MaxUpload() int64 {
	if n := rt.Cfg.MaxUploadBytes; n > 0 {
		return n
	}
	return maxUploadBytes
}

// ImmutableReleases reports whether release artifacts are immutable.
func (rt *Runtime) ImmutableReleases() bool {
	b := rt.Cfg.ImmutableReleases
	return b == nil || *b
}

// MetadataTTL returns the freshness window for maven-metadata.xml pulled
// from upstream. An unset value means the default; zero disables
// revalidation.
func (rt *Runtime) MetadataTTL() time.Duration {
	return rt.Cfg.MetadataTTLOrDefault()
}

// Server handles repository requests.
type Server struct {
	rt  atomic.Pointer[Runtime]
	log *slog.Logger

	reg *prometheus.Registry
	m   *metrics

	// inflight coalesces concurrent pull-through fetches of the same
	// path into a single upstream request. It is request state, so it
	// lives on the server rather than in the reloadable runtime.
	mu       sync.Mutex
	inflight map[string]*pullCall

	// gavMu guards gavLocks, the per-GAV serialization locks.
	gavMu    sync.Mutex
	gavLocks map[string]*gavLock
}

// pullCall is an in-flight pull of a single path. Followers wait on done;
// err is nil when the object is in the store, ErrNotFound when no
// upstream has it, or another error when the store write failed.
type pullCall struct {
	done chan struct{}
	err  error
}

type metrics struct {
	requests   *prometheus.CounterVec
	duration   *prometheus.HistogramVec
	authFailed *prometheus.CounterVec
	denied     *prometheus.CounterVec
	upBytes    prometheus.Counter
	downBytes  prometheus.Counter
	upFetches  *prometheus.CounterVec
	upstreamB  *prometheus.CounterVec
}

// New builds the HTTP server components with the given initial runtime.
func New(cfg *config.Config, st store.Backend, v *auth.Verifier, p *policy.Policy, up *upstream.Upstream, log *slog.Logger) *Server {
	reg := prometheus.NewRegistry()
	s := &Server{log: log, reg: reg, inflight: map[string]*pullCall{}, gavLocks: map[string]*gavLock{}}
	s.rt.Store(&Runtime{Cfg: cfg, Store: st, Auth: v, Policy: p, Upstream: up})
	s.m = &metrics{
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "pier_http_requests_total",
			Help: "HTTP requests by method, status and action.",
		}, []string{"method", "status", "action"}),
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "pier_http_request_duration_seconds",
			Help:    "Request duration in seconds.",
			Buckets: prometheus.DefBuckets,
		}, []string{"action"}),
		authFailed: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "pier_auth_failures_total",
			Help: "Authentication failures by reason.",
		}, []string{"reason"}),
		denied: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "pier_policy_denials_total",
			Help: "Policy denials by action.",
		}, []string{"action"}),
		upBytes: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "pier_upload_bytes_total",
			Help: "Total bytes uploaded.",
		}),
		downBytes: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "pier_download_bytes_total",
			Help: "Total bytes downloaded.",
		}),
		upFetches: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "pier_upstream_fetches_total",
			Help: "Pull-through fetches by upstream repository and outcome.",
		}, []string{"repo", "result"}),
		upstreamB: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "pier_upstream_bytes_total",
			Help: "Bytes pulled from upstream, by repository.",
		}, []string{"repo"}),
	}
	reg.MustRegister(s.m.requests, s.m.duration, s.m.authFailed, s.m.denied, s.m.upBytes, s.m.downBytes, s.m.upFetches, s.m.upstreamB)
	return s
}

// Update atomically swaps in a new runtime.
func (s *Server) Update(cfg *config.Config, st store.Backend, v *auth.Verifier, p *policy.Policy, up *upstream.Upstream) {
	s.rt.Store(&Runtime{Cfg: cfg, Store: st, Auth: v, Policy: p, Upstream: up})
}

// Handler returns the root HTTP handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok\n"))
	})
	mux.Handle("GET /metrics", promhttp.HandlerFor(s.reg, promhttp.HandlerOpts{}))
	mux.Handle("/debug/pprof/", pprofMux())
	mux.HandleFunc("/{path...}", s.handleRepo)
	return mux
}

// pprofMux serves the standard pprof endpoints under /debug/pprof/.
func pprofMux() http.Handler {
	m := http.NewServeMux()
	m.HandleFunc("GET /", pprof.Index)
	m.HandleFunc("GET /cmdline", pprof.Cmdline)
	m.HandleFunc("GET /profile", pprof.Profile)
	m.HandleFunc("GET /symbol", pprof.Symbol)
	m.HandleFunc("GET /trace", pprof.Trace)
	for _, name := range []string{"allocs", "block", "goroutine", "heap", "mutex", "sched", "threadcreate"} {
		m.Handle("GET /"+name, pprof.Handler(name))
	}
	return m
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (s *statusWriter) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

// Flush pushes any buffered response bytes to the client.
func (s *statusWriter) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (s *Server) respondError(w *statusWriter, code int, msg string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Length", strconv.Itoa(len(msg)+1))
	w.WriteHeader(code)
	io.WriteString(w, msg+"\n")
}

func (s *Server) handleRepo(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
	action := "none"
	rule := "-"
	sub := "-"
	defer func() {
		dur := time.Since(start)
		ua := r.UserAgent()
		if len(ua) > 64 {
			ua = ua[:64] + "…"
		}
		s.m.requests.WithLabelValues(r.Method, strconv.Itoa(sw.status), action).Inc()
		s.m.duration.WithLabelValues(action).Observe(dur.Seconds())
		s.log.Info("request",
			"method", r.Method,
			"path", r.URL.Path,
			"ua", ua,
			"status", sw.status,
			"duration_ms", int64(dur.Milliseconds()),
			"rule", rule,
			"sub", sub,
		)
	}()

	rt := s.rt.Load()

	path := strings.TrimPrefix(r.URL.Path, "/")
	p, perr := maven.Parse(path)
	if perr != nil {
		s.respondError(sw, http.StatusBadRequest, perr.Error())
		return
	}

	switch r.Method {
	case http.MethodGet, http.MethodHead:
		if p.Kind == maven.KindUnknown {
			s.respondError(sw, http.StatusNotFound, "not found")
			return
		}
		action = "download"
	case http.MethodPut:
		if p.Kind == maven.KindUnknown {
			s.respondError(sw, http.StatusNotFound, "not found")
			return
		}
		action = "upload"
	case http.MethodPost:
		if p.Kind != maven.KindMetadata && p.Kind != maven.KindMetadataChecksum {
			sw.Header().Set("Allow", "PUT")
			s.respondError(sw, http.StatusMethodNotAllowed, "POST is only allowed for maven-metadata.xml")
			return
		}
		action = "upload"
	case http.MethodDelete:
		if p.Kind == maven.KindUnknown {
			s.respondError(sw, http.StatusNotFound, "not found")
			return
		}
		action = "delete"
	default:
		sw.Header().Set("Allow", "GET, HEAD, PUT, POST, DELETE")
		s.respondError(sw, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	// Uploads are restricted to the reserved groups; everything else
	// belongs to the pull-through cache. Checked before authentication:
	// the path is unconditionally non-writable.
	if action == "upload" && !rt.Cfg.IsReserved(p.Group) {
		s.respondError(sw, http.StatusForbidden, "outside reserved groups: internal uploads must use a reserved group")
		return
	}

	if !rt.Cfg.Auth.Disabled {
		id, err := s.identify(r, rt)
		if err != nil {
			s.m.authFailed.WithLabelValues(auth.Reason(err)).Inc()
			s.respondError(sw, http.StatusUnauthorized, "unauthorized: "+err.Error())
			return
		}
		if v, ok := id.Claims["sub"].(string); ok {
			sub = v
		}
		rule, _ = rt.Policy.Eval(action, activation(p, id.Claims, action))
		if rule == "" {
			s.m.denied.WithLabelValues(action).Inc()
			s.respondError(sw, http.StatusForbidden, "forbidden: no rule permits "+action)
			return
		}
	}

	switch action {
	case "download":
		s.serveDownload(r, sw, p, rt)
	case "upload":
		s.serveUpload(r, sw, p, rt)
	case "delete":
		s.serveDelete(r, sw, p, rt)
	}
}

func (s *Server) identify(r *http.Request, rt *Runtime) (*auth.Identity, error) {
	h := r.Header.Get("Authorization")
	if h == "" {
		return nil, auth.ErrNoToken
	}
	const prefix = "Bearer "
	if !strings.HasPrefix(h, prefix) {
		return nil, auth.ErrBadToken
	}
	return rt.Auth.Verify(r.Context(), strings.TrimPrefix(h, prefix))
}

func activation(p maven.Path, claims map[string]any, action string) map[string]any {
	return map[string]any{
		"claims":   claims,
		"action":   action,
		"group":    p.Group,
		"artifact": p.Artifact,
		"version":  p.Version,
		"snapshot": p.Snapshot,
		"path":     p.Raw,
	}
}

func (s *Server) serveDownload(r *http.Request, w *statusWriter, p maven.Path, rt *Runtime) {
	_, _, _, modified, err := rt.Store.Head(r.Context(), p.Raw)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			s.log.Error("store head", "path", p.Raw, "err", err)
			s.respondError(w, http.StatusBadGateway, "storage error")
			return
		}
		// Local miss: a private GAV's metadata is synthesized from the
		// stored versions; everything else is pulled from upstream.
		if s.serveSynthesized(r, w, p, rt) {
			return
		}
		if rt.Cfg.IsReserved(p.Group) || rt.Upstream == nil {
			s.respondError(w, http.StatusNotFound, "not found: "+p.Raw)
			return
		}
		if r.Method == http.MethodHead {
			s.servePeek(r, w, p, rt)
			return
		}
		s.serveOrPull(r, w, p, rt, false)
		return
	}
	if s.metadataStale(p, modified, rt) {
		// The cached metadata is past its TTL: revalidate it from
		// upstream, falling back to the stale copy on failure.
		if r.Method == http.MethodHead {
			s.serveStale(r, w, p, rt)
			return
		}
		s.serveOrPull(r, w, p, rt, true)
		return
	}
	if spec := r.Header.Get("Range"); spec != "" {
		s.serveRange(r, w, p, spec, rt)
	} else {
		s.serveFull(r, w, p, rt)
	}
}

// metadataStale reports whether a present maven-metadata.xml is past its
// TTL and must be revalidated from upstream. Only pulled metadata is
// revalidated (artifact-level and snapshot-level alike): private
// metadata stays fresh through write-side invalidation, and without an
// upstream there is nothing to revalidate from.
func (s *Server) metadataStale(p maven.Path, modified time.Time, rt *Runtime) bool {
	if p.Kind != maven.KindMetadata || rt.Upstream == nil || rt.Cfg.IsReserved(p.Group) {
		return false
	}
	ttl := rt.MetadataTTL()
	return ttl > 0 && !modified.IsZero() && time.Since(modified) > ttl
}

// serveIfPresent serves p from the store when it is present and current,
// or reports a storage error. It returns true when the response is
// complete, and false when the object is absent or stale and the pull
// path must run.
func (s *Server) serveIfPresent(r *http.Request, w *statusWriter, p maven.Path, rt *Runtime) bool {
	_, _, _, modified, err := rt.Store.Head(r.Context(), p.Raw)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return false // absent: a pull may be needed
		}
		s.log.Error("store head", "path", p.Raw, "err", err)
		s.respondError(w, http.StatusBadGateway, "storage error")
		return true
	}
	if s.metadataStale(p, modified, rt) {
		return false // past its TTL: the pull path revalidates it
	}
	s.serveStale(r, w, p, rt)
	return true
}

// serveStale serves p from the store without any TTL consideration.
func (s *Server) serveStale(r *http.Request, w *statusWriter, p maven.Path, rt *Runtime) {
	if spec := r.Header.Get("Range"); spec != "" {
		s.serveRange(r, w, p, spec, rt)
	} else {
		s.serveFull(r, w, p, rt)
	}
}

// maxPullAttempts bounds how often a request re-enters the pull path
// after a leader's store write failed.
const maxPullAttempts = 3

// serveOrPull ensures p is in the store, pulling it from upstream when
// missing, then serves it. Concurrent misses on the same path are
// coalesced into one upstream fetch. fallback reports whether a stale
// store copy is to be served when the pull fails (TTL revalidation).
func (s *Server) serveOrPull(r *http.Request, w *statusWriter, p maven.Path, rt *Runtime, fallback bool) {
	for attempt := 0; attempt < maxPullAttempts; attempt++ {
		if s.serveIfPresent(r, w, p, rt) {
			return
		}
		call, leader := s.begin(p.Raw)
		if !leader {
			<-call.done
			if fallback {
				// Any revalidation failure serves the stale copy; a
				// success means the object is fresh now.
				if call.err != nil {
					s.serveStale(r, w, p, rt)
					return
				}
				continue
			}
			if errors.Is(call.err, upstream.ErrNotFound) {
				s.respondError(w, http.StatusNotFound, "not found: "+p.Raw)
				return
			}
			continue // the leader's write failed; re-check the store
		}
		s.runPull(r, w, p, rt, call, fallback) // responds and settles the call
		return
	}
	s.respondError(w, http.StatusBadGateway, "storage error")
}

// begin registers path as in-flight. The first caller becomes the
// leader; later callers receive the shared call.
func (s *Server) begin(path string) (*pullCall, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if c, ok := s.inflight[path]; ok {
		return c, false
	}
	c := &pullCall{done: make(chan struct{})}
	s.inflight[path] = c
	return c, true
}

// end settles the in-flight call for path. Only the leader calls it.
func (s *Server) end(path string, c *pullCall, err error) {
	c.err = err
	close(c.done)
	s.mu.Lock()
	delete(s.inflight, path)
	s.mu.Unlock()
}

// gavLock serializes the read-modify-write operations of one GAV
// (artifact PUT + metadata invalidation, metadata synthesis, plugin
// metadata PUTs). ref counts holders and waiters so an unused entry
// is removed.
type gavLock struct {
	mu  sync.Mutex
	ref int
}

// lockGAV serializes the GAV's read-modify-write operations and
// returns a release function. The lock is held across the caller's
// whole S3 sequence, so concurrent operations on the same GAV never
// interleave; with S3's consistent read-after-write, each critical
// section then sees every committed write of the ones before it.
func (s *Server) lockGAV(gav string) func() {
	s.gavMu.Lock()
	l := s.gavLocks[gav]
	if l == nil {
		l = &gavLock{}
		s.gavLocks[gav] = l
	}
	l.ref++
	s.gavMu.Unlock()
	l.mu.Lock()
	return func() {
		l.mu.Unlock()
		s.gavMu.Lock()
		l.ref--
		if l.ref == 0 {
			delete(s.gavLocks, gav)
		}
		s.gavMu.Unlock()
	}
}

// gavKey is the GAV's serialization key: the group path and artifact,
// shared by every version of the GAV.
func gavKey(p maven.Path) string {
	return strings.ReplaceAll(p.Group, ".", "/") + "/" + p.Artifact
}

// serveRange handles a single RFC 7233 byte range: 206 for a satisfiable
// range, 416 for a valid but unsatisfiable one, and the full 200
// representation for malformed or unsupported specs (non-byte units,
// multiple ranges).
func (s *Server) serveRange(r *http.Request, w *statusWriter, p maven.Path, spec string, rt *Runtime) {
	ctx := r.Context()
	size, ct, etag, _, err := rt.Store.Head(ctx, p.Raw)
	if errors.Is(err, store.ErrNotFound) {
		s.respondError(w, http.StatusNotFound, "not found: "+p.Raw)
		return
	}
	if err != nil {
		s.log.Error("store head", "path", p.Raw, "err", err)
		s.respondError(w, http.StatusBadGateway, "storage error")
		return
	}
	if size < 0 {
		s.serveFull(r, w, p, rt) // unknown size: fall back to the full representation
		return
	}
	start, end, ok, notSat := parseRange(spec, size)
	if notSat {
		w.Header().Set("Content-Range", "bytes */"+strconv.FormatInt(size, 10))
		s.respondError(w, http.StatusRequestedRangeNotSatisfiable, "range not satisfiable")
		return
	}
	if !ok {
		s.serveFull(r, w, p, rt)
		return
	}
	rc, err := rt.Store.GetRange(ctx, p.Raw, start, end)
	if errors.Is(err, store.ErrNotFound) {
		s.respondError(w, http.StatusNotFound, "not found: "+p.Raw)
		return
	}
	if err != nil {
		s.log.Error("store get range", "path", p.Raw, "err", err)
		s.respondError(w, http.StatusBadGateway, "storage error")
		return
	}
	defer rc.Close()
	if ct == "" {
		ct = maven.ContentType(p.Ext)
	}
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
	w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, size))
	if etag != "" {
		w.Header().Set("ETag", etag)
	}
	w.WriteHeader(http.StatusPartialContent)
	if r.Method == http.MethodHead {
		return
	}
	n, err := io.Copy(w, rc)
	if err != nil {
		s.log.Warn("download interrupted", "path", p.Raw, "err", err)
		return
	}
	s.m.downBytes.Add(float64(n))
}

// parseRange resolves a RFC 7233 Range header value against an object of
// size bytes. It reports the inclusive [start, end] range when the single
// byte range is satisfiable, notSat for a syntactically valid but
// unsatisfiable range, and ok=false for malformed or unsupported specs
// (non-byte units, multiple ranges) — the caller serves the full
// representation in that case.
func parseRange(spec string, size int64) (start, end int64, ok, notSat bool) {
	const unit = "bytes="
	if len(spec) <= len(unit) || !strings.EqualFold(spec[:len(unit)], unit) {
		return 0, 0, false, false
	}
	rest := spec[len(unit):]
	if rest == "" || strings.Contains(rest, ",") {
		return 0, 0, false, false
	}
	i := strings.IndexByte(rest, '-')
	if i < 0 {
		return 0, 0, false, false
	}
	first, last := rest[:i], rest[i+1:]
	if first == "" {
		// "-suffix": last n bytes.
		n, err := strconv.ParseInt(last, 10, 64)
		if err != nil || n < 0 {
			return 0, 0, false, false
		}
		if n == 0 || size == 0 {
			return 0, 0, false, true
		}
		if n > size {
			n = size
		}
		return size - n, size - 1, true, false
	}
	start, err := strconv.ParseInt(first, 10, 64)
	if err != nil || start < 0 {
		return 0, 0, false, false
	}
	if start >= size {
		return 0, 0, false, true
	}
	if last == "" {
		return start, size - 1, true, false
	}
	end, err = strconv.ParseInt(last, 10, 64)
	if err != nil || end < start {
		if err == nil && end < start {
			return 0, 0, false, true
		}
		return 0, 0, false, false
	}
	if end >= size {
		end = size - 1
	}
	return start, end, true, false
}

func (s *Server) serveFull(r *http.Request, w *statusWriter, p maven.Path, rt *Runtime) {
	w.Header().Set("Accept-Ranges", "bytes")
	rc, size, ct, etag, err := rt.Store.Get(r.Context(), p.Raw)
	if errors.Is(err, store.ErrNotFound) {
		s.respondError(w, http.StatusNotFound, "not found: "+p.Raw)
		return
	}
	if err != nil {
		s.log.Error("store get", "path", p.Raw, "err", err)
		s.respondError(w, http.StatusBadGateway, "storage error")
		return
	}
	defer rc.Close()
	if ct == "" {
		ct = maven.ContentType(p.Ext)
	}
	w.Header().Set("Content-Type", ct)
	if size >= 0 {
		w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	}
	if etag != "" {
		w.Header().Set("ETag", etag)
	}
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return
	}
	n, err := io.Copy(w, rc)
	if err != nil {
		s.log.Warn("download interrupted", "path", p.Raw, "err", err)
		return
	}
	s.m.downBytes.Add(float64(n))
}

func (s *Server) serveUpload(r *http.Request, w *statusWriter, p maven.Path, rt *Runtime) {
	ctx := r.Context()
	switch p.Kind {
	case maven.KindMetadata:
		body, err := readSmall(r, maxMetadataBytes)
		if err != nil {
			s.respondError(w, http.StatusRequestEntityTooLarge, "metadata too large")
			return
		}
		s.putGAV(ctx, w, p, body, maven.ContentType("xml"), rt)
	case maven.KindMetadataChecksum, maven.KindChecksum:
		body, err := readSmall(r, maxChecksumBytes)
		if err != nil {
			s.respondError(w, http.StatusBadRequest, "checksum payload too large")
			return
		}
		if !s.verifyChecksum(ctx, w, p, body, rt) {
			return
		}
		if p.Kind == maven.KindMetadataChecksum {
			s.putGAV(ctx, w, p, body, maven.ContentType(p.Algo), rt)
			return
		}
		s.putObject(ctx, w, p, body, maven.ContentType(p.Algo), false, rt)
	case maven.KindArtifact:
		if r.ContentLength > rt.MaxUpload() {
			s.respondError(w, http.StatusRequestEntityTooLarge, "upload too large")
			return
		}
		s.uploadArtifact(r, w, p, rt)
	}
}

// uploadArtifact spools the request body to a temp file (so the S3 SDK can
// seek the stream and compute request checksums over any endpoint), then
// stores it.
func (s *Server) uploadArtifact(r *http.Request, w *statusWriter, p maven.Path, rt *Runtime) {
	tmp, err := os.CreateTemp("", "pier-upload-*")
	if err != nil {
		s.log.Error("create temp file", "err", err)
		s.respondError(w, http.StatusBadGateway, "storage error")
		return
	}
	name := tmp.Name()
	defer os.Remove(name)

	n, err := io.Copy(tmp, &capReader{r: r.Body, max: rt.MaxUpload()})
	if err != nil {
		tmp.Close()
		if errors.Is(err, errUploadTooLarge) {
			s.respondError(w, http.StatusRequestEntityTooLarge, "upload too large")
			return
		}
		s.log.Warn("upload read error", "path", p.Raw, "err", err)
		s.respondError(w, http.StatusBadGateway, "upload read error")
		return
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		tmp.Close()
		s.log.Error("seek temp file", "err", err)
		s.respondError(w, http.StatusBadGateway, "storage error")
		return
	}
	// The GAV lock serializes store + invalidation against concurrent
	// metadata synthesis for this GAV. The spool above runs before it,
	// so a large body does not hold the lock.
	defer s.lockGAV(gavKey(p))()
	immutable := rt.ImmutableReleases() && !p.Snapshot
	if err := rt.Store.Put(r.Context(), p.Raw, tmp, n, maven.ContentType(p.Ext), immutable); err != nil {
		tmp.Close()
		if errors.Is(err, store.ErrConflict) {
			s.respondError(w, http.StatusConflict, "immutable release already published: "+p.Raw)
			return
		}
		s.log.Error("store put", "path", p.Raw, "err", err)
		s.respondError(w, http.StatusBadGateway, "storage error")
		return
	}
	tmp.Close()
	s.invalidateMetadata(r.Context(), p, rt)
	s.m.upBytes.Add(float64(n))
	w.WriteHeader(http.StatusCreated)
}

// invalidateMetadata removes the GAV's metadata objects so the next
// metadata GET re-derives them from the stored versions: the
// artifact-level maven-metadata.xml always, and the snapshot-level one
// as well for a snapshot artifact. Best-effort.
func (s *Server) invalidateMetadata(ctx context.Context, p maven.Path, rt *Runtime) {
	groupPath := strings.ReplaceAll(p.Group, ".", "/")
	metas := []string{groupPath + "/" + p.Artifact + "/maven-metadata.xml"}
	if p.Snapshot {
		metas = append(metas, groupPath+"/"+p.Artifact+"/"+p.Version+"/maven-metadata.xml")
	}
	for _, m := range metas {
		for _, k := range []string{m, m + ".md5", m + ".sha1", m + ".sha256", m + ".sha512"} {
			if err := rt.Store.Delete(ctx, k); err != nil {
				s.log.Warn("invalidate metadata", "path", k, "err", err)
			}
		}
	}
}

// putGAV stores a small GAV-scoped object (metadata, its checksums)
// under the GAV lock, so the write serializes with the GAV's artifact
// PUTs (invalidation) and metadata synthesis.
func (s *Server) putGAV(ctx context.Context, w *statusWriter, p maven.Path, body []byte, contentType string, rt *Runtime) {
	defer s.lockGAV(gavKey(p))()
	s.putObject(ctx, w, p, body, contentType, false, rt)
}

// putObject stores a small object (metadata, checksum).
func (s *Server) putObject(ctx context.Context, w *statusWriter, p maven.Path, body []byte, contentType string, immutable bool, rt *Runtime) {
	if err := rt.Store.Put(ctx, p.Raw, bytes.NewReader(body), int64(len(body)), contentType, immutable); err != nil {
		if errors.Is(err, store.ErrConflict) {
			s.respondError(w, http.StatusConflict, "immutable object already exists: "+p.Raw)
			return
		}
		s.log.Error("store put", "path", p.Raw, "err", err)
		s.respondError(w, http.StatusBadGateway, "storage error")
		return
	}
	s.m.upBytes.Add(float64(len(body)))
	w.WriteHeader(http.StatusCreated)
}

// verifyChecksum recomputes the checksum of the stored base object and
// rejects the upload on mismatch.
func (s *Server) verifyChecksum(ctx context.Context, w *statusWriter, p maven.Path, body []byte, rt *Runtime) bool {
	rc, _, _, _, err := rt.Store.Get(ctx, p.BasePath())
	if errors.Is(err, store.ErrNotFound) {
		s.respondError(w, http.StatusConflict, "base object not found: "+p.BasePath())
		return false
	}
	if err != nil {
		s.log.Error("store get", "path", p.BasePath(), "err", err)
		s.respondError(w, http.StatusBadGateway, "storage error")
		return false
	}
	defer rc.Close()
	h, ok := maven.NewHasher(p.Algo)
	if !ok {
		s.respondError(w, http.StatusInternalServerError, "unsupported checksum algorithm "+p.Algo)
		return false
	}
	if _, err := io.Copy(h, rc); err != nil {
		s.log.Error("hash base object", "path", p.BasePath(), "err", err)
		s.respondError(w, http.StatusBadGateway, "storage error")
		return false
	}
	got := hex.EncodeToString(h.Sum(nil))
	if !hmac.Equal([]byte(strings.TrimSpace(string(body))), []byte(got)) {
		s.respondError(w, http.StatusConflict, "checksum mismatch: "+p.Raw)
		return false
	}
	return true
}

func (s *Server) serveDelete(r *http.Request, w *statusWriter, p maven.Path, rt *Runtime) {
	if err := rt.Store.Delete(r.Context(), p.Raw); err != nil {
		s.log.Error("store delete", "path", p.Raw, "err", err)
		s.respondError(w, http.StatusBadGateway, "storage error")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// readSmall reads a bounded body.
func readSmall(r *http.Request, max int64) ([]byte, error) {
	if r.ContentLength > max {
		return nil, errors.New("too large")
	}
	b, err := io.ReadAll(io.LimitReader(r.Body, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > max {
		return nil, errors.New("too large")
	}
	return b, nil
}

var errUploadTooLarge = errors.New("upload exceeds size limit")

// capReader reports errUploadTooLarge once more than max bytes are read.
type capReader struct {
	r   io.Reader
	n   int64
	max int64
}

func (c *capReader) Read(b []byte) (int, error) {
	n, err := c.r.Read(b)
	c.n += int64(n)
	if c.n > c.max {
		return n, errUploadTooLarge
	}
	return n, err
}
