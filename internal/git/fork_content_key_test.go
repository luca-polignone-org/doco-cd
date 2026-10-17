package git

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
)

func commitFiles(t *testing.T, repo *gogit.Repository, dir string, files map[string]string) string {
	t.Helper()

	for rel, content := range files {
		path := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}

		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	wt, err := repo.Worktree()
	if err != nil {
		t.Fatal(err)
	}

	if err = wt.AddWithOptions(&gogit.AddOptions{All: true}); err != nil {
		t.Fatal(err)
	}

	h, err := wt.Commit("c", &gogit.CommitOptions{Author: &object.Signature{Name: "t", Email: "t@example.com", When: time.Now()}})
	if err != nil {
		t.Fatal(err)
	}

	return h.String()
}

func TestTreeHashAt(t *testing.T) {
	dir := t.TempDir()

	repo, err := gogit.PlainInit(dir, false)
	if err != nil {
		t.Fatal(err)
	}

	c1 := commitFiles(t, repo, dir, map[string]string{"worklog/compose.yaml": "a\n", "gymlog/compose.yaml": "b\n"})
	c2 := commitFiles(t, repo, dir, map[string]string{"gymlog/compose.yaml": "b2\n"})
	c3 := commitFiles(t, repo, dir, map[string]string{"worklog/compose.yaml": "a2\n"})

	gitDir := filepath.Join(dir, ".git")

	h1, err := TreeHashAt(gitDir, c1, "worklog")
	if err != nil {
		t.Fatal(err)
	}

	h2, _ := TreeHashAt(gitDir, c2, "worklog")
	h3, _ := TreeHashAt(gitDir, c3, "worklog")

	if h1 != h2 {
		t.Fatalf("a commit of another directory changed the tree: %s != %s", h1, h2)
	}

	if h1 == h3 {
		t.Fatal("a commit of the directory must change the tree")
	}

	root, err := TreeHashAt(gitDir, c1, ".")
	if err != nil || root == "" || root == h1 {
		t.Fatalf("root tree = %q, %v", root, err)
	}

	if _, err = TreeHashAt(gitDir, c1, "missing"); err == nil {
		t.Fatal("a missing directory must be an error")
	}

	if _, err = TreeHashAt(gitDir, "0000000000000000000000000000000000000000", "."); err == nil {
		t.Fatal("a missing commit must be an error")
	}

	if _, err = TreeHashAt(filepath.Join(dir, "nope"), c1, "."); err == nil {
		t.Fatal("a missing mirror must be an error")
	}
}

func TestTreeHashAtTreeError(t *testing.T) {
	dir := t.TempDir()

	repo, err := gogit.PlainInit(dir, false)
	if err != nil {
		t.Fatal(err)
	}

	c := commitFiles(t, repo, dir, map[string]string{"a": "a\n"})

	saved := commitTree
	commitTree = func(*object.Commit) (*object.Tree, error) { return nil, errors.New("boom") }

	t.Cleanup(func() { commitTree = saved })

	if _, err = TreeHashAt(filepath.Join(dir, ".git"), c, "."); err == nil {
		t.Fatal("a tree that cannot be read must be an error")
	}
}
