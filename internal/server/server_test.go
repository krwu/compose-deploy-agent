package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/krwu/compose-deploy-agent/internal/api"
	"github.com/krwu/compose-deploy-agent/internal/ca"
	"github.com/krwu/compose-deploy-agent/internal/config"
	"github.com/krwu/compose-deploy-agent/internal/credential"
	"github.com/krwu/compose-deploy-agent/internal/deployer"
	"github.com/krwu/compose-deploy-agent/internal/dockerx"
	"github.com/krwu/compose-deploy-agent/internal/oidc/oidctest"
	"github.com/krwu/compose-deploy-agent/internal/store"
	"github.com/krwu/compose-deploy-agent/internal/tlsconf"
)

// testHarness spins up a real mTLS listener backed by a real local CA, so
// this test exercises the full authentication stack end to end: TLS
// handshake + chain verification (internal/tlsconf), fingerprint
// registration/revocation (internal/server middleware), and the deploy
// allowlist (internal/deployer).
type testHarness struct {
	addr     string
	oidcAddr string
	mgr      *ca.Manager
	store    *store.Store
	cancel   context.CancelFunc
	errCh    chan error
}

// harnessOptions customizes startHarnessWith. Zero values keep the
// defaults used by startHarness.
type harnessOptions struct {
	manifest *config.Manifest
	timeouts *serverTimeouts
	maxConns int
	// dataDir reuses an existing data directory (restart tests).
	dataDir string
	// oidcIssuer, if set, also starts the OIDC listener trusting it.
	oidcIssuer *oidctest.Issuer
}

func startHarness(t *testing.T) *testHarness {
	t.Helper()
	return startHarnessWith(t, harnessOptions{})
}

func startHarnessWith(t *testing.T, opts harnessOptions) *testHarness {
	t.Helper()

	dataDir := opts.dataDir
	if dataDir == "" {
		dataDir = t.TempDir()
	}
	mgr := ca.NewManager(dataDir)
	if !mgr.Initialized() {
		err := mgr.Init([]string{"127.0.0.1"})
		if err != nil {
			t.Fatalf("ca init: %v", err)
		}
	}

	st, err := store.Open(filepath.Join(dataDir, "agent.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	manifest := &config.Manifest{Version: 1, Services: map[string]config.Service{
		"svc": {
			Dir:            t.TempDir(),
			File:           "compose.yml",
			ComposeService: "app",
			Image:          "example.com/app",
			TagEnv:         "IMAGE_TAG",
		},
	}}
	if opts.manifest != nil {
		manifest = opts.manifest
	}
	dep := deployer.New(manifest, dockerx.NewClient(), st)
	srv := New(dep, st, "test")
	if opts.timeouts != nil {
		srv.timeouts = *opts.timeouts
	}
	srv.SetMaxConnections(opts.maxConns)

	serverCert, err := tlsconf.LoadServerCertificate(mgr.ServerCertPath(), mgr.ServerKeyPath())
	if err != nil {
		t.Fatalf("load server cert: %v", err)
	}
	clientCAPool, err := tlsconf.LoadCAPool(mgr.CACertPath())
	if err != nil {
		t.Fatalf("load CA pool: %v", err)
	}
	tlsConfig := tlsconf.ServerConfig(serverCert, clientCAPool)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- srv.ServeMainOn(ctx, ln, tlsConfig) }()
	t.Cleanup(cancel)

	h := &testHarness{
		addr:   addr,
		mgr:    mgr,
		store:  st,
		cancel: cancel,
		errCh:  errCh,
	}
	if opts.oidcIssuer != nil {
		h.oidcAddr = startOIDCListener(t, ctx, srv, opts, tlsConfig)
	}
	return h
}

func (h *testHarness) issueClient(t *testing.T, name string) credential.Bundle {
	t.Helper()
	pemBundle, err := h.mgr.IssueClient(name, h.store)
	if err != nil {
		t.Fatalf("issue client: %v", err)
	}
	bundle, err := credential.Parse([]byte(pemBundle))
	if err != nil {
		t.Fatalf("parse issued bundle: %v", err)
	}
	return bundle
}

func (h *testHarness) httpClient(bundle credential.Bundle) *http.Client {
	return &http.Client{
		Transport: &http.Transport{TLSClientConfig: bundle.TLSConfig("127.0.0.1")},
		Timeout:   5 * time.Second,
	}
}

func TestServer_RegisteredClientCanDeploy(t *testing.T) {
	h := startHarness(t)
	bundle := h.issueClient(t, "ci-lss-web")
	client := h.httpClient(bundle)

	body, _ := json.Marshal(api.DeployRequest{
		Service: "svc",
		Tag:     "abc1234",
	})
	resp, err := client.Post("https://"+h.addr+"/v1/deploy", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST /v1/deploy: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("expected 202, got %d", resp.StatusCode)
	}

	var accepted api.DeployAccepted
	if err := json.NewDecoder(resp.Body).Decode(&accepted); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if accepted.Service != "svc" || accepted.Tag != "abc1234" || accepted.DeployID == "" {
		t.Fatalf("unexpected accepted response: %+v", accepted)
	}
}

func TestServer_UnregisteredCertRejected(t *testing.T) {
	h := startHarness(t)

	// A certificate signed by a *different* CA must fail the TLS
	// handshake itself (chain verification), before even reaching the
	// fingerprint middleware.
	otherDataDir := t.TempDir()
	otherMgr := ca.NewManager(otherDataDir)
	if err := otherMgr.Init([]string{"127.0.0.1"}); err != nil {
		t.Fatalf("init other CA: %v", err)
	}
	otherStore, err := store.Open(filepath.Join(otherDataDir, "agent.db"))
	if err != nil {
		t.Fatalf("open other store: %v", err)
	}
	defer otherStore.Close()
	pemBundle, err := otherMgr.IssueClient("intruder", otherStore)
	if err != nil {
		t.Fatalf("issue client from other CA: %v", err)
	}
	// Splice: use the intruder's cert+key, but the *target* server's CA
	// (as a real attacker would have to, since they can't know it). This
	// isolates the "not registered" path (issued by right CA, but not
	// this agent's registration table) - see next test for that. Here we
	// specifically test a cert from the wrong CA entirely fails handshake.
	bundle, err := credential.Parse([]byte(pemBundle))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	// Point RootCAs at the *target* server's CA so the client trusts the
	// right server, while presenting a client cert the target's CA never
	// signed.
	targetCAPool, err := tlsconf.LoadCAPool(h.mgr.CACertPath())
	if err != nil {
		t.Fatalf("load target CA pool: %v", err)
	}
	bundle.ServerCA = targetCAPool

	client := h.httpClient(bundle)
	body, _ := json.Marshal(api.DeployRequest{
		Service: "svc",
		Tag:     "abc1234",
	})
	_, err = client.Post("https://"+h.addr+"/v1/deploy", "application/json", bytes.NewReader(body))
	if err == nil {
		t.Fatalf("expected TLS handshake failure for a client certificate signed by a different CA")
	}
}

func TestServer_RevokedCertRejected(t *testing.T) {
	h := startHarness(t)
	bundle := h.issueClient(t, "ci-lss-web")

	certs, err := ca.List(h.store)
	if err != nil {
		t.Fatalf("list certs: %v", err)
	}
	if len(certs) != 1 {
		t.Fatalf("expected 1 cert, got %d", len(certs))
	}
	if err := ca.Revoke(h.store, certs[0].Fingerprint); err != nil {
		t.Fatalf("revoke: %v", err)
	}

	client := h.httpClient(bundle)
	body, _ := json.Marshal(api.DeployRequest{
		Service: "svc",
		Tag:     "abc1234",
	})
	resp, err := client.Post("https://"+h.addr+"/v1/deploy", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST /v1/deploy: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 for revoked cert, got %d", resp.StatusCode)
	}
	var errResp api.ErrorResponse
	if err := json.NewDecoder(resp.Body).Decode(&errResp); err != nil {
		t.Fatalf("decode error response: %v", err)
	}
	if errResp.Code != api.ErrCertRevoked {
		t.Fatalf("expected code %s, got %s", api.ErrCertRevoked, errResp.Code)
	}
}

// TestServer_PlainHTTPVersionUnauthenticated exercises the protoDemux
// path: a plain `curl http://...`
// (no TLS at all) against the same :8866 port must succeed for
// GET /v1/version, without ever presenting a client certificate.
func TestServer_PlainHTTPVersionUnauthenticated(t *testing.T) {
	h := startHarness(t)

	resp, err := http.Get("http://" + h.addr + "/v1/version")
	if err != nil {
		t.Fatalf("GET /v1/version over plain HTTP: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Type"); got != "text/plain; charset=utf-8" {
		t.Fatalf("expected Content-Type %q, got %q", "text/plain; charset=utf-8", got)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	if string(body) != "test\n" {
		t.Fatalf("expected version %q, got %q", "test\n", body)
	}
}

// TestServer_PlainHTTPCannotReachDeploy ensures the plain-HTTP sub-stream
// exposes nothing but /v1/version: a deploy request sent without TLS must
// not reach the deployer, regardless of certificate status.
func TestServer_PlainHTTPCannotReachDeploy(t *testing.T) {
	h := startHarness(t)

	body, _ := json.Marshal(api.DeployRequest{
		Service: "svc",
		Tag:     "abc1234",
	})
	resp, err := http.Post("http://"+h.addr+"/v1/deploy", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST /v1/deploy over plain HTTP: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusAccepted {
		t.Fatalf("plain HTTP must never be able to submit a deploy, got 202")
	}
}

func TestServer_UnknownServiceReturns404(t *testing.T) {
	h := startHarness(t)
	bundle := h.issueClient(t, "ci-lss-web")
	client := h.httpClient(bundle)

	body, _ := json.Marshal(api.DeployRequest{
		Service: "does-not-exist",
		Tag:     "abc1234",
	})
	resp, err := client.Post("https://"+h.addr+"/v1/deploy", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST /v1/deploy: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 for unmanaged service, got %d", resp.StatusCode)
	}
}

// TestServer_LocalSocketDeployWithoutCert exercises the unix socket
// listener used by `deploy-agent update`: no
// client certificate is presented at all, and the request must still be
// accepted, since the trust root for this channel is host access to the
// socket file rather than mTLS.
func TestServer_LocalSocketDeployWithoutCert(t *testing.T) {
	manifest := &config.Manifest{Version: 1, Services: map[string]config.Service{
		"svc": {
			Dir:            t.TempDir(),
			File:           "compose.yml",
			ComposeService: "app",
			Image:          "example.com/app",
			TagEnv:         "IMAGE_TAG",
		},
	}}
	st, err := store.Open(filepath.Join(t.TempDir(), "agent.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()
	dep := deployer.New(manifest, dockerx.NewClient(), st)
	srv := New(dep, st, "test")

	socketPath := filepath.Join(privateTempDir(t), "agent.sock")
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	errCh := make(chan error, 1)
	go func() { errCh <- srv.LocalListener(ctx, socketPath) }()

	// Wait for the socket file to appear (LocalListener creates it
	// synchronously before returning from net.Listen, but the goroutine
	// above may not have run yet).
	deadline := time.Now().Add(2 * time.Second)
	for {
		if info, statErr := os.Stat(socketPath); statErr == nil {
			if info.Mode().Perm() != 0o600 {
				t.Fatalf("expected socket mode 0600, got %v", info.Mode().Perm())
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("socket file %s never appeared", socketPath)
		}
		time.Sleep(10 * time.Millisecond)
	}

	httpClient := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", socketPath)
			},
		},
		Timeout: 5 * time.Second,
	}

	body, _ := json.Marshal(api.DeployRequest{
		Service: "svc",
		Tag:     "abc1234",
	})
	resp, err := httpClient.Post("http://deploy-agent/v1/deploy", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST /v1/deploy over unix socket: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("expected 202, got %d", resp.StatusCode)
	}

	var accepted api.DeployAccepted
	if err := json.NewDecoder(resp.Body).Decode(&accepted); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if accepted.Service != "svc" || accepted.Tag != "abc1234" || accepted.DeployID == "" {
		t.Fatalf("unexpected accepted response: %+v", accepted)
	}
}

func TestServer_Healthz(t *testing.T) {
	manifest := &config.Manifest{
		Version:  1,
		Services: map[string]config.Service{},
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "agent.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()
	dep := deployer.New(manifest, dockerx.NewClient(), st)
	srv := New(dep, st, "1.2.3")

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	srv.healthzRouter().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	var health api.HealthzResponse
	if err := json.NewDecoder(rec.Body).Decode(&health); err != nil {
		t.Fatalf("decode healthz response: %v", err)
	}
	if health.Status != "ok" || health.Version != "1.2.3" {
		t.Fatalf("unexpected healthz response: %+v", health)
	}
}
