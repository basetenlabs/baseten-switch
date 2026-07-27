package gateway

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/basetenlabs/baseten-switch/gateway/internal/config"
	"github.com/basetenlabs/baseten-switch/gateway/internal/requestcapability"
	"github.com/basetenlabs/baseten-switch/gateway/internal/upstreamerror"
)

type requestMultimodalState struct {
	hasImage bool
	stateful bool
}

func inspectMultimodalRequest(
	kind string,
	body []byte,
) requestMultimodalState {
	var endpoint requestcapability.Endpoint
	switch kind {
	case "messages":
		endpoint = requestcapability.AnthropicMessages
	case "chat":
		endpoint = requestcapability.OpenAIChat
	case "responses":
		endpoint = requestcapability.OpenAIResponses
	default:
		return requestMultimodalState{}
	}
	inspection := requestcapability.Inspect(endpoint, body)
	if inspection.Malformed {
		return requestMultimodalState{}
	}
	return requestMultimodalState{
		hasImage: inspection.HasImage,
		stateful: inspection.Stateful,
	}
}

func applyMultimodalStateForRequest(
	cl *clientListener,
	attempts []upstreamAttempt,
	kind string,
	body []byte,
) {
	if len(attempts) < 2 ||
		attempts[0].route != "baseten" ||
		attempts[1].route != config.NativeRoute(cl.cfg.ProtocolShape) {
		return
	}
	state := inspectMultimodalRequest(kind, body)
	for i := range attempts {
		attempts[i].imageInput = state.hasImage
		attempts[i].providerStateful = state.stateful
	}
}

func reactiveImageFallbackEligible(
	cl *clientListener,
	current upstreamAttempt,
	next upstreamAttempt,
	resp *http.Response,
) bool {
	return current.imageInput &&
		!current.providerStateful &&
		current.route == "baseten" &&
		next.route == config.NativeRoute(cl.cfg.ProtocolShape) &&
		resp.StatusCode == http.StatusBadRequest &&
		hasIdentityContentEncoding(resp.Header)
}

func hasIdentityContentEncoding(header http.Header) bool {
	encoding := strings.TrimSpace(header.Get("Content-Encoding"))
	return encoding == "" || strings.EqualFold(encoding, "identity")
}

func hasDeclaredBoundedClassifierBody(resp *http.Response) bool {
	return resp.ContentLength >= 0 &&
		resp.ContentLength <= upstreamerror.MaxClassifierBodyBytes
}

func upstreamErrorEndpoint(
	at upstreamAttempt,
) (upstreamerror.EndpointKind, bool) {
	switch {
	case at.kind == "responses":
		return upstreamerror.EndpointResponses, true
	case at.kind == "chat" || at.translate:
		return upstreamerror.EndpointChatCompletions, true
	case at.kind == "messages":
		return upstreamerror.EndpointMessages, true
	default:
		return "", false
	}
}

func bufferBoundedClassifierBody(
	resp *http.Response,
	ttftDeadline time.Time,
	cancel context.CancelFunc,
) ([]byte, bool, bool) {
	prefix, err, expired := readCompatibilityErrorPrefix(
		resp.Body,
		upstreamerror.MaxClassifierBodyBytes+1,
		ttftDeadline,
		cancel,
	)
	if expired {
		return nil, false, true
	}
	if err == nil && len(prefix) <= upstreamerror.MaxClassifierBodyBytes {
		_ = resp.Body.Close()
		resp.Body = io.NopCloser(bytes.NewReader(prefix))
		return prefix, true, false
	}
	resp.Body = prefixedBody{
		Reader: io.MultiReader(bytes.NewReader(prefix), resp.Body),
		Closer: resp.Body,
	}
	return nil, false, false
}

func isBasetenMultimodalUnsupported(
	at upstreamAttempt,
	statusCode int,
	body []byte,
) bool {
	endpoint, ok := upstreamErrorEndpoint(at)
	return ok && upstreamerror.IsBasetenMultimodalUnsupported400(
		endpoint,
		statusCode,
		body,
	)
}
