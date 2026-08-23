package review

import (
	"context"

	"goodkind.io/pr-review-agent/internal/runlog"
)

// recorderKey carries one review's log capture through the call chain, so every
// path that completes a check run can publish the run's own log without
// threading a parameter through each helper.
type recorderKey struct{}

func withRecorder(ctx context.Context, recorder *runlog.Recorder) context.Context {
	return context.WithValue(ctx, recorderKey{}, recorder)
}

// renderRunLog returns the current run's log as a markdown block, or the empty
// string when this context carries no capture.
func renderRunLog(ctx context.Context) string {
	recorder, ok := ctx.Value(recorderKey{}).(*runlog.Recorder)
	if !ok {
		return ""
	}
	return recorder.Render()
}
