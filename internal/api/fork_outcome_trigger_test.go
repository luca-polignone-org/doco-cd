package api

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
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
	restAPI "github.com/kimdre/doco-cd/internal/restapi"
	"github.com/kimdre/doco-cd/internal/secretprovider"
)

// A poll/run reaches the deploys marked for trigger api in their outcome events, with RBAC off
// (API key only, no principal in the context).
func TestForkPollRunMarksAPITrigger(t *testing.T) {
	t.Parallel()

	log := logger.New(logger.LevelCritical)
	h := &Handler{
		appConfig: &app.Config{ApiSecret: "poll-secret", MaxPayloadSize: 1024}, // #nosec G101 -- test fixture.
		log:       log,
	}

	marked := make(chan bool, 1)
	h.controlPlaneRuns = newTestControlPlaneRuns(t, testControlPlaneRunsOptions{
		appConfig: h.appConfig,
		log:       log,
		pollRunner: func(ctx context.Context, _ poll.Config, _ *app.Config, _ container.MountPoint,
			_ command.Cli, _ *docker.ContextRegistry, _ *slog.Logger, _ notification.Metadata, _ secretprovider.SecretProvider, _ string,
		) error {
			marked <- hooks.APITriggered(ctx)
			return nil
		},
	})

	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, APIPath+"/poll/run", strings.NewReader(`[{"url":"https://example.com/repo.git"}]`))
	request.Header.Set(restAPI.KeyHeader, h.appConfig.ApiSecret)

	h.TriggerPollHandler(response, request)

	if response.Code >= http.StatusMultipleChoices {
		t.Fatalf("status = %d: %s", response.Code, response.Body.String())
	}

	select {
	case got := <-marked:
		if !got {
			t.Fatal("the poll/run context is not marked for trigger api")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the poll runner did not run")
	}
}
