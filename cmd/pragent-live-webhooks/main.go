// Command pragent-live-webhooks verifies the deployed discussion webhooks.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	configPath   = "deploy/cloudflare/recovery-test.json"
	botLogin     = "goodkind-io-pr-agent"
	checkName    = "PR-Agent Review"
	pollInterval = 3 * time.Second
	checkTimeout = 5 * time.Minute
)

type config struct {
	Repository  string `json:"repository"`
	PullRequest int    `json:"pullRequest"`
}

type pullRequest struct {
	State      string `json:"state"`
	IsDraft    bool   `json:"isDraft"`
	HeadRefOID string `json:"headRefOid"`
	URL        string `json:"url"`
}

type checkRun struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	ExternalID string `json:"external_id"`
	Output     struct {
		Summary string `json:"summary"`
	} `json:"output"`
	App struct {
		Slug string `json:"slug"`
	} `json:"app"`
}

type checkResponse struct {
	CheckRuns []checkRun `json:"check_runs"`
}

type thread struct {
	ID         string `json:"id"`
	IsResolved bool   `json:"isResolved"`
	Comments   struct {
		Nodes []struct {
			DatabaseID int64 `json:"databaseId"`
			Author     struct {
				Login string `json:"login"`
			} `json:"author"`
		} `json:"nodes"`
	} `json:"comments"`
}

type threadResponse struct {
	Data struct {
		Repository struct {
			PullRequest struct {
				ReviewThreads struct {
					Nodes    []thread `json:"nodes"`
					PageInfo struct {
						HasNextPage bool `json:"hasNextPage"`
					} `json:"pageInfo"`
				} `json:"reviewThreads"`
			} `json:"pullRequest"`
		} `json:"repository"`
	} `json:"data"`
}

type comment struct {
	ID int64 `json:"id"`
}

type liveTest struct {
	target         config
	pr             pullRequest
	selected       thread
	lastID         int64
	marker         string
	issueCommentID int64
	plainCommentID int64
	replyID        int64
	threadResolved bool
}

const threadQuery = `query($owner:String!,$name:String!,$number:Int!){repository(owner:$owner,name:$name){pullRequest(number:$number){reviewThreads(first:100){nodes{id isResolved comments(first:1){nodes{databaseId author{login}}}} pageInfo{hasNextPage}}}}}`
const resolveMutation = `mutation($id:ID!){resolveReviewThread(input:{threadId:$id}){thread{isResolved}}}`
const unresolveMutation = `mutation($id:ID!){unresolveReviewThread(input:{threadId:$id}){thread{isResolved}}}`

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := run(ctx); err != nil {
		slog.Error("live webhook test failed", "err", err)
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(ctx context.Context) (result error) {
	data, err := os.ReadFile(configPath)
	if err != nil {
		slog.Error("read live test target", "err", err)
		return fmt.Errorf("read test target: %w", err)
	}
	var target config
	if err := json.Unmarshal(data, &target); err != nil {
		return fmt.Errorf("parse test target: %w", err)
	}
	if target.Repository == "" || target.PullRequest < 1 {
		return errors.New("test target needs a repository and pull request")
	}
	pr, err := getPullRequest(ctx, target)
	if err != nil {
		return err
	}
	if pr.State != "OPEN" || pr.IsDraft || pr.HeadRefOID == "" {
		return errors.New("test pull request must be open, ready, and have a head commit")
	}
	selected, err := selectThread(ctx, target)
	if err != nil {
		return err
	}
	lastID, err := latestCheckID(ctx, target, pr.HeadRefOID)
	if err != nil {
		return err
	}
	test := liveTest{
		target: target, pr: pr, selected: selected, lastID: lastID,
		marker: strconv.FormatInt(time.Now().UnixNano(), 36),
	}
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		result = errors.Join(result, test.cleanup(cleanupCtx))
		if result == nil {
			fmt.Printf("all live webhook scenarios passed: %s\n", pr.URL)
		}
	}()
	return test.perform(ctx)
}

func (test *liveTest) perform(ctx context.Context) error {
	if err := test.testPlainComment(ctx); err != nil {
		return err
	}
	if err := test.testTaggedComment(ctx); err != nil {
		return err
	}
	if err := test.testInlineReply(ctx); err != nil {
		return err
	}
	if err := test.testThreadState(ctx, true); err != nil {
		return err
	}
	if err := test.testThreadState(ctx, false); err != nil {
		return err
	}
	current, err := getPullRequest(ctx, test.target)
	if err != nil {
		return err
	}
	if current.HeadRefOID != test.pr.HeadRefOID {
		return errors.New("test pull request head changed during the run")
	}
	return nil
}

func (test *liveTest) testPlainComment(ctx context.Context) error {
	issuePath := fmt.Sprintf("repos/%s/issues/%d/comments", test.target.Repository, test.target.PullRequest)
	output, err := gh(ctx, "api", "-X", "POST", issuePath, "-f", "body=Live webhook test "+test.marker+" without a tag.")
	if err != nil {
		return err
	}
	var posted comment
	if err := json.Unmarshal(output, &posted); err != nil || posted.ID == 0 {
		return fmt.Errorf("decode untagged comment: %w", err)
	}
	test.plainCommentID = posted.ID
	if err := assertNoNewCheck(ctx, test.target, test.pr.HeadRefOID, test.lastID, 30*time.Second); err != nil {
		slog.Error("untagged comment started a review", "err", err)
		return fmt.Errorf("untagged comment: %w", err)
	}
	fmt.Println("untagged comment: no review check")
	return nil
}

func (test *liveTest) testTaggedComment(ctx context.Context) error {
	issuePath := fmt.Sprintf("repos/%s/issues/%d/comments", test.target.Repository, test.target.PullRequest)
	output, err := gh(ctx, "api", "-X", "POST", issuePath, "-f", "body=Live webhook test "+test.marker+". @"+botLogin+" please review this pull request.")
	if err != nil {
		return err
	}
	var posted comment
	if err := json.Unmarshal(output, &posted); err != nil || posted.ID == 0 {
		return fmt.Errorf("decode tagged comment: %w", err)
	}
	test.issueCommentID = posted.ID
	check, err := awaitCheck(ctx, test.target, test.pr.HeadRefOID, test.lastID)
	if err != nil {
		slog.Error("tagged comment review failed", "err", err)
		return fmt.Errorf("tagged comment: %w", err)
	}
	if !strings.Contains(check.Output.Summary, "| Forced run | yes |") || !strings.Contains(check.Output.Summary, "| Coverage complete | yes |") {
		return fmt.Errorf("tagged comment check %d did not complete a full forced review", check.ID)
	}
	fmt.Printf("tagged comment: check %d passed\n", check.ID)
	test.lastID = check.ID
	return nil
}

func (test *liveTest) testInlineReply(ctx context.Context) error {
	replyPath := fmt.Sprintf("repos/%s/pulls/%d/comments/%d/replies", test.target.Repository, test.target.PullRequest, test.selected.Comments.Nodes[0].DatabaseID)
	output, err := gh(ctx, "api", "-X", "POST", replyPath, "-f", "body=Live webhook test "+test.marker+": please reconsider this finding on the current head.")
	if err != nil {
		return err
	}
	var posted comment
	if err := json.Unmarshal(output, &posted); err != nil || posted.ID == 0 {
		return fmt.Errorf("decode test reply: %w", err)
	}
	test.replyID = posted.ID
	check, err := awaitCheck(ctx, test.target, test.pr.HeadRefOID, test.lastID)
	if err != nil {
		slog.Error("inline reply refresh failed", "err", err)
		return fmt.Errorf("inline reply: %w", err)
	}
	fmt.Printf("inline reply: check %d passed\n", check.ID)
	test.lastID = check.ID
	return nil
}

func (test *liveTest) testThreadState(ctx context.Context, resolved bool) error {
	if err := setThread(ctx, test.selected.ID, resolved); err != nil {
		return err
	}
	test.threadResolved = resolved
	if err := expectThreadState(ctx, test.target, test.selected.ID, resolved); err != nil {
		return err
	}
	check, err := awaitCheck(ctx, test.target, test.pr.HeadRefOID, test.lastID)
	if err != nil {
		slog.Error("thread state refresh failed", "resolved", resolved, "err", err)
		return fmt.Errorf("thread state %t: %w", resolved, err)
	}
	fmt.Printf("thread resolved=%t: check %d passed\n", resolved, check.ID)
	test.lastID = check.ID
	return nil
}

func (test *liveTest) cleanup(ctx context.Context) error {
	var result error
	if test.threadResolved {
		if err := setThread(ctx, test.selected.ID, false); err != nil {
			result = errors.Join(result, fmt.Errorf("restore thread: %w", err))
		}
	}
	for _, entry := range []struct {
		kind string
		id   int64
	}{
		{"pulls", test.replyID},
		{"issues", test.issueCommentID},
		{"issues", test.plainCommentID},
	} {
		if entry.id == 0 {
			continue
		}
		path := fmt.Sprintf("repos/%s/%s/comments/%d", test.target.Repository, entry.kind, entry.id)
		if _, err := gh(ctx, "api", "-X", "DELETE", path); err != nil {
			result = errors.Join(result, fmt.Errorf("delete test comment %d: %w", entry.id, err))
		}
	}
	if err := expectThreadState(ctx, test.target, test.selected.ID, false); err != nil {
		result = errors.Join(result, fmt.Errorf("verify restored thread: %w", err))
	}
	return result
}

func assertNoNewCheck(ctx context.Context, target config, head string, afterID int64, duration time.Duration) error {
	wait, cancel := context.WithTimeout(ctx, duration)
	defer cancel()
	for wait.Err() == nil {
		latest, err := latestCheckID(wait, target, head)
		if err != nil {
			return err
		}
		if latest > afterID {
			return fmt.Errorf("unexpected review check %d", latest)
		}
		select {
		case <-wait.Done():
		case <-time.After(pollInterval):
		}
	}
	return nil
}

func getPullRequest(ctx context.Context, target config) (pullRequest, error) {
	output, err := gh(ctx, "pr", "view", strconv.Itoa(target.PullRequest), "--repo", target.Repository, "--json", "state,isDraft,headRefOid,url")
	if err != nil {
		return pullRequest{}, err
	}
	var pr pullRequest
	err = json.Unmarshal(output, &pr)
	return pr, err
}

func selectThread(ctx context.Context, target config) (thread, error) {
	all, err := threads(ctx, target)
	if err != nil {
		return thread{}, err
	}
	for _, entry := range all {
		if entry.IsResolved || len(entry.Comments.Nodes) == 0 {
			continue
		}
		if strings.EqualFold(entry.Comments.Nodes[0].Author.Login, botLogin) && entry.Comments.Nodes[0].DatabaseID != 0 {
			return entry, nil
		}
	}
	return thread{}, errors.New("test pull request needs an unresolved bot review thread")
}

func threads(ctx context.Context, target config) ([]thread, error) {
	parts := strings.Split(target.Repository, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return nil, errors.New("repository must use owner/name format")
	}
	output, err := gh(ctx, "api", "graphql", "-f", "query="+threadQuery, "-f", "owner="+parts[0], "-f", "name="+parts[1], "-F", "number="+strconv.Itoa(target.PullRequest))
	if err != nil {
		return nil, err
	}
	var response threadResponse
	if err := json.Unmarshal(output, &response); err != nil {
		return nil, err
	}
	if response.Data.Repository.PullRequest.ReviewThreads.PageInfo.HasNextPage {
		return nil, errors.New("test pull request has more than 100 review threads")
	}
	return response.Data.Repository.PullRequest.ReviewThreads.Nodes, nil
}

func expectThreadState(ctx context.Context, target config, id string, resolved bool) error {
	all, err := threads(ctx, target)
	if err != nil {
		return err
	}
	for _, entry := range all {
		if entry.ID == id {
			if entry.IsResolved == resolved {
				return nil
			}
			return fmt.Errorf("thread %s has resolved=%t, expected %t", id, entry.IsResolved, resolved)
		}
	}
	return fmt.Errorf("thread %s disappeared", id)
}

func setThread(ctx context.Context, id string, resolved bool) error {
	mutation := unresolveMutation
	if resolved {
		mutation = resolveMutation
	}
	_, err := gh(ctx, "api", "graphql", "-f", "query="+mutation, "-f", "id="+id)
	return err
}

func latestCheckID(ctx context.Context, target config, head string) (int64, error) {
	all, err := checks(ctx, target, head)
	if err != nil {
		return 0, err
	}
	var latest int64
	for _, check := range all {
		if check.ID > latest {
			latest = check.ID
		}
	}
	return latest, nil
}

func checks(ctx context.Context, target config, head string) ([]checkRun, error) {
	path := fmt.Sprintf("repos/%s/commits/%s/check-runs?filter=all&per_page=100", target.Repository, head)
	output, err := gh(ctx, "api", path)
	if err != nil {
		return nil, err
	}
	var response checkResponse
	if err := json.Unmarshal(output, &response); err != nil {
		return nil, err
	}
	var found []checkRun
	for _, check := range response.CheckRuns {
		if check.Name == checkName && check.App.Slug == botLogin {
			found = append(found, check)
		}
	}
	return found, nil
}

func awaitCheck(ctx context.Context, target config, head string, afterID int64) (checkRun, error) {
	wait, cancel := context.WithTimeout(ctx, checkTimeout)
	defer cancel()
	for wait.Err() == nil {
		all, err := checks(wait, target, head)
		if err != nil {
			return checkRun{}, err
		}
		for _, check := range all {
			if check.ID <= afterID {
				continue
			}
			if check.Status == "completed" {
				if check.Conclusion != "success" || check.ExternalID == "" {
					return checkRun{}, fmt.Errorf("check %d ended with %q or lacks a delivery identifier", check.ID, check.Conclusion)
				}
				return check, nil
			}
		}
		select {
		case <-wait.Done():
		case <-time.After(pollInterval):
		}
	}
	if ctx.Err() != nil {
		return checkRun{}, ctx.Err()
	}
	return checkRun{}, fmt.Errorf("review check after %d did not complete within %s", afterID, checkTimeout)
}

func gh(ctx context.Context, args ...string) ([]byte, error) {
	command := exec.CommandContext(ctx, "gh", args...)
	output, err := command.CombinedOutput()
	if err != nil {
		slog.Error("GitHub command failed", "command", args[0], "err", err)
		return nil, fmt.Errorf("gh %s failed: %w: %s", args[0], err, strings.TrimSpace(string(output)))
	}
	return output, nil
}
