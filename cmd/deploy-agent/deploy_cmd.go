package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/krwu/compose-deploy-agent/internal/client"
)

// defaultCredentialEnv is the environment variable the pipeline script
// task is expected to populate with the multi-line PEM bundle.
const defaultCredentialEnv = "DEPLOY_AGENT_CREDENTIAL"

func runDeploy(args []string) error {
	fs := flag.NewFlagSet("deploy", flag.ContinueOnError)
	endpoint := stringFlag(fs, "endpoint", "e", "", "agent endpoint, e.g. https://10.0.0.5:8866")
	service := stringFlag(fs, "service", "s", "", "logical service name (must match services.yaml)")
	tag := stringFlag(fs, "tag", "t", "", "target image tag")
	digest := stringFlag(fs, "digest", "d", "", "target image digest sha256:<64 hex> (services with image_ref: digest)")
	credentialEnv := fs.String("credential-env", defaultCredentialEnv, "environment variable holding the PEM credential bundle")
	pollTimeout := fs.Duration("poll-timeout", client.DefaultPollTimeout, "total time budget for submit retries and status polling")
	oidcFlags := registerOIDCFlags(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *endpoint == "" || *service == "" {
		return fmt.Errorf("--endpoint and --service are required")
	}
	err := requireOneRef(*tag, *digest)
	if err != nil {
		return err
	}
	opts := client.Options{
		Endpoint:    *endpoint,
		Service:     *service,
		Tag:         *tag,
		Digest:      *digest,
		PollTimeout: *pollTimeout,
	}
	if oidcFlags.enabled() {
		return runDeployOIDC(opts, oidcFlags)
	}

	credentialPEM := os.Getenv(*credentialEnv)
	if credentialPEM == "" {
		return fmt.Errorf("environment variable %s is empty; the pipeline must inject the credential bundle before calling `deploy-agent deploy`", *credentialEnv)
	}
	opts.CredentialPEM = []byte(credentialPEM)
	return emitSummary(client.Run(context.Background(), opts))
}

// requireOneRef checks that exactly one of --tag and --digest was given.
func requireOneRef(tag, digest string) error {
	if (tag == "") == (digest == "") {
		return fmt.Errorf("exactly one of --tag or --digest is required")
	}
	return nil
}

// emitSummary prints summary as indented JSON to stdout and, if the deploy
// did not succeed, exits the process with the summary's exit code. Shared
// by `deploy-agent deploy` and `deploy-agent
// update`, whose outcomes must be reported identically.
func emitSummary(summary client.Summary) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(summary)

	if summary.ExitCode != client.ExitSuccess {
		os.Exit(int(summary.ExitCode))
	}
	return nil
}
