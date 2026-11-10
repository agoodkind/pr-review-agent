package githubapp

import (
	"bytes"
	"errors"
	"io/fs"
	"strings"

	gitconfig "github.com/go-git/go-git/v5/plumbing/format/config"
)

var errSubmoduleRepositoryMetadata = errors.New("invalid submodule repository metadata")

func loadSnapshotGitlinks(snapshot *repositorySnapshot) error {
	if len(snapshot.gitlinks) == 0 {
		return nil
	}

	data, err := snapshot.root.ReadFile(".gitmodules")
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}

	if err != nil {
		return repositoryAccessError("read submodule metadata", err)
	}

	modules := gitconfig.New()
	err = gitconfig.NewDecoder(bytes.NewReader(data)).Decode(modules)
	if err != nil {
		return repositoryAccessError("decode submodule metadata", err)
	}

	urls := make(map[string]string)

	for _, module := range modules.Section("submodule").Subsections {
		urls[module.Option("path")] = module.Option("url")
	}

	for name, pointer := range snapshot.gitlinks {
		address, mapped := urls[name]
		if !mapped {
			continue
		}

		if strings.TrimSpace(address) == "" || strings.ContainsAny(address, "\r\n\x00") {
			return errSubmoduleRepositoryMetadata
		}

		pointer.Metadata = "Submodule repository: " + address
		snapshot.gitlinks[name] = pointer
	}

	return nil
}
