package main

import (
	"flag"
	"strings"
	"testing"

	"github.com/krwu/compose-deploy-agent/internal/config"
)

func parseOIDCFlags(t *testing.T, args ...string) oidcFlags {
	t.Helper()
	fs := flag.NewFlagSet("deploy", flag.ContinueOnError)
	f := registerOIDCFlags(fs)
	err := fs.Parse(args)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return f
}

func Test_oidcFlagsTokenSource(t *testing.T) {
	t.Setenv(actionsTokenURLEnv, "")
	t.Setenv(actionsTokenTokenEnv, "")
	t.Setenv("MY_TOKEN", "")

	_, err := parseOIDCFlags(t, "--oidc-audience", "a", "--oidc-token-env", "MY_TOKEN").tokenSource()
	if err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("want mutually exclusive error, got %v", err)
	}
	_, err = parseOIDCFlags(t, "--oidc-audience", "a").tokenSource()
	if err == nil || !strings.Contains(err.Error(), "id-token: write") {
		t.Fatalf("want missing runner env error, got %v", err)
	}
	_, err = parseOIDCFlags(t, "--oidc-token-env", "MY_TOKEN").tokenSource()
	if err == nil {
		t.Fatal("empty token env must fail")
	}
	t.Setenv("MY_TOKEN", "tok")
	_, err = parseOIDCFlags(t, "--oidc-token-env", "MY_TOKEN").tokenSource()
	if err != nil {
		t.Fatalf("token env: %v", err)
	}
}

func Test_oidcFlagsRequireServerPin(t *testing.T) {
	_, err := parseOIDCFlags(t, "--oidc-audience", "a").serverPin()
	if err == nil {
		t.Fatal("OIDC mode without CA file or pin must fail")
	}
	_, err = parseOIDCFlags(t, "--server-cert-sha256", strings.Repeat("ab", 32)).serverPin()
	if err != nil {
		t.Fatalf("pin only: %v", err)
	}
}

func Test_checkOIDCListen(t *testing.T) {
	m := &config.Manifest{}
	err := checkOIDCListen(m, "")
	if err != nil {
		t.Fatalf("disabled: %v", err)
	}
	err = checkOIDCListen(m, "127.0.0.1:8868")
	if err == nil || !strings.Contains(err.Error(), "oidc.audience") {
		t.Fatalf("listener without audience must fail, got %v", err)
	}
	m.OIDC = &config.OIDCSettings{Audience: "https://deploy.img.ntest.top"}
	err = checkOIDCListen(m, "127.0.0.1:8868")
	if err != nil {
		t.Fatalf("valid: %v", err)
	}
	if len(oidcStartupWarnings(m, "")) != 1 {
		t.Fatal("audience without listener should warn")
	}
}
