package control

import (
	"encoding/base64"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/strigov/strigov-locallm/internal/scheduler/schedtest"
)

const testAdminKey = "admin-key-0123456789abcdef0123456789abcdef"

var t0 = time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)

func newTestAuth() (*Auth, *schedtest.SyncBuffer) {
	buf := &schedtest.SyncBuffer{}
	return NewAuth([][]byte{[]byte(testAdminKey)}, slog.New(slog.NewTextHandler(buf, nil))), buf
}

// H4: код — 32 байта base64url, однократный, живёт 60 с, не больше 8 (самый старый вытесняется).
func TestAuthOTP(t *testing.T) {
	a, buf := newTestAuth()
	code := a.MintOTP(t0)
	if raw, err := base64.RawURLEncoding.DecodeString(code); err != nil || len(raw) != 32 {
		t.Fatalf("code %q", code)
	}
	token, exp, err := a.Login(code, "", t0.Add(59*time.Second))
	if err != nil || !exp.Equal(t0.Add(59*time.Second+12*time.Hour)) {
		t.Fatalf("login: %v %v", exp, err)
	}
	if _, _, err := a.Login(code, "", t0.Add(59*time.Second)); !errors.Is(err, ErrBadCredentials) {
		t.Fatalf("reused code: %v", err)
	}
	late := a.MintOTP(t0)
	if _, _, err := a.Login(late, "", t0.Add(60*time.Second)); !errors.Is(err, ErrBadCredentials) {
		t.Fatalf("expired code: %v", err)
	}
	var codes []string
	for i := range 9 {
		codes = append(codes, a.MintOTP(t0.Add(time.Duration(i)*time.Millisecond)))
	}
	if _, _, err := a.Login(codes[0], "", t0.Add(time.Second)); !errors.Is(err, ErrBadCredentials) {
		t.Fatalf("evicted code: %v", err)
	}
	if _, _, err := a.Login(codes[8], "", t0.Add(time.Second)); err != nil {
		t.Fatalf("newest code: %v", err)
	}
	actor, err := a.Check(token, t0.Add(time.Minute))
	if err != nil || actor.Kind != KindAdmin || len(actor.Session) != 8 {
		t.Fatalf("check: %+v %v", actor, err)
	}
	if log := buf.String(); !strings.Contains(log, "method=otp") || strings.Contains(log, code) || strings.Contains(log, token) {
		t.Fatalf("log:\n%s", log)
	}
}

// H4: сессия 12 ч без продления, не больше 32, выход закрывает свою; ключ администратора — Bearer без сессии.
func TestAuthSessions(t *testing.T) {
	a, _ := newTestAuth()
	first, _, err := a.Login("", testAdminKey, t0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Check(first, t0.Add(12*time.Hour-time.Second)); err != nil {
		t.Fatalf("before expiry: %v", err)
	}
	if _, err := a.Check(first, t0.Add(12*time.Hour)); !errors.Is(err, ErrBadCredentials) {
		t.Fatalf("after expiry: %v", err)
	}
	var tokens []string
	for i := range 33 {
		tok, _, err := a.Login("", testAdminKey, t0.Add(time.Duration(i)*time.Second))
		if err != nil {
			t.Fatal(err)
		}
		tokens = append(tokens, tok)
	}
	if _, err := a.Check(tokens[0], t0.Add(time.Minute)); !errors.Is(err, ErrBadCredentials) {
		t.Fatalf("evicted session: %v", err)
	}
	if _, err := a.Check(tokens[32], t0.Add(time.Minute)); err != nil {
		t.Fatalf("newest session: %v", err)
	}
	a.Logout(tokens[32])
	if _, err := a.Check(tokens[32], t0.Add(time.Minute)); !errors.Is(err, ErrBadCredentials) {
		t.Fatalf("after logout: %v", err)
	}
	if _, err := a.Check(tokens[31], t0.Add(time.Minute)); err != nil {
		t.Fatalf("other session: %v", err)
	}
	if actor, err := a.Check(testAdminKey, t0); err != nil || actor.Kind != KindAdmin || actor.Session != "" {
		t.Fatalf("admin key: %+v %v", actor, err)
	}
	if _, err := a.Check("", t0); !errors.Is(err, ErrBadCredentials) {
		t.Fatalf("empty bearer: %v", err)
	}
}

// H5: 11 неудач за 60 с — вход и Bearer по ключу получают ErrTooManyAttempts на 60 с; сессии и код из трея работают.
func TestAuthLockout(t *testing.T) {
	a, buf := newTestAuth()
	session, _, err := a.Login("", testAdminKey, t0)
	if err != nil {
		t.Fatal(err)
	}
	code := a.MintOTP(t0)
	for i := range 11 {
		if _, _, err := a.Login("", "wrong-key", t0.Add(time.Duration(i)*time.Second)); !errors.Is(err, ErrBadCredentials) {
			t.Fatalf("failure %d: %v", i, err)
		}
	}
	at := t0.Add(11 * time.Second)
	if _, _, err := a.Login("", testAdminKey, at); !errors.Is(err, ErrTooManyAttempts) {
		t.Fatalf("login while locked: %v", err)
	}
	for _, bearer := range []string{testAdminKey, "wrong"} {
		if _, err := a.Check(bearer, at); !errors.Is(err, ErrTooManyAttempts) {
			t.Fatalf("bearer while locked: %v", err)
		}
	}
	if _, err := a.Check(session, at); err != nil {
		t.Fatalf("session while locked: %v", err)
	}
	if _, _, err := a.Login(code, "", at); err != nil {
		t.Fatalf("tray code while locked: %v", err)
	}
	if _, _, err := a.Login("", testAdminKey, t0.Add(70*time.Second)); err != nil { // 11-я неудача в t0+10 с
		t.Fatalf("after the lock: %v", err)
	}
	if log := buf.String(); !strings.Contains(log, `msg="admin login locked" failures=11`) || strings.Contains(log, "wrong-key") {
		t.Fatalf("log:\n%s", log)
	}
}
