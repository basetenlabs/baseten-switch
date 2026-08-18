package tracepackage

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
)

func TestHighConfidenceScannerPlainBody(t *testing.T) {
	content := "prefix AKIA" + strings.Repeat("A", 16) +
		" -----BEGIN PRIVATE KEY----- suffix"
	result, err := NewHighConfidenceScanner().Scan(context.Background(), BodyForScan{
		BodyBase64: base64.StdEncoding.EncodeToString([]byte(content)),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Scanned || !reflect.DeepEqual(result.DetectedCategories, []string{
		"aws_access_key_id",
		"private_key",
	}) {
		t.Fatalf("scan result = %+v", result)
	}
}

func TestHighConfidenceScannerCompressedBodies(t *testing.T) {
	content := []byte("value=sk-ant-" + strings.Repeat("A", 24))
	tests := []struct {
		name     string
		encoding string
		compress func(*testing.T, []byte) []byte
	}{
		{name: "gzip", encoding: "gzip", compress: gzipBytes},
		{name: "zlib deflate", encoding: "deflate", compress: zlibBytes},
		{name: "raw deflate", encoding: "deflate", compress: rawDeflateBytes},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result, err := NewHighConfidenceScanner().Scan(context.Background(), BodyForScan{
				ContentEncoding: test.encoding,
				BodyBase64:      base64.StdEncoding.EncodeToString(test.compress(t, content)),
			})
			if err != nil {
				t.Fatal(err)
			}
			if !result.Scanned || !reflect.DeepEqual(result.DetectedCategories, []string{"anthropic_api_key"}) {
				t.Fatalf("scan result = %+v", result)
			}
		})
	}
}

func TestHighConfidenceScannerMarksUnscannableBodies(t *testing.T) {
	tests := []BodyForScan{
		{BodyBase64: "%%%"},
		{ContentEncoding: "br", BodyBase64: base64.StdEncoding.EncodeToString([]byte("value"))},
		{ContentEncoding: "identity,gzip", BodyBase64: base64.StdEncoding.EncodeToString(gzipBytes(t, []byte("value")))},
		{ContentEncoding: "gzip", BodyBase64: base64.StdEncoding.EncodeToString([]byte("not gzip"))},
	}
	for _, body := range tests {
		result, err := NewHighConfidenceScanner().Scan(context.Background(), body)
		if err != nil {
			t.Fatal(err)
		}
		if result.Scanned || len(result.DetectedCategories) != 0 {
			t.Fatalf("scan result = %+v", result)
		}
	}
}

func TestHighConfidenceScannerEnforcesDecodedLimit(t *testing.T) {
	body := BodyForScan{
		BodyBase64: base64.StdEncoding.EncodeToString([]byte("five!")),
	}
	result, err := scanHighConfidenceCredentialsWithLimit(
		context.Background(),
		body,
		4,
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.Scanned {
		t.Fatalf("scan result = %+v", result)
	}
}

func TestHighConfidenceScannerHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := NewHighConfidenceScanner().Scan(ctx, BodyForScan{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("scan error = %v", err)
	}
}

func gzipBytes(t *testing.T, content []byte) []byte {
	t.Helper()
	var destination bytes.Buffer
	writer := gzip.NewWriter(&destination)
	writeCompressed(t, writer, content)
	return destination.Bytes()
}

func zlibBytes(t *testing.T, content []byte) []byte {
	t.Helper()
	var destination bytes.Buffer
	writer := zlib.NewWriter(&destination)
	writeCompressed(t, writer, content)
	return destination.Bytes()
}

func rawDeflateBytes(t *testing.T, content []byte) []byte {
	t.Helper()
	var destination bytes.Buffer
	writer, err := flate.NewWriter(&destination, flate.DefaultCompression)
	if err != nil {
		t.Fatal(err)
	}
	writeCompressed(t, writer, content)
	return destination.Bytes()
}

func writeCompressed(t *testing.T, writer io.WriteCloser, content []byte) {
	t.Helper()
	if _, err := writer.Write(content); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
}
