package client

import (
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ServerPin says how the client authenticates the OIDC listener's server
// certificate. At least one of CAPEM and CertSHA256 must be set; server
// certificate verification is never disabled.
type ServerPin struct {
	// CAPEM trusts only these CA certificates (e.g. the agent's ca.crt);
	// the chain and the endpoint host name are verified as usual.
	CAPEM []byte
	// CertSHA256 is the SHA-256 of the server leaf certificate (DER), hex
	// with or without colons. Alone, it replaces chain and host name
	// verification with an exact match on the leaf (plus its validity
	// period); together with CAPEM both checks must pass.
	CertSHA256 string
}

// tlsConfig builds the client TLS config for serverName.
func (p ServerPin) tlsConfig(serverName string) (*tls.Config, error) {
	if len(p.CAPEM) == 0 && p.CertSHA256 == "" {
		return nil, errors.New("server certificate pinning required: set a server CA file and/or a certificate SHA-256")
	}
	cfg := &tls.Config{
		ServerName: serverName,
		MinVersion: tls.VersionTLS13,
	}
	if len(p.CAPEM) > 0 {
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(p.CAPEM) {
			return nil, errors.New("server CA file contains no PEM certificates")
		}
		cfg.RootCAs = pool
	}
	if p.CertSHA256 == "" {
		return cfg, nil
	}
	want, err := parsePin(p.CertSHA256)
	if err != nil {
		return nil, err
	}
	// Without a CA the leaf is self-authenticating through the pin, so the
	// default chain/host name check is replaced by VerifyConnection below,
	// which always runs and fails closed.
	cfg.InsecureSkipVerify = len(p.CAPEM) == 0
	cfg.VerifyConnection = func(cs tls.ConnectionState) error {
		return checkPin(cs, want, time.Now())
	}
	return cfg, nil
}

func parsePin(pin string) ([]byte, error) {
	raw, err := hex.DecodeString(strings.ReplaceAll(strings.TrimSpace(pin), ":", ""))
	if err != nil || len(raw) != sha256.Size {
		return nil, errors.New("server certificate SHA-256 must be 64 hex digits (colons allowed)")
	}
	return raw, nil
}

func checkPin(cs tls.ConnectionState, want []byte, now time.Time) error {
	if len(cs.PeerCertificates) == 0 {
		return errors.New("server presented no certificate")
	}
	leaf := cs.PeerCertificates[0]
	got := sha256.Sum256(leaf.Raw)
	if subtle.ConstantTimeCompare(got[:], want) != 1 {
		return fmt.Errorf("server certificate SHA-256 %x does not match the pinned value", got)
	}
	if now.Before(leaf.NotBefore) || now.After(leaf.NotAfter) {
		return errors.New("pinned server certificate is not valid now")
	}
	return nil
}

// newPinnedHTTPClient builds the HTTP client for the OIDC listener: no
// client certificate, server authenticated through pin.
func newPinnedHTTPClient(pin ServerPin, endpoint string) (*http.Client, error) {
	u, err := url.Parse(endpoint)
	if err != nil {
		return nil, err
	}
	cfg, err := pin.tlsConfig(u.Hostname())
	if err != nil {
		return nil, err
	}
	return &http.Client{Transport: &http.Transport{TLSClientConfig: cfg}}, nil
}
