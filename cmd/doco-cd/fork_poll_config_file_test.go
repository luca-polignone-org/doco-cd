package main

import (
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/docker/cli/cli/command"
	"github.com/moby/moby/api/types/container"

	"github.com/kimdre/doco-cd/internal/config/app"
	"github.com/kimdre/doco-cd/internal/config/poll"
	"github.com/kimdre/doco-cd/internal/docker"
	"github.com/kimdre/doco-cd/internal/logger"
	"github.com/kimdre/doco-cd/internal/notification"
	"github.com/kimdre/doco-cd/internal/secretprovider"
)

// forkPollHandler returns a handler whose poll runs report on started and block until release is
// closed, recording whether their context was canceled.
func forkPollHandler(t *testing.T, started chan<- struct{}, release <-chan struct{}, canceled *bool, mu *sync.Mutex) *orchestrationHandler {
	t.Helper()

	log := logger.New(logger.LevelCritical)

	return &orchestrationHandler{
		log: log,
		controlPlaneRuns: newTestControlPlaneRuns(t, testControlPlaneRunsOptions{
			log: log,
			pollRunner: func(ctx context.Context, _ poll.Config, _ *app.Config, _ container.MountPoint,
				_ command.Cli, _ *docker.ContextRegistry, _ *slog.Logger, _ notification.Metadata, _ secretprovider.SecretProvider,
				_ string,
			) error {
				started <- struct{}{}

				<-release

				mu.Lock()
				*canceled = ctx.Err() != nil
				mu.Unlock()

				return nil
			},
		}),
	}
}

func forkPollEntry() poll.Config {
	return poll.Config{
		SourceUrl:    "https://github.com/kimdre/doco-cd_tests.git",
		Reference:    "main",
		Interval:     time.Hour,
		CustomTarget: "medicinali",
	}
}

func TestForkPollJobStopsAfterTheRunInProgress(t *testing.T) {
	started := make(chan struct{}, 2)
	release := make(chan struct{})

	var (
		mu       sync.Mutex
		canceled bool
	)

	h := forkPollHandler(t, started, release, &canceled, &mu)

	var wg sync.WaitGroup

	set := newPollJobSet(t.Context(), h, &wg)

	if _, err := set.Apply([]poll.Config{forkPollEntry()}); err != nil {
		t.Fatal(err)
	}

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("the startup run did not start")
	}

	// The entry leaves the configuration while its run is in progress.
	changes, err := set.Apply([]poll.Config{})
	if err != nil || changes.Stopped != 1 {
		t.Fatalf("Apply() = %+v, %v, want one stopped job", changes, err)
	}

	close(release)

	done := make(chan struct{})

	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the poll job did not end after its run, though its entry was removed")
	}

	mu.Lock()
	defer mu.Unlock()

	if canceled {
		t.Fatal("the run in progress was canceled: it must complete")
	}

	select {
	case <-started:
		t.Fatal("a removed entry ran again")
	default:
	}
}

func TestForkPollJobStoppedBeforeStartDoesNotRun(t *testing.T) {
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	close(release)

	var (
		mu       sync.Mutex
		canceled bool
	)

	h := forkPollHandler(t, started, release, &canceled, &mu)

	stop := make(chan struct{})
	close(stop)

	var wg sync.WaitGroup

	if err := startStoppablePoll(t.Context(), h, forkPollEntry(), stop, &wg); err != nil {
		t.Fatal(err)
	}

	wg.Wait()

	select {
	case <-started:
		t.Fatal("a job stopped before its first run must not run")
	default:
	}
}

func TestForkPollConfigFilePath(t *testing.T) {
	t.Setenv("POLL_CONFIG_FILE", " /config/poll.yaml \n")

	if got := pollConfigFilePath(); got != "/config/poll.yaml" {
		t.Fatalf("pollConfigFilePath() = %q, want the trimmed path", got)
	}

	t.Setenv("POLL_CONFIG_FILE", "")

	if err := enablePollConfigFileReload(t.Context(), &app.Config{}, nil, &sync.WaitGroup{}, slog.New(slog.DiscardHandler)); err != nil {
		t.Fatalf("without POLL_CONFIG_FILE: enablePollConfigFileReload() error = %v, want nil", err)
	}

	t.Setenv("POLL_CONFIG_FILE", "/nonexistent/poll.yaml")

	if err := enablePollConfigFileReload(t.Context(), &app.Config{}, nil, &sync.WaitGroup{}, slog.New(slog.DiscardHandler)); err == nil {
		t.Fatal("with a missing file: enablePollConfigFileReload() error = nil, want an error")
	}
}
