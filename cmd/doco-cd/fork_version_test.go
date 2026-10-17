package main

import "testing"

func TestForkBaseVersion(t *testing.T) {
	t.Parallel()

	for _, testCase := range []struct{ version, want string }{
		{"v0.127.0-poli.3", "v0.127.0"},
		{"v0.127.0-poli.1", "v0.127.0"},
		// Upstream and dev versions are returned unchanged, so the upstream comparison holds.
		{"v0.127.0", "v0.127.0"},
		{"v0.127.0-rc1", "v0.127.0-rc1"},
		{"dev", "dev"},
		{"dev-build.abc1234", "dev-build.abc1234"},
		{"", ""},
	} {
		if got := forkBaseVersion(testCase.version); got != testCase.want {
			t.Errorf("forkBaseVersion(%q) = %q, want %q", testCase.version, got, testCase.want)
		}
	}
}

func TestForkMergeNotice(t *testing.T) {
	t.Parallel()

	if got, want := forkMergeNotice("v0.127.0-poli.1"), " (fork luca-polignone-org/doco-cd: merge upstream needed)"; got != want {
		t.Errorf("forkMergeNotice(fork version) = %q, want %q", got, want)
	}

	// A non-fork version keeps the upstream message unchanged.
	for _, version := range []string{"v0.127.0", "dev", ""} {
		if got := forkMergeNotice(version); got != "" {
			t.Errorf("forkMergeNotice(%q) = %q, want empty", version, got)
		}
	}
}
