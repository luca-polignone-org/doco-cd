package controlplane

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/docker/cli/cli/command"
	"github.com/moby/moby/api/types/container"

	"github.com/kimdre/doco-cd/internal/config"
	"github.com/kimdre/doco-cd/internal/config/app"
	"github.com/kimdre/doco-cd/internal/config/deploy"
	"github.com/kimdre/doco-cd/internal/config/poll"
	"github.com/kimdre/doco-cd/internal/docker"
	"github.com/kimdre/doco-cd/internal/logger"
	"github.com/kimdre/doco-cd/internal/notification"
	"github.com/kimdre/doco-cd/internal/rbac"
	"github.com/kimdre/doco-cd/internal/secretprovider"
)

func TestRestrictPollConfigs(t *testing.T) {
	t.Parallel()

	operatorDeployments := []*deploy.Config{{Name: "operator"}}
	known := []poll.Config{
		{SourceUrl: validPollSourceURL, Reference: "refs/heads/main", Interval: time.Minute, Deployments: operatorDeployments},
		{SourceUrl: validPollSourceURL, Reference: "v1.0.0", CustomTarget: "tagged"},
		{Source: config.SourceTypeOCI, SourceUrl: "ghcr.io/acme/app:1", Reference: "main"},
		{SourceUrl: "/srv/repos/app", Reference: "main"},
	}

	// A request names the operator entry without its deployments, which the body may not set.
	identical := known[0]
	identical.Deployments = nil

	for _, tc := range []struct {
		name    string
		req     poll.Config
		want    int // index in known, or -1 for an error
		wantErr string
	}{
		{name: "identical entry", req: identical, want: 0},
		{name: "short branch name against refs/heads", req: poll.Config{SourceUrl: validPollSourceURL, Reference: "main"}, want: 0},
		{name: "body options are ignored", req: poll.Config{SourceUrl: validPollSourceURL, Reference: "refs/heads/main", RunOnce: false, Interval: time.Second, Watch: true}, want: 0},
		{name: "tag with custom target", req: poll.Config{Source: "GIT", SourceUrl: validPollSourceURL, Reference: "v1.0.0", CustomTarget: "tagged"}, want: 1},
		{name: "oci source trims whitespace only", req: poll.Config{Source: config.SourceTypeOCI, SourceUrl: " ghcr.io/acme/app:1 ", Reference: "main"}, want: 2},
		{name: "local path normalized as git url", req: poll.Config{SourceUrl: "/srv/repos/../repos/app", Reference: "main"}, want: 3},
		{name: "configured short name against requested refs/heads", req: poll.Config{SourceUrl: "file:///srv/repos/app", Reference: "refs/heads/main"}, want: 3},
		{name: "different source", req: poll.Config{Source: config.SourceTypeOCI, SourceUrl: validPollSourceURL, Reference: "main"}, want: -1, wantErr: "index 0: not a configured poll target"},
		{name: "different url", req: poll.Config{SourceUrl: "https://github.com/acme/other.git", Reference: "main"}, want: -1, wantErr: "not a configured poll target"},
		{name: "different tag", req: poll.Config{SourceUrl: validPollSourceURL, Reference: "v2.0.0", CustomTarget: "tagged"}, want: -1, wantErr: "not a configured poll target"},
		{name: "different custom target", req: poll.Config{SourceUrl: validPollSourceURL, Reference: "main", CustomTarget: "other"}, want: -1, wantErr: "not a configured poll target"},
		{name: "inline deployments", req: poll.Config{SourceUrl: validPollSourceURL, Reference: "main", Deployments: []*deploy.Config{{Name: "evil"}}}, want: -1, wantErr: "index 0: inline deployments are not allowed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := RestrictPollConfigs(known, []poll.Config{tc.req})
			if tc.want < 0 {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) || got != nil {
					t.Fatalf("RestrictPollConfigs() = %+v, %v; want error containing %q", got, err, tc.wantErr)
				}

				return
			}

			if err != nil {
				t.Fatalf("RestrictPollConfigs() error = %v", err)
			}

			want := known[tc.want]
			if len(got) != 1 || got[0].SourceUrl != want.SourceUrl || got[0].Reference != want.Reference ||
				got[0].Interval != want.Interval || got[0].RunOnce != want.RunOnce || len(got[0].Deployments) != len(want.Deployments) {
				t.Fatalf("RestrictPollConfigs() = %+v, want the operator entry %+v", got, want)
			}
		})
	}

	t.Run("operator deployments are returned, not the body ones", func(t *testing.T) {
		t.Parallel()

		got, err := RestrictPollConfigs(known, []poll.Config{{SourceUrl: validPollSourceURL, Reference: "main"}})
		if err != nil {
			t.Fatalf("RestrictPollConfigs() error = %v", err)
		}

		if len(got[0].Deployments) != 1 || got[0].Deployments[0] != operatorDeployments[0] {
			t.Fatalf("deployments = %+v, want the operator's", got[0].Deployments)
		}
	})

	t.Run("second entry reports its index", func(t *testing.T) {
		t.Parallel()

		_, err := RestrictPollConfigs(known, []poll.Config{identical, {SourceUrl: "https://example.com/x.git"}})
		if err == nil || !strings.HasPrefix(err.Error(), "index 1: ") {
			t.Fatalf("error = %v, want index 1", err)
		}
	})
}

func TestTriggerPollCarriesPrincipal(t *testing.T) {
	t.Parallel()

	want := rbac.Principal{Subject: "alice", Groups: []string{"devs"}}

	for _, tc := range []struct {
		name      string
		wait      bool
		principal bool
	}{
		{name: "async with principal", wait: false, principal: true},
		{name: "sync with principal", wait: true, principal: true},
		{name: "async without principal", wait: false, principal: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := make(chan context.Context, 1)
			log := logger.New(logger.LevelCritical)
			runs := newTestControlPlaneRuns(t, testControlPlaneRunsOptions{
				appConfig: &app.Config{},
				log:       log,
				pollRunner: func(ctx context.Context, _ poll.Config, _ *app.Config, _ container.MountPoint,
					_ command.Cli, _ *docker.ContextRegistry, _ *slog.Logger, _ notification.Metadata, _ secretprovider.SecretProvider, _ string,
				) error {
					got <- ctx

					return nil
				},
			})

			ctx := t.Context()
			if tc.principal {
				ctx = rbac.WithPrincipal(ctx, want)
			}

			if _, err := runs.TriggerPoll(ctx, []poll.Config{{SourceUrl: validPollSourceURL}}, tc.wait, log.Logger); err != nil {
				t.Fatalf("TriggerPoll() error = %v", err)
			}

			var runCtx context.Context
			select {
			case runCtx = <-got:
			case <-time.After(5 * time.Second):
				t.Fatal("poll runner was not called")
			}

			runs.background.Wait()

			p, ok := rbac.PrincipalFrom(runCtx)
			if ok != tc.principal {
				t.Fatalf("principal present = %v, want %v", ok, tc.principal)
			}

			if tc.principal && (p.Subject != want.Subject || len(p.Groups) != 1 || p.Groups[0] != "devs") {
				t.Fatalf("principal = %+v, want %+v", p, want)
			}
		})
	}
}
