//go:build integration

// Package integration exercises pier end-to-end: a real HTTP server with
// a real S3 backend (S3Mock) and a real OIDC issuer (an in-process test
// server serving well-known + JWKS and signing tokens).
//
// It is skipped unless PIER_TEST_S3_ENDPOINT is set, e.g.:
//
//	S3Mock at http://127.0.0.1:9090 (any credentials):
//	PIER_TEST_S3_ENDPOINT=http://127.0.0.1:9090 \
//	  PIER_TEST_S3_USER=testuser PIER_TEST_S3_PASS=testpass \
//	  go test -tags integration ./test/integration
package integration

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
	"os"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awscfg "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/golang-jwt/jwt/v5"

	"github.com/brutasse/pier/internal/api"
	"github.com/brutasse/pier/internal/auth"
	"github.com/brutasse/pier/internal/config"
	"github.com/brutasse/pier/internal/maven"
	"github.com/brutasse/pier/internal/policy"
	"github.com/brutasse/pier/internal/store"
	"github.com/brutasse/pier/internal/upstream"
)

const iss = "https://fake-issuer.example"
const audience = "pier"

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

func sha1hex(b []byte) string {
	sum := sha1.Sum(b)
	return hex.EncodeToString(sum[:])
}

type env struct {
	base     string      // server base URL
	srv      *api.Server // to wait out in-flight pulls (WaitIdle)
	fi       *fakeIssuer
	bucket   string
	prefix   string
	user     string
	pass     string
	endpoint string
}

type envOpts struct {
	upstream    []config.Upstream
	metadataTTL *time.Duration
}

func newEnv(t *testing.T) *env {
	t.Helper()
	return newEnvWith(t, envOpts{})
}

// newEnvWith starts a pier server backed by real S3 (S3Mock) with the
// test OIDC issuer and the same policy as the unit tests.
func newEnvWith(t *testing.T, opts envOpts) *env {
	t.Helper()
	endpoint := os.Getenv("PIER_TEST_S3_ENDPOINT")
	if endpoint == "" {
		t.Skip("set PIER_TEST_S3_ENDPOINT (e.g. http://127.0.0.1:9000) to run integration tests")
	}
	user := envOr("PIER_TEST_S3_USER", "testuser")
	pass := envOr("PIER_TEST_S3_PASS", "testpass")
	bucket := envOr("PIER_TEST_S3_BUCKET", "pier-integration")

	// Credentials for the S3 client under test.
	t.Setenv("AWS_ACCESS_KEY_ID", user)
	t.Setenv("AWS_SECRET_ACCESS_KEY", pass)
	ctx := context.Background()

	prefix := fmt.Sprintf("integration-%d", time.Now().UnixNano())
	sc := config.S3Config{
		Bucket:   bucket,
		Prefix:   prefix,
		Region:   "us-east-1",
		Endpoint: endpoint,
	}
	if err := ensureBucket(ctx, endpoint, bucket, user, pass); err != nil {
		t.Fatalf("ensure bucket: %v", err)
	}
	st, err := store.NewS3(ctx, sc)
	if err != nil {
		t.Fatal(err)
	}

	fi := newFakeIssuer(t)
	cfg := &config.Config{
		Listen:         "127.0.0.1:0",
		S3:             sc,
		MaxUploadBytes: 16 << 20,
		ReservedGroups: []string{"com.acme"},
		Auth: config.AuthConfig{Issuers: []config.Issuer{{
			Name:        "fake",
			WellKnown:   fi.ts.URL + "/.well-known/openid-configuration",
			ExpectedISS: iss,
			Audiences:   []string{audience},
		}}},
	}
	cfg.Upstream = opts.upstream
	cfg.MetadataTTL = opts.metadataTTL
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
	up := newUpstream(cfg)
	srv := api.New(cfg, st, auth.NewVerifier(cfg.Auth.Issuers), pol, up, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	// Best-effort cleanup of the run's objects.
	t.Cleanup(func() {
		if err := sweep(ctx, endpoint, bucket, user, pass, prefix); err != nil {
			t.Logf("cleanup: %v", err)
		}
	})
	return &env{base: ts.URL, srv: srv, fi: fi, bucket: bucket, prefix: prefix, user: user, pass: pass, endpoint: endpoint}
}

// newUpstream builds the pull-through fetcher for the config, or nil when
// no upstream repositories are configured.
func newUpstream(cfg *config.Config) *upstream.Upstream {
	if len(cfg.Upstream) == 0 {
		return nil
	}
	return upstream.New(cfg.Upstream, cfg.MaxUploadBytes)
}

// s3Key returns the bucket key for a repository path.
func (e *env) s3Client(ctx context.Context) *s3.Client {
	return s3Client(ctx, e.endpoint, e.bucket, e.user, e.pass)
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func s3Client(ctx context.Context, endpoint, bucket, user, pass string) *s3.Client {
	awsCfg, err := awscfg.LoadDefaultConfig(ctx,
		awscfg.WithRegion("us-east-1"),
		awscfg.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(user, pass, "")),
	)
	if err != nil {
		panic(err)
	}
	ep := strings.TrimRight(endpoint, "/")
	if !strings.HasPrefix(ep, "http") {
		ep = "https://" + ep
	}
	awsCfg.BaseEndpoint = &ep
	return s3.NewFromConfig(awsCfg, func(o *s3.Options) { o.UsePathStyle = true })
}

// ensureBucket creates the test bucket if it does not exist.
func ensureBucket(ctx context.Context, endpoint, bucket, user, pass string) error {
	client := s3Client(ctx, endpoint, bucket, user, pass)
	_, err := client.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: &bucket})
	if err == nil {
		return nil
	}
	_, cerr := client.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: &bucket})
	return cerr
}

// sweep deletes all objects under prefix (test cleanup).
func sweep(ctx context.Context, endpoint, bucket, user, pass, prefix string) error {
	client := s3Client(ctx, endpoint, bucket, user, pass)
	p := prefix + "/"
	var keys []string
	var token *string
	for {
		out, err := client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
			Bucket:            &bucket,
			Prefix:            &p,
			ContinuationToken: token,
		})
		if err != nil {
			return err
		}
		for _, o := range out.Contents {
			keys = append(keys, *o.Key)
		}
		if !aws.ToBool(out.IsTruncated) {
			break
		}
		token = out.NextContinuationToken
	}
	if len(keys) == 0 {
		return nil
	}
	objects := make([]types.ObjectIdentifier, 0, len(keys))
	for _, k := range keys {
		objects = append(objects, types.ObjectIdentifier{Key: &k})
	}
	_, err := client.DeleteObjects(ctx, &s3.DeleteObjectsInput{
		Bucket: &bucket,
		Delete: &types.Delete{Objects: objects},
	})
	return err
}

func (e *env) do(t *testing.T, method, path, token, body string) (*http.Response, string) {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, e.base+path, rd)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

func (e *env) publishToken(t *testing.T) string {
	return e.fi.token(t, map[string]any{
		"repository":       "acme/widget",
		"repository_owner": "acme",
		"sub":              "repo:acme/widget:ref:refs/tags/v1.0.0",
	})
}

func (e *env) opsToken(t *testing.T) string {
	return e.fi.token(t, map[string]any{"sub": "ops@example.test"})
}

// TestE2EFullDeploy simulates what maven-deploy-plugin does for a release,
// then what a resolver does to fetch it back.
func TestE2EFullDeploy(t *testing.T) {
	e := newEnv(t)
	pub := e.publishToken(t)
	ops := e.opsToken(t)
	t0 := time.Now().Unix()
	ver := fmt.Sprintf("0.0.%d", t0%100000)
	base := "com/acme/widget/" + ver
	jar := base + "/widget-" + ver + ".jar"
	pom := base + "/widget-" + ver + ".pom"
	meta := "com/acme/widget/maven-metadata.xml"

	jarBytes := []byte("fake jar content " + fmt.Sprint(t0))
	pomXML := "<project><modelVersion>4.0.0</modelVersion><groupId>com.acme</groupId><artifactId>widget</artifactId><version>" + ver + "</version></project>"

	// Probe whether the backend honors If-None-Match on conditional puts.
	condSupported := true
	probe := "com/acme/probe/1.0.0/probe-1.0.0.jar"
	if resp, _ := e.do(t, http.MethodPut, "/"+probe, pub, "x"); resp.StatusCode != 201 {
		t.Fatalf("probe PUT: %d", resp.StatusCode)
	}
	if resp, _ := e.do(t, http.MethodPut, "/"+probe, pub, "y"); resp.StatusCode != http.StatusConflict {
		condSupported = false
		t.Logf("backend does not honor If-None-Match: skipping immutable-release assertions")
	}

	// 1. Upload artifact + pom.
	if resp, body := e.do(t, http.MethodPut, "/"+jar, pub, string(jarBytes)); resp.StatusCode != 201 {
		t.Fatalf("PUT jar: %d %s", resp.StatusCode, body)
	}
	if resp, body := e.do(t, http.MethodPut, "/"+pom, pub, pomXML); resp.StatusCode != 201 {
		t.Fatalf("PUT pom: %d %s", resp.StatusCode, body)
	}

	// 2. Upload checksums (wrong first, then correct).
	if resp, body := e.do(t, http.MethodPut, "/"+jar+".sha1", pub, "deadbeef"); resp.StatusCode != 409 {
		t.Fatalf("PUT bad jar.sha1: %d %s, want 409", resp.StatusCode, body)
	}
	if resp, body := e.do(t, http.MethodPut, "/"+jar+".sha1", pub, sha1hex(jarBytes)); resp.StatusCode != 201 {
		t.Fatalf("PUT jar.sha1: %d %s", resp.StatusCode, body)
	}
	for _, algo := range []string{"md5", "sha256", "sha512"} {
		sum, ok := mavenHashHex(algo, jarBytes)
		if !ok {
			t.Fatalf("no hasher for %s", algo)
		}
		if resp, body := e.do(t, http.MethodPut, "/"+jar+"."+algo, pub, sum); resp.StatusCode != 201 {
			t.Fatalf("PUT jar.%s: %d %s", algo, resp.StatusCode, body)
		}
	}

	// 3. Re-upload of the release must be rejected (immutable), when the
	// backend supports conditional puts.
	if condSupported {
		if resp, body := e.do(t, http.MethodPut, "/"+jar, pub, "tampered"); resp.StatusCode != http.StatusConflict {
			t.Fatalf("re-PUT jar: %d %s, want 409", resp.StatusCode, body)
		}
	}

	// 4. POST maven-metadata.xml (+ checksum).
	metaXML := "<metadata><groupId>com.acme</groupId><artifactId>widget</artifactId><versioning><release>" + ver + "</release><versions><version>" + ver + "</version></versions></versioning></metadata>"
	if resp, body := e.do(t, http.MethodPost, "/"+meta, pub, metaXML); resp.StatusCode != 201 {
		t.Fatalf("POST metadata: %d %s", resp.StatusCode, body)
	}
	if resp, body := e.do(t, http.MethodPost, "/"+meta+".sha1", pub, sha1hex([]byte(metaXML))); resp.StatusCode != 201 {
		t.Fatalf("POST metadata.sha1: %d %s", resp.StatusCode, body)
	}

	// 5. Resolver flow: metadata, artifact, checksum, HEAD.
	ci := e.fi.token(t, map[string]any{
		"repository": "acme/other",
		"sub":        "repo:acme/other:ref:refs/heads/ci",
	})
	resp, body := e.do(t, http.MethodGet, "/"+meta, ci, "")
	if resp.StatusCode != 200 || body != metaXML {
		t.Fatalf("GET metadata: %d %q", resp.StatusCode, body)
	}
	resp, body = e.do(t, http.MethodGet, "/"+jar, ci, "")
	if resp.StatusCode != 200 || !bytes.Equal([]byte(body), jarBytes) {
		t.Fatalf("GET jar: %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/java-archive" {
		t.Errorf("jar content-type = %q", ct)
	}
	// Ranged download: 206 with the requested slice.
	req, err := http.NewRequest(http.MethodGet, e.base+"/"+jar, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+ci)
	req.Header.Set("Range", "bytes=0-9")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	rangeBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusPartialContent || string(rangeBody) != string(jarBytes[:10]) {
		t.Fatalf("GET jar range: %d %q", resp.StatusCode, rangeBody)
	}
	if cr := resp.Header.Get("Content-Range"); cr != fmt.Sprintf("bytes 0-9/%d", len(jarBytes)) {
		t.Fatalf("range Content-Range = %q", cr)
	}

	resp, body = e.do(t, http.MethodGet, "/"+jar+".sha1", ci, "")
	if resp.StatusCode != 200 || strings.TrimSpace(body) != sha1hex(jarBytes) {
		t.Fatalf("GET jar.sha1: %d %q", resp.StatusCode, body)
	}
	resp, _ = e.do(t, http.MethodHead, "/"+jar, ci, "")
	if resp.StatusCode != 200 || resp.Header.Get("Content-Length") != fmt.Sprint(len(jarBytes)) {
		t.Fatalf("HEAD jar: %d %q", resp.StatusCode, resp.Header.Get("Content-Length"))
	}

	// 6. Auth boundaries.
	if resp, _ := e.do(t, http.MethodGet, "/"+jar, "", ""); resp.StatusCode != 401 {
		t.Fatalf("GET without token: %d, want 401", resp.StatusCode)
	}
	badRepo := e.fi.token(t, map[string]any{"repository": "acme/secret", "sub": "repo:acme/secret:ref:refs/heads/x"})
	if resp, _ := e.do(t, http.MethodGet, "/"+jar, badRepo, ""); resp.StatusCode != 403 {
		t.Fatalf("GET with foreign repo token: %d, want 403", resp.StatusCode)
	}

	// 7. Delete (ops only).
	if resp, _ := e.do(t, http.MethodDelete, "/"+jar+".sha1", ci, ""); resp.StatusCode != 403 {
		t.Fatalf("DELETE with ci token: %d, want 403", resp.StatusCode)
	}
	if resp, _ := e.do(t, http.MethodDelete, "/"+jar+".sha1", ops, ""); resp.StatusCode != 204 {
		t.Fatalf("DELETE with ops token: %d, want 204", resp.StatusCode)
	}
	if resp, _ := e.do(t, http.MethodGet, "/"+jar+".sha1", ci, ""); resp.StatusCode != 404 {
		t.Fatalf("GET deleted: %d, want 404", resp.StatusCode)
	}
}

func mavenHashHex(algo string, b []byte) (string, bool) {
	h, ok := maven.NewHasher(algo)
	if !ok {
		return "", false
	}
	h.Write(b)
	return hex.EncodeToString(h.Sum(nil)), true
}
