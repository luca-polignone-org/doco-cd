package hooks

import (
	"strings"
	"time"

	"github.com/google/uuid"
)

// OutcomeSchema is the schema of the outcome event sent to HOOK_OUTCOME_URL.
const OutcomeSchema = "doco-cd.outcome/v1"

// Outcome is the terminal result of a deploy on one target.
type Outcome string

const (
	OutcomeDeployed        Outcome = "deployed"
	OutcomeFailed          Outcome = "failed"
	OutcomePolicyDenied    Outcome = "policy_denied"
	OutcomeRollbackBlocked Outcome = "rollback_blocked"
	OutcomeRolledBack      Outcome = "rolled_back"
	OutcomeRollbackFailed  Outcome = "rollback_failed"
	OutcomeUnblocked       Outcome = "unblocked"
	OutcomeDestroyed       Outcome = "destroyed"
)

// ViolationOut is one compose policy violation, as reported in a policy_denied event.
type ViolationOut struct {
	Service string `json:"service,omitempty"`
	Field   string `json:"field"`
	Value   string `json:"value"`
}

// ImageOut describes an image that actually runs, so that a consumer can act on its digest.
type ImageOut struct {
	Service     string   `json:"service,omitempty"`
	Image       string   `json:"image,omitempty"`
	ImageID     string   `json:"image_id,omitempty"`
	RepoDigests []string `json:"repo_digests,omitempty"`
}

// RollbackOut reports what happened to the pre_rollback veto and, when it let the rollback go
// on, which commit was restored. Decision is the string form of a Decision.
type RollbackOut struct {
	Decision string `json:"decision"`
	ToCommit string `json:"to_commit,omitempty"`
}

// Event is the payload of doco-cd.outcome/v1. Everything but schema, event_id, emitted_at,
// object, context, project and outcome is omitted when it does not apply.
type Event struct {
	Schema         string         `json:"schema"`
	EventID        string         `json:"event_id"`
	EmittedAt      time.Time      `json:"emitted_at"`
	Object         string         `json:"object"`
	Context        string         `json:"context"`
	Project        string         `json:"project"`
	Repository     string         `json:"repository,omitempty"`
	Ref            string         `json:"ref,omitempty"`
	Commit         string         `json:"commit,omitempty"`
	Trigger        string         `json:"trigger,omitempty"`
	Outcome        Outcome        `json:"outcome"`
	Stage          string         `json:"stage,omitempty"`
	Error          string         `json:"error,omitempty"`
	LastGoodCommit string         `json:"last_good_commit,omitempty"`
	Rollback       *RollbackOut   `json:"rollback,omitempty"`
	Violations     []ViolationOut `json:"violations,omitempty"`
	Images         []ImageOut     `json:"images,omitempty"`
	DurationMS     int64          `json:"duration_ms,omitempty"`
	DocoCDVersion  string         `json:"doco_cd_version,omitempty"`
}

// NewEvent returns an event with the schema, a fresh UUIDv7 event_id (time-ordered, so the
// receiver can sort and deduplicate by it) and the current UTC time. Context and Project are
// the two halves of object, split like deploy.ObjectName builds it: at the single "/".
//
// A malformed object (no "/", an empty half or more than one "/") is kept verbatim in Object
// and leaves Context and Project empty: an event is fire-and-forget and must never be
// dropped or panic because of a label, and the consumer still gets the original string.
func NewEvent(object string) Event {
	e := Event{
		Schema:    OutcomeSchema,
		EventID:   uuid.Must(uuid.NewV7()).String(),
		EmittedAt: time.Now().UTC(),
		Object:    object,
	}

	ctx, project, ok := strings.Cut(object, "/")
	if ok && ctx != "" && project != "" && !strings.Contains(project, "/") {
		e.Context, e.Project = ctx, project
	}

	return e
}
