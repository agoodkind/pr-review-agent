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
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"goodkind.io/pr-review-agent/internal/clock"
	"goodkind.io/pr-review-agent/internal/cloudflareops"
	"goodkind.io/pr-review-agent/internal/opsstats"
)

type exclusions []string

func (values *exclusions) String() string { return strings.Join(*values, ",") }
func (values *exclusions) Set(value string) error {
	target, err := opsstats.ParseTarget(value)
	if err != nil {
		return err
	}
	*values = append(*values, target.String())
	return nil
}

type statsOptions struct {
	telemetry         options
	owner             string
	botLogin          string
	runtime           string
	operatorTokenFile string
	excluded          exclusions
}

func statsFlags(args []string, stderr io.Writer) (statsOptions, error) {
	var opts statsOptions
	set := flag.NewFlagSet("stats", flag.ContinueOnError)
	set.SetOutput(stderr)
	set.StringVar(&opts.telemetry.since, "since", "", "Duration before --end; live capture defaults to 24h.")
	set.StringVar(&opts.telemetry.from, "from", "", "Start timestamp in RFC3339; excludes --since.")
	set.StringVar(&opts.telemetry.end, "end", "", "End timestamp in RFC3339; cached input defaults to the manifest end.")
	set.StringVar(&opts.telemetry.input, "input", "", "Cached events.ndjson; skips Cloudflare telemetry requests.")
	set.StringVar(&opts.telemetry.manifest, "manifest", "", "Cached manifest path; defaults to manifest.json beside input.")
	set.StringVar(&opts.telemetry.account, "account", "", "Cloudflare account ID; defaults to Wrangler account_id.")
	set.StringVar(&opts.telemetry.script, "script", "", "Worker script; defaults to Wrangler name.")
	set.StringVar(&opts.telemetry.wrangler, "wrangler", "deploy/cloudflare/wrangler.jsonc", "Public Wrangler configuration.")
	set.StringVar(&opts.telemetry.tokenFile, "token-file", "", "Existing scoped Cloudflare observability token file.")
	set.StringVar(&opts.telemetry.accountTokenFile, "account-token-file", "", "Account token file for a temporary observability credential.")
	set.DurationVar(&opts.telemetry.ttl, "credential-ttl", 90*time.Minute, "Temporary credential expiration.")
	set.IntVar(&opts.telemetry.pageSize, "page-size", 2000, "Telemetry records per page.")
	set.IntVar(&opts.telemetry.maxPages, "max-pages", 200, "Telemetry page safety cap.")
	set.StringVar(&opts.telemetry.apiURL, "api-url", cloudflareops.APIURL, "Cloudflare API URL.")
	set.StringVar(&opts.telemetry.output, "output-dir", "", "New private output directory.")
	set.StringVar(&opts.owner, "owner", "", "GitHub owner; defaults to the authenticated gh user.")
	set.StringVar(&opts.botLogin, "bot-login", "", "Review bot login; defaults to runtime GITHUB_BOT_LOGIN.")
	set.StringVar(&opts.runtime, "runtime", "runtime.json", "Public runtime configuration.")
	set.StringVar(&opts.operatorTokenFile, "operator-token-file", "", "Optional operator token file for current quota snapshots.")
	set.Var(&opts.excluded, "exclude", "Exclude OWNER/REPOSITORY#NUMBER from metrics and blockers; repeatable.")
	if err := set.Parse(args); err != nil {
		return opts, err
	}
	if set.NArg() != 0 {
		return opts, errors.New("stats does not accept positional arguments")
	}
	return opts, nil
}

func statsEvidence(ctx context.Context, opts options) (cloudflareops.Metrics, []cloudflareops.Event, string, error) {
	var manifest cloudflareops.Manifest
	var directory string
	var err error
	if opts.input == "" {
		if opts.since == "" && opts.from == "" {
			opts.since = "24h"
		}
		manifest, err = captureManifest(ctx, opts)
		if err != nil {
			return cloudflareops.Metrics{}, nil, "", err
		}
		directory = manifest.Directory
		opts.input = manifest.EventsFile
	} else {
		manifestPath := opts.manifest
		if manifestPath == "" {
			manifestPath = filepath.Join(filepath.Dir(opts.input), "manifest.json")
		}
		manifest, err = cloudflareops.LoadManifest(manifestPath)
		if err != nil {
			return cloudflareops.Metrics{}, nil, "", err
		}
		manifest.InputFile = opts.input
		if opts.since != "" || opts.from != "" {
			if opts.end == "" {
				opts.end = manifest.To.Format(time.RFC3339Nano)
			}
			from, end, rangeErr := timeRange(opts)
			if rangeErr != nil {
				return cloudflareops.Metrics{}, nil, "", rangeErr
			}
			if from.Before(manifest.From) || end.After(manifest.To) {
				return cloudflareops.Metrics{}, nil, "", errors.New("stats range exceeds the cached capture range")
			}
			manifest.From, manifest.To = from, end
		} else if opts.end != "" {
			return cloudflareops.Metrics{}, nil, "", errors.New("stats --end requires --since or --from")
		}
		if opts.script != "" {
			manifest.Script = opts.script
		}
		if opts.account != "" {
			manifest.Account = opts.account
		}
		directory, err = cloudflareops.PrivateDirectory(opts.output)
		if err != nil {
			return cloudflareops.Metrics{}, nil, "", err
		}
	}
	if !manifest.Complete {
		return cloudflareops.Metrics{}, nil, directory, errors.New("stats requires an exhausted available telemetry capture; the manifest is partial")
	}
	events, err := cloudflareops.ReadEvents(opts.input)
	if err != nil {
		return cloudflareops.Metrics{}, nil, directory, err
	}
	manifest.Directory = directory
	metrics, err := cloudflareops.Summarize(events, manifest)
	return metrics, events, directory, err
}

func statsNumber(value json.RawMessage) int64 {
	var number int64
	if json.Unmarshal(value, &number) == nil {
		return number
	}
	var text string
	if json.Unmarshal(value, &text) == nil {
		number, _ = strconv.ParseInt(text, 10, 64)
	}
	return number
}

func statsExcludedMetrics(metrics cloudflareops.Metrics, events []cloudflareops.Event, excluded []string, owner string) (cloudflareops.Metrics, error) {
	excludedRuns := make(map[string]bool)
	for _, run := range metrics.Runs {
		target := opsstats.Target{Repository: run.Repository, Number: run.PullRequest}
		if slices.Contains(excluded, target.String()) || (run.Repository != "" && run.Repository != "unidentified" && !strings.EqualFold(strings.Split(run.Repository, "/")[0], owner)) {
			excludedRuns[run.ID] = true
		}
	}
	filtered := make([]cloudflareops.Event, 0, len(events))
	for _, event := range events {
		var delivery, runID, repository string
		var number int64
		_ = json.Unmarshal(event.Source["delivery_id"], &delivery)
		_ = json.Unmarshal(event.Source["run_id"], &runID)
		_ = json.Unmarshal(event.Source["repository"], &repository)
		number = statsNumber(event.Source["pull_request"])
		if excludedRuns[delivery] || excludedRuns[runID] {
			continue
		}
		if slices.Contains(excluded, (opsstats.Target{Repository: repository, Number: number}).String()) {
			continue
		}
		if repository != "" && !strings.EqualFold(strings.Split(repository, "/")[0], owner) {
			continue
		}
		filtered = append(filtered, event)
	}
	return cloudflareops.Summarize(filtered, metrics.Manifest)
}

func stats(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	opts, err := statsFlags(args, stderr)
	if errors.Is(err, flag.ErrHelp) {
		return nil
	}
	if err != nil {
		return err
	}
	data, err := os.ReadFile(opts.runtime)
	if err != nil {
		slog.WarnContext(ctx, "Stats runtime configuration read failed", "path", opts.runtime)
		return fmt.Errorf("read stats runtime configuration: %w", err)
	}
	var runtime struct {
		BotLogin          string `json:"GITHUB_BOT_LOGIN"`
		MinimumImportance string `json:"REVIEW_MIN_IMPORTANCE"`
	}
	if err = json.Unmarshal(data, &runtime); err != nil {
		return errors.New("stats runtime configuration could not be decoded")
	}
	if opts.botLogin == "" {
		opts.botLogin = runtime.BotLogin
	}
	if opts.botLogin == "" {
		return errors.New("stats requires --bot-login or runtime GITHUB_BOT_LOGIN")
	}
	minimumImportance, err := strconv.Atoi(runtime.MinimumImportance)
	if err != nil || minimumImportance <= 0 {
		return errors.New("stats runtime requires a positive REVIEW_MIN_IMPORTANCE")
	}
	if opts.owner == "" {
		opts.owner, err = opsstats.DefaultOwner(ctx)
		if err != nil {
			return err
		}
	}
	metrics, events, directory, err := statsEvidence(ctx, opts.telemetry)
	if err != nil {
		return err
	}
	logTargets := opsstats.LogTargets(metrics)
	metrics, err = statsExcludedMetrics(metrics, events, opts.excluded, opts.owner)
	if err != nil {
		return err
	}
	if err = cloudflareops.WriteJSON(filepath.Join(directory, "metrics.json"), metrics); err != nil {
		return err
	}
	if err = cloudflareops.WriteJSON(filepath.Join(directory, "manifest.json"), metrics.Manifest); err != nil {
		return err
	}
	pullRequests, err := opsstats.CaptureGitHub(ctx, opts.owner, metrics.Manifest.From, metrics.Manifest.To, logTargets)
	if err != nil {
		slog.WarnContext(ctx, "Stats GitHub capture failed", "directory", directory)
		return fmt.Errorf("stats GitHub capture failed; telemetry remains in %s: %w", directory, err)
	}
	report := opsstats.Build(metrics, pullRequests, opts.owner, opts.botLogin, opts.excluded, minimumImportance, clock.System().UTC())
	if opts.operatorTokenFile != "" {
		quotaDirectory := filepath.Join(directory, "quotas")
		if err = counters(ctx, []string{"--runtime", opts.runtime, "--operator-token-file", opts.operatorTokenFile, "--output-dir", quotaDirectory}, io.Discard, stderr); err != nil {
			report.QuotaUnavailableReason = "Current quota snapshots could not be queried."
		} else {
			report.QuotaCapturePath = filepath.Join(quotaDirectory, "counters.json")
			report.CurrentQuota, err = os.ReadFile(report.QuotaCapturePath)
			if err != nil {
				report.QuotaUnavailableReason = "The current quota capture could not be read."
			}
		}
		queue := statsQueue(ctx, data, opts.operatorTokenFile)
		opsstats.ApplyQueue(&report, queue)
	}
	path := filepath.Join(directory, "stats.json")
	if err = cloudflareops.WriteJSON(path, &report); err != nil {
		return err
	}
	return printStats(stdout, report, path)
}
