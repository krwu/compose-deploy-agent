package server

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/krwu/compose-deploy-agent/internal/api"
	"github.com/krwu/compose-deploy-agent/internal/oidc"
	"github.com/krwu/compose-deploy-agent/internal/store"
)

// authorizeService is the single access checkpoint for every entry point:
// it reports whether the request may act on service, and otherwise logs
// the denial and writes 403. It fails closed: a request without an
// identity in its context (set explicitly by fingerprintMiddleware,
// oidcMiddleware or localIdentityMiddleware) is denied. Certificates are
// checked against access.clients, OIDC identities against the service's
// oidc rules; the local identity, whose trust root is host access to the
// unix socket, keeps full access.
func (server *Server) authorizeService(w http.ResponseWriter, r *http.Request, service string) bool {
	id, ok := r.Context().Value(clientIdentityKey{}).(clientIdentity)
	if !ok {
		slog.Error("request without client identity denied", "service", service,
			"method", r.Method, "path", r.URL.Path)
		writeError(w, http.StatusForbidden, api.ErrCertNotAllowed, "no client identity")
		return false
	}
	manifest := server.deployer.Manifest()
	switch id.kind {
	case identityLocal:
		return true
	case identityCert:
		if manifest.ClientAllowed(id.name, id.fingerprint, service) {
			return true
		}
		slog.Warn("client certificate not allowed for service",
			"cert_name", id.name, "fingerprint", id.fingerprint, "service", service,
			"method", r.Method, "path", r.URL.Path)
		writeError(w, http.StatusForbidden, api.ErrCertNotAllowed,
			fmt.Sprintf("client certificate is not allowed to access service %q", service))
		return false
	case identityOIDC:
		err := manifest.OIDCAllowed(service, oidcClaims(id.claims))
		if err == nil {
			return true
		}
		attrs := append([]any{"service", service, "method", r.Method, "path", r.URL.Path, "reason", err},
			oidcLogAttrs(id.claims)...)
		slog.Warn("OIDC token not allowed for service", attrs...)
		writeError(w, http.StatusForbidden, api.ErrAccessDenied, "access denied")
		return false
	default:
		slog.Error("request with unknown identity kind denied", "service", service, "kind", int(id.kind))
		writeError(w, http.StatusForbidden, api.ErrCertNotAllowed, "no client identity")
		return false
	}
}

// consumeOIDCToken records the jti of an OIDC identity that was allowed
// to start a deploy, so the same token cannot start another one. It runs
// only after signature, iss, aud, time and binding checks all passed, and
// is a no-op for other identities. Reads (status polling) do not consume
// the token. If the deploy then fails to start (e.g. 409 while another
// deploy runs), releaseOIDCToken frees the jti again: a token can start at
// most one deploy.
func (server *Server) consumeOIDCToken(w http.ResponseWriter, r *http.Request) bool {
	id, ok := r.Context().Value(clientIdentityKey{}).(clientIdentity)
	if !ok || id.kind != identityOIDC {
		return true
	}
	err := server.store.RecordJTI(id.claims.JTI, id.claims.Expiry().Add(oidc.ClockSkew), time.Now())
	if errors.Is(err, store.ErrReplay) {
		slog.Warn("OIDC token replay rejected", oidcLogAttrs(id.claims)...)
		writeUnauthenticated(w)
		return false
	}
	if err != nil {
		slog.Error("record OIDC jti failed", "error", err)
		writeError(w, http.StatusInternalServerError, api.ErrInternal, "internal error")
		return false
	}
	return true
}

// releaseOIDCToken undoes consumeOIDCToken when no deploy was started.
func (server *Server) releaseOIDCToken(r *http.Request) {
	id, ok := r.Context().Value(clientIdentityKey{}).(clientIdentity)
	if !ok || id.kind != identityOIDC {
		return
	}
	err := server.store.ReleaseJTI(id.claims.JTI)
	if err != nil {
		slog.Error("release OIDC jti failed", "error", err)
	}
}
