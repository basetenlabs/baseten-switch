package proxy

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
)

// ErrEmptyAPIKey is returned by BuildUpstreamHeaders when APIKey mode is
// requested with an empty key. Sending the request without Authorization
// would produce a confusing upstream 401 mid-session, so this is a hard
// error the caller must handle instead of a silent header strip.
var ErrEmptyAPIKey = errors.New("proxy: empty API key in APIKey auth mode; refusing to send unauthenticated upstream request")

var HopByHop = map[string]bool{
	"connection":          true,
	"keep-alive":          true,
	"proxy-authenticate":  true,
	"proxy-authorization": true,
	"te":                  true,
	"trailers":            true,
	"transfer-encoding":   true,
	"upgrade":             true,
	"content-length":      true,
	"host":                true,
	"authorization":       true,
	"x-api-key":           true,
}

func IsHopByHop(name string) bool {
	return HopByHop[strings.ToLower(name)]
}

type UpstreamAuthMode int

const (
	UpstreamModePassthrough UpstreamAuthMode = iota
	UpstreamModeOAuth
	UpstreamModeAPIKey
)

func BuildUpstreamHeaders(in http.Header, mode UpstreamAuthMode, basetenAPIKey string) (http.Header, error) {
	if mode == UpstreamModeAPIKey && basetenAPIKey == "" {
		return nil, ErrEmptyAPIKey
	}
	out := http.Header{}
	for k, vs := range in {
		if HopByHop[strings.ToLower(k)] {
			continue
		}
		if strings.EqualFold(k, "Accept-Encoding") {
			continue
		}
		for _, v := range vs {
			out.Add(k, v)
		}
	}
	switch mode {
	case UpstreamModePassthrough:
		if v := in.Get("Authorization"); v != "" {
			out.Set("Authorization", v)
		}
		if v := in.Get("X-Api-Key"); v != "" {
			out.Set("X-Api-Key", v)
		}
	case UpstreamModeOAuth:
		out.Del("Authorization")
		out.Del("X-Api-Key")
	case UpstreamModeAPIKey:
		out.Del("X-Api-Key")
		out.Set("Authorization", "Api-Key "+basetenAPIKey)
	}
	return out, nil
}

type RewriteResult struct {
	NewBody        []byte
	RequestedModel string
	UpstreamModel  string
	IsStream       bool
	Parsed         bool
	// Sanitized records that history sanitizers rewrote the body before
	// the model rewrite; carried here so streamForward can log it.
	Sanitized bool
}

func RewriteModelInBody(body []byte, upstreamModel string) RewriteResult {
	res := RewriteResult{NewBody: body}
	var data map[string]interface{}
	if err := json.Unmarshal(body, &data); err != nil {
		return res
	}
	res.Parsed = true
	if m, ok := data["model"].(string); ok {
		res.RequestedModel = m
	}
	res.UpstreamModel = res.RequestedModel
	if upstreamModel != "" && upstreamModel != res.RequestedModel {
		res.UpstreamModel = upstreamModel
		data["model"] = upstreamModel
		nb, err := json.Marshal(data)
		if err != nil {
			return res
		}
		res.NewBody = nb
	}
	if s, ok := data["stream"].(bool); ok {
		res.IsStream = s
	}
	return res
}

func CopyHeader(dst, src http.Header) {
	for k, vs := range src {
		if HopByHop[strings.ToLower(k)] {
			continue
		}
		for _, v := range vs {
			dst.Add(k, v)
		}
	}
}

func DrainBody(r *http.Request) {
	if r == nil || r.Body == nil {
		return
	}
	_, _ = io.Copy(io.Discard, r.Body)
	_ = r.Body.Close()
}
