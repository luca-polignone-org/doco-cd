package hooks

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/kimdre/doco-cd/internal/filesystem"
)

// The Outbox delivers outcome events at-least-once: every event is a file, deleted only on
// ack. A single worker keeps the order per object and, in every round, takes the ready head
// of each object, so an object in backoff does not hold up the others.

const (
	outboxVersion = 1
	deadDir       = "dead"
	// maxWait bounds the worker's sleep when nothing is due, so it re-evaluates the queue.
	maxWait = time.Hour
)

// readDir is os.ReadDir, replaceable in tests: the outbox directory has just been created
// when it is read, so a failure cannot be produced from the outside.
var readDir = os.ReadDir

// OutboxConfig configures an Outbox. Zero values take the defaults noted on each field.
type OutboxConfig struct {
	Dir        string // <data>/fork/outbox
	URL        string
	Signer     Signer
	HTTPClient *http.Client  // nil: a client with a 30s timeout; redirects are never followed
	MaxEvents  int           // 0: 1000
	MinBackoff time.Duration // 0: 1s
	MaxBackoff time.Duration // 0: 5m
	MaxAge     time.Duration // 0: 24h, measured from first_enqueued
	Now        func() time.Time
	Rand       func() float64 // jitter source in [0,1); nil: math/rand/v2
	Log        *slog.Logger
}

// entry is one queued event, and the content of <Dir>/<event_id>.json.
type entry struct {
	Version       int       `json:"version"`
	Event         Event     `json:"event"`
	Object        string    `json:"object"`
	Attempts      int       `json:"attempts"`
	NextAttempt   time.Time `json:"next_attempt"`
	FirstEnqueued time.Time `json:"first_enqueued"`

	inFlight bool
}

// Outbox is the on-disk queue of outcome events and its delivery worker.
type Outbox struct {
	cfg    OutboxConfig
	mu     sync.Mutex
	queues map[string][]*entry // per object, in FIFO order
	count  int
	wake   chan struct{}
}

type sendResult int

const (
	sendOK sendResult = iota
	sendRetry
	sendPoison
	// sendAborted means the outbox's own ctx ended: a shutdown, not an endpoint failure.
	sendAborted
)

// NewOutbox creates Dir (and Dir/dead), loads the events left by a previous run and
// registers the outbox metrics.
func NewOutbox(cfg OutboxConfig) (*Outbox, error) {
	if cfg.Dir == "" || cfg.URL == "" {
		return nil, errors.New("outbox requires Dir and URL")
	}

	// Never follow redirects, for the same reason as PreRollbackClient: a 307/308 would
	// re-POST the signed body to another host. A 3xx comes back as the response and is
	// poison. The caller's client is copied, not mutated. A zero timeout would let a hanging
	// endpoint stall the single worker forever, so it becomes 30s.
	client := http.Client{}
	if cfg.HTTPClient != nil {
		client = *cfg.HTTPClient
	}

	if client.Timeout == 0 {
		client.Timeout = 30 * time.Second
	}

	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	cfg.HTTPClient = &client

	if cfg.MaxEvents == 0 {
		cfg.MaxEvents = 1000
	}

	if cfg.MinBackoff == 0 {
		cfg.MinBackoff = time.Second
	}

	if cfg.MaxBackoff == 0 {
		cfg.MaxBackoff = 5 * time.Minute
	}

	if cfg.MaxAge == 0 {
		cfg.MaxAge = 24 * time.Hour
	}

	if cfg.Now == nil {
		cfg.Now = time.Now
	}

	if cfg.Rand == nil {
		cfg.Rand = rand.Float64
	}

	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}

	if err := os.MkdirAll(filepath.Join(cfg.Dir, deadDir), filesystem.PermPrivateDir); err != nil {
		return nil, fmt.Errorf("create outbox: %w", err)
	}

	o := &Outbox{cfg: cfg, queues: map[string][]*entry{}, wake: make(chan struct{}, 1)}
	if err := o.load(); err != nil {
		return nil, err
	}

	registerMetrics()

	return o, nil
}

func (o *Outbox) path(e *entry) string {
	return filepath.Join(o.cfg.Dir, e.Event.EventID+".json")
}

func (o *Outbox) deadPath(name string) string {
	return filepath.Join(o.cfg.Dir, deadDir, name)
}

// load queues again the events left by a previous run; an unreadable file goes to dead/
// and does not stop the others. Leftover AtomicWriteFile temporaries (.tmp-*) are partial
// writes interrupted by a crash, never a complete event, and are deleted: load runs before
// any Enqueue or worker, so none of them belongs to a write in progress.
func (o *Outbox) load() error {
	items, err := readDir(o.cfg.Dir)
	if err != nil {
		return fmt.Errorf("read outbox: %w", err)
	}

	for _, it := range items {
		if it.IsDir() {
			continue
		}

		full := filepath.Join(o.cfg.Dir, it.Name())

		if strings.HasPrefix(it.Name(), ".tmp-") {
			_ = os.Remove(full)
			continue
		}

		if !strings.HasSuffix(it.Name(), ".json") {
			continue
		}

		var e entry

		data, err := os.ReadFile(full) // #nosec G304 -- a file listed in the outbox directory
		if err == nil {
			err = json.Unmarshal(data, &e)
		}

		// The name must be the event_id: otherwise settling it would act on another path.
		if err != nil || e.Version != outboxVersion || e.Event.EventID == "" || it.Name() != e.Event.EventID+".json" {
			o.cfg.Log.Warn("unreadable outbox event moved to dead", slog.String("file", it.Name()))
			_ = os.Rename(full, o.deadPath(it.Name()))

			outcomeDropped.WithLabelValues("corrupt").Inc()

			continue
		}

		o.queues[e.Object] = append(o.queues[e.Object], &e)
		o.count++
	}

	for _, q := range o.queues {
		sort.SliceStable(q, func(i, j int) bool { return q[i].FirstEnqueued.Before(q[j].FirstEnqueued) })
	}

	outcomeQueueDepth.Set(float64(o.count))

	return nil
}

// write stores e atomically: after a crash the file is either the previous or the new one.
func (o *Outbox) write(e *entry) error {
	// An entry of strings, ints and times cannot fail to marshal.
	data, _ := json.Marshal(e) //nolint:errchkjson

	return filesystem.AtomicWriteFile(o.path(e), data, filesystem.PermPrivateFile)
}

// Enqueue writes the event to disk, queues it and enforces MaxEvents. The event is in the
// outbox only if Enqueue returns nil. The event_id names the file, so it must be a plain
// file name: not empty, without a path separator, and not a .tmp- name that load deletes.
func (o *Outbox) Enqueue(ev Event) error {
	if id := ev.EventID; id == "" || filepath.Base(id) != id || strings.HasPrefix(id, ".tmp-") {
		return fmt.Errorf("enqueue outcome event: invalid event_id %q", id)
	}

	now := o.cfg.Now()
	e := &entry{Version: outboxVersion, Event: ev, Object: ev.Object, NextAttempt: now, FirstEnqueued: now}

	if err := o.write(e); err != nil {
		return fmt.Errorf("enqueue outcome event: %w", err)
	}

	o.mu.Lock()
	o.queues[e.Object] = append(o.queues[e.Object], e)
	o.count++
	o.enforceCapLocked()
	outcomeQueueDepth.Set(float64(o.count))
	o.mu.Unlock()

	select {
	case o.wake <- struct{}{}:
	default:
	}

	return nil
}

// enforceCapLocked drops the oldest event that is not in flight while the queue is over
// MaxEvents. If every event is in flight it gives up: those are settled by the worker.
func (o *Outbox) enforceCapLocked() {
	for o.count > o.cfg.MaxEvents {
		var (
			oldest    *entry
			oldestObj string
			oldestIdx int
		)

		for obj, q := range o.queues {
			for i, e := range q {
				if !e.inFlight && (oldest == nil || e.FirstEnqueued.Before(oldest.FirstEnqueued)) {
					oldest, oldestObj, oldestIdx = e, obj, i
				}
			}
		}

		if oldest == nil {
			return
		}

		o.cfg.Log.Warn("outbox full, dropping oldest event",
			slog.String("event_id", oldest.Event.EventID), slog.String("object", oldestObj))

		_ = os.Remove(o.path(oldest))
		o.removeLocked(oldestObj, oldestIdx)
		outcomeDropped.WithLabelValues("overflow").Inc()
	}
}

func (o *Outbox) removeLocked(obj string, idx int) {
	q := o.queues[obj]
	q = append(q[:idx], q[idx+1:]...)

	if len(q) == 0 {
		delete(o.queues, obj)
	} else {
		o.queues[obj] = q
	}

	o.count--
}

// Run delivers until ctx is done; it wakes up at the next next_attempt or on an Enqueue.
func (o *Outbox) Run(ctx context.Context) {
	for {
		wait, _ := o.deliverOnce(ctx)
		timer := time.NewTimer(wait)

		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		case <-o.wake:
			timer.Stop()
		}
	}
}

// deliverOnce sends the ready head of every object, oldest first, and returns how long
// to wait for the next due event and whether it sent anything.
func (o *Outbox) deliverOnce(ctx context.Context) (time.Duration, bool) {
	now := o.cfg.Now()

	o.mu.Lock()

	var ready []*entry

	for _, q := range o.queues {
		if head := q[0]; !head.inFlight && !head.NextAttempt.After(now) {
			head.inFlight = true
			ready = append(ready, head)
		}
	}
	o.mu.Unlock()

	// FirstEnqueued never changes after Enqueue, so it can be read without the lock.
	sort.Slice(ready, func(i, j int) bool { return ready[i].FirstEnqueued.Before(ready[j].FirstEnqueued) })

	for _, e := range ready {
		// Once ctx is done the round stops sending; the remaining heads are only released.
		res, status := sendAborted, 0
		if ctx.Err() == nil {
			res, status = o.send(ctx, e)
		}

		o.mu.Lock()
		o.settleLocked(e, res, status, o.cfg.Now())
		o.mu.Unlock()
	}

	o.mu.Lock()
	defer o.mu.Unlock()

	outcomeQueueDepth.Set(float64(o.count))

	now = o.cfg.Now()
	wait := maxWait

	for _, q := range o.queues {
		if d := q[0].NextAttempt.Sub(now); d < wait {
			wait = max(d, 0)
		}
	}

	return wait, len(ready) > 0
}

// send makes one signed attempt and maps the answer: 2xx is ok; 408, 429, 5xx and network
// errors are retried; everything else (other 4xx, and 3xx since redirects are not followed)
// is poison. The status is 0 when there was no response. Neither the transport error nor
// the response body is returned or logged: either may echo the URL or the payload.
func (o *Outbox) send(ctx context.Context, e *entry) (sendResult, int) {
	// An Event of strings, ints and times cannot fail to marshal.
	body, _ := json.Marshal(e.Event) //nolint:errchkjson

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.cfg.URL, bytes.NewReader(body))
	if err != nil {
		return sendPoison, 0
	}

	o.cfg.Signer.Sign(req, body) // a fresh signature and delivery ID on every attempt

	resp, err := o.cfg.HTTPClient.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return sendAborted, 0
		}

		return sendRetry, 0
	}

	_ = resp.Body.Close()

	switch {
	case resp.StatusCode >= 200 && resp.StatusCode <= 299:
		return sendOK, resp.StatusCode
	case resp.StatusCode == http.StatusRequestTimeout, resp.StatusCode == http.StatusTooManyRequests, resp.StatusCode >= 500:
		return sendRetry, resp.StatusCode
	default:
		return sendPoison, resp.StatusCode
	}
}

// settleLocked applies the result of an attempt to e. An aborted attempt only releases the
// in-flight mark: it is not counted, persisted or checked for expiry, so a shutdown never
// moves an event to dead/.
func (o *Outbox) settleLocked(e *entry, res sendResult, status int, now time.Time) {
	e.inFlight = false

	idx := indexOf(o.queues[e.Object], e)
	if idx < 0 {
		// The cap skips in-flight events, so e is always queued; guard against a future bug
		// turning into a panic in removeLocked.
		return
	}

	switch res {
	case sendAborted:
		// Nothing to do: the event stays as it was on disk.
	case sendOK:
		_ = os.Remove(o.path(e))
		o.removeLocked(e.Object, idx)
		outcomeDelivered.WithLabelValues("ok").Inc()
	case sendPoison:
		o.deadLocked(e, idx, "poison", status)
	case sendRetry:
		e.Attempts++

		outcomeDelivered.WithLabelValues("retry").Inc()

		if now.Sub(e.FirstEnqueued) > o.cfg.MaxAge {
			o.deadLocked(e, idx, "expired", status)
			return
		}

		e.NextAttempt = now.Add(o.backoff(e.Attempts))
		o.cfg.Log.Debug("outcome delivery failed, will retry",
			slog.String("event_id", e.Event.EventID), slog.Int("status", status), slog.Int("attempts", e.Attempts))

		if err := o.write(e); err != nil {
			o.cfg.Log.Error("failed to update outbox event",
				slog.String("event_id", e.Event.EventID), slog.Any("error", err))
		}
	}
}

func (o *Outbox) deadLocked(e *entry, idx int, reason string, status int) {
	o.cfg.Log.Warn("outbox event moved to dead",
		slog.String("event_id", e.Event.EventID), slog.String("reason", reason), slog.Int("status", status))

	_ = os.Rename(o.path(e), o.deadPath(e.Event.EventID+".json"))
	o.removeLocked(e.Object, idx)
	outcomeDropped.WithLabelValues(reason).Inc()
}

// backoff is exponential from MinBackoff, capped at MaxBackoff, with jitter between 0.5x
// and 1x.
func (o *Outbox) backoff(attempts int) time.Duration {
	d := o.cfg.MinBackoff
	for i := 1; i < attempts && d < o.cfg.MaxBackoff; i++ {
		d *= 2
	}

	d = min(d, o.cfg.MaxBackoff)

	return time.Duration(float64(d) * (0.5 + 0.5*o.cfg.Rand()))
}

func indexOf(q []*entry, e *entry) int {
	for i, x := range q {
		if x == e {
			return i
		}
	}

	return -1
}
