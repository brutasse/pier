// Package upstream fetches objects from upstream Maven repositories
// (public or basic-authenticated) for the pull-through cache.
package upstream

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/brutasse/pier/internal/config"
)

// ErrNotFound is returned when no upstream serves the object.
var ErrNotFound = errors.New("object not found in any upstream")

// Result is an object fetched from an upstream. The caller must close Body.
type Result struct {
	Repo        string
	ContentType string
	Size        int64 // exact size, or -1 when unknown
	Body        io.ReadCloser
}

// Upstream walks an ordered list of repositories.
type Upstream struct {
	repos    []config.Upstream
	client   *http.Client
	maxBytes int64
}

// New builds an Upstream over the configured repositories. maxBytes caps
// the size of a fetched object; zero or negative disables the cap.
func New(repos []config.Upstream, maxBytes int64) *Upstream {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.ResponseHeaderTimeout = 30 * time.Second
	return &Upstream{
		repos: repos,
		client: &http.Client{
			// No overall timeout: body transfer is driven by the caller's
			// context, so an aborted client request cancels the pull.
			Transport: tr,
		},
		maxBytes: maxBytes,
	}
}

// Fetch retrieves path from the first upstream that has it, trying the
// repositories in order. It returns ErrNotFound when every repository
// answers 404; any non-404 failure yields an error.
func (u *Upstream) Fetch(ctx context.Context, path string) (*Result, error) {
	if len(u.repos) == 0 {
		return nil, ErrNotFound
	}
	var firstErr error
	for _, repo := range u.repos {
		res, err := u.fetchOne(ctx, repo, path)
		if err == nil {
			return res, nil
		}
		markError(&firstErr, err)
	}
	if firstErr == nil {
		firstErr = ErrNotFound
	}
	return nil, firstErr
}

// Peek reports whether any upstream serves path, without downloading it.
// It returns ErrNotFound when every repository answers 404, an error
// otherwise. size is the upstream Content-Length, or -1 when unknown.
func (u *Upstream) Peek(ctx context.Context, path string) (contentType string, size int64, err error) {
	if len(u.repos) == 0 {
		return "", -1, ErrNotFound
	}
	var firstErr error
	for _, repo := range u.repos {
		req, err := http.NewRequestWithContext(ctx, http.MethodHead, repo.URL+"/"+path, nil)
		if err != nil {
			return "", -1, err
		}
		if repo.Username != "" {
			req.SetBasicAuth(repo.Username, repo.Password)
		}
		resp, err := u.client.Do(req)
		if err != nil {
			markError(&firstErr, fmt.Errorf("%s: %w", repo.Name, err))
			continue
		}
		ct := resp.Header.Get("Content-Type")
		sz := resp.ContentLength
		switch resp.StatusCode {
		case http.StatusOK:
			resp.Body.Close()
			if sz < 0 {
				sz = -1
			}
			return ct, sz, nil
		case http.StatusNotFound:
			resp.Body.Close()
			markError(&firstErr, ErrNotFound)
		default:
			resp.Body.Close()
			markError(&firstErr, statusErr(repo.Name, resp))
		}
	}
	if firstErr == nil {
		firstErr = ErrNotFound
	}
	return "", -1, firstErr
}

// statusErr formats a non-2xx, non-404 upstream response as a hard error,
// surfacing the Retry-After header when the upstream sent one (rate limits).
func statusErr(name string, resp *http.Response) error {
	if ra := resp.Header.Get("Retry-After"); ra != "" {
		return fmt.Errorf("%s: status %d (retry-after=%s)", name, resp.StatusCode, ra)
	}
	return fmt.Errorf("%s: status %d", name, resp.StatusCode)
}

func (u *Upstream) fetchOne(ctx context.Context, repo config.Upstream, path string) (*Result, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, repo.URL+"/"+path, nil)
	if err != nil {
		return nil, err
	}
	if repo.Username != "" {
		req.SetBasicAuth(repo.Username, repo.Password)
	}
	resp, err := u.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", repo.Name, err)
	}
	if resp.StatusCode == http.StatusNotFound {
		resp.Body.Close()
		return nil, ErrNotFound
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		resp.Body.Close()
		return nil, statusErr(repo.Name, resp)
	}
	size := resp.ContentLength
	if size < 0 {
		size = -1
	}
	if u.maxBytes > 0 && size > u.maxBytes {
		resp.Body.Close()
		return nil, fmt.Errorf("%s: object %s exceeds size limit", repo.Name, path)
	}
	body := io.Reader(resp.Body)
	if u.maxBytes > 0 {
		body = &capReader{r: resp.Body, max: u.maxBytes}
	}
	ct := resp.Header.Get("Content-Type")
	if i := strings.Index(ct, ";"); i >= 0 {
		ct = strings.TrimSpace(ct[:i])
	}
	return &Result{
		Repo:        repo.Name,
		ContentType: ct,
		Size:        size,
		Body:        cappedBody{Reader: body, rc: resp.Body},
	}, nil
}

// markError records err as the fallback result, preferring a hard error
// over ErrNotFound.
func markError(firstErr *error, err error) {
	if *firstErr == nil {
		*firstErr = err
	} else if errors.Is(*firstErr, ErrNotFound) && !errors.Is(err, ErrNotFound) {
		*firstErr = err
	}
}

// cappedBody carries a size-capped reader while closing the underlying
// response body.
type cappedBody struct {
	io.Reader
	rc io.ReadCloser
}

func (c cappedBody) Close() error { return c.rc.Close() }

var errTooLarge = errors.New("upstream object exceeds size limit")

// capReader reports errTooLarge once more than max bytes are read.
type capReader struct {
	r   io.Reader
	n   int64
	max int64
}

func (c *capReader) Read(b []byte) (int, error) {
	n, err := c.r.Read(b)
	c.n += int64(n)
	if c.n > c.max {
		return n, errTooLarge
	}
	return n, err
}
