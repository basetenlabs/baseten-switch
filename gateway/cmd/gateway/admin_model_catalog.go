package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/basetenlabs/baseten-switch/gateway/internal/modelmeta"
	"github.com/basetenlabs/baseten-switch/gateway/internal/pricing"
)

const (
	modelCatalogTimeout                       = 3 * time.Second
	modelCatalogPageSize                      = 1000
	modelCatalogMaxPages                      = 50
	modelCatalogMaxBody                       = 8 << 20
	modelCatalogSignedOutReasonNotSignedIn    = "not_signed_in"
	modelCatalogSignedOutReasonSessionExpired = "session_expired"
	modelCatalogSignedOutReasonRejected       = "credential_rejected"
	basetenModelAPIsAvailabilitySource        = "baseten_model_apis"
)

var errModelCatalogUnauthorized = errors.New("model catalog authorization rejected")
var errModelCatalogForbidden = errors.New("model catalog access denied")
var modelCatalogNow = time.Now

type modelCatalogResponse struct {
	State           string              `json:"state"`
	SignedOutReason string              `json:"signed_out_reason"`
	Models          []modelCatalogModel `json:"models"`
	FetchedAt       string              `json:"fetched_at"`
	Error           string              `json:"error"`
}

type modelCatalogModel struct {
	Slug        string                 `json:"slug"`
	DisplayName string                 `json:"display_name"`
	Reasoning   *modelCatalogReasoning `json:"reasoning,omitempty"`
}

type modelCatalogReasoning struct {
	Supported  bool                      `json:"supported"`
	Options    []pricing.ReasoningOption `json:"options"`
	Source     string                    `json:"source"`
	LoadedFrom string                    `json:"loaded_from"`
	Revision   string                    `json:"revision"`
	CapturedAt string                    `json:"captured_at"`
	Stale      bool                      `json:"stale"`
}

func (g *Gateway) adminModelCatalog(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		g.reject(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	// The live catalog belongs to the selected Baseten CLI profile. The
	// environment fallback is intentionally excluded from this account view.
	selected, ok := g.basetenProfileAuth()
	if !ok {
		writeModelCatalogJSON(w, r.Method, modelCatalogResponse{
			State:           "signed_out",
			SignedOutReason: modelCatalogSignedOutReasonNotSignedIn,
			Models:          []modelCatalogModel{},
		})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), modelCatalogTimeout)
	defer cancel()
	models, err := g.fetchModelCatalog(ctx, selected.client, selected.authorization())
	if err != nil {
		if errors.Is(err, errModelCatalogUnauthorized) {
			reason := modelCatalogSignedOutReasonSessionExpired
			if selected.source == basetenAuthProfileAPIKey {
				reason = modelCatalogSignedOutReasonRejected
			}
			writeModelCatalogJSON(w, r.Method, modelCatalogResponse{
				State:           "signed_out",
				SignedOutReason: reason,
				Models:          []modelCatalogModel{},
			})
			return
		}
		if errors.Is(err, errModelCatalogForbidden) {
			writeModelCatalogJSON(w, r.Method, modelCatalogResponse{
				State:  "error",
				Models: []modelCatalogModel{},
				Error:  "The selected Baseten credential does not have access to the model catalog",
			})
			return
		}
		writeModelCatalogJSON(w, r.Method, modelCatalogResponse{
			State:  "error",
			Models: []modelCatalogModel{},
			Error:  modelCatalogPublicError(err),
		})
		return
	}
	fetchedAt := modelCatalogNow().UTC()
	if err := g.publishBasetenModelAPIAvailability(models, fetchedAt); err != nil {
		writeModelCatalogJSON(w, r.Method, modelCatalogResponse{
			State:  "error",
			Models: []modelCatalogModel{},
			Error:  modelCatalogPublicError(err),
		})
		return
	}
	snapshot := g.pricing.Capture()
	models = modelCatalogModelsFromSnapshot(snapshot, models)
	writeModelCatalogJSON(w, r.Method, modelCatalogResponse{
		State:     "ready",
		Models:    models,
		FetchedAt: fetchedAt.Format(time.RFC3339),
	})
}

func (g *Gateway) publishBasetenModelAPIAvailability(
	models []modelCatalogModel,
	capturedAt time.Time,
) error {
	if len(models) == 0 {
		// A valid empty account list is useful live state, but it is not enough
		// evidence to erase last-observed presentation metadata.
		return nil
	}
	snapshot := g.pricing.Capture()
	availability := make([]pricing.AvailabilityModel, 0, len(models))
	for _, model := range models {
		displayName := strings.TrimSpace(model.DisplayName)
		if displayName == "" {
			displayName = basetenModelDisplayName(snapshot, model.Slug)
		}
		availability = append(availability, pricing.AvailabilityModel{
			CanonicalModelID: model.Slug,
			DisplayName:      displayName,
		})
	}
	if err := g.pricing.ReplaceProviderAvailability(
		pricing.ProviderBaseten,
		availability,
		basetenModelAPIsAvailabilitySource,
		capturedAt,
		availabilityRevision(availability),
	); err != nil {
		return err
	}
	if strings.TrimSpace(g.runtimeConfig().ConfigPath) == "" {
		return nil
	}
	if err := g.persistProviderCatalogCaches(); err != nil {
		// Cache persistence is best effort for this interactive endpoint. The
		// complete live response and its in-memory snapshot remain valid.
		fmt.Fprintf(os.Stderr,
			"[gateway] could not persist Baseten Model APIs catalog: %v\n",
			err)
	}
	return nil
}

func modelCatalogModelsFromSnapshot(
	snapshot *pricing.Snapshot,
	models []modelCatalogModel,
) []modelCatalogModel {
	resolved := make([]modelCatalogModel, len(models))
	now := modelCatalogNow().UTC()
	for i, model := range models {
		resolved[i] = modelCatalogModel{
			Slug:        model.Slug,
			DisplayName: basetenModelDisplayName(snapshot, model.Slug),
			Reasoning: modelCatalogReasoningFromSnapshot(
				snapshot,
				model.Slug,
				now,
			),
		}
	}
	return resolved
}

func modelCatalogReasoningFromSnapshot(
	snapshot *pricing.Snapshot,
	canonicalID string,
	now time.Time,
) *modelCatalogReasoning {
	capability, ok := snapshot.ModelReasoning(
		pricing.ProviderBaseten,
		canonicalID,
	)
	if !ok {
		return nil
	}
	freshFrom := snapshot.ModelsDevValidatedAt(pricing.ProviderBaseten)
	if freshFrom.IsZero() {
		freshFrom = capability.Provenance.CapturedAt
	}
	stale := capability.Provenance.LoadedFrom !=
		pricing.LoadedFromVendoredFallback &&
		!freshFrom.IsZero() &&
		now.Sub(freshFrom) > publicCatalogStaleAfter
	return &modelCatalogReasoning{
		Supported:  capability.Supported,
		Options:    capability.Options,
		Source:     capability.Provenance.Source,
		LoadedFrom: string(capability.Provenance.LoadedFrom),
		Revision:   capability.Provenance.Revision,
		CapturedAt: capability.Provenance.CapturedAt.UTC().Format(
			time.RFC3339,
		),
		Stale: stale,
	}
}

func basetenModelDisplayName(
	snapshot *pricing.Snapshot,
	canonicalID string,
) string {
	if displayName, ok := snapshot.DisplayName(
		pricing.ProviderBaseten,
		canonicalID,
	); ok {
		return displayName
	}
	return modelmeta.ResolveBaseten(canonicalID).DisplayName
}

func writeModelCatalogJSON(w http.ResponseWriter, method string, response modelCatalogResponse) {
	body, _ := json.Marshal(response)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	if method != http.MethodHead {
		_, _ = w.Write(body)
	}
}

func (g *Gateway) fetchModelCatalog(ctx context.Context, client *http.Client, authorization string) ([]modelCatalogModel, error) {
	endpoint, err := modelCatalogEndpoint(g.runtimeConfig().OAuthHost)
	if err != nil {
		return nil, err
	}

	models := make([]modelCatalogModel, 0)
	seenCursors := make(map[string]struct{})
	cursor := ""
	for pageNumber := 0; pageNumber < modelCatalogMaxPages; pageNumber++ {
		pageURL := *endpoint
		query := pageURL.Query()
		if cursor != "" {
			query.Set("cursor", cursor)
		}
		pageURL.RawQuery = query.Encode()

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, pageURL.String(), nil)
		if err != nil {
			return nil, fmt.Errorf("build model catalog request: %w", err)
		}
		if authorization != "" {
			req.Header.Set("Authorization", authorization)
		}
		resp, err := client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("request model catalog: %w", err)
		}
		body, err := readModelCatalogBody(resp)
		if err != nil {
			return nil, err
		}

		pageModels, nextCursor, hasMore, err := decodeModelCatalogPage(body)
		if err != nil {
			return nil, fmt.Errorf("decode model catalog: %w", err)
		}
		models = append(models, pageModels...)
		if !hasMore {
			return models, nil
		}
		if nextCursor == "" {
			return nil, errors.New("model catalog pagination omitted its cursor")
		}
		if _, exists := seenCursors[nextCursor]; exists {
			return nil, errors.New("model catalog returned a repeated pagination cursor")
		}
		seenCursors[nextCursor] = struct{}{}
		cursor = nextCursor
	}
	return nil, errors.New("model catalog exceeded pagination limit")
}

func modelCatalogEndpoint(oauthHost string) (*url.URL, error) {
	endpoint, err := url.Parse(strings.TrimRight(oauthHost, "/") + "/v1/model_apis")
	if err != nil {
		return nil, fmt.Errorf("build model catalog endpoint: %w", err)
	}
	if endpoint.Scheme != "http" && endpoint.Scheme != "https" {
		return nil, errors.New("build model catalog endpoint: unsupported scheme")
	}
	if endpoint.Host == "" {
		return nil, errors.New("build model catalog endpoint: missing host")
	}
	query := endpoint.Query()
	query.Set("added_only", "false")
	query.Set("limit", strconv.Itoa(modelCatalogPageSize))
	endpoint.RawQuery = query.Encode()
	return endpoint, nil
}

func readModelCatalogBody(resp *http.Response) ([]byte, error) {
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, modelCatalogMaxBody+1))
	closeErr := resp.Body.Close()
	if readErr != nil {
		return nil, fmt.Errorf("read model catalog response: %w", readErr)
	}
	if closeErr != nil {
		return nil, fmt.Errorf("close model catalog response: %w", closeErr)
	}
	if len(body) > modelCatalogMaxBody {
		return nil, errors.New("model catalog response is too large")
	}
	if resp.StatusCode == http.StatusUnauthorized {
		return nil, errModelCatalogUnauthorized
	}
	if resp.StatusCode == http.StatusForbidden {
		return nil, errModelCatalogForbidden
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("model catalog upstream returned a non-success status")
	}
	return body, nil
}

func decodeModelCatalogPage(body []byte) ([]modelCatalogModel, string, bool, error) {
	var page map[string]json.RawMessage
	decoder := json.NewDecoder(bytes.NewReader(body))
	if err := decoder.Decode(&page); err != nil {
		return nil, "", false, err
	}
	if err := requireJSONEOF(decoder); err != nil {
		return nil, "", false, err
	}
	if page == nil {
		return nil, "", false, errors.New("response was not an object")
	}

	rawItems, exists := page["items"]
	if !exists || bytes.Equal(bytes.TrimSpace(rawItems), []byte("null")) {
		return nil, "", false, errors.New("response did not contain an items array")
	}
	var items []json.RawMessage
	if err := json.Unmarshal(rawItems, &items); err != nil {
		return nil, "", false, errors.New("response items was not an array")
	}

	models := make([]modelCatalogModel, 0, len(items))
	for _, item := range items {
		model, keep, err := decodeModelCatalogItem(item)
		if err != nil {
			return nil, "", false, err
		}
		if keep {
			models = append(models, model)
		}
	}

	rawPagination, exists := page["pagination"]
	if !exists || bytes.Equal(bytes.TrimSpace(rawPagination), []byte("null")) {
		return nil, "", false, errors.New("response did not contain pagination")
	}
	var pagination map[string]json.RawMessage
	if err := json.Unmarshal(rawPagination, &pagination); err != nil || pagination == nil {
		return nil, "", false, errors.New("response pagination was not an object")
	}
	rawHasMore, exists := pagination["has_more"]
	if !exists {
		return nil, "", false, errors.New("pagination omitted has_more")
	}
	var hasMore bool
	if err := json.Unmarshal(rawHasMore, &hasMore); err != nil {
		return nil, "", false, errors.New("pagination has_more was not a boolean")
	}

	cursor, err := decodeModelCatalogCursor(pagination["cursor"])
	if err != nil {
		return nil, "", false, err
	}
	if hasMore && cursor == "" {
		return nil, "", false, errors.New("pagination omitted its cursor")
	}
	return models, cursor, hasMore, nil
}

func decodeModelCatalogItem(raw json.RawMessage) (modelCatalogModel, bool, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return modelCatalogModel{}, false, errors.New("model entry was not an object")
	}

	var slug string
	rawName, exists := fields["name"]
	if !exists || json.Unmarshal(rawName, &slug) != nil {
		return modelCatalogModel{}, false, nil
	}
	slug = strings.TrimSpace(slug)
	if slug == "" {
		return modelCatalogModel{}, false, nil
	}

	displayName := ""
	if rawDisplayName, exists := fields["display_name"]; exists {
		_ = json.Unmarshal(rawDisplayName, &displayName)
		displayName = strings.TrimSpace(displayName)
	}

	return modelCatalogModel{
		Slug:        slug,
		DisplayName: displayName,
	}, true, nil
}

func decodeModelCatalogCursor(raw json.RawMessage) (string, error) {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return "", nil
	}
	var cursor string
	if err := json.Unmarshal(raw, &cursor); err != nil {
		return "", errors.New("pagination cursor was not a string or null")
	}
	return cursor, nil
}

func requireJSONEOF(decoder *json.Decoder) error {
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("response contained multiple JSON values")
		}
		return err
	}
	return nil
}

func modelCatalogPublicError(err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return "Baseten model catalog request timed out"
	}
	return "Unable to load the Baseten model catalog"
}
