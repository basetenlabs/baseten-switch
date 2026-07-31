package pricing

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestFetchModelsDevUsesCredentialFreeBoundedRequest(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(
		w http.ResponseWriter,
		r *http.Request,
	) {
		if r.Method != http.MethodGet {
			t.Errorf("method = %q", r.Method)
		}
		if r.Header.Get("Accept") != "application/json" {
			t.Errorf("Accept = %q", r.Header.Get("Accept"))
		}
		if r.Header.Get("If-None-Match") != `"catalog-v1"` {
			t.Errorf("If-None-Match = %q", r.Header.Get("If-None-Match"))
		}
		if r.Header.Get("Authorization") != "" {
			t.Error("models.dev request included authorization")
		}
		w.Header().Set("ETag", `"catalog-v2"`)
		_, _ = io.WriteString(w, `{"baseten":{"id":"baseten","models":{}}}`)
	}))
	defer server.Close()

	result, err := FetchModelsDev(
		context.Background(),
		server.Client(),
		server.URL,
		`"catalog-v1"`,
		1024,
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.NotModified || result.ETag != `"catalog-v2"` ||
		!strings.Contains(string(result.Body), `"baseten"`) {
		t.Fatalf("result = %+v", result)
	}
}

func TestFetchModelsDevHandlesNotModified(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(
		w http.ResponseWriter,
		_ *http.Request,
	) {
		w.WriteHeader(http.StatusNotModified)
	}))
	defer server.Close()

	result, err := FetchModelsDev(
		context.Background(),
		server.Client(),
		server.URL,
		`"catalog-v1"`,
		1024,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !result.NotModified || result.ETag != `"catalog-v1"` ||
		result.Body != nil {
		t.Fatalf("result = %+v", result)
	}
}

func TestFetchModelsDevRejectsRedirectAndOversizedResponse(t *testing.T) {
	large := httptest.NewServer(http.HandlerFunc(func(
		w http.ResponseWriter,
		_ *http.Request,
	) {
		_, _ = io.WriteString(w, "12345")
	}))
	defer large.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(
		w http.ResponseWriter,
		r *http.Request,
	) {
		http.Redirect(w, r, large.URL, http.StatusFound)
	}))
	defer redirect.Close()

	if _, err := FetchModelsDev(
		context.Background(),
		redirect.Client(),
		redirect.URL,
		"",
		1024,
	); err == nil || !strings.Contains(err.Error(), "returned 302") {
		t.Fatalf("redirect error = %v", err)
	}
	if _, err := FetchModelsDev(
		context.Background(),
		large.Client(),
		large.URL,
		"",
		4,
	); err == nil || !strings.Contains(err.Error(), "exceeds 4 bytes") {
		t.Fatalf("oversize error = %v", err)
	}
}
