package gateway

import (
	"context"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// tlsGateway serves the plan listing Ping uses over TLS, with a certificate no
// system root trusts, and writes that certificate to a PEM file.
func tlsGateway(t *testing.T) (baseURL, caFile string) {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"subscriptionPlans":[]}`))
	}))
	t.Cleanup(srv.Close)

	caFile = filepath.Join(t.TempDir(), "ca.crt")
	block := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	if err := os.WriteFile(caFile, block, 0o600); err != nil {
		t.Fatalf("WriteFile() = %v", err)
	}
	return srv.URL, caFile
}

func TestClientTrustsTheConfiguredCA(t *testing.T) {
	baseURL, caFile := tlsGateway(t)
	client, err := New(baseURL, "foo", "bar", caFile)
	if err != nil {
		t.Fatalf("New() = %v", err)
	}
	if err := client.Ping(context.Background()); err != nil {
		t.Fatalf("Ping() with the gateway's CA configured = %v", err)
	}
}

func TestClientRejectsAnUntrustedCertificate(t *testing.T) {
	baseURL, _ := tlsGateway(t)
	client, err := New(baseURL, "foo", "bar", "")
	if err != nil {
		t.Fatalf("New() = %v", err)
	}
	if err := client.Ping(context.Background()); err == nil {
		t.Fatal("Ping() to a gateway with an untrusted certificate = nil, want a verification error")
	}
}

func TestClientRefusesACAFileWithoutCertificates(t *testing.T) {
	empty := filepath.Join(t.TempDir(), "ca.crt")
	if err := os.WriteFile(empty, []byte("not a certificate"), 0o600); err != nil {
		t.Fatalf("WriteFile() = %v", err)
	}
	if _, err := New("https://gateway", "foo", "bar", empty); err == nil {
		t.Fatal("New() with a CA file holding no certificate = nil, want an error")
	}
	if _, err := New("https://gateway", "foo", "bar", filepath.Join(t.TempDir(), "missing.crt")); err == nil {
		t.Fatal("New() with a missing CA file = nil, want an error")
	}
}
