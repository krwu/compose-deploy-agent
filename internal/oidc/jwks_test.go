package oidc

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"fmt"
	"math/big"
	"testing"
)

func Test_parseJWKSSkipsWeakAndForeignKeys(t *testing.T) {
	weak, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	strong, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	enc := base64.RawURLEncoding.EncodeToString
	e := enc(big.NewInt(65537).Bytes())
	doc := fmt.Sprintf(`{"keys":[
		{"kty":"RSA","kid":"weak","n":%q,"e":%q},
		{"kty":"RSA","kid":"enc","use":"enc","n":%q,"e":%q},
		{"kty":"RSA","kid":"ps","alg":"PS256","n":%q,"e":%q},
		{"kty":"EC","kid":"ec"},
		{"kty":"RSA","kid":"ok","use":"sig","alg":"RS256","n":%q,"e":%q}]}`,
		enc(weak.N.Bytes()), e, enc(strong.N.Bytes()), e, enc(strong.N.Bytes()), e, enc(strong.N.Bytes()), e)
	keys, err := parseJWKS([]byte(doc))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(keys) != 1 || keys["ok"] == nil {
		t.Fatalf("want only kid ok, got %v", keys)
	}
	_, err = parseJWKS([]byte(`{"keys":[]}`))
	if err == nil {
		t.Fatal("empty JWKS must be refused")
	}
}
