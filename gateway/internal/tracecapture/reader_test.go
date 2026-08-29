package tracecapture

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestReaderReturnsValidCompleteRowsAndReportsRejectedRows(t *testing.T) {
	dir := privateTempDir(t)
	trace := validTrace(t, time.Now().UTC(), "41")
	valid, _ := json.Marshal(trace)
	invalid := trace
	invalid.SchemaVersion = 9
	invalidJSON, _ := json.Marshal(invalid)
	content := append(append(append(append([]byte{}, valid...), '\n'), []byte("not-json\n")...), invalidJSON...)
	content = append(content, '\n')
	content = append(content, []byte(`{"schema_version":1`)...)
	path := filepath.Join(dir, "trace-content-2026-08-17-001.jsonl")
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	traces, _, err := ReadSegmentTraces(path, 0)
	if len(traces) != 1 || traces[0].EventID != trace.EventID {
		t.Fatalf("unexpected traces: %+v", traces)
	}
	var readErr *SegmentReadError
	if !errors.As(err, &readErr) || readErr.MalformedLines != 1 || readErr.InvalidLines != 1 {
		t.Fatalf("unexpected reader error: %#v", err)
	}
}
