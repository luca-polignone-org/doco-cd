package hooks

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// These tests are not parallel: the outbox metrics are package globals, and they are
// measured as deltas.

var outboxT0 = time.Date(2026, 10, 17, 9, 0, 0, 0, time.UTC)

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

type received struct {
	event     Event
	body      []byte
	signature string
	delivery  string
}

// outcomeServer verifies the signature of every request, records it and answers with the
// status that status returns for the event (0 means 200).
type outcomeServer struct {
	*httptest.Server

	t        *testing.T
	clock    *fakeClock
	status   func(n int, e Event) int
	location string // Location header of every answer, when set

	mu   sync.Mutex
	reqs []received
}

func newOutcomeServer(t *testing.T, clock *fakeClock, status func(n int, e Event) int) *outcomeServer {
	t.Helper()

	return newRedirectingServer(t, clock, status, "")
}

// newRedirectingServer is newOutcomeServer with a Location header on every answer. The
// fields are set before the server starts, so its handlers read them without a race.
func newRedirectingServer(t *testing.T, clock *fakeClock, status func(n int, e Event) int, location string) *outcomeServer {
	t.Helper()

	s := &outcomeServer{t: t, clock: clock, status: status, location: location}
	s.Server = httptest.NewServer(http.HandlerFunc(s.handle))
	t.Cleanup(s.Close)

	return s
}

func (s *outcomeServer) handle(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)

	if err := Verify(testSecret, r.Header.Get("X-Doco-Signature"), body, s.clock.Now(), time.Minute); err != nil {
		s.t.Errorf("signature check: %v", err)
	}

	var ev Event
	if err := json.Unmarshal(body, &ev); err != nil {
		s.t.Errorf("body is not an Event: %v", err)
	}

	s.mu.Lock()
	n := len(s.reqs)
	s.reqs = append(s.reqs, received{
		event: ev, body: body,
		signature: r.Header.Get("X-Doco-Signature"), delivery: r.Header.Get("X-Doco-Delivery"),
	})
	s.mu.Unlock()

	code := http.StatusOK

	if s.status != nil {
		if c := s.status(n, ev); c != 0 {
			code = c
		}
	}

	if s.location != "" {
		w.Header().Set("Location", s.location)
	}

	w.WriteHeader(code)
	_, _ = w.Write([]byte("leaked-response-body"))
}

func (s *outcomeServer) received() []received {
	s.mu.Lock()
	defer s.mu.Unlock()

	return slices.Clone(s.reqs)
}

func (s *outcomeServer) ids() []string {
	var out []string
	for _, r := range s.received() {
		out = append(out, r.event.EventID)
	}

	return out
}

type testOutbox struct {
	*Outbox

	clock *fakeClock
	log   *bytes.Buffer
}

func newTestOutbox(t *testing.T, dir, url string, mutate func(*OutboxConfig)) *testOutbox {
	t.Helper()

	clock := &fakeClock{now: outboxT0}
	logBuf := &bytes.Buffer{}
	ids := 0

	cfg := OutboxConfig{
		Dir: dir,
		URL: url,
		Signer: Signer{Secret: testSecret, Now: clock.Now, NewID: func() string {
			ids++
			return fmt.Sprintf("delivery-%d", ids)
		}},
		Now:  clock.Now,
		Rand: func() float64 { return 0.5 },
		Log:  slog.New(slog.NewTextHandler(logBuf, &slog.HandlerOptions{Level: slog.LevelDebug})),
	}

	if mutate != nil {
		mutate(&cfg)
	}

	o, err := NewOutbox(cfg)
	if err != nil {
		t.Fatalf("NewOutbox: %v", err)
	}

	return &testOutbox{Outbox: o, clock: clock, log: logBuf}
}

func testEvent(id, object string) Event {
	e := NewEvent(object)
	e.EventID = id
	e.EmittedAt = outboxT0
	e.Outcome = OutcomeDeployed

	return e
}

func mustEnqueue(t *testing.T, o *testOutbox, e Event) {
	t.Helper()

	if err := o.Enqueue(e); err != nil {
		t.Fatalf("Enqueue(%s): %v", e.EventID, err)
	}
}

func readEntry(t *testing.T, path string) entry {
	t.Helper()

	data, err := os.ReadFile(path) // #nosec G304
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}

	var e entry
	if err = json.Unmarshal(data, &e); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}

	return e
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func delivered(result string) float64 {
	return testutil.ToFloat64(outcomeDelivered.WithLabelValues(result))
}

func dropped(reason string) float64 {
	return testutil.ToFloat64(outcomeDropped.WithLabelValues(reason))
}

func gathered(t *testing.T, name string) bool {
	t.Helper()

	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}

	for _, f := range families {
		if f.GetName() == name {
			return true
		}
	}

	return false
}

// This test must stay the first one of the package to call NewOutbox: it checks that the
// metrics are not registered before.
func TestOutboxMetricsRegisteredByNewOutbox(t *testing.T) {
	if gathered(t, "doco_cd_outcome_queue_depth") {
		t.Fatal("outcome metrics are registered before NewOutbox")
	}

	newTestOutbox(t, t.TempDir(), "http://127.0.0.1:1", nil)

	if !gathered(t, "doco_cd_outcome_queue_depth") {
		t.Error("doco_cd_outcome_queue_depth is not registered after NewOutbox")
	}

	// A second outbox must not register them again (MustRegister would panic).
	newTestOutbox(t, t.TempDir(), "http://127.0.0.1:1", nil)
}

func TestOutboxDelivers2xx(t *testing.T) {
	dir := t.TempDir()
	o := newTestOutbox(t, dir, "http://unused", nil)
	srv := newOutcomeServer(t, o.clock, nil)
	o.cfg.URL = srv.URL

	before := delivered("ok")

	ev := testEvent("e1", "default/app")
	mustEnqueue(t, o, ev)

	file := filepath.Join(dir, "e1.json")
	if got := readEntry(t, file); got.Version != 1 || got.Object != "default/app" || got.Attempts != 0 ||
		!got.FirstEnqueued.Equal(outboxT0) || !got.NextAttempt.Equal(outboxT0) || got.Event.EventID != "e1" {
		t.Fatalf("unexpected file content: %+v", got)
	}

	if v := testutil.ToFloat64(outcomeQueueDepth); v != 1 {
		t.Errorf("queue depth after Enqueue = %v, want 1", v)
	}

	wait, sent := o.deliverOnce(context.Background())
	if !sent || wait != maxWait {
		t.Errorf("deliverOnce = (%v, %v), want (%v, true)", wait, sent, maxWait)
	}

	if exists(file) {
		t.Error("event file still there after a 2xx")
	}

	if got := delivered("ok") - before; got != 1 {
		t.Errorf("delivered_total{result=ok} delta = %v, want 1", got)
	}

	if v := testutil.ToFloat64(outcomeQueueDepth); v != 0 {
		t.Errorf("queue depth after delivery = %v, want 0", v)
	}

	reqs := srv.received()
	if len(reqs) != 1 || reqs[0].event.EventID != "e1" || reqs[0].event.Schema != OutcomeSchema {
		t.Fatalf("unexpected requests: %+v", reqs)
	}

	if _, sent = o.deliverOnce(context.Background()); sent {
		t.Error("deliverOnce sent something from an empty outbox")
	}
}

func TestOutboxRetryThenDeliver(t *testing.T) {
	dir := t.TempDir()
	o := newTestOutbox(t, dir, "http://unused", nil)
	srv := newOutcomeServer(t, o.clock, func(n int, _ Event) int {
		if n == 0 {
			return http.StatusServiceUnavailable
		}

		return http.StatusOK
	})
	o.cfg.URL = srv.URL

	beforeRetry, beforeOK := delivered("retry"), delivered("ok")

	mustEnqueue(t, o, testEvent("e1", "default/app"))

	wait, sent := o.deliverOnce(context.Background())
	if !sent {
		t.Fatal("first round sent nothing")
	}

	// MinBackoff 1s, jitter 0.5+0.5*0.5 = 0.75.
	wantNext := outboxT0.Add(750 * time.Millisecond)
	if wait != 750*time.Millisecond {
		t.Errorf("wait after 503 = %v, want 750ms", wait)
	}

	file := filepath.Join(dir, "e1.json")

	got := readEntry(t, file)
	if got.Attempts != 1 || !got.NextAttempt.Equal(wantNext) || !got.FirstEnqueued.Equal(outboxT0) {
		t.Fatalf("after 503: attempts=%d next=%v first=%v", got.Attempts, got.NextAttempt, got.FirstEnqueued)
	}

	if d := delivered("retry") - beforeRetry; d != 1 {
		t.Errorf("delivered_total{result=retry} delta = %v, want 1", d)
	}

	// Not due yet: nothing is sent and the wait shrinks.
	o.clock.Advance(500 * time.Millisecond)

	if wait, sent = o.deliverOnce(context.Background()); sent || wait != 250*time.Millisecond {
		t.Errorf("deliverOnce before next_attempt = (%v, %v), want (250ms, false)", wait, sent)
	}

	// A whole second later, so that the signature timestamp changes too.
	o.clock.Advance(1500 * time.Millisecond)

	if _, sent = o.deliverOnce(context.Background()); !sent {
		t.Fatal("second round sent nothing")
	}

	if exists(file) {
		t.Error("event file still there after the 200")
	}

	if d := delivered("ok") - beforeOK; d != 1 {
		t.Errorf("delivered_total{result=ok} delta = %v, want 1", d)
	}

	reqs := srv.received()
	if len(reqs) != 2 {
		t.Fatalf("got %d requests, want 2", len(reqs))
	}

	if !bytes.Equal(reqs[0].body, reqs[1].body) || reqs[0].event.EventID != "e1" || reqs[1].event.EventID != "e1" {
		t.Error("the two attempts must carry the same body and event_id")
	}

	if reqs[0].delivery == reqs[1].delivery {
		t.Errorf("X-Doco-Delivery repeated: %q", reqs[0].delivery)
	}

	t0, _, _ := strings.Cut(reqs[0].signature, ",")
	t1, _, _ := strings.Cut(reqs[1].signature, ",")

	if t0 == t1 {
		t.Errorf("signature timestamp repeated: %q", t0)
	}
}

func TestOutboxRetryableAnswers(t *testing.T) {
	for _, code := range []int{http.StatusRequestTimeout, http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusServiceUnavailable} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			dir := t.TempDir()
			o := newTestOutbox(t, dir, "http://unused", nil)
			srv := newOutcomeServer(t, o.clock, func(int, Event) int { return code })
			o.cfg.URL = srv.URL

			mustEnqueue(t, o, testEvent("e1", "default/app"))
			o.deliverOnce(context.Background())

			if got := readEntry(t, filepath.Join(dir, "e1.json")); got.Attempts != 1 {
				t.Errorf("attempts = %d, want 1", got.Attempts)
			}

			if !strings.Contains(o.log.String(), fmt.Sprintf("status=%d", code)) {
				t.Errorf("retry log does not carry the status: %s", o.log.String())
			}
		})
	}

	t.Run("network error", func(t *testing.T) {
		closed := httptest.NewServer(http.NotFoundHandler())
		closed.Close()

		dir := t.TempDir()
		o := newTestOutbox(t, dir, closed.URL, nil)

		mustEnqueue(t, o, testEvent("e1", "default/app"))
		o.deliverOnce(context.Background())

		if got := readEntry(t, filepath.Join(dir, "e1.json")); got.Attempts != 1 {
			t.Errorf("attempts = %d, want 1", got.Attempts)
		}
	})
}

func TestOutboxPoison(t *testing.T) {
	codes := []int{http.StatusBadRequest, http.StatusUnauthorized, http.StatusNotFound, http.StatusFound, http.StatusPermanentRedirect}
	for _, code := range codes {
		t.Run(http.StatusText(code), func(t *testing.T) {
			var redirected atomic.Bool

			target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { redirected.Store(true) }))
			t.Cleanup(target.Close)

			dir := t.TempDir()
			o := newTestOutbox(t, dir, "http://unused", func(c *OutboxConfig) {
				c.HTTPClient = &http.Client{Timeout: 5 * time.Second}
			})
			srv := newRedirectingServer(t, o.clock, func(int, Event) int { return code }, target.URL)
			o.cfg.URL = srv.URL

			before := dropped("poison")

			mustEnqueue(t, o, testEvent("e1", "default/app"))
			o.deliverOnce(context.Background())

			if exists(filepath.Join(dir, "e1.json")) {
				t.Error("poison event still queued")
			}

			if got := readEntry(t, filepath.Join(dir, "dead", "e1.json")); got.Event.EventID != "e1" {
				t.Errorf("dead file has event %q", got.Event.EventID)
			}

			if d := dropped("poison") - before; d != 1 {
				t.Errorf("dropped_total{reason=poison} delta = %v, want 1", d)
			}

			if redirected.Load() {
				t.Error("the redirect was followed")
			}

			if len(srv.received()) != 1 {
				t.Errorf("got %d attempts, want 1 (no retry)", len(srv.received()))
			}

			log := o.log.String()
			if !strings.Contains(log, "event_id=e1") || !strings.Contains(log, fmt.Sprintf("status=%d", code)) {
				t.Errorf("poison log lacks event_id or status: %s", log)
			}

			if strings.Contains(log, "leaked-response-body") || strings.Contains(log, string(testSecret)) ||
				strings.Contains(log, "v1=") {
				t.Errorf("log leaks the response body, the secret or the signature: %s", log)
			}

			if o.count != 0 {
				t.Errorf("count = %d, want 0", o.count)
			}
		})
	}
}

func TestOutboxBadURLIsPoison(t *testing.T) {
	dir := t.TempDir()
	o := newTestOutbox(t, dir, "http://example.invalid/\x7f", nil)

	mustEnqueue(t, o, testEvent("e1", "default/app"))
	o.deliverOnce(context.Background())

	if !exists(filepath.Join(dir, "dead", "e1.json")) {
		t.Error("an event that cannot become a request must go to dead/")
	}
}

func TestOutboxExpired(t *testing.T) {
	dir := t.TempDir()
	o := newTestOutbox(t, dir, "http://unused", func(c *OutboxConfig) { c.MaxAge = time.Hour })
	srv := newOutcomeServer(t, o.clock, func(int, Event) int { return http.StatusBadGateway })
	o.cfg.URL = srv.URL

	before := dropped("expired")

	mustEnqueue(t, o, testEvent("e1", "default/app"))
	o.deliverOnce(context.Background())

	if !exists(filepath.Join(dir, "e1.json")) {
		t.Fatal("a young event must stay queued after a 502")
	}

	o.clock.Advance(time.Hour + time.Second)
	o.deliverOnce(context.Background())

	if exists(filepath.Join(dir, "e1.json")) || !exists(filepath.Join(dir, "dead", "e1.json")) {
		t.Error("an event retried past MaxAge must go to dead/")
	}

	if d := dropped("expired") - before; d != 1 {
		t.Errorf("dropped_total{reason=expired} delta = %v, want 1", d)
	}
}

func TestOutboxBackoff(t *testing.T) {
	o := newTestOutbox(t, t.TempDir(), "http://unused", nil)

	want := []time.Duration{
		time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second, 32 * time.Second,
		64 * time.Second, 128 * time.Second, 256 * time.Second, 5 * time.Minute, 5 * time.Minute,
	}

	for _, tc := range []struct {
		rand   float64
		factor float64
	}{{0, 0.5}, {1, 1}} {
		o.cfg.Rand = func() float64 { return tc.rand }

		for i, w := range want {
			if got := o.backoff(i + 1); got != time.Duration(float64(w)*tc.factor) {
				t.Errorf("rand=%v backoff(%d) = %v, want %v", tc.rand, i+1, got, time.Duration(float64(w)*tc.factor))
			}
		}
	}

	if got := o.backoff(1000); got != 5*time.Minute {
		t.Errorf("backoff(1000) = %v, want the 5m cap", got)
	}
}

func TestOutboxFIFOPerObject(t *testing.T) {
	dir := t.TempDir()
	o := newTestOutbox(t, dir, "http://unused", nil)
	srv := newOutcomeServer(t, o.clock, func(n int, _ Event) int {
		if n == 0 {
			return http.StatusServiceUnavailable
		}

		return http.StatusOK
	})
	o.cfg.URL = srv.URL

	mustEnqueue(t, o, testEvent("a1", "default/a"))
	mustEnqueue(t, o, testEvent("a2", "default/a"))

	o.deliverOnce(context.Background()) // a1: 503

	if got := srv.ids(); !slices.Equal(got, []string{"a1"}) {
		t.Fatalf("after round 1: %v, want [a1]", got)
	}

	o.clock.Advance(time.Minute)
	o.deliverOnce(context.Background()) // a1: 200, a2 waits for the next round

	if got := srv.ids(); !slices.Equal(got, []string{"a1", "a1"}) {
		t.Fatalf("after round 2: %v, want [a1 a1]", got)
	}

	o.deliverOnce(context.Background())

	if got := srv.ids(); !slices.Equal(got, []string{"a1", "a1", "a2"}) {
		t.Fatalf("after round 3: %v, want [a1 a1 a2]", got)
	}
}

func TestOutboxBackoffDoesNotBlockOtherObjects(t *testing.T) {
	dir := t.TempDir()
	o := newTestOutbox(t, dir, "http://unused", nil)
	srv := newOutcomeServer(t, o.clock, func(_ int, e Event) int {
		if e.Object == "default/a" {
			return http.StatusServiceUnavailable
		}

		return http.StatusOK
	})
	o.cfg.URL = srv.URL

	mustEnqueue(t, o, testEvent("a1", "default/a"))
	mustEnqueue(t, o, testEvent("b1", "default/b"))

	o.deliverOnce(context.Background())

	if exists(filepath.Join(dir, "b1.json")) {
		t.Error("b1 not delivered in the round where a1 went into backoff")
	}

	// a1 is now in backoff: b2 goes out without waiting for it.
	mustEnqueue(t, o, testEvent("b2", "default/b"))
	o.deliverOnce(context.Background())

	if got := srv.ids(); len(got) != 3 || got[2] != "b2" || !slices.Contains(got[:2], "a1") || !slices.Contains(got[:2], "b1") {
		t.Errorf("requests = %v, want a1 and b1, then b2 only", got)
	}

	if !exists(filepath.Join(dir, "a1.json")) {
		t.Error("a1 must still be queued")
	}
}

func TestOutboxCap(t *testing.T) {
	dir := t.TempDir()
	o := newTestOutbox(t, dir, "http://unused", func(c *OutboxConfig) { c.MaxEvents = 3 })

	before := dropped("overflow")

	for i := 1; i <= 4; i++ {
		mustEnqueue(t, o, testEvent(fmt.Sprintf("e%d", i), "default/app"))
		o.clock.Advance(time.Second)
	}

	if exists(filepath.Join(dir, "e1.json")) {
		t.Error("the oldest event was not dropped")
	}

	for _, id := range []string{"e2", "e3", "e4"} {
		if !exists(filepath.Join(dir, id+".json")) {
			t.Errorf("%s was dropped", id)
		}
	}

	if o.count != 3 {
		t.Errorf("count = %d, want 3", o.count)
	}

	if d := dropped("overflow") - before; d != 1 {
		t.Errorf("dropped_total{reason=overflow} delta = %v, want 1", d)
	}

	if log := o.log.String(); !strings.Contains(log, "level=WARN") || !strings.Contains(log, "event_id=e1") {
		t.Errorf("no warn log for the dropped event: %s", log)
	}

	// The oldest left (e2) is in flight: the next drop takes e3 instead.
	o.queues["default/app"][0].inFlight = true

	mustEnqueue(t, o, testEvent("e5", "default/app"))

	if !exists(filepath.Join(dir, "e2.json")) || exists(filepath.Join(dir, "e3.json")) {
		t.Error("the overflow drop must skip the in-flight event and take the next oldest")
	}
}

func TestOutboxCapAllInFlight(t *testing.T) {
	o := newTestOutbox(t, t.TempDir(), "http://unused", func(c *OutboxConfig) { c.MaxEvents = 1 })

	a := &entry{Event: testEvent("a", "default/a"), Object: "default/a", inFlight: true}
	b := &entry{Event: testEvent("b", "default/b"), Object: "default/b", inFlight: true}
	o.queues = map[string][]*entry{"default/a": {a}, "default/b": {b}}
	o.count = 2

	o.enforceCapLocked()

	if o.count != 2 {
		t.Errorf("count = %d, want 2: in-flight events are never dropped", o.count)
	}
}

func writeEntry(t *testing.T, dir string, e entry) {
	t.Helper()

	data, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}

	if err = os.WriteFile(filepath.Join(dir, e.Event.EventID+".json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestOutboxRestart(t *testing.T) {
	dir := t.TempDir()

	// File names sort opposite to first_enqueued, so the order must come from the content.
	for _, e := range []struct {
		id, object string
		first      time.Duration
	}{
		{"z-a-old", "default/a", 0},
		{"y-b", "default/b", time.Second},
		{"x-a-new", "default/a", 2 * time.Second},
	} {
		writeEntry(t, dir, entry{
			Version: 1, Event: testEvent(e.id, e.object), Object: e.object, Attempts: 2,
			NextAttempt: outboxT0, FirstEnqueued: outboxT0.Add(e.first),
		})
	}

	o := newTestOutbox(t, dir, "http://unused", nil)
	srv := newOutcomeServer(t, o.clock, nil)
	o.cfg.URL = srv.URL

	if o.count != 3 || testutil.ToFloat64(outcomeQueueDepth) != 3 {
		t.Fatalf("loaded %d events, want 3", o.count)
	}

	o.deliverOnce(context.Background())
	o.deliverOnce(context.Background())

	if got := srv.ids(); !slices.Equal(got, []string{"z-a-old", "y-b", "x-a-new"}) {
		t.Errorf("delivery order = %v, want first_enqueued order", got)
	}
}

func TestOutboxCorruptFilesGoToDead(t *testing.T) {
	dir := t.TempDir()

	writeEntry(t, dir, entry{Version: 1, Event: testEvent("good", "default/app"), Object: "default/app", FirstEnqueued: outboxT0})
	writeEntry(t, dir, entry{Version: 2, Event: testEvent("future", "default/app"), Object: "default/app"})

	for name, content := range map[string]string{"garbage.json": "{not json", "noid.json": `{"version":1}`, "notes.txt": "x"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	if err := os.Mkdir(filepath.Join(dir, "subdir.json"), 0o700); err != nil {
		t.Fatal(err)
	}

	before := dropped("corrupt")

	o := newTestOutbox(t, dir, "http://unused", nil)

	if o.count != 1 || len(o.queues["default/app"]) != 1 || o.queues["default/app"][0].Event.EventID != "good" {
		t.Errorf("loaded %d events, want only the good one", o.count)
	}

	for _, name := range []string{"garbage.json", "noid.json", "future.json"} {
		if !exists(filepath.Join(dir, "dead", name)) || exists(filepath.Join(dir, name)) {
			t.Errorf("%s was not moved to dead/", name)
		}
	}

	if !exists(filepath.Join(dir, "notes.txt")) || !exists(filepath.Join(dir, "subdir.json")) {
		t.Error("files that are not events must be left alone")
	}

	if d := dropped("corrupt") - before; d != 3 {
		t.Errorf("dropped_total{reason=corrupt} delta = %v, want 3", d)
	}
}

func TestNewOutboxErrors(t *testing.T) {
	if _, err := NewOutbox(OutboxConfig{URL: "http://x"}); err == nil {
		t.Error("no error without Dir")
	}

	if _, err := NewOutbox(OutboxConfig{Dir: t.TempDir()}); err == nil {
		t.Error("no error without URL")
	}

	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := NewOutbox(OutboxConfig{Dir: file, URL: "http://x"}); err == nil {
		t.Error("no error when Dir is a file")
	}

	readDir = func(string) ([]os.DirEntry, error) { return nil, errors.New("boom") }

	t.Cleanup(func() { readDir = os.ReadDir })

	if _, err := NewOutbox(OutboxConfig{Dir: t.TempDir(), URL: "http://x"}); err == nil {
		t.Error("no error when the directory cannot be read")
	}
}

func TestNewOutboxDefaults(t *testing.T) {
	dir := t.TempDir()

	injected := &http.Client{Timeout: 7 * time.Second}

	o, err := NewOutbox(OutboxConfig{Dir: dir, URL: "http://x", HTTPClient: injected})
	if err != nil {
		t.Fatal(err)
	}

	c := o.cfg
	if c.MaxEvents != 1000 || c.MinBackoff != time.Second || c.MaxBackoff != 5*time.Minute ||
		c.MaxAge != 24*time.Hour || c.Now == nil || c.Rand == nil || c.Log == nil {
		t.Errorf("defaults not applied: %+v", c)
	}

	if c.HTTPClient == injected || c.HTTPClient.Timeout != 7*time.Second || c.HTTPClient.CheckRedirect == nil {
		t.Error("the injected client must be copied, keep its timeout and gain CheckRedirect")
	}

	if injected.CheckRedirect != nil {
		t.Error("the injected client was mutated")
	}

	if !exists(filepath.Join(dir, "dead")) {
		t.Error("dead/ not created")
	}

	o, err = NewOutbox(OutboxConfig{Dir: t.TempDir(), URL: "http://x"})
	if err != nil {
		t.Fatal(err)
	}

	if o.cfg.HTTPClient == nil || o.cfg.HTTPClient.Timeout != 30*time.Second || o.cfg.HTTPClient.CheckRedirect == nil {
		t.Error("default client must have a 30s timeout and refuse redirects")
	}
}

func TestOutboxEnqueueWriteError(t *testing.T) {
	dir := t.TempDir()
	o := newTestOutbox(t, dir, "http://unused", nil)

	// A non-empty directory where the event file should go: the atomic rename fails.
	if err := os.MkdirAll(filepath.Join(dir, "e1.json", "x"), 0o700); err != nil {
		t.Fatal(err)
	}

	if err := o.Enqueue(testEvent("e1", "default/app")); err == nil {
		t.Error("Enqueue returned nil although the file could not be written")
	}

	if o.count != 0 {
		t.Errorf("count = %d, want 0", o.count)
	}
}

func TestOutboxRetryWriteErrorIsLogged(t *testing.T) {
	dir := t.TempDir()
	o := newTestOutbox(t, dir, "http://unused", nil)
	srv := newOutcomeServer(t, o.clock, func(int, Event) int { return http.StatusServiceUnavailable })
	o.cfg.URL = srv.URL

	mustEnqueue(t, o, testEvent("e1", "default/app"))

	file := filepath.Join(dir, "e1.json")
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}

	if err := os.MkdirAll(filepath.Join(file, "x"), 0o700); err != nil {
		t.Fatal(err)
	}

	o.deliverOnce(context.Background())

	if !strings.Contains(o.log.String(), "failed to update outbox event") {
		t.Errorf("write failure not logged: %s", o.log.String())
	}

	if o.count != 1 {
		t.Errorf("count = %d, want 1: the event stays queued in memory", o.count)
	}
}

func TestOutboxSettleUnknownEntry(t *testing.T) {
	o := newTestOutbox(t, t.TempDir(), "http://unused", nil)
	mustEnqueue(t, o, testEvent("e1", "default/app"))

	stray := &entry{Event: testEvent("stray", "default/app"), Object: "default/app", inFlight: true}

	o.settleLocked(stray, sendOK, http.StatusOK, o.clock.Now())

	if o.count != 1 || len(o.queues["default/app"]) != 1 {
		t.Error("settling an entry that is not queued must change nothing")
	}
}

// TestOutboxRun is the only test with a goroutine. It runs on the real clock with a 1ms
// backoff, and every wait is bounded.
func TestOutboxRun(t *testing.T) {
	const bound = 10 * time.Second

	deliveredCh := make(chan struct{}, 1)

	var calls atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}

		deliveredCh <- struct{}{}
	}))
	t.Cleanup(srv.Close)

	dir := t.TempDir()

	o, err := NewOutbox(OutboxConfig{
		Dir: dir, URL: srv.URL, Signer: Signer{Secret: testSecret},
		MinBackoff: time.Millisecond, MaxBackoff: time.Millisecond,
		Log: slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatal(err)
	}

	if err = o.Enqueue(testEvent("e1", "default/app")); err != nil {
		t.Fatal(err)
	}

	// Drain the wake-up of Enqueue: the first round answers 503, and the worker must then
	// wake up on its backoff timer.
	<-o.wake

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})

	go func() {
		o.Run(ctx)
		close(done)
	}()

	select {
	case <-deliveredCh:
	case <-time.After(bound):
		cancel()
		t.Fatal("event not delivered after a retry")
	}

	// The queue is empty and the worker sleeps for maxWait: a wake-up must reach it. The
	// second send blocks until the worker has consumed the first one.
	for range 2 {
		select {
		case o.wake <- struct{}{}:
		case <-time.After(bound):
			cancel()
			t.Fatal("the worker does not consume wake-ups")
		}
	}

	cancel()

	select {
	case <-done:
	case <-time.After(bound):
		t.Fatal("Run did not return after ctx was cancelled")
	}

	if exists(filepath.Join(dir, "e1.json")) {
		t.Error("event file still there after delivery")
	}
}
