package main

import (
	"cmp"
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/krwu/compose-deploy-agent/internal/ca"
	"github.com/krwu/compose-deploy-agent/internal/config"
	"github.com/krwu/compose-deploy-agent/internal/deployer"
	"github.com/krwu/compose-deploy-agent/internal/dockerx"
	"github.com/krwu/compose-deploy-agent/internal/paths"
	"github.com/krwu/compose-deploy-agent/internal/server"
	"github.com/krwu/compose-deploy-agent/internal/singleton"
	"github.com/krwu/compose-deploy-agent/internal/store"
	"github.com/krwu/compose-deploy-agent/internal/tlsconf"
)

// socketFlagUsage documents --socket for serve and update.
const socketFlagUsage = "local unix socket path (default " + paths.DefaultSocketPath +
	", or <data-dir>/agent.sock with a non-default --data-dir); " +
	"its directory must be 0700 and owned by the running user"

// expiryCheckInterval is how often the running agent re-checks for
// soon-to-expire client certificates ("每日定时检查").
const expiryCheckInterval = 24 * time.Hour

// defaultHealthzAddr is serve's default --healthz-bind, also polled by
// `deploy-agent self-upgrade` after restarting the service.
const defaultHealthzAddr = "127.0.0.1:8867"

func runServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	manifestPath := fs.String("manifest", paths.DefaultManifestPath, "path to services.yaml")
	dataDir := fs.String("data-dir", paths.DefaultDataDir, "data directory (sqlite db, CA material)")
	bindAddr := fs.String("bind", "", "address:port for the mTLS listener, e.g. 10.0.0.5:8866")
	healthzAddr := fs.String("healthz-bind", defaultHealthzAddr, "address:port for the plain-HTTP healthz listener")
	socketFlag := fs.String("socket", "", socketFlagUsage)
	maxConns := fs.Int("max-conns", server.DefaultMaxConnections, "max open connections on --bind (and on --oidc-listen)")
	oidcListen := fs.String("oidc-listen", "", "address:port for the GitHub Actions OIDC listener (TLS, bearer token, "+
		"no client certificate); empty disables OIDC")
	testOnly := fs.Bool("t", false, "test the configuration and exit, without taking the instance lock or opening the database")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *bindAddr == "" {
		return fmt.Errorf("--bind is required, e.g. --bind 10.0.0.5:8866")
	}

	cfg, err := loadServeConfig(*manifestPath, *dataDir)
	if err != nil {
		return err
	}
	err = checkOIDCListen(cfg.manifest, *oidcListen)
	if err != nil {
		return err
	}
	if *testOnly {
		return runServeTest(cfg, *bindAddr, *oidcListen)
	}

	release, err := singleton.Lock(paths.LockPath(*dataDir))
	if err != nil {
		return err
	}
	defer release()

	st, err := store.Open(paths.DBPath(*dataDir))
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer st.Close()

	tlsConfig := tlsconf.ServerConfig(cfg.serverCert, cfg.clientCAPool)
	dep := deployer.New(cfg.manifest, dockerx.NewClient(), st)

	if n, err := dep.MarkInterruptedOnStartup(); err != nil {
		slog.Error("mark interrupted deploys failed", "error", err)
	} else if n > 0 {
		slog.Warn("marked in-progress deploys as interrupted after restart", "count", n)
	}

	logAccessWarnings(cfg.manifest, *oidcListen)
	runReverseCalibration(context.Background(), dep, cfg.manifest)
	logExpiringCerts(st)

	srv := server.New(dep, st, version)
	srv.SetMaxConnections(*maxConns)
	err = enableOIDC(srv, cfg.manifest, *oidcListen)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go checkExpiringCertsDaily(ctx, st)
	go purgeExpiredJTIs(ctx, st)

	return serveListeners(ctx, srv, tlsConfig, listenAddrs{
		bind:    *bindAddr,
		healthz: *healthzAddr,
		socket:  cmp.Or(*socketFlag, paths.SocketPath(*dataDir)),
		oidc:    *oidcListen,
	})
}

// runServeTest implements `serve -t` after the configuration has loaded.
func runServeTest(cfg *serveConfig, bindAddr, oidcListen string) error {
	notAfter, err := testServeConfig(cfg, bindAddr)
	if err != nil {
		return err
	}
	fmt.Printf("deploy-agent: configuration test is successful (%d services, server certificate valid until %s)\n",
		len(cfg.manifest.Services), notAfter.Format(time.DateTime))
	printAccessWarnings(cfg.manifest, oidcListen)
	return nil
}

// listenAddrs are the addresses serve listens on; an empty oidc disables
// the OIDC listener.
type listenAddrs struct {
	bind    string
	healthz string
	socket  string
	oidc    string
}

// serveListeners runs every listener until ctx is done or one fails.
func serveListeners(ctx context.Context, srv *server.Server, tlsConfig *tls.Config, addrs listenAddrs) error {
	errCh := make(chan error, 4)
	go func() { errCh <- srv.MainListener(ctx, addrs.bind, tlsConfig) }()
	go func() { errCh <- srv.HealthzListener(ctx, addrs.healthz) }()
	go func() { errCh <- srv.LocalListener(ctx, addrs.socket) }()
	if addrs.oidc != "" {
		go func() { errCh <- srv.OIDCListener(ctx, addrs.oidc, tlsConfig) }()
	}

	slog.Info("deploy-agent serve started", "bind", addrs.bind, "healthz", addrs.healthz, "socket", addrs.socket,
		"oidc_listen", addrs.oidc, "version", version)

	select {
	case err := <-errCh:
		if err != nil {
			return fmt.Errorf("listener error: %w", err)
		}
	case <-ctx.Done():
	}
	return nil
}

// runReverseCalibration compares .env, last_healthy and docker's actual
// running state for every managed service, logging discrepancies without
// mutating any persisted state.
func runReverseCalibration(ctx context.Context, dep *deployer.Deployer, manifest *config.Manifest) {
	for name := range manifest.Services {
		status, err := dep.GetServiceStatus(ctx, name)
		if err != nil {
			slog.Warn("reverse calibration: read status failed", "service", name, "error", err)
			continue
		}
		if status.EnvTag != "" && status.Running.Tag != "" && status.EnvTag != status.Running.Tag {
			slog.Warn("reverse calibration: .env tag differs from running tag",
				"service", name, "env_tag", status.EnvTag, "running_tag", status.Running.Tag)
		}
		if status.LastHealthy != nil && status.Running.ImageID != "" && status.LastHealthy.ImageID != status.Running.ImageID {
			slog.Warn("reverse calibration: last_healthy image differs from running image",
				"service", name, "last_healthy_image_id", status.LastHealthy.ImageID, "running_image_id", status.Running.ImageID)
		}
	}
}

// checkExpiringCertsDaily re-runs logExpiringCerts every
// expiryCheckInterval until ctx is done.
func checkExpiringCertsDaily(ctx context.Context, st *store.Store) {
	ticker := time.NewTicker(expiryCheckInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			logExpiringCerts(st)
		}
	}
}

// logAccessWarnings logs every services.yaml access configuration under
// which some client certificate may deploy every service.
func logAccessWarnings(manifest *config.Manifest, oidcListen string) {
	for _, warning := range append(manifest.AccessWarnings(), oidcStartupWarnings(manifest, oidcListen)...) {
		slog.Warn("client certificate access is not restricted", "detail", warning)
	}
}

// printAccessWarnings is logAccessWarnings for `serve -t`.
func printAccessWarnings(manifest *config.Manifest, oidcListen string) {
	for _, warning := range append(manifest.AccessWarnings(), oidcStartupWarnings(manifest, oidcListen)...) {
		fmt.Fprintf(os.Stderr, "deploy-agent: warning: %s\n", warning)
	}
}

func logExpiringCerts(st *store.Store) {
	expiring, err := ca.ExpiringCerts(st)
	if err != nil {
		slog.Error("check expiring certificates failed", "error", err)
		return
	}
	for _, c := range expiring {
		slog.Warn("client certificate expiring soon", "fingerprint", c.Fingerprint, "name", c.Name, "expires_at", c.ExpiresAt)
	}
}
