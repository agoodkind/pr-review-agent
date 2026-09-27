// Command pragent-live-recovery verifies recovery of an interrupted live review.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	configPath        = "deploy/cloudflare/recovery-test.json"
	cloudflarePath    = "deploy/cloudflare"
	checkName         = "PR-Agent Review"
	checkApp          = "goodkind-io-pr-agent"
	workerName        = "agoodkind-nano-pr-reviewer"
	pollInterval      = 3 * time.Second
	checkStartTimeout = 3 * time.Minute
	recoveryTimeout   = 15 * time.Minute
)

type recoveryConfig struct {
	Repository     string `json:"repository"`
	PullRequest    int    `json:"pullRequest"`
	ForceLabel     string `json:"forceLabel"`
	InterruptLabel string `json:"interruptLabel"`
}

type label struct {
	Name string `json:"name"`
}

type pullRequest struct {
	State      string  `json:"state"`
	HeadRefOID string  `json:"headRefOid"`
	URL        string  `json:"url"`
	Labels     []label `json:"labels"`
}

type checkRun struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	ExternalID string `json:"external_id"`
	App        struct {
		Slug string `json:"slug"`
	} `json:"app"`
}

type checkResponse struct {
	CheckRuns []checkRun `json:"check_runs"`
}

type liveLogs struct {
	mu          sync.Mutex
	interrupted bool
	resumed     []string
	err         error
}

func main() {
	slog.Info("live recovery test started")
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := run(ctx); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(ctx context.Context) error {
	cfg, pr, err := loadTarget(ctx)
	if err != nil {
		return err
	}

	previous, err := checks(ctx, cfg, pr.HeadRefOID)
	if err != nil {
		return err
	}
	previousIDs := make(map[int64]bool, len(previous))
	for _, check := range previous {
		previousIDs[check.ID] = true
	}
	var available []label
	labelOutput, err := gh(ctx, "label", "list", "--repo", cfg.Repository, "--limit", "1000", "--json", "name")
	if err != nil {
		return err
	}
	if err := json.Unmarshal(labelOutput, &available); err != nil {
		return err
	}
	existing := make(map[string]bool, len(available))
	for _, current := range available {
		existing[current.Name] = true
	}

	var created []string
	added := false
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if added {
			if _, cleanupErr := gh(cleanupCtx, "pr", "edit", strconv.Itoa(cfg.PullRequest), "--repo", cfg.Repository, "--remove-label", cfg.ForceLabel+","+cfg.InterruptLabel); cleanupErr != nil {
				fmt.Fprintf(os.Stderr, "remove recovery labels: %v\n", cleanupErr)
			}
		}
		for _, name := range created {
			if _, cleanupErr := gh(cleanupCtx, "label", "delete", name, "--repo", cfg.Repository, "--yes"); cleanupErr != nil {
				fmt.Fprintf(os.Stderr, "delete recovery label %s: %v\n", name, cleanupErr)
			}
		}
	}()
	for _, entry := range []struct{ name, color string }{
		{cfg.ForceLabel, "5319E7"},
		{cfg.InterruptLabel, "B60205"},
	} {
		if !existing[entry.name] {
			if _, err := gh(ctx, "label", "create", entry.name, "--repo", cfg.Repository, "--color", entry.color); err != nil {
				return err
			}
			created = append(created, entry.name)
		}
	}

	logs, stopTail, err := startTail(ctx)
	if err != nil {
		return err
	}
	defer stopTail()
	if err := pause(ctx, pollInterval); err != nil {
		return err
	}
	if err := logs.failure(); err != nil {
		return err
	}

	if _, err := gh(ctx, "pr", "edit", strconv.Itoa(cfg.PullRequest), "--repo", cfg.Repository, "--add-label", cfg.ForceLabel); err != nil {
		return err
	}
	added = true
	check, err := waitForNewCheck(ctx, cfg, pr.HeadRefOID, previousIDs)
	if err != nil {
		return err
	}
	if check.ExternalID == "" {
		return errors.New("the new review check has no delivery identifier")
	}
	if _, err := gh(ctx, "pr", "edit", strconv.Itoa(cfg.PullRequest), "--repo", cfg.Repository, "--add-label", cfg.InterruptLabel); err != nil {
		return err
	}
	completionCtx, cancel := context.WithTimeout(ctx, recoveryTimeout)
	defer cancel()
	completed, err := waitForCompletion(completionCtx, cfg, pr.HeadRefOID, check.ID)
	if err != nil {
		return err
	}
	if completed.Conclusion != "success" || completed.ExternalID != check.ExternalID {
		return fmt.Errorf("review check %d completed with %q or changed its delivery identifier", check.ID, completed.Conclusion)
	}
	for !logs.recovered(check.ExternalID) {
		if err := logs.failure(); err != nil {
			return err
		}
		if err := pause(completionCtx, pollInterval); err != nil {
			return errors.New("Worker logs did not record both interruption and resumed delivery")
		}
	}
	fmt.Printf("Recovered delivery %s on check %d.\n%s\n", check.ExternalID, check.ID, pr.URL)
	return nil
}

func loadTarget(ctx context.Context) (recoveryConfig, pullRequest, error) {
	data, err := os.ReadFile(configPath)
	if err != nil {
		slog.Error("read recovery test configuration", "err", err)
		return recoveryConfig{}, pullRequest{}, fmt.Errorf("read recovery test configuration: %w", err)
	}
	var cfg recoveryConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return cfg, pullRequest{}, fmt.Errorf("parse recovery test configuration: %w", err)
	}
	if cfg.Repository == "" || cfg.PullRequest <= 0 || cfg.ForceLabel == "" || cfg.InterruptLabel == "" {
		return cfg, pullRequest{}, errors.New("recovery test configuration is incomplete")
	}

	var pr pullRequest
	prOutput, err := gh(ctx, "pr", "view", strconv.Itoa(cfg.PullRequest), "--repo", cfg.Repository, "--json", "state,headRefOid,labels,url")
	if err != nil {
		return cfg, pr, err
	}
	if err := json.Unmarshal(prOutput, &pr); err != nil {
		return cfg, pr, err
	}
	if pr.State != "OPEN" {
		return cfg, pr, errors.New("the configured test pull request is closed")
	}
	for _, current := range pr.Labels {
		if current.Name == cfg.ForceLabel || current.Name == cfg.InterruptLabel {
			return cfg, pr, errors.New("remove the recovery test labels from the pull request before starting")
		}
	}
	return cfg, pr, nil
}

func gh(ctx context.Context, args ...string) ([]byte, error) {
	command := exec.CommandContext(ctx, "gh", args...)
	output, err := command.CombinedOutput()
	if err != nil {
		slog.Error("GitHub command failed", "command", args[0], "err", err)
		return nil, fmt.Errorf("gh %s: %w: %s", args[0], err, strings.TrimSpace(string(output)))
	}
	return output, nil
}

func checks(ctx context.Context, cfg recoveryConfig, head string) ([]checkRun, error) {
	var response checkResponse
	path := fmt.Sprintf("repos/%s/commits/%s/check-runs?filter=all&per_page=100", cfg.Repository, head)
	output, err := gh(ctx, "api", path)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(output, &response); err != nil {
		return nil, err
	}
	var reviewChecks []checkRun
	for _, check := range response.CheckRuns {
		if check.Name == checkName && check.App.Slug == checkApp {
			reviewChecks = append(reviewChecks, check)
		}
	}
	return reviewChecks, nil
}

func waitForNewCheck(ctx context.Context, cfg recoveryConfig, head string, previous map[int64]bool) (checkRun, error) {
	deadline, cancel := context.WithTimeout(ctx, checkStartTimeout)
	defer cancel()
	for deadline.Err() == nil {
		all, err := checks(deadline, cfg, head)
		if err != nil {
			return checkRun{}, err
		}
		for _, check := range all {
			if previous[check.ID] {
				continue
			}
			if check.Status == "completed" {
				return checkRun{}, errors.New("the review completed before the container interruption")
			}
			if check.Status == "in_progress" {
				return check, nil
			}
		}
		if err := pause(deadline, pollInterval); err != nil {
			break
		}
	}
	return checkRun{}, errors.New("GitHub did not start a new review check")
}

func waitForCompletion(ctx context.Context, cfg recoveryConfig, head string, id int64) (checkRun, error) {
	for ctx.Err() == nil {
		all, err := checks(ctx, cfg, head)
		if err != nil {
			return checkRun{}, err
		}
		for _, check := range all {
			if check.ID == id && check.Status == "completed" {
				return check, nil
			}
		}
		if err := pause(ctx, pollInterval); err != nil {
			break
		}
	}
	return checkRun{}, fmt.Errorf("review check %d did not complete", id)
}

func pause(ctx context.Context, duration time.Duration) error {
	select {
	case <-time.After(duration):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func startTail(ctx context.Context) (*liveLogs, func(), error) {
	tailCtx, cancel := context.WithCancel(ctx)
	wrangler, err := filepath.Abs(filepath.Join(cloudflarePath, "node_modules", ".bin", "wrangler"))
	if err != nil {
		cancel()
		return nil, nil, err
	}
	command := exec.CommandContext(tailCtx, wrangler, "tail", workerName, "--format", "json")
	command.Dir = cloudflarePath
	stdout, err := command.StdoutPipe()
	if err != nil {
		cancel()
		return nil, nil, err
	}
	stderr, err := command.StderrPipe()
	if err != nil {
		cancel()
		return nil, nil, err
	}
	if err := command.Start(); err != nil {
		cancel()
		slog.Error("start Wrangler tail", "err", err)
		return nil, nil, fmt.Errorf("start Wrangler tail: %w", err)
	}
	logs := &liveLogs{}
	go func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				slog.Error("Wrangler tail reader panicked", "err", recovered)
			}
		}()
		logs.read(stdout)
	}()
	go func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				slog.Error("Wrangler diagnostics reader panicked", "err", recovered)
			}
		}()
		_, _ = io.Copy(io.Discard, stderr)
	}()
	go func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				slog.Error("Wrangler waiter panicked", "err", recovered)
			}
		}()
		err := command.Wait()
		logs.mu.Lock()
		if tailCtx.Err() == nil {
			logs.err = fmt.Errorf("Wrangler tail stopped: %w", err)
		}
		logs.mu.Unlock()
	}()
	return logs, cancel, nil
}

func (logs *liveLogs) read(source io.Reader) {
	scanner := bufio.NewScanner(source)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		logs.mu.Lock()
		if strings.Contains(line, "live interruption completed") {
			logs.interrupted = true
		}
		if strings.Contains(line, "resuming a review delivery that never finished") {
			logs.resumed = append(logs.resumed, line)
			if len(logs.resumed) > 20 {
				logs.resumed = logs.resumed[len(logs.resumed)-20:]
			}
		}
		logs.mu.Unlock()
	}
	if err := scanner.Err(); err != nil {
		logs.mu.Lock()
		logs.err = fmt.Errorf("read Wrangler tail: %w", err)
		logs.mu.Unlock()
	}
}

func (logs *liveLogs) failure() error {
	logs.mu.Lock()
	defer logs.mu.Unlock()
	return logs.err
}

func (logs *liveLogs) recovered(deliveryID string) bool {
	logs.mu.Lock()
	defer logs.mu.Unlock()
	if !logs.interrupted {
		return false
	}
	for _, line := range logs.resumed {
		if strings.Contains(line, deliveryID) {
			return true
		}
	}
	return false
}
