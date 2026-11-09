package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"

	"github.com/kimdre/doco-cd/internal/config"
	"github.com/kimdre/doco-cd/internal/config/app"
	"github.com/kimdre/doco-cd/internal/config/poll"
	"github.com/kimdre/doco-cd/internal/graceful"
	"github.com/kimdre/doco-cd/internal/logger"
)

// POLL_CONFIG_FILE reload (Portico fork, poli.7). The Portico reconciler rewrites the file whenever
// platform-config changes; upstream reads it once. The poll jobs are kept in a poll.JobSet, and a
// watcher (poll.WatchConfigFile) re-reads the file when it changes, installs the new entries for
// the API, MCP and webhook handlers (app.Config.SetPollConfig) and makes the jobs follow them. A
// stopped job ends after its run in progress (poll.Job.Stop in PollHandler).

// pollConfigFilePath returns the poll configuration file to reload, or "" without one.
func pollConfigFilePath() string {
	return strings.TrimSpace(os.Getenv("POLL_CONFIG_FILE"))
}

// newPollJobSet returns the set that starts the poll jobs of h with startStoppablePoll.
func newPollJobSet(ctx context.Context, h *orchestrationHandler, wg *sync.WaitGroup) *poll.JobSet {
	return poll.NewJobSet(func(cfg poll.Config, stop <-chan struct{}) error {
		return startStoppablePoll(ctx, h, cfg, stop, wg)
	})
}

// enablePollConfigFileReload starts the POLL_CONFIG_FILE watcher. It must run before anything reads
// c concurrently, and after the jobs of c.PollConfig were applied to jobs.
func enablePollConfigFileReload(ctx context.Context, c *app.Config, jobs *poll.JobSet, wg *sync.WaitGroup, log *slog.Logger) error {
	path := pollConfigFilePath()
	if path == "" {
		return nil
	}

	f, err := config.NewReloadingFile("POLL_CONFIG", path, log)
	if err != nil {
		return fmt.Errorf("POLL_CONFIG_FILE reload: %w", err)
	}

	c.EnableLivePollConfig()

	apply := func(configs []poll.Config) error {
		c.SetPollConfig(configs)

		changes, err := jobs.Apply(configs)

		log.Info("poll configuration reloaded from POLL_CONFIG_FILE",
			slog.Int("entries", len(configs)),
			slog.Int("jobs_started", changes.Started),
			slog.Int("jobs_stopped", changes.Stopped),
			slog.Int("jobs_kept", changes.Kept),
			slog.Any("poll_config", logger.BuildSliceLogValue(configs, "Deployments.Internal")))

		return err
	}

	// c.PollConfigFile holds the content parsed at start: a change made since then is applied on
	// the first check.
	graceful.SafeGo(wg, log, func() {
		poll.WatchConfigFile(ctx, f, c.PollConfigFile, poll.ConfigFileReloadInterval, apply, log)
	})

	return nil
}

// pollJobStopped reports whether the entry of pollJob left the poll configuration, logging it.
func pollJobStopped(log *slog.Logger, pollJob *poll.Job) bool {
	select {
	case <-pollJob.Stop:
		log.Info("poll entry removed from the poll configuration, poll job stopped",
			slog.String("reference", pollJob.Config.Reference),
			slog.String("target", pollJob.Config.CustomTarget))

		return true
	default:
		return false
	}
}
