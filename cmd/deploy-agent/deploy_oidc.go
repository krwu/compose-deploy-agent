package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"

	"github.com/krwu/compose-deploy-agent/internal/client"
)

// Environment variables the GitHub Actions runner sets for jobs granted
// `permissions: id-token: write`.
const (
	actionsTokenURLEnv   = "ACTIONS_ID_TOKEN_REQUEST_URL"
	actionsTokenTokenEnv = "ACTIONS_ID_TOKEN_REQUEST_TOKEN"
)

// oidcFlags are the `deploy` flags for the OIDC listener.
type oidcFlags struct {
	audience   *string
	tokenEnv   *string
	caFile     *string
	certSHA256 *string
}

func registerOIDCFlags(fs *flag.FlagSet) oidcFlags {
	return oidcFlags{
		audience: fs.String("oidc-audience", "", "request a GitHub Actions OIDC token for this audience "+
			"(needs permissions: id-token: write) and deploy through the agent's OIDC listener"),
		tokenEnv: fs.String("oidc-token-env", "", "environment variable holding an OIDC token to send instead "+
			"of requesting one"),
		caFile: fs.String("server-ca-file", "", "OIDC mode: PEM CA file the server certificate must chain to"),
		certSHA256: fs.String("server-cert-sha256", "", "OIDC mode: SHA-256 of the server certificate (DER), "+
			"hex; at least one of --server-ca-file and --server-cert-sha256 is required"),
	}
}

func (f oidcFlags) enabled() bool {
	return *f.audience != "" || *f.tokenEnv != ""
}

// tokenSource resolves the OIDC token source from the flags and environment.
func (f oidcFlags) tokenSource() (client.TokenSource, error) {
	if *f.audience != "" && *f.tokenEnv != "" {
		return nil, errors.New("--oidc-audience and --oidc-token-env are mutually exclusive")
	}
	if *f.tokenEnv != "" {
		token := os.Getenv(*f.tokenEnv)
		if token == "" {
			return nil, fmt.Errorf("environment variable %s is empty", *f.tokenEnv)
		}
		return client.StaticToken(token), nil
	}
	requestURL := os.Getenv(actionsTokenURLEnv)
	requestToken := os.Getenv(actionsTokenTokenEnv)
	if requestURL == "" || requestToken == "" {
		return nil, fmt.Errorf("%s / %s are not set; grant the job `permissions: id-token: write`",
			actionsTokenURLEnv, actionsTokenTokenEnv)
	}
	return client.ActionsToken(requestURL, requestToken, *f.audience), nil
}

// serverPin reads the server certificate pinning flags.
func (f oidcFlags) serverPin() (client.ServerPin, error) {
	pin := client.ServerPin{CertSHA256: *f.certSHA256}
	if *f.caFile != "" {
		data, err := os.ReadFile(*f.caFile)
		if err != nil {
			return pin, fmt.Errorf("read --server-ca-file: %w", err)
		}
		pin.CAPEM = data
	}
	if len(pin.CAPEM) == 0 && pin.CertSHA256 == "" {
		return pin, errors.New("OIDC mode requires --server-ca-file and/or --server-cert-sha256")
	}
	return pin, nil
}

func runDeployOIDC(opts client.Options, f oidcFlags) error {
	token, err := f.tokenSource()
	if err != nil {
		return err
	}
	pin, err := f.serverPin()
	if err != nil {
		return err
	}
	opts.Token = token
	return emitSummary(client.RunOIDC(context.Background(), opts, pin))
}
