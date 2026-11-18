package control

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"slices"
	"sync"
	"time"
)

// Правила §9.4 спецификации v2.
const (
	otpTTL      = 60 * time.Second
	maxOTPs     = 8
	sessionTTL  = 12 * time.Hour
	maxSessions = 32
	failWindow  = 60 * time.Second
	maxFailures = 10
	lockFor     = 60 * time.Second
)

// Ошибки входа и проверки Bearer (§9.6: 401 invalid_admin_credentials и 429 too_many_attempts).
var (
	ErrBadCredentials  = errors.New("missing or invalid administrator credentials")
	ErrTooManyAttempts = errors.New("too many failed attempts; retry in 60 s")
)

// Auth — вход в панель (§9.4): одноразовые коды, сессии, ограничение попыток. Коды и токены хранятся только как
// SHA-256; время передаёт вызывающий (тесты — подменяемые часы).
type Auth struct {
	keys [][]byte
	log  *slog.Logger

	mu          sync.Mutex
	otps        []secret
	sessions    []secret
	failures    []time.Time
	lockedUntil time.Time
}

// secret — SHA-256 кода или токена и срок.
type secret struct {
	hash    [32]byte
	expires time.Time
}

// NewAuth — adminKeys из LoadOrCreateAdminKeys; log nil — журнал отбрасывается.
func NewAuth(adminKeys [][]byte, log *slog.Logger) *Auth {
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Auth{keys: adminKeys, log: log}
}

// MintOTP — одноразовый код для трея: 32 байта crypto/rand в base64url, живёт 60 с; кодов не больше 8.
func (a *Auth) MintOTP(now time.Time) string {
	code := randomToken()
	a.mu.Lock()
	defer a.mu.Unlock()
	a.otps = pushBounded(unexpired(a.otps, now), secret{hash: sha256.Sum256([]byte(code)), expires: now.Add(otpTTL)}, maxOTPs)
	return code
}

// Login — вход по коду (otp != "") или по ключу администратора. Код работает один раз и во время блокировки; вход по
// ключу во время блокировки — ErrTooManyAttempts. Неверный код или ключ — ErrBadCredentials и неудача в счёт.
// Успех — токен сессии (12 ч, сессий не больше 32) и строка журнала с методом и номером сессии.
func (a *Auth) Login(otp, adminKey string, now time.Time) (string, time.Time, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	method, ok := "otp", false
	if otp != "" {
		a.otps = unexpired(a.otps, now)
		if i := find(a.otps, otp); i >= 0 {
			a.otps = slices.Delete(a.otps, i, i+1)
			ok = true
		}
	} else {
		method = "admin_key"
		if a.locked(now) {
			return "", time.Time{}, ErrTooManyAttempts
		}
		ok = a.isAdminKey(adminKey)
	}
	if !ok {
		a.fail(now)
		return "", time.Time{}, ErrBadCredentials
	}
	token := randomToken()
	expires := now.Add(sessionTTL)
	a.sessions = pushBounded(unexpired(a.sessions, now), secret{hash: sha256.Sum256([]byte(token)), expires: expires}, maxSessions)
	a.log.Info("admin login", "method", method, "session", sessionID(token))
	return token, expires, nil
}

// Check — Bearer администратора: токен действующей сессии (работает и во время блокировки) или ключ администратора.
// Пустой Bearer — ErrBadCredentials без неудачи в счёт; во время блокировки остальное — ErrTooManyAttempts; неверный
// Bearer — ErrBadCredentials и неудача.
func (a *Auth) Check(bearer string, now time.Time) (Actor, error) {
	if bearer == "" {
		return Actor{}, ErrBadCredentials
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.sessions = unexpired(a.sessions, now)
	if find(a.sessions, bearer) >= 0 {
		return Actor{Kind: KindAdmin, Session: sessionID(bearer)}, nil
	}
	if a.locked(now) {
		return Actor{}, ErrTooManyAttempts
	}
	if a.isAdminKey(bearer) {
		return Actor{Kind: KindAdmin}, nil
	}
	a.fail(now)
	return Actor{}, ErrBadCredentials
}

// Logout закрывает сессию токена bearer; ключ администратора и неизвестный токен ничего не меняют.
func (a *Auth) Logout(bearer string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if i := find(a.sessions, bearer); i >= 0 {
		a.sessions = slices.Delete(a.sessions, i, i+1)
	}
}

// pushBounded — s с новым элементом e в конце; больше limit — самые старые вытесняются.
func pushBounded(s []secret, e secret, limit int) []secret {
	if len(s) >= limit {
		s = slices.Delete(s, 0, len(s)-limit+1)
	}
	return append(s, e)
}

// find — номер элемента с SHA-256 значения v; сравнение за постоянное время со всеми элементами. -1 — нет.
func find(s []secret, v string) int {
	h := sha256.Sum256([]byte(v))
	found := -1
	for i, e := range s {
		if subtle.ConstantTimeCompare(h[:], e.hash[:]) == 1 {
			found = i
		}
	}
	return found
}

// unexpired — элементы со сроком позже now.
func unexpired(s []secret, now time.Time) []secret {
	return slices.DeleteFunc(s, func(e secret) bool { return !now.Before(e.expires) })
}

// isAdminKey — сравнение за постоянное время со всеми ключами администратора.
func (a *Auth) isAdminKey(v string) bool {
	match := 0
	for _, k := range a.keys {
		match |= subtle.ConstantTimeCompare([]byte(v), k)
	}
	return match == 1
}

func (a *Auth) locked(now time.Time) bool { return now.Before(a.lockedUntil) }

// fail — неудача в счёт: больше maxFailures за failWindow — блокировка на lockFor, счёт сбрасывается.
func (a *Auth) fail(now time.Time) {
	a.failures = slices.DeleteFunc(a.failures, func(t time.Time) bool { return !t.After(now.Add(-failWindow)) })
	a.failures = append(a.failures, now)
	if len(a.failures) > maxFailures {
		a.lockedUntil = now.Add(lockFor)
		a.log.Warn("admin login locked", "failures", len(a.failures), "until", a.lockedUntil.UTC().Format(time.RFC3339))
		a.failures = nil
	}
}

// randomToken — 32 байта crypto/rand в base64url без дополнения.
func randomToken() string {
	var b [32]byte
	_, _ = rand.Read(b[:]) // crypto/rand.Read не возвращает ошибку (Go ≥ 1.24)
	return base64.RawURLEncoding.EncodeToString(b[:])
}

// sessionID — первые 8 hex SHA-256 токена: номер сессии в журнале (§13), сам токен не пишется.
func sessionID(token string) string {
	h := sha256.Sum256([]byte(token))
	return hex.EncodeToString(h[:4])
}
