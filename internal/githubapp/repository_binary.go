package githubapp

import (
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
)

func localChangeIsBinary(entry object.ChangeEntry) (bool, error) {
	if entry.Name == "" || entry.TreeEntry.Mode == filemode.Submodule {
		return false, nil
	}

	file, err := entry.Tree.TreeEntryFile(&entry.TreeEntry)
	if err != nil {
		return false, repositoryAccessError("inspect changed Git entry", err)
	}

	binary, err := file.IsBinary()
	if err != nil {
		return false, repositoryAccessError("probe changed Git binary content", err)
	}

	return binary, nil
}
