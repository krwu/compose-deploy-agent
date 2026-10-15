package client

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	// tokenRefreshMargin re-requests an Actions token this long before it
	// expires, so a long poll never sends an expired token.
	tokenRefreshMargin = time.Minute
	// tokenResponseLimit caps the Actions token endpoint response.
	tokenResponseLimit = 64 << 10
)

// TokenSource returns the OIDC bearer token to send. Errors must never
// contain a token.
type TokenSource func(ctx context.Context) (string, error)

// StaticToken always returns token (e.g. read from an environment variable).
func StaticToken(token string) TokenSource {
	return func(context.Context) (string, error) {
		return token, nil
	}
}

// ActionsToken requests GitHub Actions OIDC tokens for audience from the
// runner's ACTIONS_ID_TOKEN_REQUEST_URL using ACTIONS_ID_TOKEN_REQUEST_TOKEN
// (the job needs `permissions: id-token: write`). A token is reused until
// tokenRefreshMargin before its exp.
func ActionsToken(requestURL, requestToken, audience string) TokenSource {
	src := &actionsTokenSource{
		requestURL:   requestURL,
		requestToken: requestToken,
		audience:     audience,
		httpClient:   &http.Client{Timeout: DefaultConnectTimeout},
	}
	return src.token
}

type actionsTokenSource struct {
	requestURL   string
	requestToken string
	audience     string
	httpClient   *http.Client

	mu     sync.Mutex
	cached string
	expiry time.Time
}

func (s *actionsTokenSource) token(ctx context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cached != "" && time.Now().Add(tokenRefreshMargin).Before(s.expiry) {
		return s.cached, nil
	}
	token, err := s.fetch(ctx)
	if err != nil {
		return "", err
	}
	s.cached = token
	s.expiry = unverifiedExpiry(token)
	return token, nil
}

func (s *actionsTokenSource) fetch(ctx context.Context) (string, error) {
	u, err := url.Parse(s.requestURL)
	if err != nil {
		return "", errors.New("request OIDC token: invalid ACTIONS_ID_TOKEN_REQUEST_URL")
	}
	q := u.Query()
	q.Set("audience", s.audience)
	u.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", errors.New("request OIDC token: build request failed")
	}
	req.Header.Set("Authorization", "Bearer "+s.requestToken)
	req.Header.Set("Accept", "application/json")
	resp, err := s.httpClient.Do(req)
	if err != nil {
		return "", errors.New("request OIDC token: token endpoint unreachable")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("request OIDC token: token endpoint returned http %d", resp.StatusCode)
	}
	var body struct {
		Value string `json:"value"`
	}
	err = json.NewDecoder(io.LimitReader(resp.Body, tokenResponseLimit)).Decode(&body)
	if err != nil || body.Value == "" {
		return "", errors.New("request OIDC token: malformed token endpoint response")
	}
	return body.Value, nil
}

// unverifiedExpiry reads exp from a JWT without verifying it, only to
// decide when to fetch a new one; zero means "do not cache".
func unverifiedExpiry(token string) time.Time {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return time.Time{}
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return time.Time{}
	}
	var claims struct {
		Exp int64 `json:"exp"`
	}
	err = json.Unmarshal(raw, &claims)
	if err != nil || claims.Exp == 0 {
		return time.Time{}
	}
	return time.Unix(claims.Exp, 0)
}
