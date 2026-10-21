package gateway

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestDefaultTransportRejectsUntrustedTLSBeforeHTTPRequest(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Header.Get("Authorization") != "Bearer synthetic-test-token" {
			t.Error("synthetic authorization header missing")
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	withRoots := func(transport *http.Transport, roots *x509.CertPool) *http.Transport {
		if transport.TLSClientConfig == nil {
			transport.TLSClientConfig = &tls.Config{}
		} else {
			transport.TLSClientConfig = transport.TLSClientConfig.Clone()
		}
		transport.TLSClientConfig.RootCAs = roots
		return transport
	}
	transport := withRoots(defaultTransport(), x509.NewCertPool())
	transport.Proxy = nil
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 5 * time.Second}
	request := func() (*http.Response, error) {
		req, err := http.NewRequest(http.MethodPost, server.URL, strings.NewReader("synthetic request content"))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer synthetic-test-token")
		return client.Do(req)
	}
	response, err := request()
	if response != nil {
		response.Body.Close()
	}
	var unknown x509.UnknownAuthorityError
	if !errors.As(err, &unknown) {
		t.Fatalf("expected untrusted TLS certificate rejection, got %v", err)
	}
	if got := requests.Load(); got != 0 {
		t.Fatalf("untrusted server received %d HTTP requests", got)
	}

	roots := x509.NewCertPool()
	roots.AddCert(server.Certificate())
	trustedTransport := withRoots(defaultTransport(), roots)
	trustedTransport.Proxy = nil
	defer trustedTransport.CloseIdleConnections()
	client.Transport = trustedTransport
	response, err = request()
	if err != nil {
		t.Fatalf("trusted TLS control failed: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusNoContent || requests.Load() != 1 {
		t.Fatalf("trusted TLS control status=%d requests=%d", response.StatusCode, requests.Load())
	}
}
