package main

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/basetenlabs/baseten-switch/gateway/internal/pricing"
)

type modelsDevPiCapabilitySource struct {
	client   *http.Client
	endpoint string
	timeout  time.Duration
	now      func() time.Time
}

func newModelsDevPiCapabilitySource() piCapabilitySource {
	return modelsDevPiCapabilitySource{
		client:   &http.Client{},
		endpoint: pricing.ModelsDevURL,
		timeout:  pricing.ModelsDevFetchTimeout,
		now:      time.Now,
	}
}

func (s modelsDevPiCapabilitySource) Enrich(
	ctx context.Context,
	models []piProviderModel,
) (piCapabilityEnrichment, error) {
	timeout := s.timeout
	if timeout <= 0 {
		timeout = pricing.ModelsDevFetchTimeout
	}
	fetchCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	result, err := pricing.FetchModelsDev(
		fetchCtx,
		s.client,
		s.endpoint,
		"",
		pricing.ModelsDevMaxResponseBytes,
	)
	if err != nil {
		return piCapabilityEnrichment{}, err
	}
	if result.NotModified {
		return piCapabilityEnrichment{}, fmt.Errorf(
			"models.dev returned not-modified without a local candidate",
		)
	}
	capturedAt := time.Now().UTC()
	if s.now != nil {
		capturedAt = s.now().UTC()
	}
	catalog := pricing.New()
	if err := catalog.ReplaceModelsDev(
		result.Body,
		capturedAt,
		result.ETag,
	); err != nil {
		return piCapabilityEnrichment{}, err
	}
	snapshot := catalog.Capture()
	enriched := make([]piProviderModel, len(models))
	matched := 0
	for index, model := range models {
		enriched[index] = model
		enriched[index].Input = []string{"text"}
		reasoning, reasoningKnown := snapshot.ModelReasoning(
			pricing.ProviderBaseten,
			model.ID,
		)
		if reasoningKnown {
			enriched[index].Reasoning = reasoning.Supported
		}
		modalities, modalitiesKnown := snapshot.ModelInputModalities(
			pricing.ProviderBaseten,
			model.ID,
		)
		if projected, ok := piInputModalities(modalities); modalitiesKnown && ok {
			enriched[index].Input = projected
		} else {
			modalitiesKnown = false
		}
		enriched[index].CapabilityKnown = reasoningKnown && modalitiesKnown
		if enriched[index].CapabilityKnown {
			matched++
		}
	}
	return piCapabilityEnrichment{
		Models:  enriched,
		Matched: matched,
	}, nil
}

func piInputModalities(source []string) ([]string, bool) {
	projected := make([]string, 0, 2)
	hasText := false
	for _, modality := range source {
		switch modality {
		case "text":
			hasText = true
			projected = append(projected, modality)
		case "image":
			projected = append(projected, modality)
		}
	}
	if !hasText {
		return nil, false
	}
	return projected, true
}
