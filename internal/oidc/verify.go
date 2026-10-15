// Package oidc verifies GitHub Actions OIDC ID tokens (RS256 JWTs) for the
// agent's dedicated OIDC listener. The issuer and the JWKS location are
// compile-time constants: nothing in a token decides where keys come from
// or which issuer is trusted. Errors never contain the token itself.
package oidc

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

const (
	// GitHubIssuer is the only accepted iss claim.
	GitHubIssuer = "https://token.actions.githubusercontent.com"
	// GitHubJWKSURL is the only place signing keys are fetched from.
	GitHubJWKSURL = GitHubIssuer + "/.well-known/jwks"
	// ClockSkew is the tolerance applied to exp, nbf and iat.
	ClockSkew = 30 * time.Second

	algRS256 = "RS256"
	// maxTokenBytes bounds the compact JWT; GitHub tokens are ~1-2 KiB.
	maxTokenBytes = 8 << 10
	// maxTokenLifetime refuses tokens valid for longer than GitHub issues
	// them, which also bounds how long a jti must be remembered.
	maxTokenLifetime = time.Hour
)

// ErrInvalidToken wraps every verification failure.
var ErrInvalidToken = errors.New("invalid OIDC token")

// Claims are the verified claims the agent uses. GitHub encodes the
// numeric ids as JSON strings.
type Claims struct {
	Issuer            string   `json:"iss"`
	Subject           string   `json:"sub"`
	Audience          audience `json:"aud"`
	ExpiresAt         *int64   `json:"exp"`
	NotBefore         *int64   `json:"nbf"`
	IssuedAt          *int64   `json:"iat"`
	JTI               string   `json:"jti"`
	Repository        string   `json:"repository"`
	RepositoryID      string   `json:"repository_id"`
	RepositoryOwner   string   `json:"repository_owner"`
	RepositoryOwnerID string   `json:"repository_owner_id"`
	Ref               string   `json:"ref"`
	EventName         string   `json:"event_name"`
	Environment       string   `json:"environment"`
	JobWorkflowRef    string   `json:"job_workflow_ref"`
	Actor             string   `json:"actor"`
	RunID             string   `json:"run_id"`
}

// Expiry returns the exp claim as a time. Only valid on verified claims.
func (c Claims) Expiry() time.Time {
	if c.ExpiresAt == nil {
		return time.Time{}
	}
	return time.Unix(*c.ExpiresAt, 0)
}

// audience accepts the JSON string or array form of aud.
type audience []string

func (a *audience) UnmarshalJSON(data []byte) error {
	var single string
	err := json.Unmarshal(data, &single)
	if err == nil {
		*a = audience{single}
		return nil
	}
	var many []string
	err = json.Unmarshal(data, &many)
	if err != nil {
		return errors.New("aud must be a string or an array of strings")
	}
	*a = many
	return nil
}

type header struct {
	Alg  string          `json:"alg"`
	Kid  string          `json:"kid"`
	Crit json.RawMessage `json:"crit"`
}

// Verifier checks GitHub Actions OIDC tokens against one audience.
type Verifier struct {
	audience string
	keys     *keySet
	now      func() time.Time
}

// NewVerifier returns a Verifier for the given audience, which must be
// the agent-specific value configured in services.yaml. httpClient (nil:
// a default client) and now (nil: time.Now) exist for tests; the JWKS URL
// itself is always GitHubJWKSURL.
func NewVerifier(aud string, httpClient *http.Client, now func() time.Time) (*Verifier, error) {
	if aud == "" {
		return nil, errors.New("oidc audience must not be empty")
	}
	if httpClient == nil {
		httpClient = &http.Client{Timeout: jwksFetchTimeout}
	}
	if now == nil {
		now = time.Now
	}
	return &Verifier{
		audience: aud,
		keys:     newKeySet(GitHubJWKSURL, httpClient, now),
		now:      now,
	}, nil
}

// Verify checks the token's signature (RS256 only), issuer, audience and
// time claims, and that jti and the claims used for binding are present.
// The returned error wraps ErrInvalidToken and never contains the token.
func (v *Verifier) Verify(ctx context.Context, token string) (Claims, error) {
	claims, err := v.verify(ctx, token)
	if err != nil {
		return Claims{}, fmt.Errorf("%w: %w", ErrInvalidToken, err)
	}
	return claims, nil
}

func (v *Verifier) verify(ctx context.Context, token string) (Claims, error) {
	if len(token) > maxTokenBytes {
		return Claims{}, errors.New("token too large")
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return Claims{}, errors.New("malformed token")
	}
	var hdr header
	err := decodeSegment(parts[0], &hdr)
	if err != nil {
		return Claims{}, fmt.Errorf("header: %w", err)
	}
	if hdr.Alg != algRS256 {
		return Claims{}, fmt.Errorf("alg %q not allowed", truncate(hdr.Alg))
	}
	if len(hdr.Crit) > 0 || hdr.Kid == "" {
		return Claims{}, errors.New("header must carry kid and no crit")
	}
	err = v.checkSignature(ctx, hdr.Kid, parts)
	if err != nil {
		return Claims{}, err
	}
	var claims Claims
	err = decodeSegment(parts[1], &claims)
	if err != nil {
		return Claims{}, fmt.Errorf("claims: %w", err)
	}
	err = v.checkClaims(claims)
	if err != nil {
		return Claims{}, err
	}
	return claims, nil
}

func (v *Verifier) checkSignature(ctx context.Context, kid string, parts []string) error {
	sig, err := base64.RawURLEncoding.Strict().DecodeString(parts[2])
	if err != nil {
		return errors.New("signature: bad encoding")
	}
	pub, err := v.keys.key(ctx, kid)
	if err != nil {
		return fmt.Errorf("kid %q: %w", truncate(kid), err)
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	err = rsa.VerifyPKCS1v15(pub, crypto.SHA256, digest[:], sig)
	if err != nil {
		return errors.New("signature mismatch")
	}
	return nil
}

func (v *Verifier) checkClaims(c Claims) error {
	if c.Issuer != GitHubIssuer {
		return fmt.Errorf("iss %q not trusted", truncate(c.Issuer))
	}
	if len(c.Audience) != 1 || c.Audience[0] != v.audience {
		return errors.New("aud mismatch")
	}
	err := v.checkTimes(c)
	if err != nil {
		return err
	}
	if c.JTI == "" || c.RepositoryID == "" || c.RepositoryOwnerID == "" || c.Ref == "" || c.EventName == "" {
		return errors.New("missing jti, repository_id, repository_owner_id, ref or event_name")
	}
	return nil
}

func (v *Verifier) checkTimes(c Claims) error {
	if c.ExpiresAt == nil || c.IssuedAt == nil || c.NotBefore == nil {
		return errors.New("missing exp, iat or nbf")
	}
	now := v.now()
	exp := time.Unix(*c.ExpiresAt, 0)
	iat := time.Unix(*c.IssuedAt, 0)
	nbf := time.Unix(*c.NotBefore, 0)
	switch {
	case !now.Before(exp.Add(ClockSkew)):
		return errors.New("token expired")
	case now.Add(ClockSkew).Before(nbf):
		return errors.New("token not yet valid (nbf)")
	case now.Add(ClockSkew).Before(iat):
		return errors.New("token issued in the future (iat)")
	case exp.Sub(iat) > maxTokenLifetime || !exp.After(iat):
		return errors.New("token lifetime out of range")
	default:
		return nil
	}
}

// decodeSegment decodes one base64url JWT segment into v, rejecting
// padding, unknown trailing data and invalid JSON.
func decodeSegment(seg string, v any) error {
	raw, err := base64.RawURLEncoding.Strict().DecodeString(seg)
	if err != nil {
		return errors.New("bad base64url encoding")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	err = dec.Decode(v)
	if err != nil {
		return fmt.Errorf("bad JSON: %w", err)
	}
	if dec.More() {
		return errors.New("trailing data")
	}
	return nil
}

// truncate bounds attacker-controlled strings before they reach a log.
func truncate(s string) string {
	const limit = 64
	if len(s) <= limit {
		return s
	}
	return s[:limit] + "..."
}
