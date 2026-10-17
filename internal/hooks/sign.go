// Package hooks signs and sends the fork's outgoing hooks (pre_rollback veto, outcome events).
package hooks

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Signer signs every attempt from scratch: t, the signature and X-Doco-Delivery
// change on every send, so an outbox retry does not fall outside the receiver's
// 5 min window.
type Signer struct {
	Secret []byte
	Now    func() time.Time
	NewID  func() string
}

func (s Signer) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}

	return time.Now()
}

func (s Signer) newID() string {
	if s.NewID != nil {
		return s.NewID()
	}

	return uuid.NewString()
}

func mac(secret []byte, t string, body []byte) string {
	m := hmac.New(sha256.New, secret)
	m.Write([]byte(t))
	m.Write([]byte("."))
	m.Write(body)

	return hex.EncodeToString(m.Sum(nil))
}

// Sign sets X-Doco-Signature (t=<unix>,v1=hex(HMAC-SHA256(secret, "<t>.<body>"))),
// X-Doco-Delivery and Content-Type on req.
func (s Signer) Sign(req *http.Request, body []byte) {
	t := strconv.FormatInt(s.now().Unix(), 10)
	req.Header.Set("X-Doco-Signature", "t="+t+",v1="+mac(s.Secret, t, body))
	req.Header.Set("X-Doco-Delivery", s.newID())
	req.Header.Set("Content-Type", "application/json")
}

// ErrBadSignature is returned by Verify for every rejection. The errors never
// carry the signature or the expected value.
var ErrBadSignature = errors.New("bad signature")

// Verify checks header against body: it must be well-formed, its timestamp within
// maxAge of now (in both directions), and the MAC must match (constant-time compare).
func Verify(secret []byte, header string, body []byte, now time.Time, maxAge time.Duration) error {
	var t, v1 string

	for _, part := range strings.Split(header, ",") {
		k, v, _ := strings.Cut(strings.TrimSpace(part), "=")
		switch k {
		case "t":
			t = v
		case "v1":
			v1 = v
		}
	}

	ts, err := strconv.ParseInt(t, 10, 64)
	if err != nil || v1 == "" {
		return fmt.Errorf("%w: malformed header", ErrBadSignature)
	}

	if age := now.Sub(time.Unix(ts, 0)); age > maxAge || age < -maxAge {
		return fmt.Errorf("%w: timestamp outside window", ErrBadSignature)
	}

	if !hmac.Equal([]byte(mac(secret, t, body)), []byte(v1)) {
		return ErrBadSignature
	}

	return nil
}
