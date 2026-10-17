package hooks

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"
)

// PreRollbackSchema is the schema of the pre_rollback request.
const PreRollbackSchema = "doco-cd.pre_rollback/v1"

// Decision is the outcome of the veto call. Only a 2xx with {"decision":"proceed"}
// lets the rollback go on; every other outcome is a reason to block, and the
// decision says which one.
type Decision string

const (
	DecisionProceed      Decision = "proceed"
	DecisionBlock        Decision = "block"
	DecisionTimeout      Decision = "timeout"
	DecisionHTTPError    Decision = "http_error"
	DecisionInvalid      Decision = "invalid"
	DecisionUnconfigured Decision = "unconfigured"
)

// PreRollbackRequest is the body sent to HOOK_PRE_ROLLBACK_URL.
type PreRollbackRequest struct {
	Schema         string `json:"schema"`
	Object         string `json:"object"`
	FailedCommit   string `json:"failed_commit"`
	LastGoodCommit string `json:"last_good_commit"`
	Stage          string `json:"stage"`
	Error          string `json:"error"`
}

// PreRollbackClient asks the gate for a veto before a rollback.
type PreRollbackClient struct {
	URL        string
	Signer     Signer
	HTTPClient *http.Client
}

const maxDecisionBytes = 64 << 10

// Decide sends the signed request and maps the answer to a Decision. It never
// returns the response body nor the transport error: neither can be trusted not
// to echo secrets, and the caller only needs the category.
func (c *PreRollbackClient) Decide(ctx context.Context, in PreRollbackRequest, timeout time.Duration) Decision {
	if c == nil || c.URL == "" {
		return DecisionUnconfigured
	}

	in.Schema = PreRollbackSchema

	// A struct of strings cannot fail to marshal.
	body, _ := json.Marshal(in) //nolint:errchkjson

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.URL, bytes.NewReader(body))
	if err != nil {
		return DecisionHTTPError
	}

	c.Signer.Sign(req, body)

	client := c.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}

	resp, err := client.Do(req)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return DecisionTimeout
		}

		return DecisionHTTPError
	}
	defer resp.Body.Close() // nolint:errcheck

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return DecisionHTTPError
	}

	var out struct {
		Decision Decision `json:"decision"`
	}

	if err = json.NewDecoder(io.LimitReader(resp.Body, maxDecisionBytes)).Decode(&out); err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return DecisionTimeout
		}

		return DecisionInvalid
	}

	switch out.Decision {
	case DecisionProceed, DecisionBlock:
		return out.Decision
	default:
		return DecisionInvalid
	}
}
