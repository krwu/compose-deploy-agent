package client

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/krwu/compose-deploy-agent/internal/api"
)

const fakeToken = "eyJhbGciOiJSUzI1NiJ9.eyJleHAiOjB9.c2VjcmV0LXNpZ25hdHVyZQ"

// fakeAgent accepts a deploy and reports success, requiring the bearer token.
func fakeAgent(t *testing.T, token string) *httptest.Server {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			w.WriteHeader(http.StatusUnauthorized)
			_ = json.NewEncoder(w).Encode(api.ErrorResponse{
				Code:    api.ErrUnauthenticated,
				Message: "authentication required",
			})
			return
		}
		if r.Method == http.MethodPost {
			w.WriteHeader(http.StatusAccepted)
			_ = json.NewEncoder(w).Encode(api.DeployAccepted{DeployID: "d1", State: api.StateDeploying})
			return
		}
		_ = json.NewEncoder(w).Encode(api.DeployStatus{
			DeployID: "d1",
			State:    api.StateSucceeded,
			Result:   api.ResultSuccess,
		})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func certPin(srv *httptest.Server) string {
	sum := sha256.Sum256(srv.Certificate().Raw)
	return hex.EncodeToString(sum[:])
}

func caPEM(srv *httptest.Server) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
}

func runOIDC(srv *httptest.Server, token string, pin ServerPin) Summary {
	return RunOIDC(context.Background(), Options{
		Endpoint:       srv.URL,
		Service:        "main",
		Tag:            "abc1234",
		Token:          StaticToken(token),
		PollInterval:   10 * time.Millisecond,
		PollTimeout:    5 * time.Second,
		ConnectBackoff: time.Millisecond,
	}, pin)
}

func Test_RunOIDCServerVerification(t *testing.T) {
	srv := fakeAgent(t, fakeToken)
	wrongPin := strings.Repeat("ab", 32)
	cases := map[string]struct {
		pin  ServerPin
		want ExitCode
	}{
		"pin only":           {ServerPin{CertSHA256: certPin(srv)}, ExitSuccess},
		"pin with colons":    {ServerPin{CertSHA256: colonize(certPin(srv))}, ExitSuccess},
		"ca only":            {ServerPin{CAPEM: caPEM(srv)}, ExitSuccess},
		"ca and pin":         {ServerPin{CAPEM: caPEM(srv), CertSHA256: certPin(srv)}, ExitSuccess},
		"wrong pin":          {ServerPin{CertSHA256: wrongPin}, ExitRequestFailed},
		"right ca wrong pin": {ServerPin{CAPEM: caPEM(srv), CertSHA256: wrongPin}, ExitRequestFailed},
		"neither":            {ServerPin{}, ExitRequestFailed},
		"malformed pin":      {ServerPin{CertSHA256: "abc"}, ExitRequestFailed},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			summary := runOIDC(srv, fakeToken, tc.pin)
			if summary.ExitCode != tc.want {
				t.Fatalf("want exit %d, got %+v", tc.want, summary)
			}
		})
	}
}

func Test_RunOIDCUntrustedServerRefused(t *testing.T) {
	srv := fakeAgent(t, fakeToken)
	summary := runOIDC(srv, fakeToken, ServerPin{CAPEM: selfSignedCA(t)})
	if summary.ExitCode != ExitRequestFailed {
		t.Fatalf("a server outside the pinned CA must be refused, got %+v", summary)
	}
}

func Test_RunOIDCUnauthorizedDoesNotLeakToken(t *testing.T) {
	srv := fakeAgent(t, "another-token")
	summary := runOIDC(srv, fakeToken, ServerPin{CertSHA256: certPin(srv)})
	if summary.ExitCode != ExitRequestFailed {
		t.Fatalf("401 must be a request failure, got %+v", summary)
	}
	out, _ := json.Marshal(summary)
	if strings.Contains(string(out), fakeToken) || strings.Contains(string(out), "c2VjcmV0") {
		t.Fatalf("summary leaks the token: %s", out)
	}
}

// selfSignedCA returns a PEM CA certificate unrelated to httptest's.
func selfSignedCA(t *testing.T) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "other CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func colonize(h string) string {
	parts := make([]string, 0, len(h)/2)
	for i := 0; i < len(h); i += 2 {
		parts = append(parts, strings.ToUpper(h[i:i+2]))
	}
	return strings.Join(parts, ":")
}

func Test_ActionsTokenRequestsAudienceAndCaches(t *testing.T) {
	var hits atomic.Int64
	exp := time.Now().Add(5 * time.Minute).Unix()
	payload := base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(`{"exp":%d}`, exp)))
	idToken := "h." + payload + ".s"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.Header.Get("Authorization") != "Bearer request-token" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		if r.URL.Query().Get("audience") != "https://deploy.img.ntest.top" || r.URL.Query().Get("api-version") != "2.0" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"value": idToken})
	}))
	defer srv.Close()

	src := ActionsToken(srv.URL+"/token?api-version=2.0", "request-token", "https://deploy.img.ntest.top")
	for range 3 {
		got, err := src(context.Background())
		if err != nil {
			t.Fatalf("token: %v", err)
		}
		if got != idToken {
			t.Fatalf("got %q", got)
		}
	}
	if hits.Load() != 1 {
		t.Fatalf("token must be cached until near exp, got %d requests", hits.Load())
	}

	bad := ActionsToken(srv.URL+"/token?api-version=2.0", "wrong-request-token", "https://deploy.img.ntest.top")
	_, err := bad(context.Background())
	if err == nil || strings.Contains(err.Error(), "wrong-request-token") {
		t.Fatalf("want an error without the request token, got %v", err)
	}
}
