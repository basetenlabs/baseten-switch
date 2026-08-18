package gateway

import (
	"bytes"
	"errors"
	"net/http"
	"testing"
)

type traceWriterObserver struct {
	starts   int
	status   int
	content  string
	encoded  string
	body     bytes.Buffer
	panic    bool
	writeErr bool
}

func (o *traceWriterObserver) ObserveResponseWriteError(error) {
	if o.panic {
		panic("observer failure")
	}
	o.writeErr = true
}

func (o *traceWriterObserver) ObserveResponseStart(
	status int,
	contentType string,
	contentEncoding string,
) {
	if o.panic {
		panic("observer failure")
	}
	o.starts++
	o.status = status
	o.content = contentType
	o.encoded = contentEncoding
}

func (o *traceWriterObserver) ObserveResponseBytes(p []byte) {
	if o.panic {
		panic("observer failure")
	}
	o.body.Write(p)
}

type partialResponseWriter struct {
	header  http.Header
	status  int
	body    bytes.Buffer
	accept  int
	err     error
	flushes int
}

func (w *partialResponseWriter) Header() http.Header {
	return w.header
}

func (w *partialResponseWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
}

func (w *partialResponseWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n := len(p)
	if w.accept >= 0 && n > w.accept {
		n = w.accept
	}
	w.body.Write(p[:n])
	return n, w.err
}

func (w *partialResponseWriter) Flush() {
	w.flushes++
}

func TestTraceResponseWriterRecordsOnlyAcceptedBytes(t *testing.T) {
	wantErr := errors.New("client disconnected")
	underlying := &partialResponseWriter{
		header: make(http.Header),
		accept: 3,
		err:    wantErr,
	}
	underlying.header.Set("Content-Type", "text/event-stream")
	observer := &traceWriterObserver{}
	w := newTraceResponseWriter(underlying, observer)

	n, err := w.Write([]byte("abcdef"))
	if n != 3 || !errors.Is(err, wantErr) {
		t.Fatalf("Write() = %d, %v, want 3, %v", n, err, wantErr)
	}
	if got := observer.body.String(); got != "abc" {
		t.Fatalf("captured body = %q, want %q", got, "abc")
	}
	if !observer.writeErr {
		t.Fatal("observer did not receive response write failure")
	}
	if observer.starts != 1 || observer.status != http.StatusOK ||
		observer.content != "text/event-stream" {
		t.Fatalf("start = count %d status %d content %q", observer.starts, observer.status, observer.content)
	}
}

func TestTraceResponseWriterPreservesStatusFlushAndUnwrap(t *testing.T) {
	underlying := &partialResponseWriter{header: make(http.Header), accept: -1}
	underlying.header.Set("Content-Encoding", "gzip")
	observer := &traceWriterObserver{}
	w := newTraceResponseWriter(underlying, observer)

	w.WriteHeader(http.StatusAccepted)
	w.WriteHeader(http.StatusNoContent)
	w.(http.Flusher).Flush()

	if underlying.status != http.StatusAccepted {
		t.Fatalf("underlying status = %d, want %d", underlying.status, http.StatusAccepted)
	}
	if underlying.flushes != 1 {
		t.Fatalf("flushes = %d, want 1", underlying.flushes)
	}
	if observer.starts != 1 || observer.status != http.StatusAccepted || observer.encoded != "gzip" {
		t.Fatalf("observer start = count %d status %d encoding %q", observer.starts, observer.status, observer.encoded)
	}
	unwrapper, ok := w.(interface{ Unwrap() http.ResponseWriter })
	if !ok || unwrapper.Unwrap() != underlying {
		t.Fatal("Unwrap() did not return original writer")
	}
}

func TestTraceResponseWriterPreservesDuplicateContentEncodings(t *testing.T) {
	underlying := &partialResponseWriter{header: make(http.Header), accept: -1}
	underlying.header.Add("Content-Encoding", "identity")
	underlying.header.Add("Content-Encoding", "gzip")
	observer := &traceWriterObserver{}
	w := newTraceResponseWriter(underlying, observer)

	w.WriteHeader(http.StatusOK)

	if observer.encoded != "identity,gzip" {
		t.Fatalf("captured Content-Encoding = %q, want %q", observer.encoded, "identity,gzip")
	}
}

func TestTraceResponseWriterDoesNotInventFlusher(t *testing.T) {
	underlying := &nonFlushingResponseWriter{header: make(http.Header)}
	w := newTraceResponseWriter(underlying, &traceWriterObserver{})
	if _, ok := w.(http.Flusher); ok {
		t.Fatal("wrapper implements http.Flusher when original writer does not")
	}
}

func TestTraceResponseWriterObserverPanicDoesNotAffectClient(t *testing.T) {
	underlying := &partialResponseWriter{header: make(http.Header), accept: -1}
	observer := &traceWriterObserver{panic: true}
	w := newTraceResponseWriter(underlying, observer)

	n, err := w.Write([]byte("client bytes"))
	if err != nil || n != len("client bytes") {
		t.Fatalf("Write() = %d, %v", n, err)
	}
	if got := underlying.body.String(); got != "client bytes" {
		t.Fatalf("client body = %q", got)
	}
}

type nonFlushingResponseWriter struct {
	header http.Header
	body   bytes.Buffer
}

func (w *nonFlushingResponseWriter) Header() http.Header { return w.header }
func (w *nonFlushingResponseWriter) WriteHeader(int)     {}
func (w *nonFlushingResponseWriter) Write(p []byte) (int, error) {
	return w.body.Write(p)
}
