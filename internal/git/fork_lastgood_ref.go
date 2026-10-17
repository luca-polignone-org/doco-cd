package git

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/go-git/go-git/v5/plumbing"
)

// The mirror fetch prunes only refs/remotes/origin/* and refs/tags/* (see the refspecs in
// types.go), as it already does for refs/doco-cd/pinned/*: a ref under refs/doco-cd/lastgood/
// keeps the commit of the last good deploy reachable even after a force push. Mirror
// compaction keeps every stored object regardless of references.

const lastGoodRefPrefix = "refs/doco-cd/lastgood/"

// LastGoodRefName returns the mirror ref that pins the last good commit of object
// (<context>/<project>), as refs/doco-cd/lastgood/<context>/<project> with both
// components path-escaped.
func LastGoodRefName(object string) (plumbing.ReferenceName, error) {
	ctx, project, ok := strings.Cut(object, "/")
	if !ok || ctx == "" || project == "" || strings.Contains(project, "/") {
		return "", fmt.Errorf("invalid object %q", object)
	}

	name := plumbing.ReferenceName(lastGoodRefPrefix + url.PathEscape(ctx) + "/" + url.PathEscape(project))
	if err := name.Validate(); err != nil {
		return "", fmt.Errorf("invalid ref for object %q: %w", object, err)
	}

	return name, nil
}

// PinLastGood creates or moves the last good ref of object in the bare mirror at mirrorDir
// to commit, under the mirror's exclusive lock. The commit must already be in the mirror.
func PinLastGood(mirrorDir, object, commit string) error {
	name, err := LastGoodRefName(object)
	if err != nil {
		return err
	}

	unlock := AcquireExclusiveMirrorLock(mirrorDir)
	defer unlock()

	repo, err := OpenRepository(mirrorDir)
	if err != nil {
		return fmt.Errorf("open mirror: %w", err)
	}

	hash := plumbing.NewHash(commit)
	if _, err = repo.CommitObject(hash); err != nil {
		return fmt.Errorf("pin last good %s: commit %s not in mirror: %w", object, commit, err)
	}

	if err = repo.Storer.SetReference(plumbing.NewHashReference(name, hash)); err != nil {
		return fmt.Errorf("pin last good %s: %w", object, err)
	}

	return nil
}
