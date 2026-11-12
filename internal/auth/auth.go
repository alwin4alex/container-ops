// Package auth handles sign-in (HTTP basic with PBKDF2 hashes, or a trusted
// reverse-proxy header), roles, and CSRF tokens for state-changing requests.
package auth

import (
	"context"
	"crypto/hmac"
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

	"container-ops/internal/config"
)

type Role string

const (
	Viewer   Role = "viewer"
	Deployer Role = "deployer"
)

type User struct {
	Name string
	Role Role
}

func (u User) CanDeploy() bool { return u.Role == Deployer }

type ctxKey struct{}

func FromContext(ctx context.Context) User {
	u, _ := ctx.Value(ctxKey{}).(User)
	return u
}

type Authenticator struct {
	cfg      config.Auth
	users    map[string]config.User
	trusted  []*net.IPNet
	csrfKey  []byte
	dummyPwd string

	// Verified basic-auth credentials, keyed by an HMAC of user+password, so
	// the slow PBKDF2 check runs once per few minutes instead of per request.
	okMu    sync.Mutex
	okCache map[string]time.Time
}

const okCacheTTL = 5 * time.Minute

func New(cfg config.Auth, secret string) (*Authenticator, error) {
	a := &Authenticator{cfg: cfg, users: map[string]config.User{}, okCache: map[string]time.Time{}}
	for _, u := range cfg.Users {
		a.users[u.Name] = u
	}
	for _, cidr := range cfg.TrustedProxies {
		if !strings.Contains(cidr, "/") {
			if strings.Contains(cidr, ":") {
				cidr += "/128"
			} else {
				cidr += "/32"
			}
		}
		_, n, err := net.ParseCIDR(cidr)
		if err != nil {
			return nil, fmt.Errorf("trusted_proxies: %w", err)
		}
		a.trusted = append(a.trusted, n)
	}
	if secret != "" {
		a.csrfKey = []byte(secret)
	} else {
		a.csrfKey = make([]byte, 32)
		if _, err := rand.Read(a.csrfKey); err != nil {
			return nil, err
		}
	}
	// Used to keep timing the same for unknown users.
	a.dummyPwd, _ = HashPassword("not-a-real-password")
	return a, nil
}

// Middleware authenticates every request and stores the user in the context.
func (a *Authenticator) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, err := a.authenticate(r)
		if err != nil {
			if a.cfg.Mode == "basic" {
				w.Header().Set("WWW-Authenticate", `Basic realm="Container Ops", charset="UTF-8"`)
			}
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, u)))
	})
}

func (a *Authenticator) authenticate(r *http.Request) (User, error) {
	switch a.cfg.Mode {
	case "none":
		return User{Name: "local", Role: Deployer}, nil
	case "header":
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil {
			return User{}, err
		}
		ip := net.ParseIP(host)
		trusted := false
		for _, n := range a.trusted {
			if n.Contains(ip) {
				trusted = true
				break
			}
		}
		name := strings.TrimSpace(r.Header.Get(a.cfg.UserHeader))
		if !trusted || name == "" {
			return User{}, errors.New("untrusted or missing user header")
		}
		role := Role(a.cfg.DefaultRole)
		if a.cfg.RoleHeader != "" {
			if v := Role(r.Header.Get(a.cfg.RoleHeader)); v == Viewer || v == Deployer {
				role = v
			}
		}
		return User{Name: name, Role: role}, nil
	default: // basic
		name, pass, ok := r.BasicAuth()
		if !ok {
			return User{}, errors.New("no credentials")
		}
		u, known := a.users[name]
		key := a.credKey(name, pass, u.PasswordHash)
		a.okMu.Lock()
		at, cached := a.okCache[key]
		a.okMu.Unlock()
		if known && cached && time.Since(at) < okCacheTTL {
			return User{Name: u.Name, Role: Role(u.Role)}, nil
		}
		hash := u.PasswordHash
		if !known {
			hash = a.dummyPwd
		}
		if !VerifyPassword(hash, pass) || !known {
			return User{}, errors.New("bad credentials")
		}
		a.okMu.Lock()
		if len(a.okCache) > 1000 {
			a.okCache = map[string]time.Time{}
		}
		a.okCache[key] = time.Now()
		a.okMu.Unlock()
		return User{Name: u.Name, Role: Role(u.Role)}, nil
	}
}

// credKey binds the cache entry to the current hash, so changing a user's
// password in the config invalidates cached sign-ins after a restart.
func (a *Authenticator) credKey(name, pass, hash string) string {
	m := hmac.New(sha256.New, a.csrfKey)
	m.Write([]byte("cred:" + name + "\x00" + pass + "\x00" + hash))
	return hex.EncodeToString(m.Sum(nil))
}

// CSRFToken is bound to the user name and signed with the server secret.
func (a *Authenticator) CSRFToken(u User) string {
	m := hmac.New(sha256.New, a.csrfKey)
	m.Write([]byte("csrf:" + u.Name))
	return hex.EncodeToString(m.Sum(nil))
}

func (a *Authenticator) CheckCSRF(r *http.Request) bool {
	want := a.CSRFToken(FromContext(r.Context()))
	got := r.Header.Get("X-CSRF-Token")
	if got == "" {
		got = r.PostFormValue("csrf")
	}
	return subtle.ConstantTimeCompare([]byte(want), []byte(got)) == 1
}

// ---- Password hashing: pbkdf2-sha256$<iterations>$<salt b64>$<hash b64> ----

const iterations = 600_000

func HashPassword(pw string) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key, err := pbkdf2.Key(sha256.New, pw, salt, iterations, 32)
	if err != nil {
		return "", err
	}
	enc := base64.RawStdEncoding
	return fmt.Sprintf("pbkdf2-sha256$%d$%s$%s", iterations, enc.EncodeToString(salt), enc.EncodeToString(key)), nil
}

func VerifyPassword(encoded, pw string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 4 || parts[0] != "pbkdf2-sha256" {
		return false
	}
	iter, err := strconv.Atoi(parts[1])
	if err != nil || iter < 10_000 {
		return false
	}
	enc := base64.RawStdEncoding
	salt, err1 := enc.DecodeString(parts[2])
	want, err2 := enc.DecodeString(parts[3])
	if err1 != nil || err2 != nil {
		return false
	}
	got, err := pbkdf2.Key(sha256.New, pw, salt, iter, len(want))
	if err != nil {
		return false
	}
	return subtle.ConstantTimeCompare(got, want) == 1
}
