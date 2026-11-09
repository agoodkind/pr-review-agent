package githubapp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"

	gitconfig "github.com/go-git/go-git/v5/plumbing/format/config"

	"goodkind.io/pr-review-agent/internal/domain"
)

func (client *Client) loadSnapshotGitlinks(
	ctx context.Context,
	installationID int64,
	repo domain.Repository,
	ref domain.HeadSHA,
	snapshot *repositorySnapshot,
) error {
	if _, exists := snapshot.metadata[".gitmodules"]; !exists {
		return nil
	}

	data, err := snapshot.root.ReadFile(".gitmodules")
	if err != nil {
		client.logger.WarnContext(ctx, "Read snapshot submodule configuration", "err", err)
		return fmt.Errorf("read snapshot submodule configuration: %w", err)
	}

	modules := gitconfig.New()
	if err := gitconfig.NewDecoder(bytes.NewReader(data)).Decode(modules); err != nil {
		client.logger.WarnContext(ctx, "Parse snapshot submodule configuration", "err", err)
		return fmt.Errorf("parse snapshot submodule configuration: %w", err)
	}

	query := url.Values{"recursive": {"1"}}
	body, err := client.doREST(
		ctx,
		installationID,
		http.MethodGet,
		client.repoPath(repo, "/git/trees/"+string(ref)),
		query,
		nil,
	)
	if err != nil {
		return err
	}

	var tree struct {
		Truncated bool `json:"truncated"`
		Tree      []struct {
			Path string `json:"path"`
			Mode string `json:"mode"`
			SHA  string `json:"sha"`
		} `json:"tree"`
	}
	if err := json.Unmarshal(body, &tree); err != nil {
		client.logger.WarnContext(ctx, "Decode snapshot gitlink metadata", "err", err)
		return fmt.Errorf("decode snapshot gitlink metadata: %w", err)
	}

	if tree.Truncated {
		return errors.New(
			"repository tree metadata is incomplete; gitlink scope cannot be established",
		)
	}

	urls := make(map[string]string)

	for _, module := range modules.Section("submodule").Subsections {
		urls[module.Option("path")] = module.Option("url")
	}

	for _, entry := range tree.Tree {
		if entry.Mode != "160000" {
			continue
		}

		if err := validateSnapshotPath(entry.Path); err != nil {
			return err
		}

		content, err := submoduleFileContent(entry.SHA, urls[entry.Path])
		if err != nil {
			return err
		}

		snapshot.gitlinks[entry.Path] = content
	}

	return nil
}
