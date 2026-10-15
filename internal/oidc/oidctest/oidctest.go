// Package oidctest provides a fake GitHub Actions OIDC issuer for tests:
// a locally generated RSA key, a JWKS served by httptest, and an
// http.Client that routes the verifier's fixed JWKS URL to it.
package oidctest

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Issuer is a fake OIDC issuer.
type Issuer struct {
	Key    *rsa.PrivateKey
	KID    string
	server *httptest.Server
	hits   atomic.Int64

	mu   sync.Mutex
	keys map[string]*rsa.PublicKey
}

// NewIssuer starts a JWKS server publishing one fresh 2048-bit key.
func NewIssuer(t *testing.T) *Issuer {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate RSA key: %v", err)
	}
	iss := &Issuer{
		Key:  key,
		KID:  "test-kid-1",
		keys: map[string]*rsa.PublicKey{"test-kid-1": &key.PublicKey},
	}
	iss.server = httptest.NewServer(http.HandlerFunc(iss.serveJWKS))
	t.Cleanup(iss.server.Close)
	return iss
}

// AddKey publishes pub under kid from now on.
func (iss *Issuer) AddKey(kid string, pub *rsa.PublicKey) {
	iss.mu.Lock()
	defer iss.mu.Unlock()
	iss.keys[kid] = pub
}

// Fetches reports how many times the JWKS was requested.
func (iss *Issuer) Fetches() int64 {
	return iss.hits.Load()
}

func (iss *Issuer) serveJWKS(w http.ResponseWriter, _ *http.Request) {
	iss.hits.Add(1)
	iss.mu.Lock()
	defer iss.mu.Unlock()
	type jwk struct {
		Kty string `json:"kty"`
		Kid string `json:"kid"`
		Alg string `json:"alg"`
		Use string `json:"use"`
		N   string `json:"n"`
		E   string `json:"e"`
	}
	doc := struct {
		Keys []jwk `json:"keys"`
	}{}
	for kid, pub := range iss.keys {
		doc.Keys = append(doc.Keys, jwk{
			Kty: "RSA",
			Kid: kid,
			Alg: "RS256",
			Use: "sig",
			N:   b64(pub.N.Bytes()),
			E:   b64(big.NewInt(int64(pub.E)).Bytes()),
		})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(doc)
}

// HTTPClient returns a client that sends every request to the JWKS server,
// whatever URL it names.
func (iss *Issuer) HTTPClient() *http.Client {
	target, _ := url.Parse(iss.server.URL)
	return &http.Client{
		Timeout: 5 * time.Second,
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			r = r.Clone(r.Context())
			r.URL.Scheme = target.Scheme
			r.URL.Host = target.Host
			return http.DefaultTransport.RoundTrip(r)
		}),
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// Claims returns a valid claim set for audience at now: GitHub's issuer,
// a push to refs/heads/main of repository 1001 owned by 2002.
func Claims(audience string, now time.Time) map[string]any {
	return map[string]any{
		"iss":                 "https://token.actions.githubusercontent.com",
		"sub":                 "repo:kairee/img-webdn:environment:preview-main",
		"aud":                 audience,
		"iat":                 now.Unix(),
		"nbf":                 now.Add(-time.Minute).Unix(),
		"exp":                 now.Add(5 * time.Minute).Unix(),
		"jti":                 "jti-" + now.Format(time.RFC3339Nano),
		"repository":          "kairee/img-webdn",
		"repository_id":       "1001",
		"repository_owner":    "kairee",
		"repository_owner_id": "2002",
		"ref":                 "refs/heads/main",
		"event_name":          "push",
		"environment":         "preview-main",
		"job_workflow_ref":    "kairee/img-webdn/.github/workflows/deploy.yml@refs/heads/main",
		"actor":               "kairee",
		"run_id":              "42",
	}
}

// Mint signs claims with the issuer key under its kid (RS256).
func (iss *Issuer) Mint(t *testing.T, claims map[string]any) string {
	t.Helper()
	return Sign(t, map[string]any{"alg": "RS256", "typ": "JWT", "kid": iss.KID}, claims, iss.Key)
}

// Sign builds a compact JWT; with a nil key the signature is empty.
func Sign(t *testing.T, header, claims map[string]any, key *rsa.PrivateKey) string {
	t.Helper()
	h, err := json.Marshal(header)
	if err != nil {
		t.Fatalf("marshal header: %v", err)
	}
	c, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("marshal claims: %v", err)
	}
	signingInput := b64(h) + "." + b64(c)
	if key == nil {
		return signingInput + "."
	}
	digest := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	return signingInput + "." + b64(sig)
}

func b64(b []byte) string {
	return base64.RawURLEncoding.EncodeToString(b)
}
