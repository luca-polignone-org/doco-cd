package app

import (
	"sync"
	"testing"
	"time"

	"github.com/kimdre/doco-cd/internal/config/poll"
)

func TestForkPollConfig_WithoutLiveIsTheField(t *testing.T) {
	t.Parallel()

	cfg := &Config{PollConfig: []poll.Config{{CustomTarget: "a"}}}

	if got := cfg.CurrentPollConfig(); len(got) != 1 || got[0].CustomTarget != "a" {
		t.Fatalf("CurrentPollConfig() = %+v, want PollConfig", got)
	}

	cfg.SetPollConfig([]poll.Config{{CustomTarget: "b"}})

	if cfg.PollConfig[0].CustomTarget != "b" || cfg.CurrentPollConfig()[0].CustomTarget != "b" {
		t.Fatal("SetPollConfig() without EnableLivePollConfig must replace PollConfig")
	}
}

func TestForkPollConfig_LiveIsReplacedUnderLock(t *testing.T) {
	t.Parallel()

	startup := []poll.Config{{CustomTarget: "a"}}
	cfg := &Config{PollConfig: startup}
	cfg.EnableLivePollConfig()
	cfg.EnableLivePollConfig() // idempotent

	if got := cfg.CurrentPollConfig(); len(got) != 1 || got[0].CustomTarget != "a" {
		t.Fatalf("CurrentPollConfig() = %+v, want the startup entries", got)
	}

	// Copies of the config, as handlers keep, see the replacement too.
	copied := *cfg

	var wg sync.WaitGroup

	for range 4 {
		wg.Go(func() {
			deadline := time.Now().Add(20 * time.Millisecond)
			for time.Now().Before(deadline) {
				for _, c := range copied.CurrentPollConfig() {
					_ = c.CustomTarget
				}
			}
		})
	}

	for range 100 {
		cfg.SetPollConfig([]poll.Config{{CustomTarget: "b"}, {CustomTarget: "c"}})
	}

	wg.Wait()

	if got := copied.CurrentPollConfig(); len(got) != 2 || got[1].CustomTarget != "c" {
		t.Fatalf("CurrentPollConfig() = %+v, want the replacement", got)
	}

	if len(cfg.PollConfig) != 1 || cfg.PollConfig[0].CustomTarget != "a" {
		t.Fatal("SetPollConfig() must not write PollConfig once live")
	}
}
