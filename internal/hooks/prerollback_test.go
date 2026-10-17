package hooks

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

const decideTimeout = 5 * time.Second

func testClient(url string) *PreRollbackClient {
	return &PreRollbackClient{
		URL: url,
		Signer: Signer{
			Secret: testSecret,
			Now:    time.Now,
			NewID:  func() string { return "id" },
		},
	}
}

var testReq = PreRollbackRequest{
	Object:         "default/app",
	FailedCommit:   "bad",
	LastGoodCommit: "good",
	Stage:          "deploy",
	Error:          "boom",
}

// verifyingServer checks signature and body, then answers with status and body.
func verifyingServer(t *testing.T, status int, resp string) *httptest.Server {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)

		if err := Verify(testSecret, r.Header.Get("X-Doco-Signature"), body, time.Now(), time.Minute); err != nil {
			t.Errorf("signature check: %v", err)
		}

		var got PreRollbackRequest
		if err := json.Unmarshal(body, &got); err != nil {
			t.Errorf("body is not a PreRollbackRequest: %v", err)
		}

		if got.Schema != "doco-cd.pre_rollback/v1" {
			t.Errorf("schema = %q", got.Schema)
		}

		if got.Object != testReq.Object || got.FailedCommit != testReq.FailedCommit ||
			got.LastGoodCommit != testReq.LastGoodCommit || got.Stage != testReq.Stage || got.Error != testReq.Error {
			t.Errorf("unexpected body: %+v", got)
		}

		if r.Header.Get("X-Doco-Delivery") != "id" {
			t.Errorf("missing X-Doco-Delivery")
		}

		w.WriteHeader(status)

		if resp != "" {
			_, _ = w.Write([]byte(resp))
		}
	}))
	t.Cleanup(srv.Close)

	return srv
}

func TestDecide(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
		want   Decision
	}{
		{"proceed", 200, `{"decision":"proceed"}`, DecisionProceed},
		{"block", 200, `{"decision":"block"}`, DecisionBlock},
		{"204 without body", 204, "", DecisionInvalid},
		{"unknown decision", 200, `{"decision":"maybe"}`, DecisionInvalid},
		{"not JSON", 200, `not json`, DecisionInvalid},
		{"500", 500, `{"decision":"proceed"}`, DecisionHTTPError},
		{"redirect status is not 2xx", 302, "", DecisionHTTPError},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := verifyingServer(t, tt.status, tt.body)

			if got := testClient(srv.URL).Decide(context.Background(), testReq, decideTimeout); got != tt.want {
				t.Errorf("Decide() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestDecideConnectionRefused(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL

	srv.Close()

	if got := testClient(url).Decide(context.Background(), testReq, decideTimeout); got != DecisionHTTPError {
		t.Errorf("Decide() = %q, want http_error", got)
	}
}

func TestDecideInvalidURL(t *testing.T) {
	if got := testClient("http://bad\x7fhost/").Decide(context.Background(), testReq, decideTimeout); got != DecisionHTTPError {
		t.Errorf("Decide() = %q, want http_error", got)
	}
}

func TestDecideTimeoutBeforeHeaders(t *testing.T) {
	release := make(chan struct{})

	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) { <-release }))
	// Cleanups run last-in first-out: release the handler before Close waits for it.
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(release) })

	if got := testClient(srv.URL).Decide(context.Background(), testReq, 50*time.Millisecond); got != DecisionTimeout {
		t.Errorf("Decide() = %q, want timeout", got)
	}
}

func TestDecideTimeoutWhileReadingBody(t *testing.T) {
	release := make(chan struct{})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-release
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(release) })

	if got := testClient(srv.URL).Decide(context.Background(), testReq, 50*time.Millisecond); got != DecisionTimeout {
		t.Errorf("Decide() = %q, want timeout", got)
	}
}

func TestDecideUnconfigured(t *testing.T) {
	var calls atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) { calls.Add(1) }))
	t.Cleanup(srv.Close)

	// Empty URL: no request, even with a server around.
	if got := testClient("").Decide(context.Background(), testReq, decideTimeout); got != DecisionUnconfigured {
		t.Errorf("empty URL: Decide() = %q, want unconfigured", got)
	}

	if got := (*PreRollbackClient)(nil).Decide(context.Background(), testReq, decideTimeout); got != DecisionUnconfigured {
		t.Errorf("nil receiver: Decide() = %q, want unconfigured", got)
	}

	if calls.Load() != 0 {
		t.Errorf("unconfigured client sent %d requests", calls.Load())
	}
}

func TestDecideUsesInjectedHTTPClient(t *testing.T) {
	srv := verifyingServer(t, 200, `{"decision":"proceed"}`)
	c := testClient(srv.URL)
	c.HTTPClient = srv.Client()

	if got := c.Decide(context.Background(), testReq, decideTimeout); got != DecisionProceed {
		t.Errorf("Decide() = %q, want proceed", got)
	}
}
