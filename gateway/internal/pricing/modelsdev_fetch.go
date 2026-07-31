package pricing

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const (
	ModelsDevURL              = "https://models.dev/api.json"
	ModelsDevFetchTimeout     = 10 * time.Second
	ModelsDevMaxResponseBytes = 8 << 20
)

// ModelsDevFetchResult is one bounded public-catalog response. A not-modified
// response carries no body and preserves the caller's prior ETag.
type ModelsDevFetchResult struct {
	Body        []byte
	ETag        string
	NotModified bool
}

// FetchModelsDev performs one credential-free models.dev request. Scheduling,
// persistence, and publication remain the caller's responsibility.
func FetchModelsDev(
	ctx context.Context,
	client *http.Client,
	endpoint string,
	etag string,
	maxResponseBytes int64,
) (ModelsDevFetchResult, error) {
	if client == nil {
		return ModelsDevFetchResult{}, fmt.Errorf("models.dev HTTP client is nil")
	}
	if strings.TrimSpace(endpoint) == "" {
		return ModelsDevFetchResult{}, fmt.Errorf("models.dev endpoint is empty")
	}
	if maxResponseBytes <= 0 {
		return ModelsDevFetchResult{}, fmt.Errorf("models.dev response limit must be positive")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return ModelsDevFetchResult{}, err
	}
	req.Header.Set("Accept", "application/json")
	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}

	clientCopy := *client
	clientCopy.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	}
	resp, err := clientCopy.Do(req)
	if err != nil {
		return ModelsDevFetchResult{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotModified {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		return ModelsDevFetchResult{
			ETag:        etag,
			NotModified: true,
		}, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		return ModelsDevFetchResult{}, fmt.Errorf(
			"models.dev returned %d",
			resp.StatusCode,
		)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return ModelsDevFetchResult{}, err
	}
	if int64(len(body)) > maxResponseBytes {
		return ModelsDevFetchResult{}, fmt.Errorf(
			"models.dev response exceeds %d bytes",
			maxResponseBytes,
		)
	}
	return ModelsDevFetchResult{
		Body: body,
		ETag: resp.Header.Get("ETag"),
	}, nil
}
