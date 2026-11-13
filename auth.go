package main

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	pbkdf2Iterations = 600_000
	sessionCookie    = "hd_session"
	sessionTTL       = 30 * 24 * time.Hour
)

func hashPassword(pw string) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key, err := pbkdf2.Key(sha256.New, pw, salt, pbkdf2Iterations, 32)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("pbkdf2-sha256$%d$%s$%s", pbkdf2Iterations,
		base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(key)), nil
}

func checkPassword(stored, pw string) bool {
	parts := strings.Split(stored, "$")
	if len(parts) != 4 || parts[0] != "pbkdf2-sha256" {
		return false
	}
	iter, err := strconv.Atoi(parts[1])
	if err != nil || iter < 1 {
		return false
	}
	salt, err1 := base64.RawStdEncoding.DecodeString(parts[2])
	want, err2 := base64.RawStdEncoding.DecodeString(parts[3])
	if err1 != nil || err2 != nil {
		return false
	}
	got, err := pbkdf2.Key(sha256.New, pw, salt, iter, len(want))
	if err != nil {
		return false
	}
	return subtle.ConstantTimeCompare(got, want) == 1
}

// dummyHash is checked against when an email is unknown, so login timing
// doesn't reveal which emails exist.
var dummyHash = func() string { h, _ := hashPassword("not-a-real-password"); return h }()

func newToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

func hashToken(t string) string {
	sum := sha256.Sum256([]byte(t))
	return hex.EncodeToString(sum[:])
}

func validPassword(pw string) error {
	if len(pw) < 10 {
		return errors.New("password must be at least 10 characters")
	}
	if len(pw) > 200 {
		return errors.New("password is too long")
	}
	return nil
}

// ---------- rate limiting ----------

type window struct {
	start time.Time
	count int
}

// RateLimiter is a fixed-window counter per key; small and good enough for
// one instance.
type RateLimiter struct {
	mu   sync.Mutex
	hits map[string]*window
}

func NewRateLimiter() *RateLimiter {
	rl := &RateLimiter{hits: map[string]*window{}}
	go func() {
		for range time.Tick(5 * time.Minute) {
			rl.mu.Lock()
			for k, w := range rl.hits {
				if time.Since(w.start) > 10*time.Minute {
					delete(rl.hits, k)
				}
			}
			rl.mu.Unlock()
		}
	}()
	return rl
}

func (rl *RateLimiter) Allow(key string, max int, per time.Duration) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	w := rl.hits[key]
	now := time.Now()
	if w == nil || now.Sub(w.start) > per {
		rl.hits[key] = &window{start: now, count: 1}
		return true
	}
	w.count++
	return w.count <= max
}

func (a *App) clientIP(r *http.Request) string {
	if a.cfg.TrustProxy {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			parts := strings.Split(xff, ",")
			return strings.TrimSpace(parts[len(parts)-1])
		}
		if xr := r.Header.Get("X-Real-IP"); xr != "" {
			return strings.TrimSpace(xr)
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
