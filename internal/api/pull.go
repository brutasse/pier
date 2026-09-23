package api

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/brutasse/pier/internal/maven"
	"github.com/brutasse/pier/internal/store"
	"github.com/brutasse/pier/internal/upstream"
)

// PullPartSize is the S3 part size for pull-through uploads. Per-pull
// memory use is bounded by one part buffer. It is a variable so tests can
// shrink it.
var PullPartSize = 8 << 20 // 8 MiB

// maxMultipartParts is S3's hard limit on parts per upload.
const maxMultipartParts = 10000

// pullContentType picks the content type for a pulled object: the
// upstream's, unless it is empty or the generic octet-stream default, in
// which case it is derived from the path.
func pullContentType(p maven.Path, ct string) string {
	if ct != "" && ct != "application/octet-stream" {
		return ct
	}
	if p.Kind == maven.KindChecksum || p.Kind == maven.KindMetadataChecksum {
		return maven.ContentType(p.Algo)
	}
	return maven.ContentType(p.Ext)
}

// servePeek answers HEAD for a local miss by checking the upstreams,
// without storing anything: the object is pulled by the first GET.
func (s *Server) servePeek(r *http.Request, w *statusWriter, p maven.Path, rt *Runtime) {
	ct, size, err := rt.Upstream.Peek(r.Context(), p.Raw)
	if errors.Is(err, upstream.ErrNotFound) {
		s.respondError(w, http.StatusNotFound, "not found: "+p.Raw)
		return
	}
	if err != nil {
		s.log.Error("upstream peek", "path", p.Raw, "err", err)
		s.respondError(w, http.StatusBadGateway, "upstream error")
		return
	}
	ct = pullContentType(p, ct)
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("Content-Type", ct)
	if size >= 0 {
		w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	}
	w.WriteHeader(http.StatusOK)
}

// serveSynthesized answers an artifact-level maven-metadata.xml miss for
// a GAV that is not served by pull-through by synthesizing the metadata
// from the stored versions. It reports whether it responded.
func (s *Server) serveSynthesized(r *http.Request, w *statusWriter, p maven.Path, rt *Runtime) bool {
	if !p.IsArtifactMetadata() {
		return false
	}
	if rt.Upstream != nil && !rt.Cfg.IsReserved(p.Group) {
		return false // pull-through: the upstream's metadata is authoritative
	}
	if r.Method == http.MethodHead {
		// Report without storing; read-only, no lock.
		return s.serveSynthesizedHead(r, w, p, rt)
	}
	// The GAV lock serializes the list + store below against concurrent
	// artifact PUTs (invalidation) and plugin metadata PUTs for this
	// GAV: under the lock the version list sees every committed
	// artifact PUT (S3 read-after-write), so the stored document is
	// complete.
	defer s.lockGAV(gavKey(p))()
	if _, _, _, _, err := rt.Store.Head(r.Context(), p.Raw); err == nil {
		// A plugin metadata PUT landed while we waited; serve it
		// rather than clobber it with the bucket-derived document.
		s.serveFull(r, w, p, rt)
		return true
	} else if !errors.Is(err, store.ErrNotFound) {
		s.log.Error("store head", "path", p.Raw, "err", err)
		s.respondError(w, http.StatusBadGateway, "storage error")
		return true
	}
	versions, err := s.listVersions(r.Context(), p, rt)
	if err != nil {
		s.log.Error("list versions", "path", p.Raw, "err", err)
		s.respondError(w, http.StatusBadGateway, "storage error")
		return true
	}
	if len(versions) == 0 {
		s.respondError(w, http.StatusNotFound, "not found: "+p.Raw)
		return true
	}
	body := []byte(maven.SynthesizeMetadata(p.Group, p.Artifact, versions, time.Now()))
	if err := rt.Store.Put(r.Context(), p.Raw, bytes.NewReader(body), int64(len(body)), maven.ContentType("xml"), false); err != nil {
		s.log.Error("store metadata", "path", p.Raw, "err", err)
		s.respondError(w, http.StatusBadGateway, "storage error")
		return true
	}
	if err := s.storeSynthSidecars(r.Context(), p, rt, body); err != nil {
		s.log.Error("store metadata checksums", "path", p.Raw, "err", err)
		s.respondError(w, http.StatusBadGateway, "storage error")
		return true
	}
	s.serveFull(r, w, p, rt)
	return true
}

// serveSynthesizedHead answers an artifact-level metadata HEAD miss by
// reporting the synthesized document's size without storing. Read-only,
// so it runs without the GAV lock.
func (s *Server) serveSynthesizedHead(r *http.Request, w *statusWriter, p maven.Path, rt *Runtime) bool {
	versions, err := s.listVersions(r.Context(), p, rt)
	if err != nil {
		s.log.Error("list versions", "path", p.Raw, "err", err)
		s.respondError(w, http.StatusBadGateway, "storage error")
		return true
	}
	if len(versions) == 0 {
		s.respondError(w, http.StatusNotFound, "not found: "+p.Raw)
		return true
	}
	body := []byte(maven.SynthesizeMetadata(p.Group, p.Artifact, versions, time.Now()))
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("Content-Type", maven.ContentType("xml"))
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusOK)
	return true
}

// listVersions returns the sorted versions of a GAV present in the
// store: the distinct version directories under group/artifact/ that
// contain at least one artifact or checksum object of the GAV.
func (s *Server) listVersions(ctx context.Context, p maven.Path, rt *Runtime) ([]string, error) {
	prefix := strings.ReplaceAll(p.Group, ".", "/") + "/" + p.Artifact
	paths, err := rt.Store.List(ctx, prefix)
	if err != nil {
		return nil, err
	}
	fullPrefix := prefix + "/"
	seen := map[string]bool{}
	var versions []string
	for _, rel := range paths {
		i := strings.IndexByte(rel, '/')
		if i < 0 {
			continue // not inside a version directory
		}
		ver := rel[:i]
		if seen[ver] {
			continue
		}
		// Count the version only when one of its objects is a real
		// artifact or checksum of this GAV.
		if qp, err := maven.Parse(fullPrefix + rel); err == nil &&
			(qp.Kind == maven.KindArtifact || qp.Kind == maven.KindChecksum) &&
			qp.Artifact == p.Artifact && qp.Version == ver {
			seen[ver] = true
			versions = append(versions, ver)
		}
	}
	maven.SortVersions(versions)
	return versions, nil
}

// storeSynthSidecars stores the four checksum objects of a synthesized
// body.
func (s *Server) storeSynthSidecars(ctx context.Context, p maven.Path, rt *Runtime, body []byte) error {
	for _, algo := range []string{"md5", "sha1", "sha256", "sha512"} {
		h, ok := maven.NewHasher(algo)
		if !ok {
			return fmt.Errorf("unsupported checksum algorithm %s", algo)
		}
		h.Write(body)
		digest := hex.EncodeToString(h.Sum(nil)) + "\n"
		if err := rt.Store.Put(ctx, p.Raw+"."+algo, strings.NewReader(digest), int64(len(digest)), maven.ContentType(algo), false); err != nil {
			return err
		}
	}
	return nil
}

// runPull is the single-flight leader for a cold GET: it pulls p from the
// upstream into the store (streamed, multipart) and serves the client. It
// always writes a response and settles call. fallback reports whether the
// stale store copy is to be served when the pull fails (TTL
// revalidation).
func (s *Server) runPull(r *http.Request, w *statusWriter, p maven.Path, rt *Runtime, call *pullCall, fallback bool) {
	ctx := r.Context()
	path := p.Raw
	start := time.Now()

	if s.tryComputedChecksum(r, w, p, rt) {
		s.end(path, call, nil)
		return
	}

	res, err := rt.Upstream.Fetch(ctx, path)
	if err != nil {
		// No single repository decided the outcome; report it under "-".
		result := "error"
		if errors.Is(err, upstream.ErrNotFound) {
			result = "not_found"
		} else {
			s.log.Error("upstream fetch", "path", path, "err", err)
		}
		s.m.upFetches.WithLabelValues("-", result).Inc()
		if fallback {
			// The revalidation failed; the cached copy is still a
			// better answer than nothing.
			s.log.Warn("stale revalidation failed; serving cached copy", "path", path, "result", result)
			s.end(path, call, err)
			s.serveStale(r, w, p, rt)
			return
		}
		if result == "not_found" {
			s.respondError(w, http.StatusNotFound, "not found: "+path)
		} else {
			s.respondError(w, http.StatusBadGateway, "upstream error")
		}
		s.end(path, call, err)
		return
	}
	defer res.Body.Close()

	spec := r.Header.Get("Range")
	gate, toClient, answered := s.planRangePull(w, spec, res)
	if answered {
		s.end(path, call, nil)
		return
	}
	n, err := s.streamPull(ctx, r, w, p, rt, res, gate, toClient)
	if err != nil {
		s.m.upFetches.WithLabelValues(res.Repo, "error").Inc()
		s.log.Warn("pull failed", "path", path, "repo", res.Repo, "bytes", n, "err", err)
		s.end(path, call, err)
		if toClient {
			// The client already received a partial body; the connection
			// simply ends.
			return
		}
		if fallback {
			// The range response is not committed yet; serve the range
			// from the cached copy.
			s.serveStale(r, w, p, rt)
			return
		}
		s.respondError(w, http.StatusBadGateway, "upstream error")
		return
	}
	s.m.upFetches.WithLabelValues(res.Repo, "ok").Inc()
	s.m.upstreamB.WithLabelValues(res.Repo).Add(float64(n))
	s.log.Info("pulled", "path", path, "repo", res.Repo, "bytes", n, "duration_ms", time.Since(start).Milliseconds())
	s.end(path, call, nil)
	if spec != "" && !toClient {
		// The upstream reported no size, so the range was withheld
		// until the object was complete; it is now served from the
		// store.
		s.serveRange(r, w, p, spec, rt)
	}
}

// planRangePull decides how a cold pull answers the request's Range
// header, given the upstream-reported size. It returns the gate that
// restricts the client's bytes to the range, whether the response is
// served inline by streamPull, and whether it already answered the
// request (an unsatisfiable range is a 416 without pulling the body).
func (s *Server) planRangePull(w *statusWriter, spec string, res *upstream.Result) (gate *rangeGate, toClient, answered bool) {
	if spec == "" || res.Size < 0 {
		// No range, or the upstream sent no size: a 206 cannot be built
		// without the total, so the range (if any) is served from the
		// store once the pull completes.
		return nil, spec == "", false
	}
	start, end, ok, notSat := parseRange(spec, res.Size)
	switch {
	case notSat:
		w.Header().Set("Content-Range", "bytes */"+strconv.FormatInt(res.Size, 10))
		s.respondError(w, http.StatusRequestedRangeNotSatisfiable, "range not satisfiable")
		return nil, true, true
	case ok:
		return &rangeGate{w: w, flush: w, start: start, end: end}, true, false
	default:
		return nil, true, false // malformed spec: the full representation
	}
}

// streamPull reads res.Body once and fans it out to the S3 multipart
// upload and, when toClient, to the client response, while computing the
// checksum sidecars. When gate is set, only the bytes within its range
// reach the client; the rest of the stream is still cached. It reports
// the bytes transferred.
func (s *Server) streamPull(ctx context.Context, r *http.Request, w *statusWriter, p maven.Path, rt *Runtime, res *upstream.Result, gate *rangeGate, toClient bool) (int64, error) {
	ct := pullContentType(p, res.ContentType)

	uploadID, err := rt.Store.CreateMultipart(ctx, p.Raw, ct)
	if err != nil {
		return 0, err
	}
	pw := &partWriter{
		st:          rt.Store,
		ctx:         ctx,
		path:        p.Raw,
		uploadID:    uploadID,
		contentType: ct,
		partSize:    PullPartSize,
	}
	defer func() {
		// If the response below failed before the upload was completed,
		// discard the in-progress upload.
		if !pw.completed {
			pw.abort()
		}
	}()

	if toClient {
		// The body is streamed while the upload runs, so the headers go
		// out first.
		w.Header().Set("Accept-Ranges", "bytes")
		w.Header().Set("Content-Type", ct)
		if gate != nil {
			w.Header().Set("Content-Length", strconv.FormatInt(gate.end-gate.start+1, 10))
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", gate.start, gate.end, res.Size))
			w.WriteHeader(http.StatusPartialContent)
		} else {
			if res.Size >= 0 {
				w.Header().Set("Content-Length", strconv.FormatInt(res.Size, 10))
			}
			w.WriteHeader(http.StatusOK)
		}
	}

	md5h, _ := maven.NewHasher("md5")
	sha1h, _ := maven.NewHasher("sha1")
	sha256h, _ := maven.NewHasher("sha256")
	sha512h, _ := maven.NewHasher("sha512")

	var sink io.Writer = pw
	if toClient {
		if gate != nil {
			sink = io.MultiWriter(pw, gate)
		} else {
			sink = io.MultiWriter(w, pw)
		}
	}
	n, err := io.Copy(io.MultiWriter(sink, md5h, sha1h, sha256h, sha512h), res.Body)
	if err != nil {
		return n, err
	}
	if err := pw.complete(); err != nil {
		return n, err
	}
	if p.Kind == maven.KindArtifact || p.Kind == maven.KindMetadata {
		if err := s.storeSidecars(ctx, p, rt, md5h, sha1h, sha256h, sha512h); err != nil {
			return n, err
		}
	}
	return n, nil
}

// partWriter buffers bytes into S3 multipart parts of at most partSize
// and finalizes the upload on complete.
type partWriter struct {
	st          store.Backend
	ctx         context.Context
	path        string
	uploadID    string
	contentType string
	partSize    int
	partNum     int32
	parts       []store.Part
	buf         []byte
	completed   bool
}

func (pw *partWriter) Write(b []byte) (int, error) {
	total := len(b)
	for len(b) > 0 {
		if len(pw.buf) == pw.partSize {
			if err := pw.flush(); err != nil {
				return 0, err
			}
		}
		n := len(b)
		if space := pw.partSize - len(pw.buf); n > space {
			n = space
		}
		pw.buf = append(pw.buf, b[:n]...)
		b = b[n:]
	}
	return total, nil
}

// flush uploads the buffered part.
func (pw *partWriter) flush() error {
	if len(pw.buf) == 0 {
		return nil
	}
	if pw.partNum >= maxMultipartParts {
		return fmt.Errorf("object too large: exceeds %d multipart parts", maxMultipartParts)
	}
	etag, err := pw.st.UploadPart(pw.ctx, pw.path, pw.uploadID, pw.partNum+1, bytes.NewReader(pw.buf), int64(len(pw.buf)))
	if err != nil {
		return err
	}
	pw.parts = append(pw.parts, store.Part{Num: pw.partNum + 1, ETag: etag, Size: int64(len(pw.buf))})
	pw.partNum++
	pw.buf = pw.buf[:0]
	return nil
}

// complete flushes the final part and completes the upload. An empty
// object cannot be completed as a multipart upload, so it falls back to a
// plain put.
func (pw *partWriter) complete() error {
	if err := pw.flush(); err != nil {
		return err
	}
	var err error
	if len(pw.parts) == 0 {
		err = pw.st.Put(pw.ctx, pw.path, bytes.NewReader(nil), 0, pw.contentType, false)
	} else {
		err = pw.st.CompleteMultipart(pw.ctx, pw.path, pw.uploadID, pw.parts)
	}
	if err != nil {
		pw.abort()
		return err
	}
	pw.completed = true
	return nil
}

// abort discards the in-progress upload. It is best-effort.
func (pw *partWriter) abort() {
	_ = pw.st.AbortMultipart(pw.ctx, pw.path, pw.uploadID)
}

// rangeGate forwards stream bytes to the client while the running
// offset is within [start, end]; bytes outside the range are dropped.
// It sits in a MultiWriter fed by the same single stream as the part
// writer, so the offset stays in sync with the bytes. When the range is
// done it flushes the response: without it, the last partial buffer of
// range bytes would only leave the server when the handler returns —
// i.e. after the full object has been cached.
type rangeGate struct {
	w       io.Writer
	flush   http.Flusher
	start   int64
	end     int64
	off     int64
	flushed bool
}

func (g *rangeGate) Write(b []byte) (int, error) {
	total := len(b)
	for len(b) > 0 && g.off <= g.end {
		if g.off < g.start {
			skip := g.start - g.off
			if skip >= int64(len(b)) {
				g.off += skip
				return total, nil
			}
			b = b[skip:]
			g.off += skip
		}
		m := int64(len(b))
		if m > g.end-g.off+1 {
			m = g.end - g.off + 1
		}
		if _, err := g.w.Write(b[:m]); err != nil {
			return total, err
		}
		g.off += m
		b = b[m:]
	}
	if g.off > g.end && !g.flushed {
		g.flushed = true
		g.flush.Flush()
	}
	return total, nil
}

// tryComputedChecksum computes and serves the checksum of a locally
// present base object instead of pulling it from upstream. It reports
// whether the response was served.
func (s *Server) tryComputedChecksum(r *http.Request, w *statusWriter, p maven.Path, rt *Runtime) bool {
	if p.Kind != maven.KindChecksum && p.Kind != maven.KindMetadataChecksum {
		return false
	}
	rc, _, _, _, err := rt.Store.Get(r.Context(), p.BasePath())
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			s.log.Error("store get", "path", p.BasePath(), "err", err)
			s.respondError(w, http.StatusBadGateway, "storage error")
			return true
		}
		return false // base absent: pull the checksum object itself
	}
	defer rc.Close()
	h, ok := maven.NewHasher(p.Algo)
	if !ok {
		s.respondError(w, http.StatusInternalServerError, "unsupported checksum algorithm "+p.Algo)
		return true
	}
	if _, err := io.Copy(h, rc); err != nil {
		s.log.Error("hash base object", "path", p.BasePath(), "err", err)
		s.respondError(w, http.StatusBadGateway, "storage error")
		return true
	}
	digest := hex.EncodeToString(h.Sum(nil))
	if err := rt.Store.Put(r.Context(), p.Raw, strings.NewReader(digest+"\n"), int64(len(digest)+1), maven.ContentType(p.Algo), false); err != nil {
		s.log.Error("store checksum", "path", p.Raw, "err", err)
		s.respondError(w, http.StatusBadGateway, "storage error")
		return true
	}
	s.serveFull(r, w, p, rt)
	return true
}

// WaitIdle blocks until no pull-through fetches are in flight. It exists
// for tests.
func (s *Server) WaitIdle() {
	for {
		s.mu.Lock()
		n := len(s.inflight)
		s.mu.Unlock()
		if n == 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// storeSidecars stores the four checksum objects of a pulled artifact or
// metadata object.
func (s *Server) storeSidecars(ctx context.Context, p maven.Path, rt *Runtime, md5h, sha1h, sha256h, sha512h hash.Hash) error {
	for algo, h := range map[string]hash.Hash{
		"md5": md5h, "sha1": sha1h, "sha256": sha256h, "sha512": sha512h,
	} {
		digest := hex.EncodeToString(h.Sum(nil))
		body := digest + "\n"
		if err := rt.Store.Put(ctx, p.BasePath()+"."+algo, strings.NewReader(body), int64(len(body)), maven.ContentType(algo), false); err != nil {
			return err
		}
	}
	return nil
}
