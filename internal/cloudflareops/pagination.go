package cloudflareops

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
)

var (
	errCapturePaginationStalled = errors.New(
		"cloudflare pagination did not advance; saved results are partial",
	)
	errCapturePageLimit = errors.New(
		"cloudflare page safety limit exceeded; saved results are partial",
	)
)

func (collector *captureCollector) capture(
	ctx context.Context,
	client *Client,
	parameters json.RawMessage,
) error {
	options := collector.options
	manifest := collector.manifest

	upper := options.To.UnixMilli()
	for page := 1; page <= options.MaxPages; page++ {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("telemetry capture canceled: %w", err)
		}

		result, err := client.Query(
			ctx,
			options.Account,
			QueryRequest{
				QueryID:    "pragent-ops-capture",
				Timeframe:  Timeframe{From: options.From.UnixMilli(), To: upper},
				Parameters: parameters,
				Limit:      options.PageSize,
				View:       "events",
			},
		)
		if err != nil {
			return err
		}

		if err = os.WriteFile(
			filepath.Join(options.Directory, fmt.Sprintf("page-%04d.json", page)),
			result,
			0o600,
		); err != nil {
			slog.WarnContext(ctx, "Telemetry page recording failed", "page", page)

			return fmt.Errorf("write telemetry page: %w", err)
		}

		manifest.Pages = page

		events, err := ReadQueryEvents(result)
		if err != nil {
			return err
		}

		if len(events) == 0 {
			manifest.Complete = true
			manifest.Completeness = "available telemetry exhausted; retention and sampling are not proven"

			return nil
		}

		oldest, newRecords, err := collector.page(events, upper)
		if err != nil {
			return err
		}

		if oldest >= upper || newRecords == 0 {
			return errCapturePaginationStalled
		}

		upper = oldest
	}

	return errCapturePageLimit
}
