package server

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/krwu/compose-deploy-agent/internal/api"
	"github.com/krwu/compose-deploy-agent/internal/credential"
	"github.com/krwu/compose-deploy-agent/internal/oidc"
	"github.com/krwu/compose-deploy-agent/internal/store"
)

// clientIdentityKey is the request context key for the authenticated
// client certificate, set by fingerprintMiddleware.
type clientIdentityKey struct{}

// identityKind says which entry point authenticated a request. Each entry
// point sets it explicitly; the zero value is never a valid identity.
type identityKind int

const (
	// identityCert is a registered client certificate on the mTLS listener.
	identityCert identityKind = iota + 1
	// identityLocal is the local unix socket.
	identityLocal
	// identityOIDC is a verified GitHub Actions OIDC token on the OIDC listener.
	identityOIDC
)

// clientIdentity identifies who sent a request. claims is set only for
// identityOIDC.
type clientIdentity struct {
	kind        identityKind
	name        string
	fingerprint string
	claims      *oidc.Claims
}

// localIdentityMiddleware marks every request on the unix socket listener
// with the local identity. Access checks deny requests that carry no
// identity at all, so this must wrap the router served on the socket.
func localIdentityMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := clientIdentity{
			kind: identityLocal,
			name: "local",
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), clientIdentityKey{}, id)))
	})
}

// limitBody caps every request body at maxRequestBodyBytes; reading past
// the limit fails with *http.MaxBytesError.
func limitBody(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodyBytes)
		next.ServeHTTP(w, r)
	})
}

// fingerprintMiddleware runs after the TLS handshake has already verified
// the certificate chain and expiry (tls.RequireAndVerifyClientCert in
// internal/tlsconf). It performs the second authentication layer by
// checking the leaf certificate's fingerprint against the client_certs
// registration table, so that individual certificates can be revoked
// without invalidating the whole CA.
func fingerprintMiddleware(s *store.Store) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
				// Should be unreachable given RequireAndVerifyClientCert,
				// but fail closed if it ever happens.
				writeError(w, http.StatusForbidden, api.ErrCertNotRegistered, "no client certificate presented")
				return
			}
			leaf := r.TLS.PeerCertificates[0]
			fingerprint := credential.Fingerprint(leaf)

			rec, err := s.GetCert(fingerprint)
			if errors.Is(err, store.ErrNotFound) {
				writeError(w, http.StatusForbidden, api.ErrCertNotRegistered, "client certificate not registered")
				return
			}
			if err != nil {
				slog.Error("lookup client certificate failed", "error", err)
				writeError(w, http.StatusInternalServerError, api.ErrInternal, "internal error")
				return
			}
			if rec.Revoked() {
				writeError(w, http.StatusForbidden, api.ErrCertRevoked, "client certificate revoked")
				return
			}

			id := clientIdentity{
				kind:        identityCert,
				name:        rec.Name,
				fingerprint: fingerprint,
			}
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), clientIdentityKey{}, id)))
		})
	}
}
