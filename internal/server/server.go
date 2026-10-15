// Package server implements the agent's three HTTP listeners: the mTLS
// main listener on :8866 serving /v1/*, the plain-HTTP healthz listener on
// 127.0.0.1:8867, and a local-only unix socket listener serving the same
// /v1/* routes for `deploy-agent update`. The unix socket carries no mTLS:
// its trust root is host/root access to the socket file, the same trust
// root the host already has via docker.sock.
package server

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/krwu/compose-deploy-agent/internal/deployer"
	"github.com/krwu/compose-deploy-agent/internal/oidc"
	"github.com/krwu/compose-deploy-agent/internal/store"
)

// Connection limits applied to every listener. The main listener is meant
// to be reachable from CI (possibly through a TCP passthrough on a public
// 443), so a slow or idle client must not be able to hold connections or
// goroutines open indefinitely.
const (
	// defaultReadHeaderTimeout also bounds the TLS handshake: net/http
	// uses the smallest non-zero read/write timeout as the handshake
	// deadline.
	defaultReadHeaderTimeout = 10 * time.Second
	defaultReadTimeout       = 30 * time.Second
	defaultWriteTimeout      = 30 * time.Second
	defaultIdleTimeout       = 60 * time.Second
	maxHeaderBytes           = 16 << 10
	// maxRequestBodyBytes caps request bodies; a deploy request is a few
	// hundred bytes.
	maxRequestBodyBytes = 64 << 10
)

// serverTimeouts are the per-listener http.Server timeouts; tests shrink them.
type serverTimeouts struct {
	readHeader time.Duration
	read       time.Duration
	write      time.Duration
	idle       time.Duration
}

// Server bundles everything the HTTP handlers need.
type Server struct {
	deployer  *deployer.Deployer
	store     *store.Store
	version   string
	startedAt time.Time
	timeouts  serverTimeouts
	maxConns  int
	verifier  tokenVerifier
}

// New returns a Server ready to build handlers for.
func New(dep *deployer.Deployer, s *store.Store, version string) *Server {
	return &Server{
		deployer:  dep,
		store:     s,
		version:   version,
		startedAt: time.Now(),
		timeouts: serverTimeouts{
			readHeader: defaultReadHeaderTimeout,
			read:       defaultReadTimeout,
			write:      defaultWriteTimeout,
			idle:       defaultIdleTimeout,
		},
		maxConns: DefaultMaxConnections,
	}
}

// SetMaxConnections sets the cap on concurrently open connections to the
// main listener (see limitListener). Values < 1 keep the current cap. It
// must be called before MainListener/ServeMainOn.
func (s *Server) SetMaxConnections(n int) {
	if n >= 1 {
		s.maxConns = n
	}
}

// EnableOIDC installs the token verifier used by the OIDC listener. It
// must be called before OIDCListener/ServeOIDCOn.
func (s *Server) EnableOIDC(v *oidc.Verifier) {
	if v != nil {
		s.verifier = v
	}
}

// newHTTPServer returns an http.Server for handler with the connection
// limits applied.
func (s *Server) newHTTPServer(handler http.Handler) *http.Server {
	return &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: s.timeouts.readHeader,
		ReadTimeout:       s.timeouts.read,
		WriteTimeout:      s.timeouts.write,
		IdleTimeout:       s.timeouts.idle,
		MaxHeaderBytes:    maxHeaderBytes,
	}
}

// MainListener starts the mTLS listener on addr (e.g. "10.0.0.5:8866") and
// blocks until ctx is cancelled or the listener errors.
func (s *Server) MainListener(ctx context.Context, addr string, tlsConfig *tls.Config) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	return s.ServeMainOn(ctx, ln, tlsConfig)
}

// ServeMainOn serves the mTLS router and the unauthenticated /v1/version
// router on an already-bound listener, splitting the two protocols with
// protoDemux. Split out from MainListener so
// tests can bind an ephemeral port ("127.0.0.1:0") and learn the actual
// port before connecting.
func (server *Server) ServeMainOn(ctx context.Context, rawLn net.Listener, tlsConfig *tls.Config) error {
	ln := newLimitListener(rawLn, server.maxConns)
	demux := newProtoDemux(ln)

	tlsSrv := server.newHTTPServer(fingerprintMiddleware(server.store)(server.mainRouter()))
	tlsSrv.TLSConfig = tlsConfig
	plainSrv := server.newHTTPServer(server.publicRouter())

	errCh := make(chan error, 2)
	go func() { errCh <- tlsSrv.Serve(tls.NewListener(demux.tls, tlsConfig)) }()
	go func() { errCh <- plainSrv.Serve(demux.plain) }()

	select {
	case err := <-errCh:
		ln.Close()
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := tlsSrv.Shutdown(shutdownCtx); err != nil {
			slog.Warn("mTLS listener shutdown error", "error", err)
		}
		if err := plainSrv.Shutdown(shutdownCtx); err != nil {
			slog.Warn("plain version listener shutdown error", "error", err)
		}
		ln.Close()
		return nil
	}
}

// OIDCListener starts the OIDC listener on addr and blocks until ctx is
// cancelled or the listener errors. See ServeOIDCOn.
func (s *Server) OIDCListener(ctx context.Context, addr string, tlsConfig *tls.Config) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	return s.ServeOIDCOn(ctx, ln, tlsConfig)
}

// ServeOIDCOn serves the /v1/* routes on a separate TLS listener that
// authenticates with GitHub Actions OIDC bearer tokens instead of client
// certificates. The agent terminates TLS itself with the same server
// certificate; no client certificate is requested. tlsConfig is cloned, so
// the mTLS listener's RequireAndVerifyClientCert is never touched. The
// listener gets the same timeouts, body limit and connection cap as the
// mTLS one, but its own cap.
func (server *Server) ServeOIDCOn(ctx context.Context, rawLn net.Listener, tlsConfig *tls.Config) error {
	if server.verifier == nil {
		rawLn.Close()
		return errors.New("OIDC listener started without a verifier")
	}
	oidcTLS := tlsConfig.Clone()
	oidcTLS.ClientAuth = tls.NoClientCert
	oidcTLS.ClientCAs = nil
	ln := newLimitListener(rawLn, server.maxConns)
	srv := server.newHTTPServer(oidcMiddleware(server.verifier)(server.mainRouter()))
	srv.TLSConfig = oidcTLS
	return serveUntilDone(ctx, srv, func() error {
		return srv.Serve(tls.NewListener(ln, oidcTLS))
	})
}

// HealthzListener starts the plain-HTTP healthz listener, always bound to
// loopback so that monitoring never needs a client certificate.
func (s *Server) HealthzListener(ctx context.Context, addr string) error {
	srv := s.newHTTPServer(s.healthzRouter())
	srv.Addr = addr
	return serveUntilDone(ctx, srv, srv.ListenAndServe)
}

// LocalListener starts a unix socket listener at socketPath serving the
// same /v1/* routes as the mTLS listener, but without mTLS or the
// fingerprint middleware: whoever can reach the socket file has already
// proven host access, which this system treats as the real trust root.
// Requests are tagged with the local identity (localIdentityMiddleware).
//
// The socket's parent directory is the real access boundary: it must be
// owned by the running user and inaccessible to group and others (see
// ensurePrivateDir), so the socket is never reachable by anyone else, not
// even between Listen and the Chmod below. The process umask is
// deliberately left alone - it is process-wide and would race the sqlite
// and .env writes of other goroutines. Any stale socket file left behind by
// a previous unclean shutdown is removed first; this is safe because the
// caller already holds the single-instance lock (internal/singleton). The
// 0600 chmod is a second layer.
func (s *Server) LocalListener(ctx context.Context, socketPath string) error {
	err := ensurePrivateDir(filepath.Dir(socketPath))
	if err != nil {
		return fmt.Errorf("unix socket %s: %w", socketPath, err)
	}
	err = os.Remove(socketPath)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		return err
	}
	err = os.Chmod(socketPath, 0o600)
	if err != nil {
		ln.Close()
		return err
	}
	srv := s.newHTTPServer(localIdentityMiddleware(s.mainRouter()))
	return serveUntilDone(ctx, srv, func() error {
		return srv.Serve(ln)
	})
}

func serveUntilDone(ctx context.Context, srv *http.Server, listenAndServe func() error) error {
	errCh := make(chan error, 1)
	go func() { errCh <- listenAndServe() }()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			slog.Warn("server shutdown error", "addr", srv.Addr, "error", err)
		}
		return nil
	}
}
