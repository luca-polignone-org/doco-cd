package mcp

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/docker/cli/cli/command"
	"github.com/moby/moby/api/types/container"

	"github.com/kimdre/doco-cd/internal/config/app"
	"github.com/kimdre/doco-cd/internal/config/poll"
	"github.com/kimdre/doco-cd/internal/docker"
	"github.com/kimdre/doco-cd/internal/hooks"
	"github.com/kimdre/doco-cd/internal/logger"
	"github.com/kimdre/doco-cd/internal/notification"
	"github.com/kimdre/doco-cd/internal/secretprovider"
)

// A trigger_poll reaches the deploys marked for trigger api in their outcome events, with RBAC off
// (API key only, no principal in the context).
func TestForkTriggerPollMarksAPITrigger(t *testing.T) {
	log := logger.New(logger.LevelCritical)
	h := &Handler{log: log}

	marked := make(chan bool, 1)
	h.controlPlaneRuns = newTestControlPlaneRuns(t, testControlPlaneRunsOptions{
		appConfig: &app.Config{},
		log:       log,
		pollRunner: func(ctx context.Context, _ poll.Config, _ *app.Config, _ container.MountPoint,
			_ command.Cli, _ *docker.ContextRegistry, _ *slog.Logger, _ notification.Metadata, _ secretprovider.SecretProvider, _ string,
		) error {
			marked <- hooks.APITriggered(ctx)
			return nil
		},
	})
	server, _ := newMCPTestServerWithHandler(t, true, testMCPAPIKey, 1024, h)
	session := connectMCPTestClient(t, server)

	callMCPTool(t, session, "trigger_poll", map[string]any{
		"configs": []any{map[string]any{"url": validPollSourceURL}},
	})

	select {
	case got := <-marked:
		if !got {
			t.Fatal("the trigger_poll context is not marked for trigger api")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the poll runner did not run")
	}
}
