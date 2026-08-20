package tracepackage

import (
	"context"
	"encoding/json"
	"errors"
	"io"

	"github.com/basetenlabs/baseten-switch/gateway/internal/tracecapture"
)

// TraceValuesSource exposes one already bounded, verified trace selection as
// a repeatable streaming snapshot. It does not concatenate the rows in memory.
func TraceValuesSource(traces []tracecapture.TraceV1, estimatedBytes int64) TraceSnapshotFunc {
	fixed := append([]tracecapture.TraceV1(nil), traces...)
	return func(ctx context.Context, _ Selection) (Snapshot, error) {
		if err := ctx.Err(); err != nil {
			return Snapshot{}, err
		}
		return Snapshot{
			EstimatedBytes: estimatedBytes,
			Open: func(openCtx context.Context) (io.ReadCloser, error) {
				if err := openCtx.Err(); err != nil {
					return nil, err
				}
				return io.NopCloser(&traceValuesReader{ctx: openCtx, traces: fixed}), nil
			},
			Verify: func(verifyCtx context.Context) error { return verifyCtx.Err() },
		}, nil
	}
}

type traceValuesReader struct {
	ctx     context.Context
	traces  []tracecapture.TraceV1
	index   int
	current []byte
}

func (r *traceValuesReader) Read(destination []byte) (int, error) {
	if len(destination) == 0 {
		return 0, nil
	}
	for len(r.current) == 0 {
		if err := r.ctx.Err(); err != nil {
			return 0, err
		}
		if r.index >= len(r.traces) {
			return 0, io.EOF
		}
		encoded, err := json.Marshal(r.traces[r.index])
		if err != nil {
			return 0, errors.New("trace package: encode fixed trace selection")
		}
		r.index++
		r.current = append(encoded, '\n')
	}
	written := copy(destination, r.current)
	r.current = r.current[written:]
	return written, nil
}
