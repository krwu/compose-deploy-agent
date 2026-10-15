package server

import (
	"context"
	"log/slog"
	"net/http"
	"strings"

	"github.com/krwu/compose-deploy-agent/internal/api"
	"github.com/krwu/compose-deploy-agent/internal/config"
	"github.com/krwu/compose-deploy-agent/internal/oidc"
)

// tokenVerifier verifies a bearer token; *oidc.Verifier implements it.
type tokenVerifier interface {
	Verify(ctx context.Context, token string) (oidc.Claims, error)
}

// oidcMiddleware authenticates every request on the OIDC listener with a
// GitHub Actions OIDC token (Authorization: Bearer). A missing or invalid
// token gets 401 with a generic body; the reason is logged, the token
// never is. A valid token only establishes identity: what it may do is
// decided by authorizeService, the same checkpoint used for certificates.
func oidcMiddleware(verifier tokenVerifier) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			token, ok := bearerToken(r)
			if !ok {
				slog.Warn("OIDC request without bearer token", "method", r.Method, "path", r.URL.Path,
					"remote", r.RemoteAddr)
				writeUnauthenticated(w)
				return
			}
			claims, err := verifier.Verify(r.Context(), token)
			if err != nil {
				slog.Warn("OIDC token rejected", "error", err, "method", r.Method, "path", r.URL.Path,
					"remote", r.RemoteAddr)
				writeUnauthenticated(w)
				return
			}
			id := clientIdentity{
				kind:   identityOIDC,
				name:   "oidc:" + claims.Repository,
				claims: &claims,
			}
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), clientIdentityKey{}, id)))
		})
	}
}

// bearerToken extracts the token of an "Authorization: Bearer" header.
func bearerToken(r *http.Request) (string, bool) {
	scheme, token, found := strings.Cut(r.Header.Get("Authorization"), " ")
	if !found || !strings.EqualFold(scheme, "Bearer") {
		return "", false
	}
	token = strings.TrimSpace(token)
	return token, token != ""
}

func writeUnauthenticated(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", `Bearer realm="deploy-agent"`)
	writeError(w, http.StatusUnauthorized, api.ErrUnauthenticated, "authentication required")
}

// oidcClaims maps verified token claims onto the manifest's rule input.
func oidcClaims(c *oidc.Claims) config.OIDCClaims {
	return config.OIDCClaims{
		RepositoryID:      c.RepositoryID,
		RepositoryOwnerID: c.RepositoryOwnerID,
		Repository:        c.Repository,
		Ref:               c.Ref,
		EventName:         c.EventName,
		Environment:       c.Environment,
		JobWorkflowRef:    c.JobWorkflowRef,
	}
}

// oidcLogAttrs are the claims logged for an OIDC decision. The token and
// its signature are never logged.
func oidcLogAttrs(c *oidc.Claims) []any {
	return []any{
		"repository", c.Repository, "repository_id", c.RepositoryID,
		"repository_owner_id", c.RepositoryOwnerID, "ref", c.Ref, "event_name", c.EventName,
		"environment", c.Environment, "job_workflow_ref", c.JobWorkflowRef,
		"actor", c.Actor, "run_id", c.RunID, "jti", c.JTI,
	}
}
