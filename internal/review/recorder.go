package review

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"goodkind.io/gklog"
	"goodkind.io/pr-review-agent/internal/runlog"
)

var errReviewRunLogRender = errors.New("review service could not render review run log")

// recorderKey carries one review's log capture through the call chain, so every
// path that completes a check run can publish the run's own log without
// threading a parameter through each helper.
type recorderKey struct{}

func withRecorder(ctx context.Context, recorder *runlog.Recorder) context.Context {
	return context.WithValue(ctx, recorderKey{}, recorder)
}

// renderRunLog returns the current run's log as a markdown block, or the empty
// string when this context carries no capture.
func renderRunLog(ctx context.Context) (string, error) {
	recorder, ok := ctx.Value(recorderKey{}).(*runlog.Recorder)
	if !ok {
		return "", nil
	}

	text, err := recorder.Render()
	if err != nil {
		logger := gklog.L(ctx)
		logger.WarnContext(
			ctx,
			"The review service could not render the review run log.",
			slog.String("err", err.Error()),
		)

		return "", fmt.Errorf("%w: %w", errReviewRunLogRender, err)
	}

	return text, nil
}
