package tracepackage

import (
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"context"
	"encoding/base64"
	"io"
	"regexp"
	"slices"
	"strings"
)

const HighConfidenceScannerVersion = "credential-high-confidence-v1"

type credentialPattern struct {
	category string
	pattern  *regexp.Regexp
}

var highConfidenceCredentialPatterns = []credentialPattern{
	{
		category: "private_key",
		pattern:  regexp.MustCompile(`-----BEGIN (?:RSA |EC |DSA |OPENSSH )?PRIVATE KEY-----`),
	},
	{
		category: "aws_access_key_id",
		pattern:  regexp.MustCompile(`(?:AKIA|ASIA)[A-Z0-9]{16}`),
	},
	{
		category: "github_token",
		pattern:  regexp.MustCompile(`(?:gh[pousr]_[A-Za-z0-9]{36,}|github_pat_[A-Za-z0-9_]{40,})`),
	},
	{
		category: "slack_token",
		pattern:  regexp.MustCompile(`xox[baprs]-[A-Za-z0-9-]{20,}`),
	},
	{
		category: "anthropic_api_key",
		pattern:  regexp.MustCompile(`sk-ant-[A-Za-z0-9_-]{20,}`),
	},
	{
		category: "openai_api_key",
		pattern: regexp.MustCompile(
			`sk-(?:proj-[A-Za-z0-9_-]{20,}|svcacct-[A-Za-z0-9_-]{20,}|[A-Za-z0-9]{20,})`,
		),
	},
	{
		category: "google_api_key",
		pattern:  regexp.MustCompile(`AIza[0-9A-Za-z_-]{35}`),
	},
	{
		category: "stripe_live_secret",
		pattern:  regexp.MustCompile(`sk_live_[0-9A-Za-z]{16,}`),
	},
	{
		category: "jwt",
		pattern: regexp.MustCompile(
			`eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}`,
		),
	},
}

// NewHighConfidenceScanner returns the built-in narrow credential guard. A
// false Scanned result means decoding, decompression, or the body-size limit
// prevented a complete scan. The result never contains captured content.
func NewHighConfidenceScanner() Scanner {
	return Scanner{
		Version: HighConfidenceScannerVersion,
		Scan:    scanHighConfidenceCredentials,
	}
}

func scanHighConfidenceCredentials(
	ctx context.Context,
	body BodyForScan,
) (BodyScanResult, error) {
	return scanHighConfidenceCredentialsWithLimit(
		ctx,
		body,
		MaxDecodedBodyScanBytes,
	)
}

func scanHighConfidenceCredentialsWithLimit(
	ctx context.Context,
	body BodyForScan,
	maxDecodedBytes int64,
) (BodyScanResult, error) {
	if err := ctx.Err(); err != nil {
		return BodyScanResult{}, err
	}
	if maxDecodedBytes < 0 {
		return BodyScanResult{}, nil
	}
	reader, closeReader, ok := decodedBodyReader(body)
	if !ok {
		return BodyScanResult{}, nil
	}
	if closeReader != nil {
		defer closeReader()
	}

	limited := &io.LimitedReader{R: reader, N: maxDecodedBytes + 1}
	decoded, err := io.ReadAll(limited)
	if err != nil || limited.N <= 0 {
		return BodyScanResult{}, nil
	}
	if err := ctx.Err(); err != nil {
		return BodyScanResult{}, err
	}

	categories := make([]string, 0, len(highConfidenceCredentialPatterns))
	for _, candidate := range highConfidenceCredentialPatterns {
		if candidate.pattern.Match(decoded) {
			categories = append(categories, candidate.category)
		}
	}
	slices.Sort(categories)
	return BodyScanResult{
		Scanned:            true,
		DetectedCategories: categories,
	}, nil
}

func decodedBodyReader(body BodyForScan) (io.Reader, func() error, bool) {
	base64Reader := func() io.Reader {
		return base64.NewDecoder(base64.StdEncoding, strings.NewReader(body.BodyBase64))
	}
	encoding := strings.ToLower(strings.TrimSpace(body.ContentEncoding))
	switch encoding {
	case "", "identity":
		return base64Reader(), nil, true
	case "gzip", "x-gzip":
		reader, err := gzip.NewReader(base64Reader())
		if err != nil {
			return nil, nil, false
		}
		return reader, reader.Close, true
	case "deflate":
		reader, err := zlib.NewReader(base64Reader())
		if err == nil {
			return reader, reader.Close, true
		}
		// Some clients use raw RFC 1951 DEFLATE despite the HTTP token's
		// historical zlib ambiguity. Retry from a fresh base64 decoder.
		raw := flate.NewReader(base64Reader())
		return raw, raw.Close, true
	default:
		return nil, nil, false
	}
}
