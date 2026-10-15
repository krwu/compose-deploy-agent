package server

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/krwu/compose-deploy-agent/internal/api"
	"github.com/krwu/compose-deploy-agent/internal/config"
	"github.com/krwu/compose-deploy-agent/internal/oidc"
	"github.com/krwu/compose-deploy-agent/internal/oidc/oidctest"
	"github.com/krwu/compose-deploy-agent/internal/tlsconf"
)

const testAudience = "https://deploy.img.ntest.top"

// startOIDCListener serves the OIDC listener for a harness on its own port.
func startOIDCListener(t *testing.T, ctx context.Context, srv *Server, opts harnessOptions,
	tlsConfig *tls.Config) string {
	t.Helper()
	verifier, err := oidc.NewVerifier(testAudience, opts.oidcIssuer.HTTPClient(), nil)
	if err != nil {
		t.Fatalf("new verifier: %v", err)
	}
	srv.EnableOIDC(verifier)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen oidc: %v", err)
	}
	go func() { _ = srv.ServeOIDCOn(ctx, ln, tlsConfig) }()
	return ln.Addr().String()
}

// oidcManifest has "main" bound to pushes of refs/heads/main from
// repository 1001 (owner 2002) in environment preview-main, and "certonly"
// with no oidc rules.
func oidcManifest(t *testing.T) *config.Manifest {
	t.Helper()
	m := boundManifest(t)
	svc := m.Services["main"]
	svc.OIDC = []config.OIDCRule{{
		RepositoryID:      "1001",
		RepositoryOwnerID: "2002",
		Repository:        "kairee/img-webdn",
		Ref:               "refs/heads/main",
		EventName:         []config.OIDCEvent{config.OIDCEventPush},
		Environment:       "preview-main",
		JobWorkflowRef:    "kairee/img-webdn/.github/workflows/deploy.yml@refs/heads/main",
	}}
	m.Services["main"] = svc
	m.OIDC = &config.OIDCSettings{Audience: testAudience}
	return m
}

func startOIDCHarness(t *testing.T, dataDir string) (*testHarness, *oidctest.Issuer) {
	t.Helper()
	iss := oidctest.NewIssuer(t)
	h := startHarnessWith(t, harnessOptions{
		manifest:   oidcManifest(t),
		dataDir:    dataDir,
		oidcIssuer: iss,
	})
	return h, iss
}

// oidcClient trusts the agent's CA and presents no client certificate.
func (h *testHarness) oidcClient(t *testing.T) *http.Client {
	t.Helper()
	pool, err := tlsconf.LoadCAPool(h.mgr.CACertPath())
	if err != nil {
		t.Fatalf("load CA: %v", err)
	}
	return &http.Client{
		Transport: &http.Transport{TLSClientConfig: &tls.Config{
			RootCAs:    pool,
			ServerName: "127.0.0.1",
			MinVersion: tls.VersionTLS13,
		}},
		Timeout: 5 * time.Second,
	}
}

func oidcRequest(t *testing.T, client *http.Client, method, url, token string, body []byte) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, url, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	return resp
}

func oidcDeploy(t *testing.T, h *testHarness, token, service string) *http.Response {
	t.Helper()
	body, _ := json.Marshal(api.DeployRequest{
		Service: service,
		Tag:     "abc1234",
	})
	return oidcRequest(t, h.oidcClient(t), http.MethodPost, "https://"+h.oidcAddr+"/v1/deploy", token, body)
}

func expectStatus(t *testing.T, resp *http.Response, want int, wantCode api.ErrorCode) string {
	t.Helper()
	data, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != want {
		t.Fatalf("want %d, got %d: %s", want, resp.StatusCode, data)
	}
	if wantCode != "" {
		var e api.ErrorResponse
		err := json.Unmarshal(data, &e)
		if err != nil || e.Code != wantCode {
			t.Fatalf("want code %s, got %s", wantCode, data)
		}
	}
	return string(data)
}

func TestServer_OIDCDeployAndReplay(t *testing.T) {
	h, iss := startOIDCHarness(t, "")
	token := iss.Mint(t, oidctest.Claims(testAudience, time.Now()))

	body := expectStatus(t, oidcDeploy(t, h, token, "main"), http.StatusAccepted, "")
	var accepted api.DeployAccepted
	err := json.Unmarshal([]byte(body), &accepted)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}

	resp := oidcRequest(t, h.oidcClient(t), http.MethodGet, "https://"+h.oidcAddr+"/v1/deploys/"+accepted.DeployID,
		token, nil)
	status := expectStatus(t, resp, http.StatusOK, "")
	if strings.Contains(status, token) {
		t.Fatal("deploy record contains the token")
	}

	resp = oidcDeploy(t, h, token, "main")
	if resp.Header.Get("WWW-Authenticate") == "" {
		t.Fatal("401 must carry WWW-Authenticate")
	}
	expectStatus(t, resp, http.StatusUnauthorized, api.ErrUnauthenticated)
}

func TestServer_OIDCReplayAfterRestart(t *testing.T) {
	dataDir := t.TempDir()
	h1, iss := startOIDCHarness(t, dataDir)
	token := iss.Mint(t, oidctest.Claims(testAudience, time.Now()))
	expectStatus(t, oidcDeploy(t, h1, token, "main"), http.StatusAccepted, "")
	h1.cancel()
	h1.store.Close()

	h2 := startHarnessWith(t, harnessOptions{
		manifest:   oidcManifest(t),
		dataDir:    dataDir,
		oidcIssuer: iss,
	})
	expectStatus(t, oidcDeploy(t, h2, token, "main"), http.StatusUnauthorized, api.ErrUnauthenticated)
}

func TestServer_OIDCForgedTokenDoesNotRecordJTI(t *testing.T) {
	h, iss := startOIDCHarness(t, "")
	claims := oidctest.Claims(testAudience, time.Now())
	other, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	forged := oidctest.Sign(t, map[string]any{"alg": "RS256", "kid": iss.KID}, claims, other)
	expectStatus(t, oidcDeploy(t, h, forged, "main"), http.StatusUnauthorized, api.ErrUnauthenticated)

	wrongAud := oidctest.Claims(testAudience, time.Now())
	wrongAud["jti"] = claims["jti"]
	wrongAud["aud"] = "https://github.com/kairee"
	expectStatus(t, oidcDeploy(t, h, iss.Mint(t, wrongAud), "main"), http.StatusUnauthorized, api.ErrUnauthenticated)

	mismatch := oidctest.Claims(testAudience, time.Now())
	mismatch["jti"] = claims["jti"]
	mismatch["environment"] = "other"
	expectStatus(t, oidcDeploy(t, h, iss.Mint(t, mismatch), "main"), http.StatusForbidden, api.ErrAccessDenied)

	expectStatus(t, oidcDeploy(t, h, iss.Mint(t, claims), "main"), http.StatusAccepted, "")
}

func TestServer_OIDCBindingDenied(t *testing.T) {
	h, iss := startOIDCHarness(t, "")
	cases := map[string]func(c map[string]any){
		"repository_id differs, same name": func(c map[string]any) { c["repository_id"] = "9999" },
		"owner id differs":                 func(c map[string]any) { c["repository_owner_id"] = "9999" },
		"ref differs":                      func(c map[string]any) { c["ref"] = "refs/heads/dev" },
		"event not allowed":                func(c map[string]any) { c["event_name"] = "workflow_dispatch" },
		"pull_request_target":              func(c map[string]any) { c["event_name"] = "pull_request_target" },
		"environment differs":              func(c map[string]any) { c["environment"] = "production" },
		"job_workflow_ref differs": func(c map[string]any) {
			c["job_workflow_ref"] = "kairee/img-webdn/.github/workflows/evil.yml@refs/heads/main"
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			claims := oidctest.Claims(testAudience, time.Now())
			mutate(claims)
			body := expectStatus(t, oidcDeploy(t, h, iss.Mint(t, claims), "main"), http.StatusForbidden,
				api.ErrAccessDenied)
			if strings.Contains(body, "repository") || strings.Contains(body, "environment") {
				t.Fatalf("response leaks validation detail: %s", body)
			}
		})
	}
	t.Run("service without oidc rules", func(t *testing.T) {
		token := iss.Mint(t, oidctest.Claims(testAudience, time.Now()))
		expectStatus(t, oidcDeploy(t, h, token, "pr-1"), http.StatusForbidden, api.ErrAccessDenied)
		resp := oidcRequest(t, h.oidcClient(t), http.MethodGet, "https://"+h.oidcAddr+"/v1/status/pr-1", token, nil)
		expectStatus(t, resp, http.StatusForbidden, api.ErrAccessDenied)
	})
}

func TestServer_OIDCEndpointRequiresToken(t *testing.T) {
	h, _ := startOIDCHarness(t, "")
	expectStatus(t, oidcDeploy(t, h, "", "main"), http.StatusUnauthorized, api.ErrUnauthenticated)
	resp := oidcRequest(t, h.oidcClient(t), http.MethodGet, "https://"+h.oidcAddr+"/v1/status/main", "", nil)
	expectStatus(t, resp, http.StatusUnauthorized, api.ErrUnauthenticated)
	expectStatus(t, oidcDeploy(t, h, "not-a-jwt", "main"), http.StatusUnauthorized, api.ErrUnauthenticated)

	// A client certificate does not substitute for a token here.
	bundle := h.issueClient(t, "ci-main")
	cfg := bundle.TLSConfig("127.0.0.1")
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: cfg}, Timeout: 5 * time.Second}
	body, _ := json.Marshal(api.DeployRequest{
		Service: "main",
		Tag:     "abc1234",
	})
	resp = oidcRequest(t, client, http.MethodPost, "https://"+h.oidcAddr+"/v1/deploy", "", body)
	expectStatus(t, resp, http.StatusUnauthorized, api.ErrUnauthenticated)
}

func TestServer_CertEndpointRejectsBearerWithoutCert(t *testing.T) {
	h, iss := startOIDCHarness(t, "")
	token := iss.Mint(t, oidctest.Claims(testAudience, time.Now()))
	body, _ := json.Marshal(api.DeployRequest{
		Service: "main",
		Tag:     "abc1234",
	})
	req, err := http.NewRequest(http.MethodPost, "https://"+h.addr+"/v1/deploy", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := h.oidcClient(t).Do(req)
	if err == nil {
		resp.Body.Close()
		t.Fatalf("mTLS listener accepted a request without a client certificate: %d", resp.StatusCode)
	}
}

// syncBuffer is a goroutine-safe log sink.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestServer_OIDCTokenNeverLoggedOrEchoed(t *testing.T) {
	logs := &syncBuffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	h, iss := startOIDCHarness(t, "")
	valid := iss.Mint(t, oidctest.Claims(testAudience, time.Now()))
	expired := oidctest.Claims(testAudience, time.Now().Add(-time.Hour))
	denied := oidctest.Claims(testAudience, time.Now())
	denied["repository_id"] = "9999"
	tokens := []string{valid, valid, iss.Mint(t, expired), iss.Mint(t, denied), valid + "x"}

	var bodies strings.Builder
	for _, token := range tokens {
		resp := oidcDeploy(t, h, token, "main")
		data, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		bodies.Write(data)
	}
	for _, token := range tokens {
		sig := token[strings.LastIndex(token, ".")+1:]
		if strings.Contains(logs.String(), sig) || strings.Contains(bodies.String(), sig) {
			t.Fatal("token signature found in logs or response bodies")
		}
		if strings.Contains(logs.String(), token) || strings.Contains(bodies.String(), token) {
			t.Fatal("token found in logs or response bodies")
		}
	}
	if strings.Contains(logs.String(), "Bearer") || strings.Contains(logs.String(), "Authorization") {
		t.Fatal("Authorization header found in logs")
	}
	if !strings.Contains(logs.String(), "OIDC token replay rejected") {
		t.Fatalf("expected the replay to be logged, logs:\n%s", logs.String())
	}
}
