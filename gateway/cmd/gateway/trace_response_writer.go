package gateway

import (
	"net/http"
	"strings"
)

// traceResponseObserver receives only response bytes accepted by the wrapped
// writer. Implementations must be non-blocking and best-effort. The wrapper
// protects the client path from observer panics as a final failure-isolation
// boundary.
type traceResponseObserver interface {
	ObserveResponseStart(status int, contentType, contentEncoding string)
	ObserveResponseBytes([]byte)
}

type traceResponseWriteObserver interface {
	ObserveResponseWriteError(error)
}

type traceResponseWriter struct {
	w        http.ResponseWriter
	observer traceResponseObserver
	started  bool
}

func newTraceResponseWriter(
	w http.ResponseWriter,
	observer traceResponseObserver,
) http.ResponseWriter {
	wrapped := &traceResponseWriter{w: w, observer: observer}
	if flusher, ok := w.(http.Flusher); ok {
		return &traceFlushingResponseWriter{
			traceResponseWriter: wrapped,
			flusher:             flusher,
		}
	}
	return wrapped
}

func (w *traceResponseWriter) Header() http.Header {
	return w.w.Header()
}

func (w *traceResponseWriter) WriteHeader(status int) {
	w.w.WriteHeader(status)
	w.observeStart(status)
}

func (w *traceResponseWriter) Write(p []byte) (int, error) {
	n, err := w.w.Write(p)
	if n > 0 {
		w.observeStart(http.StatusOK)
		w.observeBytes(p[:n])
	}
	if err != nil {
		if observer, ok := w.observer.(traceResponseWriteObserver); ok {
			traceObserverCall(func() { observer.ObserveResponseWriteError(err) })
		}
	}
	return n, err
}

// Unwrap allows http.ResponseController to discover optional interfaces on
// the original writer.
func (w *traceResponseWriter) Unwrap() http.ResponseWriter {
	return w.w
}

type traceFlushingResponseWriter struct {
	*traceResponseWriter
	flusher http.Flusher
}

// Flush preserves the streaming contract without buffering or coalescing.
// Flush can commit an implicit 200 before any Write call, so capture that
// status at the same boundary.
func (w *traceFlushingResponseWriter) Flush() {
	w.observeStart(http.StatusOK)
	w.flusher.Flush()
}

func (w *traceResponseWriter) observeStart(status int) {
	if w.started {
		return
	}
	w.started = true
	if w.observer == nil {
		return
	}
	h := w.w.Header()
	traceObserverCall(func() {
		w.observer.ObserveResponseStart(
			status,
			traceHeaderValues(h, "Content-Type"),
			traceHeaderValues(h, "Content-Encoding"),
		)
	})
}

// traceHeaderValues preserves every field value. Collapsing duplicate
// Content-Encoding fields to the first value could cause the package scanner
// to mistake a layered or ambiguous body for fully decoded content.
func traceHeaderValues(header http.Header, name string) string {
	return strings.Join(header.Values(name), ",")
}

func (w *traceResponseWriter) observeBytes(p []byte) {
	if w.observer == nil {
		return
	}
	traceObserverCall(func() { w.observer.ObserveResponseBytes(p) })
}

func traceObserverCall(call func()) {
	defer func() {
		_ = recover()
	}()
	call()
}
