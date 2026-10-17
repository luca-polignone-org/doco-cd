package git

import (
	"fmt"
	"path"
	"strings"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// commitTree reads the root tree of a commit. It is a variable so that a test can make it fail:
// on a commit read from the same storer the error cannot be provoked otherwise.
var commitTree = (*object.Commit).Tree

// TreeHashAt returns the hash of the tree of dir at commit in the bare mirror at mirrorDir, under
// the mirror's shared lock. dir is relative to the repository root; "." or "" is the root. It is
// the tree part of the rollback content key (rollback.unblock_on: content): a commit that does not
// touch dir keeps the same hash.
func TreeHashAt(mirrorDir, commit, dir string) (string, error) {
	unlock := AcquireSharedMirrorLock(mirrorDir)
	defer unlock()

	repo, err := OpenRepository(mirrorDir)
	if err != nil {
		return "", fmt.Errorf("open mirror: %w", err)
	}

	c, err := repo.CommitObject(plumbing.NewHash(commit))
	if err != nil {
		return "", fmt.Errorf("commit %s: %w", commit, err)
	}

	tree, err := commitTree(c)
	if err != nil {
		return "", fmt.Errorf("tree of %s: %w", commit, err)
	}

	dir = strings.Trim(path.Clean("/"+dir), "/")
	if dir == "" {
		return tree.Hash.String(), nil
	}

	sub, err := tree.Tree(dir)
	if err != nil {
		return "", fmt.Errorf("tree %s at %s: %w", dir, commit, err)
	}

	return sub.Hash.String(), nil
}
