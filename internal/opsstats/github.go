// Package opsstats correlates retained telemetry with separately timed GitHub snapshots.
package opsstats

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"text/template"
	"time"
)

const githubSearchLimit = 1000

//go:embed queries.graphql
var querySource string

var queryTemplates = template.Must(template.New("queries").Parse(querySource))

type queryValues struct {
	ID         string
	Kind       string
	Connection string
	Cursor     string
	Selection  string
	Search     string
	Owner      string
	Repository string
	Number     int64
}

func renderQuery(name string, values queryValues) (string, error) {
	for _, value := range []*string{&values.ID, &values.Cursor, &values.Search, &values.Owner, &values.Repository} {
		encoded, err := json.Marshal(*value)
		if err != nil {
			slog.Warn("GitHub query string encoding failed", "operation", name)
			return "", fmt.Errorf("encode GraphQL string: %w", err)
		}
		*value = string(encoded)
	}
	var output bytes.Buffer
	if err := queryTemplates.ExecuteTemplate(&output, name, values); err != nil {
		return "", errors.New("GitHub query template could not be rendered")
	}
	return output.String(), nil
}

// Actor omits authentication data from captured GitHub identities.
type Actor struct {
	Login string `json:"login"`
}

// Commit identifies the immutable revision associated with a review.
type Commit struct {
	OID string `json:"oid"`
}

// Review retains bodies only in private evidence artifacts.
type Review struct {
	ID          string    `json:"id"`
	DatabaseID  int64     `json:"databaseId"`
	State       string    `json:"state"`
	SubmittedAt time.Time `json:"submittedAt"`
	Commit      Commit    `json:"commit"`
	Author      *Actor    `json:"author"`
	Body        string    `json:"body"`
}

// Comment records creation and edit times separately for trigger classification.
type Comment struct {
	ID                string    `json:"id"`
	DatabaseID        int64     `json:"databaseId"`
	PullRequestReview *Review   `json:"pullRequestReview"`
	Author            *Actor    `json:"author"`
	CreatedAt         time.Time `json:"createdAt"`
	UpdatedAt         time.Time `json:"updatedAt"`
	Path              string    `json:"path"`
	Body              string    `json:"body"`
}

// PageInfo prevents a truncated connection from establishing capture completeness.
type PageInfo struct {
	HasNextPage bool   `json:"hasNextPage"`
	EndCursor   string `json:"endCursor"`
}
type connectionNode interface {
	Review | Comment | Thread | Check
}

// Connection requires its captured node count to match the GitHub total.
type Connection[T connectionNode] struct {
	TotalCount int      `json:"totalCount"`
	Nodes      []T      `json:"nodes"`
	PageInfo   PageInfo `json:"pageInfo"`
	Pages      int      `json:"pages"`
}

// Thread includes every reply and the current resolution state.
type Thread struct {
	ID         string              `json:"id"`
	IsResolved bool                `json:"isResolved"`
	IsOutdated bool                `json:"isOutdated"`
	Comments   Connection[Comment] `json:"comments"`
}

// Check associates a check result with its admitted delivery or logged check identifier.
type Check struct {
	ExternalID  string     `json:"externalId"`
	ID          string     `json:"id"`
	DatabaseID  int64      `json:"databaseId"`
	Name        string     `json:"name"`
	Status      string     `json:"status"`
	Conclusion  string     `json:"conclusion"`
	StartedAt   *time.Time `json:"startedAt"`
	CompletedAt *time.Time `json:"completedAt"`
	DetailsURL  string     `json:"detailsUrl"`
	CheckSuite  struct {
		App struct {
			Slug string `json:"slug"`
		} `json:"app"`
	} `json:"checkSuite"`
}

// CheckRollup stores check contexts for the pull request head at capture time.
type CheckRollup struct {
	ID       string            `json:"id"`
	State    string            `json:"state"`
	Contexts Connection[Check] `json:"contexts"`
}

// PullRequest records current GitHub state with complete review and comment connections.
type PullRequest struct {
	BaseRefName string      `json:"baseRefName"`
	BranchRules BranchRules `json:"branch_rules"`
	ID          string      `json:"id"`
	Number      int64       `json:"number"`
	Repository  struct {
		NameWithOwner string `json:"nameWithOwner"`
	} `json:"repository"`
	URL                      string              `json:"url"`
	State                    string              `json:"state"`
	IsDraft                  bool                `json:"isDraft"`
	HeadRefOID               string              `json:"headRefOid"`
	ReviewDecision           string              `json:"reviewDecision"`
	Mergeable                string              `json:"mergeable"`
	MergeStateStatus         string              `json:"mergeStateStatus"`
	UpdatedAt                time.Time           `json:"updatedAt"`
	MergedAt                 *time.Time          `json:"mergedAt"`
	Reviews                  Connection[Review]  `json:"reviews"`
	LatestOpinionatedReviews Connection[Review]  `json:"latestOpinionatedReviews"`
	ReviewThreads            Connection[Thread]  `json:"reviewThreads"`
	Comments                 Connection[Comment] `json:"comments"`
	StatusCheckRollup        *CheckRollup        `json:"statusCheckRollup"`
}

// Target requires an owner, repository, and positive pull request number.
type Target struct {
	Repository string `json:"repository"`
	Number     int64  `json:"number"`
}

// BranchRule records active ruleset requirements separately from finding importance.
type BranchRule struct {
	Type       string `json:"type"`
	Parameters struct {
		RequiresThreadResolution bool `json:"required_review_thread_resolution"`
		RequiredApprovals        int  `json:"required_approving_review_count"`
		DismissesStaleReviews    bool `json:"dismiss_stale_reviews_on_push"`
	} `json:"parameters"`
}

// BranchRules covers active rulesets; classic protection requires separate evidence.
type BranchRules struct {
	Known bool         `json:"known"`
	Rules []BranchRule `json:"rules"`
	Error string       `json:"error,omitempty"`
}

// String uses OWNER/REPOSITORY#NUMBER for stable exclusion matching.
func (target Target) String() string {
	return target.Repository + "#" + strconv.FormatInt(target.Number, 10)
}

// ParseTarget rejects ambiguous identifiers before exclusion matching.
func ParseTarget(value string) (Target, error) {
	repository, numberText, found := strings.Cut(value, "#")
	parts := strings.Split(repository, "/")
	number, err := strconv.ParseInt(numberText, 10, 64)
	if !found || len(parts) != 2 || parts[0] == "" || parts[1] == "" || err != nil || number <= 0 {
		return Target{}, errors.New("pull request identifier must use OWNER/REPOSITORY#NUMBER")
	}
	return Target{Repository: repository, Number: number}, nil
}

func gh(ctx context.Context, args ...string) ([]byte, error) {
	slog.DebugContext(ctx, "GitHub evidence request started", "operation", args[0])
	data, err := exec.CommandContext(ctx, "gh", args...).Output()
	if err != nil {
		if ctx.Err() != nil {
			slog.WarnContext(ctx, "GitHub request canceled", "operation", args[0])
			return nil, fmt.Errorf("GitHub request canceled: %w", ctx.Err())
		}
		var exitError *exec.ExitError
		if errors.As(err, &exitError) {
			return nil, fmt.Errorf("GitHub CLI request failed with exit status %d; provider output remains private", exitError.ExitCode())
		}
		return nil, errors.New("GitHub CLI could not start")
	}
	return data, nil
}

// DefaultOwner reads the authenticated GitHub identity without assuming a particular account.
func DefaultOwner(ctx context.Context) (string, error) {
	data, err := gh(ctx, "api", "user")
	if err != nil {
		return "", err
	}
	var actor Actor
	if err = json.Unmarshal(data, &actor); err != nil || actor.Login == "" {
		return "", errors.New("GitHub authenticated owner could not be decoded; provide --owner")
	}
	return actor.Login, nil
}

type graphResponse struct {
	Data   map[string]json.RawMessage `json:"data"`
	Errors []json.RawMessage          `json:"errors"`
}

func graph(ctx context.Context, query string) (map[string]json.RawMessage, error) {
	data, err := gh(ctx, "api", "graphql", "-f", "query="+query)
	if err != nil {
		return nil, err
	}
	var response graphResponse
	if err = json.Unmarshal(data, &response); err != nil {
		return nil, errors.New("GitHub GraphQL response could not be decoded")
	}
	if len(response.Errors) != 0 {
		return nil, errors.New("GitHub GraphQL returned errors; the capture is incomplete")
	}
	return response.Data, nil
}

func completeConnection[T connectionNode](ctx context.Context, id, kind, name, fields string, connection *Connection[T]) error {
	connection.Pages = 1
	seen := make(map[string]bool)
	for connection.PageInfo.HasNextPage {
		cursor := connection.PageInfo.EndCursor
		if cursor == "" || seen[cursor] {
			return errors.New("GitHub connection pagination did not advance")
		}
		seen[cursor] = true
		var values queryValues
		selection, err := renderQuery(fields, values)
		if err != nil {
			return err
		}
		values.ID, values.Kind, values.Connection, values.Cursor, values.Selection = id, kind, name, cursor, selection
		query, err := renderQuery("connection", values)
		if err != nil {
			return err
		}
		data, err := graph(ctx, query)
		if err != nil {
			return err
		}
		var node map[string]json.RawMessage
		if err = json.Unmarshal(data["node"], &node); err != nil {
			return errors.New("GitHub paginated node could not be decoded")
		}
		var next Connection[T]
		if err = json.Unmarshal(node[name], &next); err != nil {
			return errors.New("GitHub connection could not be decoded")
		}
		connection.Nodes = append(connection.Nodes, next.Nodes...)
		connection.PageInfo = next.PageInfo
		connection.Pages++
		if next.TotalCount != connection.TotalCount {
			return errors.New("GitHub connection changed during pagination; retry the capture")
		}
	}
	if len(connection.Nodes) != connection.TotalCount {
		return errors.New("GitHub connection count does not match its complete page list")
	}
	return nil
}

func discover(ctx context.Context, owner string, from, end time.Time) ([]Target, error) {
	search := "is:pr user:" + owner + " updated:" + from.Format(time.RFC3339) + ".." + end.Format(time.RFC3339)
	var targets []Target
	cursor := ""
	for {
		var values queryValues
		values.Search, values.Cursor = search, cursor
		query, err := renderQuery("search", values)
		if err != nil {
			return nil, err
		}
		data, err := graph(ctx, query)
		if err != nil {
			return nil, err
		}
		var result struct {
			IssueCount int           `json:"issueCount"`
			PageInfo   PageInfo      `json:"pageInfo"`
			Nodes      []PullRequest `json:"nodes"`
		}
		if err = json.Unmarshal(data["search"], &result); err != nil {
			return nil, errors.New("GitHub search results could not be decoded")
		}
		if result.IssueCount >= githubSearchLimit {
			return nil, errors.New("GitHub search contains at least 1000 results; use a narrower window")
		}
		for _, pullRequest := range result.Nodes {
			if !pullRequest.UpdatedAt.Before(from) && !pullRequest.UpdatedAt.After(end) {
				targets = append(targets, Target{Repository: pullRequest.Repository.NameWithOwner, Number: pullRequest.Number})
			}
		}
		if !result.PageInfo.HasNextPage {
			return targets, nil
		}
		if result.PageInfo.EndCursor == "" || result.PageInfo.EndCursor == cursor {
			return nil, errors.New("GitHub search pagination did not advance")
		}
		cursor = result.PageInfo.EndCursor
	}
}

// CaptureGitHub exhausts every connection and rejects searches at the API result cap.
func CaptureGitHub(ctx context.Context, owner string, from, end time.Time, logTargets []Target) ([]PullRequest, error) {
	targets, err := discover(ctx, owner, from, end)
	if err != nil {
		return nil, err
	}
	targets = uniqueTargets(owner, append(targets, logTargets...))
	captured := make([]PullRequest, 0, len(targets))
	rulesByBranch := make(map[string]BranchRules)
	for _, target := range targets {
		pr, captureErr := capturePullRequest(ctx, target)
		if captureErr != nil {
			slog.WarnContext(ctx, "GitHub pull request capture failed", "pull_request", target.String())
			return nil, fmt.Errorf("capture %s: %w", target.String(), captureErr)
		}
		key := target.Repository + "/" + pr.BaseRefName
		rules, exists := rulesByBranch[key]
		if !exists {
			rules = readBranchRules(ctx, target.Repository, pr.BaseRefName)
			rulesByBranch[key] = rules
		}
		pr.BranchRules = rules
		captured = append(captured, *pr)
	}
	return captured, nil
}

func uniqueTargets(owner string, targets []Target) []Target {
	unique := make(map[string]Target)
	for _, target := range targets {
		if strings.EqualFold(strings.Split(target.Repository, "/")[0], owner) {
			unique[target.String()] = target
		}
	}
	result := make([]Target, 0, len(unique))
	for _, target := range unique {
		result = append(result, target)
	}
	sort.Slice(result, func(left, right int) bool { return result[left].String() < result[right].String() })
	return result
}

func capturePullRequest(ctx context.Context, target Target) (*PullRequest, error) {
	parts := strings.SplitN(target.Repository, "/", 2)
	var values queryValues
	values.Owner, values.Repository, values.Number = parts[0], parts[1], target.Number
	query, err := renderQuery("pullRequest", values)
	if err != nil {
		return nil, err
	}
	data, err := graph(ctx, query)
	if err != nil {
		return nil, err
	}
	var repository struct {
		PullRequest *PullRequest `json:"pullRequest"`
	}
	if json.Unmarshal(data["repository"], &repository) != nil || repository.PullRequest == nil {
		return nil, errors.New("GitHub pull request could not be decoded")
	}
	if err = completePullRequest(ctx, repository.PullRequest); err != nil {
		return nil, err
	}
	return repository.PullRequest, nil
}

func completePullRequest(ctx context.Context, pr *PullRequest) error {
	for _, err := range []error{
		completeConnection(ctx, pr.ID, "PullRequest", "reviews", "reviews", &pr.Reviews),
		completeConnection(ctx, pr.ID, "PullRequest", "latestOpinionatedReviews", "reviews", &pr.LatestOpinionatedReviews),
		completeConnection(ctx, pr.ID, "PullRequest", "reviewThreads", "threads", &pr.ReviewThreads),
		completeConnection(ctx, pr.ID, "PullRequest", "comments", "comments", &pr.Comments),
	} {
		if err != nil {
			return err
		}
	}
	for index := range pr.ReviewThreads.Nodes {
		thread := &pr.ReviewThreads.Nodes[index]
		if err := completeConnection(ctx, thread.ID, "PullRequestReviewThread", "comments", "threadComments", &thread.Comments); err != nil {
			return err
		}
	}
	if pr.StatusCheckRollup == nil {
		return nil
	}
	rollup := pr.StatusCheckRollup
	return completeConnection(ctx, rollup.ID, "StatusCheckRollup", "contexts", "checks", &rollup.Contexts)
}

func readBranchRules(ctx context.Context, repository, branch string) BranchRules {
	var rules BranchRules
	raw, err := gh(ctx, "api", "--paginate", "--slurp", "repos/"+repository+"/rules/branches/"+url.PathEscape(branch)+"?per_page=100")
	if err != nil {
		rules.Error = "GitHub branch rules could not be read"
		return rules
	}
	var pages [][]BranchRule
	if json.Unmarshal(raw, &pages) != nil {
		rules.Error = "GitHub branch rules could not be decoded"
		return rules
	}
	for _, page := range pages {
		rules.Rules = append(rules.Rules, page...)
	}
	rules.Known = true
	return rules
}
