package git_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/transport"

	"github.com/kimdre/doco-cd/internal/git"
)

// newLastGoodMirror creates a source repository with a commit on main and a second
// commit that only the "feature" branch points to, plus a bare mirror that fetched
// every branch of it.
func newLastGoodMirror(t *testing.T) (srcRepo *gogit.Repository, srcURL, mirrorPath string, mainCommit, featureCommit plumbing.Hash) {
	t.Helper()

	srcPath := filepath.Join(t.TempDir(), "src")
	srcRepo = initLocalTestRepo(t, srcPath)

	head, err := srcRepo.Head()
	if err != nil {
		t.Fatal(err)
	}

	mainCommit = head.Hash()
	featureCommit = commitLocalTestFile(t, srcRepo, srcPath, "FEATURE.md", "feature\n", "feature commit")

	if err := srcRepo.Storer.SetReference(plumbing.NewHashReference(plumbing.NewBranchReferenceName("feature"), featureCommit)); err != nil {
		t.Fatal(err)
	}

	if err := srcRepo.Storer.SetReference(plumbing.NewHashReference(plumbing.NewBranchReferenceName("main"), mainCommit)); err != nil {
		t.Fatal(err)
	}

	srcURL = "file://" + srcPath
	mirrorPath = filepath.Join(t.TempDir(), "mirror")

	repo, err := git.CloneOrUpdateBareMirror(nil, srcURL, git.MainBranch, mirrorPath,
		false, "", "", "", false, transport.ProxyOptions{}, 0)
	if err != nil {
		t.Fatalf("CloneOrUpdateBareMirror() error = %v", err)
	}

	if err := git.FetchRepository(repo, srcURL, false, transport.ProxyOptions{}, nil, 0); err != nil {
		t.Fatalf("FetchRepository() error = %v", err)
	}

	return srcRepo, srcURL, mirrorPath, mainCommit, featureCommit
}

func lastGoodRef(t *testing.T, mirrorPath string) (plumbing.Hash, error) {
	t.Helper()

	repo, err := gogit.PlainOpen(mirrorPath)
	if err != nil {
		t.Fatal(err)
	}

	ref, err := repo.Reference("refs/doco-cd/lastgood/default/web", false)
	if err != nil {
		return plumbing.ZeroHash, err
	}

	return ref.Hash(), nil
}

func TestPinLastGoodCreatesAndMovesRef(t *testing.T) {
	t.Parallel()

	_, _, mirrorPath, mainCommit, featureCommit := newLastGoodMirror(t)

	if err := git.PinLastGood(mirrorPath, "default/web", mainCommit.String()); err != nil {
		t.Fatalf("PinLastGood() error = %v", err)
	}

	if got, err := lastGoodRef(t, mirrorPath); err != nil || got != mainCommit {
		t.Fatalf("ref = %v (err %v), want %v", got, err, mainCommit)
	}

	if err := git.PinLastGood(mirrorPath, "default/web", featureCommit.String()); err != nil {
		t.Fatalf("second PinLastGood() error = %v", err)
	}

	if got, err := lastGoodRef(t, mirrorPath); err != nil || got != featureCommit {
		t.Fatalf("ref after move = %v (err %v), want %v", got, err, featureCommit)
	}
}

func TestPinLastGoodUnknownCommitKeepsRef(t *testing.T) {
	t.Parallel()

	_, _, mirrorPath, mainCommit, _ := newLastGoodMirror(t)

	if err := git.PinLastGood(mirrorPath, "default/web", mainCommit.String()); err != nil {
		t.Fatal(err)
	}

	if err := git.PinLastGood(mirrorPath, "default/web", strings.Repeat("ab", 20)); err == nil {
		t.Fatal("expected an error for a commit that is not in the mirror")
	}

	if got, err := lastGoodRef(t, mirrorPath); err != nil || got != mainCommit {
		t.Fatalf("ref = %v (err %v), want it unchanged at %v", got, err, mainCommit)
	}
}

// The commit only a deleted branch pointed to stays pinned after a fetch with prune.
func TestPinLastGoodSurvivesPrune(t *testing.T) {
	t.Parallel()

	srcRepo, srcURL, mirrorPath, _, featureCommit := newLastGoodMirror(t)

	if err := git.PinLastGood(mirrorPath, "default/web", featureCommit.String()); err != nil {
		t.Fatal(err)
	}

	if err := srcRepo.Storer.RemoveReference(plumbing.NewBranchReferenceName("feature")); err != nil {
		t.Fatal(err)
	}

	repo, err := gogit.PlainOpen(mirrorPath)
	if err != nil {
		t.Fatal(err)
	}

	if err := git.FetchRepository(repo, srcURL, false, transport.ProxyOptions{}, nil, 0); err != nil {
		t.Fatalf("FetchRepository() error = %v", err)
	}

	repo, err = gogit.PlainOpen(mirrorPath)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := repo.Reference("refs/remotes/origin/feature", false); err == nil {
		t.Fatal("expected the fetch to prune refs/remotes/origin/feature")
	}

	if got, err := lastGoodRef(t, mirrorPath); err != nil || got != featureCommit {
		t.Fatalf("ref after prune = %v (err %v), want %v", got, err, featureCommit)
	}

	if _, err := repo.CommitObject(featureCommit); err != nil {
		t.Fatalf("pinned commit no longer in mirror: %v", err)
	}
}

func TestPinLastGoodErrors(t *testing.T) {
	t.Parallel()

	_, _, mirrorPath, mainCommit, _ := newLastGoodMirror(t)

	if err := git.PinLastGood(mirrorPath, "noslash", mainCommit.String()); err == nil {
		t.Fatal("expected an error for an invalid object")
	}

	if err := git.PinLastGood(filepath.Join(t.TempDir(), "missing"), "default/web", mainCommit.String()); err == nil {
		t.Fatal("expected an error for a missing mirror")
	}

	// A directory in place of the ref file makes the ref write fail.
	if err := os.MkdirAll(filepath.Join(mirrorPath, "refs", "doco-cd", "lastgood", "default", "web", "x"), 0o700); err != nil {
		t.Fatal(err)
	}

	if err := git.PinLastGood(mirrorPath, "default/web", mainCommit.String()); err == nil {
		t.Fatal("expected an error when the ref cannot be written")
	}
}

func TestLastGoodRefName(t *testing.T) {
	t.Parallel()

	name, err := git.LastGoodRefName("a b/c")
	if err != nil {
		t.Fatalf("LastGoodRefName() error = %v", err)
	}

	if name != "refs/doco-cd/lastgood/a%20b/c" || strings.Contains(name.String(), " ") {
		t.Fatalf("LastGoodRefName() = %q", name)
	}

	for _, object := range []string{"", "noslash", "/x", "x/", "a/b/c"} {
		if _, err := git.LastGoodRefName(object); err == nil {
			t.Errorf("object %q must be rejected", object)
		}
	}

	// Escaped components that git still refuses as ref names.
	for _, object := range []string{"default/web.lock", "../x", "x/.."} {
		if _, err := git.LastGoodRefName(object); err == nil {
			t.Errorf("object %q must be rejected by ref validation", object)
		}
	}
}
