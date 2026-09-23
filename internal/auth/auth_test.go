package auth

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/brutasse/pier/internal/config"
)

const fakeISS = "https://fake-issuer.example"

type fakeIssuer struct {
	ts    *httptest.Server
	key   *rsa.PrivateKey
	keyID string
}

// newFakeIssuer starts an OpenID issuer with one RSA key.
func newFakeIssuer(t *testing.T) *fakeIssuer {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	fi := &fakeIssuer{key: key, keyID: "k1"}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{
			"issuer":   fakeISS,
			"jwks_uri": fi.ts.URL + "/jwks",
		})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, r *http.Request) {
		n := base64.RawURLEncoding.EncodeToString(key.N.Bytes())
		e := base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes())
		json.NewEncoder(w).Encode(map[string]any{
			"keys": []map[string]string{{"kty": "RSA", "kid": fi.keyID, "n": n, "e": e, "alg": "RS256", "use": "sig"}},
		})
	})
	fi.ts = httptest.NewServer(mux)
	t.Cleanup(fi.ts.Close)
	return fi
}

// token signs a token with the issuer key and the given claims.
func (fi *fakeIssuer) token(t *testing.T, mutate func(m jwt.MapClaims)) string {
	t.Helper()
	m := jwt.MapClaims{
		"iss": fakeISS,
		"aud": []string{"pier"},
		"sub": "repo:acme/widget:ref:refs/tags/v1.0.0",
		"exp": time.Now().Add(time.Hour).Unix(),
		"iat": time.Now().Unix(),
		"nbf": time.Now().Add(-time.Minute).Unix(),
	}
	if mutate != nil {
		mutate(m)
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, m)
	tok.Header["kid"] = fi.keyID
	s, err := tok.SignedString(fi.key)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func newVerifier(t *testing.T, fi *fakeIssuer) *Verifier {
	t.Helper()
	return NewVerifier([]config.Issuer{{
		Name:        "fake",
		WellKnown:   fi.ts.URL + "/.well-known/openid-configuration",
		ExpectedISS: fakeISS,
		Audiences:   []string{"pier"},
	}})
}

func TestVerifyValid(t *testing.T) {
	fi := newFakeIssuer(t)
	v := newVerifier(t, fi)
	id, err := v.Verify(context.Background(), fi.token(t, func(m jwt.MapClaims) {
		m["repository"] = "acme/widget"
		m["repository_owner"] = "acme"
		m["some_int"] = float64(42)
	}))
	if err != nil {
		t.Fatal(err)
	}
	if id.Issuer != "fake" {
		t.Errorf("issuer = %q", id.Issuer)
	}
	if id.Claims["repository"] != "acme/widget" {
		t.Errorf("repository claim = %v", id.Claims["repository"])
	}
	if v, ok := id.Claims["some_int"].(int64); !ok || v != 42 {
		t.Errorf("int claim not normalized to int64: %T %v", id.Claims["some_int"], id.Claims["some_int"])
	}
	if a, ok := id.Claims["aud"].([]any); !ok || len(a) != 1 || a[0] != "pier" {
		t.Errorf("aud claim = %v", id.Claims["aud"])
	}
}

func TestVerifyRejections(t *testing.T) {
	fi := newFakeIssuer(t)
	v := newVerifier(t, fi)
	ctx := context.Background()

	cases := []struct {
		name    string
		token   string
		mutate  func(m jwt.MapClaims)
		wantErr error
	}{
		{"garbage", "not.a.jwt", nil, ErrMalformed},
		{"expired", "", func(m jwt.MapClaims) { m["exp"] = time.Now().Add(-time.Hour).Unix() }, ErrExpired},
		{"not yet valid", "", func(m jwt.MapClaims) { m["nbf"] = time.Now().Add(time.Hour).Unix() }, ErrNotValidYet},
		{"wrong audience", "", func(m jwt.MapClaims) { m["aud"] = []string{"other-service"} }, ErrWrongAudience},
		{"no audience", "", func(m jwt.MapClaims) { delete(m, "aud") }, ErrWrongAudience},
		{"unknown issuer", "", func(m jwt.MapClaims) { m["iss"] = "https://other-issuer.example" }, ErrUnknownIssuer},
		{"wrong kid", "", nil, ErrBadSignature},
		{"no kid", "", nil, ErrBadSignature},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var tok string
			switch c.name {
			case "garbage":
				tok = c.token
			case "wrong kid":
				tok = swapKid(fi.token(t, nil), "missing-kid")
			case "no kid":
				tok = dropKid(fi.token(t, nil))
			default:
				tok = fi.token(t, c.mutate)
			}
			_, err := v.Verify(ctx, tok)
			if !errors.Is(err, c.wantErr) {
				t.Fatalf("err = %v, want %v", err, c.wantErr)
			}
		})
	}
}

func TestReason(t *testing.T) {
	if Reason(ErrNoToken) != "no_token" {
		t.Error("no_token")
	}
	if Reason(ErrUnknownIssuer) != "unknown_issuer" {
		t.Error("unknown_issuer")
	}
	if Reason(ErrMalformed) != "malformed" {
		t.Error("malformed")
	}
	if Reason(ErrJWKS) != "jwks" {
		t.Error("jwks")
	}
	if Reason(ErrBadSignature) != "bad_signature" {
		t.Error("bad_signature")
	}
	if Reason(ErrWrongAudience) != "wrong_audience" {
		t.Error("wrong_audience")
	}
	if Reason(ErrExpired) != "expired" {
		t.Error("expired")
	}
	if Reason(ErrNotValidYet) != "not_valid_yet" {
		t.Error("not_valid_yet")
	}
	if Reason(ErrBadToken) != "invalid_token" {
		t.Error("invalid_token")
	}
}

// TestVerifyErrorMessages checks that rejection errors name the failing
// claim values, so the cause is readable in a 401 body or on stderr.
func TestVerifyErrorMessages(t *testing.T) {
	fi := newFakeIssuer(t)
	v := newVerifier(t, fi)
	ctx := context.Background()

	tok := fi.token(t, func(m jwt.MapClaims) { m["aud"] = []string{"other-service"} })
	_, err := v.Verify(ctx, tok)
	if !errors.Is(err, ErrWrongAudience) ||
		!strings.Contains(err.Error(), "other-service") || !strings.Contains(err.Error(), "pier") {
		t.Fatalf("audience error %q", err)
	}

	tok = fi.token(t, func(m jwt.MapClaims) { m["exp"] = time.Now().Add(-time.Hour).Unix() })
	_, err = v.Verify(ctx, tok)
	if !errors.Is(err, ErrExpired) || !strings.Contains(err.Error(), "exp ") {
		t.Fatalf("expired error %q", err)
	}

	tok = swapKid(fi.token(t, nil), "missing-kid")
	_, err = v.Verify(ctx, tok)
	if !errors.Is(err, ErrBadSignature) || !strings.Contains(err.Error(), "missing-kid") {
		t.Fatalf("kid error %q", err)
	}

	_, err = v.Verify(ctx, "https://other-issuer.example/.well-known")
	if !errors.Is(err, ErrMalformed) {
		t.Fatalf("garbage error %q", err)
	}
}

// TestVerifyJWKSFailure checks that a failed key fetch is reported as a
// JWKS problem, not as an invalid token.
func TestVerifyJWKSFailure(t *testing.T) {
	fi := newFakeIssuer(t)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	t.Cleanup(ts.Close)
	v := NewVerifier([]config.Issuer{{
		Name:        "fake",
		WellKnown:   ts.URL + "/.well-known/openid-configuration",
		ExpectedISS: fakeISS,
		Audiences:   []string{"pier"},
	}})
	_, err := v.Verify(context.Background(), fi.token(t, nil))
	if !errors.Is(err, ErrJWKS) {
		t.Fatalf("err = %v, want ErrJWKS", err)
	}
	if !strings.Contains(err.Error(), "status 500") {
		t.Fatalf("err %q does not name the fetch failure", err)
	}
}

// dropKid returns tok with the header kid removed.
func dropKid(tok string) string {
	parts := splitDots(tok, 3)
	hdr, _ := base64.RawURLEncoding.DecodeString(parts[0])
	var h map[string]any
	_ = json.Unmarshal(hdr, &h)
	delete(h, "kid")
	b, _ := json.Marshal(h)
	return base64.RawURLEncoding.EncodeToString(b) + "." + parts[1] + "." + parts[2]
}

// swapKid returns tok with the header kid replaced.
func swapKid(tok, kid string) string {
	parts := splitDots(tok, 3)
	hdr, _ := base64.RawURLEncoding.DecodeString(parts[0])
	var h map[string]any
	_ = json.Unmarshal(hdr, &h)
	h["kid"] = kid
	b, _ := json.Marshal(h)
	return base64.RawURLEncoding.EncodeToString(b) + "." + parts[1] + "." + parts[2]
}

func splitDots(s string, n int) []string {
	out := make([]string, 0, n)
	for i := 0; i < n; i++ {
		j := 0
		for j < len(s) && s[j] != '.' {
			j++
		}
		out = append(out, s[:j])
		if j < len(s) {
			s = s[j+1:]
		}
	}
	return out
}

// TestVerifyKeyRotation verifies the kid-miss refetch: a token signed with a
// new key is accepted once the JWKS is refreshed.
func TestVerifyKeyRotation(t *testing.T) {
	fi := newFakeIssuer(t)
	v := newVerifier(t, fi)
	// No refetch window: rotation is orthogonal to the rate limit, which
	// TestVerifyMissRateLimit covers.
	v.miss = 0
	ctx := context.Background()

	// Prime the cache with k1.
	if _, err := v.Verify(ctx, fi.token(t, nil)); err != nil {
		t.Fatal(err)
	}

	// The issuer rotates to k2; the old kid is no longer served.
	fi.keyID = "k2"
	// The kid miss triggers a refetch and the rotated token is accepted.
	if _, err := v.Verify(ctx, fi.token(t, nil)); err != nil {
		t.Fatalf("rotated token not accepted: %v", err)
	}
}

// TestVerifyMissRateLimit verifies the kid-miss refetch window: while it is
// open a rotated token is rejected against the stale keys; once it has
// passed, the JWKS is refetched and the token is accepted.
func TestVerifyMissRateLimit(t *testing.T) {
	fi := newFakeIssuer(t)
	v := newVerifier(t, fi)
	v.miss = 100 * time.Millisecond
	ctx := context.Background()

	// Prime the cache with k1.
	if _, err := v.Verify(ctx, fi.token(t, nil)); err != nil {
		t.Fatal(err)
	}

	// The issuer rotates to k2; the old kid is no longer served.
	fi.keyID = "k2"

	// Within the window: no refetch, so the rotated token is rejected
	// with an unknown kid.
	if _, err := v.Verify(ctx, fi.token(t, nil)); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("within window: err = %v, want ErrBadSignature", err)
	}

	// After the window: the refetch is allowed and the token is accepted.
	time.Sleep(150 * time.Millisecond)
	if _, err := v.Verify(ctx, fi.token(t, nil)); err != nil {
		t.Fatalf("after window: rotated token not accepted: %v", err)
	}
}
