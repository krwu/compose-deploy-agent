package oidc

import (
	"context"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"sync"
	"time"
)

// JWKS cache policy. Keys are fetched on first use and afterwards only
// refreshed when a token names a kid that is not cached, at most once per
// jwksMinRefreshInterval (counting failed attempts), so a stream of forged
// kids cannot turn the agent into a request amplifier against the issuer.
// A failed refresh keeps the previous keys.
const (
	jwksMinRefreshInterval = time.Minute
	jwksFetchTimeout       = 10 * time.Second
	jwksMaxBodyBytes       = 1 << 20
	minRSAKeyBits          = 2048
)

var errUnknownKID = errors.New("unknown kid")

// keySet is a lazily fetched, cached JWKS.
type keySet struct {
	url        string
	httpClient *http.Client
	now        func() time.Time

	mu          sync.Mutex
	keys        map[string]*rsa.PublicKey
	lastAttempt time.Time
}

func newKeySet(url string, httpClient *http.Client, now func() time.Time) *keySet {
	return &keySet{
		url:        url,
		httpClient: httpClient,
		now:        now,
	}
}

// key returns the RSA public key for kid, fetching the JWKS on first use
// and refreshing it for an unknown kid within the rate limit.
func (ks *keySet) key(ctx context.Context, kid string) (*rsa.PublicKey, error) {
	ks.mu.Lock()
	defer ks.mu.Unlock()

	pub, ok := ks.keys[kid]
	if ok {
		return pub, nil
	}
	now := ks.now()
	if !ks.lastAttempt.IsZero() && now.Sub(ks.lastAttempt) < jwksMinRefreshInterval {
		return nil, errUnknownKID
	}
	ks.lastAttempt = now
	keys, err := ks.fetch(ctx)
	if err != nil {
		slog.Error("fetch OIDC JWKS failed", "url", ks.url, "error", err)
		return nil, errUnknownKID
	}
	ks.keys = keys
	pub, ok = ks.keys[kid]
	if !ok {
		return nil, errUnknownKID
	}
	return pub, nil
}

type jwkDoc struct {
	Keys []jwk `json:"keys"`
}

type jwk struct {
	Kty string `json:"kty"`
	Kid string `json:"kid"`
	Use string `json:"use"`
	Alg string `json:"alg"`
	N   string `json:"n"`
	E   string `json:"e"`
}

func (ks *keySet) fetch(ctx context.Context) (map[string]*rsa.PublicKey, error) {
	ctx, cancel := context.WithTimeout(ctx, jwksFetchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ks.url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := ks.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, jwksMaxBodyBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > jwksMaxBodyBytes {
		return nil, errors.New("jwks document too large")
	}
	return parseJWKS(data)
}

// parseJWKS keeps the RSA signing keys of a JWKS document, skipping keys
// that are not usable for RS256 and refusing a document without any.
func parseJWKS(data []byte) (map[string]*rsa.PublicKey, error) {
	var doc jwkDoc
	err := json.Unmarshal(data, &doc)
	if err != nil {
		return nil, fmt.Errorf("parse jwks: %w", err)
	}
	keys := make(map[string]*rsa.PublicKey, len(doc.Keys))
	for _, k := range doc.Keys {
		if k.Kty != "RSA" || k.Kid == "" || (k.Use != "" && k.Use != "sig") || (k.Alg != "" && k.Alg != algRS256) {
			continue
		}
		pub, err := rsaPublicKey(k.N, k.E)
		if err != nil {
			slog.Warn("skip invalid OIDC JWKS key", "kid", k.Kid, "error", err)
			continue
		}
		keys[k.Kid] = pub
	}
	if len(keys) == 0 {
		return nil, errors.New("jwks contains no usable RSA keys")
	}
	return keys, nil
}

func rsaPublicKey(nB64, eB64 string) (*rsa.PublicKey, error) {
	nBytes, err := base64.RawURLEncoding.DecodeString(nB64)
	if err != nil {
		return nil, fmt.Errorf("decode n: %w", err)
	}
	eBytes, err := base64.RawURLEncoding.DecodeString(eB64)
	if err != nil {
		return nil, fmt.Errorf("decode e: %w", err)
	}
	n := new(big.Int).SetBytes(nBytes)
	if n.BitLen() < minRSAKeyBits {
		return nil, fmt.Errorf("modulus has %d bits, need at least %d", n.BitLen(), minRSAKeyBits)
	}
	e := new(big.Int).SetBytes(eBytes)
	if !e.IsInt64() || e.Int64() < 3 || e.Int64() > 1<<31-1 || e.Bit(0) == 0 {
		return nil, errors.New("invalid exponent")
	}
	return &rsa.PublicKey{
		N: n,
		E: int(e.Int64()),
	}, nil
}
