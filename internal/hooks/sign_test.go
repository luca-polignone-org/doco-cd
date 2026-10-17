package hooks

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
)

var (
	testSecret = []byte("test-secret")
	t0         = time.Unix(1_800_000_000, 0)
)

func newReq(t *testing.T) *http.Request {
	t.Helper()

	req, err := http.NewRequest(http.MethodPost, "http://example.invalid/", nil)
	if err != nil {
		t.Fatal(err)
	}

	return req
}

func TestSignFormat(t *testing.T) {
	body := []byte(`{"a":1}`)
	s := Signer{Secret: testSecret, Now: func() time.Time { return t0 }, NewID: func() string { return "id-1" }}
	req := newReq(t)

	s.Sign(req, body)

	m := hmac.New(sha256.New, testSecret)
	m.Write([]byte(strconv.FormatInt(t0.Unix(), 10) + "." + string(body)))

	want := "t=" + strconv.FormatInt(t0.Unix(), 10) + ",v1=" + hex.EncodeToString(m.Sum(nil))
	if got := req.Header.Get("X-Doco-Signature"); got != want {
		t.Errorf("X-Doco-Signature = %q, want %q", got, want)
	}

	if got := req.Header.Get("X-Doco-Delivery"); got != "id-1" {
		t.Errorf("X-Doco-Delivery = %q, want id-1", got)
	}

	if got := req.Header.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q", got)
	}
}

func TestSignDefaults(t *testing.T) {
	s := Signer{Secret: testSecret}
	r1, r2 := newReq(t), newReq(t)

	before := time.Now().Add(-time.Second)

	s.Sign(r1, nil)
	s.Sign(r2, nil)

	if r1.Header.Get("X-Doco-Delivery") == r2.Header.Get("X-Doco-Delivery") {
		t.Error("two Sign calls produced the same X-Doco-Delivery")
	}

	if len(r1.Header.Get("X-Doco-Delivery")) != 36 {
		t.Errorf("default delivery id is not a UUID: %q", r1.Header.Get("X-Doco-Delivery"))
	}

	if err := Verify(testSecret, r1.Header.Get("X-Doco-Signature"), nil, before.Add(2*time.Second), time.Minute); err != nil {
		t.Errorf("Verify() with the default clock = %v", err)
	}
}

func TestSignResignsEveryAttempt(t *testing.T) {
	now := t0
	n := 0
	s := Signer{
		Secret: testSecret,
		Now:    func() time.Time { return now },
		NewID:  func() string { n++; return "id-" + strconv.Itoa(n) },
	}
	r1, r2 := newReq(t), newReq(t)

	s.Sign(r1, []byte("x"))

	now = now.Add(90 * time.Second)

	s.Sign(r2, []byte("x"))

	if r1.Header.Get("X-Doco-Delivery") == r2.Header.Get("X-Doco-Delivery") {
		t.Error("delivery ids are equal")
	}

	if r1.Header.Get("X-Doco-Signature") == r2.Header.Get("X-Doco-Signature") {
		t.Error("signatures are equal although t advanced")
	}

	if !strings.HasPrefix(r2.Header.Get("X-Doco-Signature"), "t="+strconv.FormatInt(t0.Add(90*time.Second).Unix(), 10)+",") {
		t.Errorf("second t not advanced: %q", r2.Header.Get("X-Doco-Signature"))
	}
}

func TestVerify(t *testing.T) {
	body := []byte(`{"k":"v"}`)
	s := Signer{Secret: testSecret, Now: func() time.Time { return t0 }}
	req := newReq(t)

	s.Sign(req, body)

	sig := req.Header.Get("X-Doco-Signature")
	maxAge := 5 * time.Minute

	tests := []struct {
		name    string
		secret  []byte
		header  string
		body    []byte
		now     time.Time
		wantErr bool
	}{
		{"valid", testSecret, sig, body, t0.Add(time.Minute), false},
		{"valid at the edge of the window", testSecret, sig, body, t0.Add(maxAge), false},
		{"altered body", testSecret, sig, []byte(`{"k":"w"}`), t0, true},
		{"different secret", []byte("other"), sig, body, t0, true},
		{"older than maxAge", testSecret, sig, body, t0.Add(maxAge + time.Second), true},
		{"from the future beyond maxAge", testSecret, sig, body, t0.Add(-maxAge - time.Second), true},
		{"t not a number", testSecret, "t=x,v1=abc", body, t0, true},
		{"no v1", testSecret, "t=" + strconv.FormatInt(t0.Unix(), 10), body, t0, true},
		{"no t", testSecret, "v1=abc", body, t0, true},
		{"empty header", testSecret, "", body, t0, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := Verify(tt.secret, tt.header, tt.body, tt.now, maxAge)
			if (err != nil) != tt.wantErr {
				t.Fatalf("Verify() error = %v, wantErr %v", err, tt.wantErr)
			}

			if err != nil && !errors.Is(err, ErrBadSignature) {
				t.Errorf("error %v does not wrap ErrBadSignature", err)
			}

			if err != nil && strings.Contains(err.Error(), "abc") {
				t.Errorf("error echoes header material: %v", err)
			}
		})
	}
}
