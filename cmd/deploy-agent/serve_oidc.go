package main

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"time"

	"github.com/krwu/compose-deploy-agent/internal/config"
	"github.com/krwu/compose-deploy-agent/internal/oidc"
	"github.com/krwu/compose-deploy-agent/internal/server"
	"github.com/krwu/compose-deploy-agent/internal/store"
)

// jtiPurgeInterval is how often expired OIDC token ids are deleted.
const jtiPurgeInterval = 10 * time.Minute

// checkOIDCListen validates --oidc-listen against the manifest: the OIDC
// listener needs an agent-specific oidc.audience, and is off when the flag
// is empty.
func checkOIDCListen(manifest *config.Manifest, oidcListen string) error {
	if oidcListen == "" {
		return nil
	}
	_, _, err := net.SplitHostPort(oidcListen)
	if err != nil {
		return fmt.Errorf("invalid --oidc-listen %q: %w", oidcListen, err)
	}
	if manifest.OIDCAudience() == "" {
		return fmt.Errorf("--oidc-listen requires oidc.audience in the manifest")
	}
	return nil
}

// oidcStartupWarnings describes OIDC configuration that will not take
// effect as written.
func oidcStartupWarnings(manifest *config.Manifest, oidcListen string) []string {
	if oidcListen == "" && manifest.OIDCAudience() != "" {
		return []string{"oidc.audience is set but --oidc-listen is not: OIDC authentication is disabled"}
	}
	return nil
}

// enableOIDC installs the production verifier (fixed issuer and JWKS URL)
// when the OIDC listener is configured.
func enableOIDC(srv *server.Server, manifest *config.Manifest, oidcListen string) error {
	if oidcListen == "" {
		return nil
	}
	verifier, err := oidc.NewVerifier(manifest.OIDCAudience(), nil, nil)
	if err != nil {
		return err
	}
	srv.EnableOIDC(verifier)
	return nil
}

// purgeExpiredJTIs deletes expired OIDC token ids now and then every
// jtiPurgeInterval until ctx is done.
func purgeExpiredJTIs(ctx context.Context, st *store.Store) {
	ticker := time.NewTicker(jtiPurgeInterval)
	defer ticker.Stop()
	for {
		n, err := st.PurgeExpiredJTI(time.Now())
		if err != nil {
			slog.Error("purge expired OIDC jti failed", "error", err)
		} else if n > 0 {
			slog.Info("purged expired OIDC jti records", "count", n)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
