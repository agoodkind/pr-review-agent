package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"goodkind.io/pr-review-agent/internal/clock"
	"goodkind.io/pr-review-agent/internal/cloudflareops"
)

type options struct {
	since            string
	from             string
	end              string
	account          string
	script           string
	wrangler         string
	tokenFile        string
	accountTokenFile string
	ttl              time.Duration
	output           string
	input            string
	manifest         string
	pageSize         int
	maxPages         int
	apiURL           string
}

type command string

const (
	logsCommand    command = "logs"
	metricsCommand command = "metrics"
)

func flags(command string, args []string, output io.Writer) (options, error) {
	var opts options
	set := flag.NewFlagSet(command, flag.ContinueOnError)
	set.SetOutput(output)
	set.StringVar(&opts.since, "since", "", "Duration before --end, including 7d or 168h.")
	set.StringVar(&opts.from, "from", "", "Start timestamp in RFC3339; mutually exclusive with --since.")
	set.StringVar(&opts.end, "end", "", "End timestamp in RFC3339; defaults to the current UTC time for live capture.")
	set.StringVar(&opts.account, "account", "", "Cloudflare account ID; defaults to --wrangler account_id.")
	set.StringVar(&opts.script, "script", "", "Exact Worker script name; defaults to --wrangler name.")
	set.StringVar(&opts.wrangler, "wrangler", "deploy/cloudflare/wrangler.jsonc", "Public Wrangler configuration.")
	set.StringVar(&opts.tokenFile, "token-file", "", "Existing scoped Cloudflare token file; this token is not revoked.")
	set.StringVar(&opts.accountTokenFile, "account-token-file", "", "Account token file used to mint and revoke a temporary observability token.")
	set.DurationVar(&opts.ttl, "credential-ttl", 90*time.Minute, "Temporary token expiration; maximum 24h.")
	set.StringVar(&opts.output, "output-dir", "", "New private output directory; defaults to a private temporary directory.")
	set.StringVar(&opts.input, "input", "", "Cached event NDJSON or a bare event array; metrics performs no Cloudflare requests.")
	set.StringVar(&opts.manifest, "manifest", "", "Input capture manifest; defaults to manifest.json beside --input.")
	set.IntVar(&opts.pageSize, "page-size", 2000, "Maximum records per telemetry page.")
	set.IntVar(&opts.maxPages, "max-pages", 200, "Page safety cap; exceeding it fails the capture.")
	set.StringVar(&opts.apiURL, "api-url", cloudflareops.APIURL, "Cloudflare API URL; HTTP is permitted only for loopback verification.")
	if err := set.Parse(args); err != nil {
		return opts, err
	}
	if set.NArg() != 0 {
		return opts, errors.New("unexpected positional arguments")
	}
	return opts, nil
}

func duration(value string) (time.Duration, error) {
	if strings.HasSuffix(value, "d") {
		days, err := strconv.ParseInt(strings.TrimSuffix(value, "d"), 10, 32)
		if err != nil || days <= 0 || days > 36500 {
			return 0, errors.New("day duration must be an integer in 1..36500")
		}
		return time.Duration(days) * 24 * time.Hour, nil
	}
	parsed, err := time.ParseDuration(value)
	if err != nil || parsed <= 0 {
		return 0, errors.New("duration must be positive, such as 7d or 168h")
	}
	return parsed, nil
}

func timeRange(opts options) (time.Time, time.Time, error) {
	end := clock.System().UTC()
	if opts.end != "" {
		parsed, err := time.Parse(time.RFC3339, opts.end)
		if err != nil {
			return time.Time{}, time.Time{}, errors.New("end timestamp must use RFC3339")
		}
		end = parsed.UTC()
	}
	if (opts.since == "") == (opts.from == "") {
		return time.Time{}, time.Time{}, errors.New("provide exactly one --since duration or --from timestamp")
	}
	var from time.Time
	if opts.from != "" {
		parsed, err := time.Parse(time.RFC3339, opts.from)
		if err != nil {
			return from, end, errors.New("from timestamp must use RFC3339")
		}
		from = parsed.UTC()
	} else {
		span, err := duration(opts.since)
		if err != nil {
			return from, end, err
		}
		from = end.Add(-span)
	}
	if !from.Before(end) {
		return from, end, errors.New("from timestamp must precede end timestamp")
	}
	return from, end, nil
}

func configuration(opts *options) error {
	if opts.account != "" && opts.script != "" {
		return nil
	}
	data, err := os.ReadFile(opts.wrangler)
	if err != nil {
		slog.Warn("Wrangler configuration read failed", "path", opts.wrangler)
		return fmt.Errorf("read Wrangler configuration: %w", err)
	}
	var config struct {
		Account string `json:"account_id"`
		Script  string `json:"name"`
	}
	if json.Unmarshal(data, &config) != nil {
		return errors.New("Wrangler configuration must contain readable account_id and name fields")
	}
	if opts.account == "" {
		opts.account = config.Account
	}
	if opts.script == "" {
		opts.script = config.Script
	}
	if opts.account == "" || opts.script == "" {
		return errors.New("Cloudflare account and script are required")
	}
	return nil
}

func capture(ctx context.Context, opts options, output io.Writer) error {
	manifest, err := captureManifest(ctx, opts)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(output, "Records: %d. Pages: %d. Available telemetry exhausted: %t. Ephemeral credential revoked: %t.\nRange: %s through %s.\nPrivate evidence: %s\n", manifest.Records, manifest.Pages, manifest.Complete, manifest.CredentialRevoked, manifest.From.Format(time.RFC3339), manifest.To.Format(time.RFC3339), manifest.Directory)
	return err
}

func captureManifest(ctx context.Context, opts options) (manifest cloudflareops.Manifest, returnErr error) {
	if opts.input != "" {
		return manifest, errors.New("logs captures live telemetry; cached input belongs to metrics")
	}
	from, end, err := timeRange(opts)
	if err != nil {
		return manifest, err
	}
	if err = configuration(&opts); err != nil {
		return manifest, err
	}
	directory, err := cloudflareops.PrivateDirectory(opts.output)
	if err != nil {
		return manifest, err
	}
	credentials, err := cloudflareops.AcquireCredentials(ctx, cloudflareops.CredentialOptions{TokenFile: opts.tokenFile, AccountTokenFile: opts.accountTokenFile, Account: opts.account, TTL: opts.ttl, APIURL: opts.apiURL})
	if err != nil {
		return manifest, err
	}
	defer func() {
		cleanupErr := credentials.Close(ctx)
		returnErr = errors.Join(returnErr, cleanupErr)
		manifest.CredentialRevoked = credentials.Revoked
		if cleanupErr != nil {
			manifest.Error = "temporary credential cleanup failed; command error includes recovery identifiers"
		}
		returnErr = errors.Join(returnErr, cloudflareops.WriteJSON(filepath.Join(directory, "manifest.json"), manifest))
	}()
	manifest, err = credentials.Client.Capture(ctx, cloudflareops.CaptureOptions{Account: opts.account, Script: opts.script, From: from, To: end, PageSize: opts.pageSize, MaxPages: opts.maxPages, Directory: directory})
	if err != nil {
		slog.WarnContext(ctx, "Telemetry capture failed", "directory", directory)
		return manifest, fmt.Errorf("capture failed; private partial evidence directory %s: %w", directory, err)
	}
	return manifest, nil
}

func metrics(opts options, output io.Writer) error {
	if opts.input == "" {
		return errors.New("metrics requires --input cached evidence")
	}
	if err := configuration(&opts); err != nil {
		return err
	}
	manifestPath := opts.manifest
	if manifestPath == "" {
		manifestPath = filepath.Join(filepath.Dir(opts.input), "manifest.json")
	}
	manifest, err := cloudflareops.LoadManifest(manifestPath)
	if err != nil {
		slog.Warn("Input manifest read failed", "path", manifestPath)
		return fmt.Errorf("read input capture manifest: %w", err)
	}
	manifest.InputFile = opts.input
	manifest.Script = opts.script
	manifest.Account = opts.account
	if opts.since != "" || opts.from != "" {
		from, end, rangeErr := timeRange(opts)
		if rangeErr != nil {
			return rangeErr
		}
		if from.Before(manifest.From) || end.After(manifest.To) {
			return errors.New("metrics range exceeds the cached capture range")
		}
		manifest.From = from
		manifest.To = end
	} else if opts.end != "" {
		return errors.New("metrics --end requires --since or --from")
	}
	events, err := cloudflareops.ReadEvents(opts.input)
	if err != nil {
		return err
	}
	results, err := cloudflareops.Summarize(events, manifest)
	if err != nil {
		return err
	}
	directory, err := cloudflareops.PrivateDirectory(opts.output)
	if err != nil {
		return err
	}
	results.Manifest.Directory = directory
	if err = cloudflareops.WriteJSON(filepath.Join(directory, "metrics.json"), results); err != nil {
		return err
	}
	if err = cloudflareops.WriteJSON(filepath.Join(directory, "manifest.json"), results.Manifest); err != nil {
		return err
	}
	_, err = fmt.Fprintf(output, "Records: %d. Runs: %d. Runs with observed failures: %d. Runs reporting measured token usage: %d.\nPrivate metrics: %s\n", results.Total.Records, results.Total.Runs, results.Total.RunsWithFailureObserved, results.Total.RunsReportingUsage, filepath.Join(directory, "metrics.json"))
	return err
}

func run(ctx context.Context, args []string, stdout io.Writer, stderr io.Writer) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" || args[0] == "-h" {
		_, err := fmt.Fprintln(stdout, "pragent-ops logs --since 7d --account-token-file PATH\npragent-ops metrics --input CAPTURE/events.ndjson\npragent-ops counters --runtime runtime.json --operator-token-file PATH\npragent-ops operator-token --account-token-file PATH\nEach subcommand accepts --help. Raw telemetry is saved only in private files.")
		return err
	}
	if args[0] == "counters" {
		return counters(ctx, args[1:], stdout, stderr)
	}
	if args[0] == "operator-token" {
		return registerOperator(ctx, args[1:], stdout, stderr)
	}
	opts, err := flags(args[0], args[1:], stderr)
	if errors.Is(err, flag.ErrHelp) {
		return nil
	}
	if err != nil {
		return err
	}
	switch command(args[0]) {
	case logsCommand:
		return capture(ctx, opts, stdout)
	case metricsCommand:
		return metrics(opts, stdout)
	default:
		return errors.New("command must be logs, metrics, counters, or operator-token")
	}
}

func main() {
	slog.Info("Operations CLI invoked")
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := run(ctx, os.Args[1:], os.Stdout, os.Stderr); err != nil {
		slog.ErrorContext(ctx, "Operations command failed", "error", err)
		_, _ = fmt.Fprintln(os.Stderr, err)
		if ctx.Err() != nil {
			os.Exit(130)
		}
		os.Exit(1)
	}
}
