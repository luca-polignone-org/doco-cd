package main

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/kimdre/doco-cd/internal/hooks"
	"github.com/kimdre/doco-cd/internal/logger"
	"github.com/kimdre/doco-cd/internal/selfupdate"
	"github.com/kimdre/doco-cd/internal/stages"
)

// readSelfUpdateOutcomes returns the events the outbox in dir holds.
func readSelfUpdateOutcomes(t *testing.T, dir string) []hooks.Event {
	t.Helper()

	files, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		t.Fatal(err)
	}

	events := make([]hooks.Event, 0, len(files))

	for _, file := range files {
		raw, err := os.ReadFile(file) //nolint:gosec // test files under t.TempDir()
		if err != nil {
			t.Fatal(err)
		}

		var e struct {
			Event hooks.Event `json:"event"`
		}

		if err := json.Unmarshal(raw, &e); err != nil {
			t.Fatalf("decode %s: %v", raw, err)
		}

		events = append(events, e.Event)
	}

	return events
}

// The successor of a self-update announces deployed (the predecessor stayed silent on
// ErrHandover); a self-update rolled back is failed. Not parallel: the outbox is process-wide.
func TestSelfUpdateReporterEmitsOutcome(t *testing.T) {
	log := logger.New(slog.LevelError)

	start := func(t *testing.T) string {
		t.Helper()

		dir := filepath.Join(t.TempDir(), "outbox")

		o, err := hooks.NewOutbox(hooks.OutboxConfig{Dir: dir, URL: "http://127.0.0.1:1/outcome"})
		if err != nil {
			t.Fatal(err)
		}

		stages.SetOutbox(o)
		t.Cleanup(func() { stages.SetOutbox(nil) })

		return dir
	}

	newRecord := func(state selfupdate.State) selfupdate.Record {
		record := handedOverRecord("http://127.0.0.1:1", state)
		record.Context = "nas"
		record.Source.RepoName = "owner/infra"
		record.Source.Reference = "refs/heads/main"

		return record
	}

	t.Run("success is deployed", func(t *testing.T) {
		dir := start(t)

		newSelfUpdateReporter(nil, nil).reportSuccess(t.Context(), log, newRecord(selfupdate.StateFinalising))

		events := readSelfUpdateOutcomes(t, dir)
		if len(events) != 1 {
			t.Fatalf("events %+v, want one", events)
		}

		ev := events[0]
		if ev.Outcome != hooks.OutcomeDeployed || ev.Object != "nas/self" || ev.Commit != selfUpdateTestCommitSHA ||
			ev.Repository != "owner/infra" || ev.Ref != "refs/heads/main" || ev.DurationMS < 90_000 || ev.Error != "" {
			t.Fatalf("event %+v", ev)
		}
	})

	t.Run("failure is failed, without a commit status no duration", func(t *testing.T) {
		dir := start(t)

		record := newRecord(selfupdate.StateRolledBack)
		record.Source.CommitStatus = nil
		record.Error = "successor did not become healthy"

		newSelfUpdateReporter(nil, nil).reportFailure(t.Context(), log, record)

		events := readSelfUpdateOutcomes(t, dir)
		if len(events) != 1 || events[0].Outcome != hooks.OutcomeFailed || events[0].Error != record.Error ||
			events[0].Object != "nas/self" || events[0].DurationMS != 0 {
			t.Fatalf("events %+v", events)
		}
	})

	t.Run("failure with a commit status has a duration", func(t *testing.T) {
		dir := start(t)

		newSelfUpdateReporter(nil, nil).reportFailure(t.Context(), log, newRecord(selfupdate.StateRolledBack))

		events := readSelfUpdateOutcomes(t, dir)
		if len(events) != 1 || events[0].Outcome != hooks.OutcomeFailed || events[0].DurationMS < 90_000 ||
			events[0].Error != "the new version did not become healthy" {
			t.Fatalf("events %+v", events)
		}
	})
}
