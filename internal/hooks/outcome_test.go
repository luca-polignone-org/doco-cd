package hooks

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestEventMinimalJSON(t *testing.T) {
	e := Event{
		Schema:    OutcomeSchema,
		EventID:   "id",
		EmittedAt: time.Date(2026, 10, 17, 9, 12, 3, 0, time.UTC),
		Object:    "noi/poli-gate",
		Context:   "noi",
		Project:   "poli-gate",
		Outcome:   OutcomeDeployed,
	}

	got, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}

	const golden = `{"schema":"doco-cd.outcome/v1","event_id":"id","emitted_at":"2026-10-17T09:12:03Z",` +
		`"object":"noi/poli-gate","context":"noi","project":"poli-gate","outcome":"deployed"}`
	if string(got) != golden {
		t.Errorf("minimal event:\n got %s\nwant %s", got, golden)
	}
}

func TestEventFullJSON(t *testing.T) {
	e := Event{
		Schema:         OutcomeSchema,
		EventID:        "id",
		EmittedAt:      time.Date(2026, 10, 17, 9, 12, 3, 0, time.UTC),
		Object:         "noi/poli-gate",
		Context:        "noi",
		Project:        "poli-gate",
		Repository:     "r",
		Ref:            "refs/heads/main",
		Commit:         "c1",
		Trigger:        "webhook",
		Outcome:        OutcomeRolledBack,
		Stage:          "deploy",
		Error:          "boom",
		LastGoodCommit: "c0",
		Rollback:       &RollbackOut{Decision: string(DecisionProceed), ToCommit: "c0"},
		Violations:     []ViolationOut{{Service: "web", Field: "privileged", Value: "true"}},
		Images: []ImageOut{{
			Service: "web", Image: "ghcr.io/x/web:1.4", ImageID: "sha256:a",
			RepoDigests: []string{"ghcr.io/x/web@sha256:a"},
		}},
		DurationMS:    41230,
		DocoCDVersion: "v0.127.0-poli.1",
	}

	got, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}

	const golden = `{"schema":"doco-cd.outcome/v1","event_id":"id","emitted_at":"2026-10-17T09:12:03Z",` +
		`"object":"noi/poli-gate","context":"noi","project":"poli-gate","repository":"r",` +
		`"ref":"refs/heads/main","commit":"c1","trigger":"webhook","outcome":"rolled_back",` +
		`"stage":"deploy","error":"boom","last_good_commit":"c0",` +
		`"rollback":{"decision":"proceed","to_commit":"c0"},` +
		`"violations":[{"service":"web","field":"privileged","value":"true"}],` +
		`"images":[{"service":"web","image":"ghcr.io/x/web:1.4","image_id":"sha256:a",` +
		`"repo_digests":["ghcr.io/x/web@sha256:a"]}],"duration_ms":41230,"doco_cd_version":"v0.127.0-poli.1"}`
	if string(got) != golden {
		t.Errorf("full event:\n got %s\nwant %s", got, golden)
	}
}

func TestNewEvent(t *testing.T) {
	before := time.Now().Add(-time.Second)
	a := NewEvent("noi/poli-gate")
	b := NewEvent("noi/poli-gate")

	if a.Schema != OutcomeSchema || a.Object != "noi/poli-gate" || a.Context != "noi" || a.Project != "poli-gate" {
		t.Errorf("unexpected event: %+v", a)
	}

	id, err := uuid.Parse(a.EventID)
	if err != nil || id.Version() != 7 {
		t.Errorf("event_id %q: err=%v version=%d, want a UUIDv7", a.EventID, err, id.Version())
	}

	if a.EventID == b.EventID {
		t.Error("two events share an event_id")
	}

	if a.EventID >= b.EventID {
		t.Errorf("event ids are not ordered: %s >= %s", a.EventID, b.EventID)
	}

	if a.EmittedAt.Before(before) || a.EmittedAt.Location() != time.UTC {
		t.Errorf("emitted_at %v is not a recent UTC time", a.EmittedAt)
	}
}

func TestNewEventMalformedObject(t *testing.T) {
	for _, object := range []string{"", "solo", "/p", "c/", "a/b/c", "/"} {
		e := NewEvent(object)
		if e.Object != object || e.Context != "" || e.Project != "" {
			t.Errorf("NewEvent(%q) = object %q context %q project %q, want object kept and empty halves",
				object, e.Object, e.Context, e.Project)
		}
	}
}
