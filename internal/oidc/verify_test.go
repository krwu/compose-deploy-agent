package oidc_test

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/krwu/compose-deploy-agent/internal/oidc"
	"github.com/krwu/compose-deploy-agent/internal/oidc/oidctest"
)

const testAudience = "https://deploy.img.ntest.top"

// fakeClock is a settable time source shared by verifier and test.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func newVerifier(t *testing.T, iss *oidctest.Issuer) (*oidc.Verifier, *fakeClock) {
	t.Helper()
	clock := &fakeClock{now: time.Unix(1_800_000_000, 0)}
	v, err := oidc.NewVerifier(testAudience, iss.HTTPClient(), clock.Now)
	if err != nil {
		t.Fatalf("new verifier: %v", err)
	}
	return v, clock
}

func TestVerifier_Valid(t *testing.T) {
	iss := oidctest.NewIssuer(t)
	v, clock := newVerifier(t, iss)
	token := iss.Mint(t, oidctest.Claims(testAudience, clock.Now()))
	claims, err := v.Verify(context.Background(), token)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if claims.RepositoryID != "1001" || claims.RepositoryOwnerID != "2002" || claims.Ref != "refs/heads/main" {
		t.Fatalf("unexpected claims: %+v", claims)
	}
}

func Test_NewVerifierRejectsEmptyAudience(t *testing.T) {
	_, err := oidc.NewVerifier("", nil, nil)
	if err == nil {
		t.Fatal("empty audience must be refused")
	}
}

func TestVerifier_RejectsBadClaims(t *testing.T) {
	cases := map[string]func(c map[string]any, now time.Time){
		"aud mismatch":        func(c map[string]any, _ time.Time) { c["aud"] = "https://github.com/kairee" },
		"aud array of two":    func(c map[string]any, _ time.Time) { c["aud"] = []string{testAudience, "other"} },
		"iss mismatch":        func(c map[string]any, _ time.Time) { c["iss"] = "https://evil.example.com" },
		"expired":             func(c map[string]any, now time.Time) { c["exp"] = now.Add(-31 * time.Second).Unix() },
		"nbf in future":       func(c map[string]any, now time.Time) { c["nbf"] = now.Add(time.Minute).Unix() },
		"iat in future":       func(c map[string]any, now time.Time) { c["iat"] = now.Add(time.Minute).Unix() },
		"lifetime too long":   func(c map[string]any, now time.Time) { c["exp"] = now.Add(2 * time.Hour).Unix() },
		"missing exp":         func(c map[string]any, _ time.Time) { delete(c, "exp") },
		"missing nbf":         func(c map[string]any, _ time.Time) { delete(c, "nbf") },
		"missing jti":         func(c map[string]any, _ time.Time) { delete(c, "jti") },
		"missing repo id":     func(c map[string]any, _ time.Time) { delete(c, "repository_id") },
		"missing owner id":    func(c map[string]any, _ time.Time) { delete(c, "repository_owner_id") },
		"exp not an integer":  func(c map[string]any, _ time.Time) { c["exp"] = "soon" },
		"missing event_name":  func(c map[string]any, _ time.Time) { delete(c, "event_name") },
		"missing ref":         func(c map[string]any, _ time.Time) { delete(c, "ref") },
		"aud missing":         func(c map[string]any, _ time.Time) { delete(c, "aud") },
		"iss missing":         func(c map[string]any, _ time.Time) { delete(c, "iss") },
		"exp before iat":      func(c map[string]any, now time.Time) { c["iat"] = now.Unix() + 1; c["exp"] = now.Unix() },
		"aud wrong type":      func(c map[string]any, _ time.Time) { c["aud"] = 7 },
		"iat missing":         func(c map[string]any, _ time.Time) { delete(c, "iat") },
		"repo id wrong type":  func(c map[string]any, _ time.Time) { c["repository_id"] = 1001 },
		"jti empty":           func(c map[string]any, _ time.Time) { c["jti"] = "" },
		"aud empty array":     func(c map[string]any, _ time.Time) { c["aud"] = []string{} },
		"aud case mismatch":   func(c map[string]any, _ time.Time) { c["aud"] = strings.ToUpper(testAudience) },
		"issuer trailing /":   func(c map[string]any, _ time.Time) { c["iss"] = oidc.GitHubIssuer + "/" },
		"expired beyond skew": func(c map[string]any, now time.Time) { c["exp"] = now.Add(-time.Hour).Unix() },
	}
	iss := oidctest.NewIssuer(t)
	v, clock := newVerifier(t, iss)
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			claims := oidctest.Claims(testAudience, clock.Now())
			mutate(claims, clock.Now())
			token := iss.Mint(t, claims)
			_, err := v.Verify(context.Background(), token)
			if !errors.Is(err, oidc.ErrInvalidToken) {
				t.Fatalf("want ErrInvalidToken, got %v", err)
			}
			if strings.Contains(err.Error(), token) || strings.Contains(err.Error(), strings.Split(token, ".")[2]) {
				t.Fatal("error message leaks the token")
			}
		})
	}
}

func TestVerifier_AcceptsSmallClockSkew(t *testing.T) {
	iss := oidctest.NewIssuer(t)
	v, clock := newVerifier(t, iss)
	claims := oidctest.Claims(testAudience, clock.Now())
	claims["exp"] = clock.Now().Add(-20 * time.Second).Unix()
	claims["iat"] = clock.Now().Add(-4 * time.Minute).Unix()
	_, err := v.Verify(context.Background(), iss.Mint(t, claims))
	if err != nil {
		t.Fatalf("exp 20s ago is within the 30s skew: %v", err)
	}
	claims = oidctest.Claims(testAudience, clock.Now())
	claims["iat"] = clock.Now().Add(20 * time.Second).Unix()
	_, err = v.Verify(context.Background(), iss.Mint(t, claims))
	if err != nil {
		t.Fatalf("iat 20s ahead is within the 30s skew: %v", err)
	}
}

func TestVerifier_RejectsAlgorithms(t *testing.T) {
	iss := oidctest.NewIssuer(t)
	v, clock := newVerifier(t, iss)
	claims := oidctest.Claims(testAudience, clock.Now())

	none := oidctest.Sign(t, map[string]any{"alg": "none", "kid": iss.KID}, claims, nil)
	hs256 := hs256Token(t, iss, claims)
	rs512 := oidctest.Sign(t, map[string]any{"alg": "RS512", "kid": iss.KID}, claims, iss.Key)
	crit := oidctest.Sign(t, map[string]any{"alg": "RS256", "kid": iss.KID, "crit": []string{"exp"}}, claims, iss.Key)
	noKID := oidctest.Sign(t, map[string]any{"alg": "RS256"}, claims, iss.Key)
	for name, token := range map[string]string{
		"none": none, "HS256": hs256, "RS512": rs512, "crit": crit, "no kid": noKID,
		"two segments": "a.b", "garbage": "not-a-jwt", "padded": iss.Mint(t, claims) + "=",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := v.Verify(context.Background(), token)
			if !errors.Is(err, oidc.ErrInvalidToken) {
				t.Fatalf("want ErrInvalidToken, got %v", err)
			}
		})
	}
}

// hs256Token signs with HMAC using the RSA public modulus as the secret,
// the classic algorithm-confusion attack.
func hs256Token(t *testing.T, iss *oidctest.Issuer, claims map[string]any) string {
	t.Helper()
	h, _ := json.Marshal(map[string]any{"alg": "HS256", "kid": iss.KID})
	c, _ := json.Marshal(claims)
	input := base64.RawURLEncoding.EncodeToString(h) + "." + base64.RawURLEncoding.EncodeToString(c)
	mac := hmac.New(sha256.New, iss.Key.PublicKey.N.Bytes())
	mac.Write([]byte(input))
	return input + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func TestVerifier_RejectsForgedSignature(t *testing.T) {
	iss := oidctest.NewIssuer(t)
	v, clock := newVerifier(t, iss)
	other, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	claims := oidctest.Claims(testAudience, clock.Now())
	forged := oidctest.Sign(t, map[string]any{"alg": "RS256", "kid": iss.KID}, claims, other)
	_, err = v.Verify(context.Background(), forged)
	if !errors.Is(err, oidc.ErrInvalidToken) {
		t.Fatalf("forged signature: want ErrInvalidToken, got %v", err)
	}

	valid := iss.Mint(t, claims)
	parts := strings.Split(valid, ".")
	tampered := oidctest.Claims(testAudience, clock.Now())
	tampered["repository_id"] = "9999"
	body, _ := json.Marshal(tampered)
	swapped := parts[0] + "." + base64.RawURLEncoding.EncodeToString(body) + "." + parts[2]
	_, err = v.Verify(context.Background(), swapped)
	if !errors.Is(err, oidc.ErrInvalidToken) {
		t.Fatalf("tampered payload: want ErrInvalidToken, got %v", err)
	}
}

func TestVerifier_UnknownKIDRefreshIsRateLimited(t *testing.T) {
	iss := oidctest.NewIssuer(t)
	v, clock := newVerifier(t, iss)
	claims := oidctest.Claims(testAudience, clock.Now())
	_, err := v.Verify(context.Background(), iss.Mint(t, claims))
	if err != nil {
		t.Fatalf("warm up: %v", err)
	}
	if iss.Fetches() != 1 {
		t.Fatalf("first verify: want 1 JWKS fetch, got %d", iss.Fetches())
	}

	rotated, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	unknown := oidctest.Sign(t, map[string]any{"alg": "RS256", "kid": "kid-2"}, claims, rotated)
	for range 5 {
		_, err = v.Verify(context.Background(), unknown)
		if !errors.Is(err, oidc.ErrInvalidToken) {
			t.Fatalf("unknown kid: want ErrInvalidToken, got %v", err)
		}
	}
	if iss.Fetches() != 1 {
		t.Fatalf("unknown kid within a minute of the last fetch must not refetch, got %d fetches", iss.Fetches())
	}
	_, err = v.Verify(context.Background(), iss.Mint(t, claims))
	if err != nil || iss.Fetches() != 1 {
		t.Fatalf("known kid must be served from cache: err=%v fetches=%d", err, iss.Fetches())
	}

	iss.AddKey("kid-2", &rotated.PublicKey)
	clock.Advance(61 * time.Second)
	claims = oidctest.Claims(testAudience, clock.Now())
	rotatedToken := oidctest.Sign(t, map[string]any{"alg": "RS256", "kid": "kid-2"}, claims, rotated)
	_, err = v.Verify(context.Background(), rotatedToken)
	if err != nil {
		t.Fatalf("rotated key after a minute: %v", err)
	}
	if iss.Fetches() != 2 {
		t.Fatalf("want exactly one refresh, got %d fetches", iss.Fetches())
	}
	kid3 := oidctest.Sign(t, map[string]any{"alg": "RS256", "kid": "kid-3"}, claims, rotated)
	for range 3 {
		_, _ = v.Verify(context.Background(), kid3)
	}
	if iss.Fetches() != 2 {
		t.Fatalf("refresh limit must hold after a successful refresh, got %d fetches", iss.Fetches())
	}
}
