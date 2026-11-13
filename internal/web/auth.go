package web

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/argon2"
)

// Password hashes look like argon2id:65536:3:2:<salt>:<hash>. Colons instead
// of the usual dollar signs, because compose files and shells expand "$".
const (
	argonMemory  = 64 * 1024 // KiB
	argonTime    = 3
	argonThreads = 2
)

var b64 = base64.RawURLEncoding

func HashPassword(password string) string {
	salt := make([]byte, 16)
	rand.Read(salt)
	key := argon2.IDKey([]byte(password), salt, argonTime, argonMemory, argonThreads, 32)
	return fmt.Sprintf("argon2id:%d:%d:%d:%s:%s", argonMemory, argonTime, argonThreads, b64.EncodeToString(salt), b64.EncodeToString(key))
}

func CheckPassword(encoded, password string) bool {
	p := strings.Split(encoded, ":")
	if len(p) != 6 || p[0] != "argon2id" {
		return false
	}
	mem, err1 := strconv.ParseUint(p[1], 10, 32)
	iters, err2 := strconv.ParseUint(p[2], 10, 32)
	threads, err3 := strconv.ParseUint(p[3], 10, 8)
	salt, err4 := b64.DecodeString(p[4])
	want, err5 := b64.DecodeString(p[5])
	if err1 != nil || err2 != nil || err3 != nil || err4 != nil || err5 != nil || len(want) == 0 {
		return false
	}
	got := argon2.IDKey([]byte(password), salt, uint32(iters), uint32(mem), uint8(threads), uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1
}

// NewTOTPSecret returns a random base32 secret for an authenticator app.
func NewTOTPSecret() string {
	b := make([]byte, 20)
	rand.Read(b)
	return base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b)
}

// totpCode is RFC 6238: HMAC-SHA1, 30-second steps, six digits.
func totpCode(secret string, step int64) (string, error) {
	key, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(strings.ToUpper(strings.TrimRight(strings.ReplaceAll(secret, " ", ""), "=")))
	if err != nil {
		return "", err
	}
	var msg [8]byte
	binary.BigEndian.PutUint64(msg[:], uint64(step))
	mac := hmac.New(sha1.New, key)
	mac.Write(msg[:])
	sum := mac.Sum(nil)
	off := sum[len(sum)-1] & 0x0f
	code := binary.BigEndian.Uint32(sum[off:off+4]) & 0x7fffffff
	return fmt.Sprintf("%06d", code%1_000_000), nil
}

// CheckTOTP accepts the current code and the ones either side of it. It
// returns the time step that matched, so the caller can refuse a replay.
func CheckTOTP(secret, code string, now time.Time) (step int64, ok bool) {
	code = strings.ReplaceAll(strings.TrimSpace(code), " ", "")
	for d := int64(-1); d <= 1; d++ {
		want, err := totpCode(secret, now.Unix()/30+d)
		if err == nil && subtle.ConstantTimeCompare([]byte(want), []byte(code)) == 1 {
			step, ok = now.Unix()/30+d, true
		}
	}
	return step, ok
}

const (
	sessionLife   = 30 * 24 * time.Hour
	loginWindow   = 15 * time.Minute
	loginMaxFails = 5
)

func (s *Server) cookieName() string {
	if s.cfg.CookieSecure {
		return "__Host-session"
	}
	return "session"
}

func tokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func (s *Server) clientIP(r *http.Request) string {
	if s.cfg.TrustProxy {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			// The last hop is the one our own proxy appended.
			parts := strings.Split(xff, ",")
			return strings.TrimSpace(parts[len(parts)-1])
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func (s *Server) loggedIn(r *http.Request) bool {
	if s.cfg.AuthDisabled {
		return true
	}
	c, err := r.Cookie(s.cookieName())
	if err != nil || c.Value == "" {
		return false
	}
	var n int
	s.db.QueryRowContext(r.Context(), `SELECT COUNT(*) FROM sessions WHERE token_hash=? AND expires_at>?`,
		tokenHash(c.Value), time.Now().Unix()).Scan(&n)
	return n == 1
}

// sameOrigin rejects cross-site form posts. Browsers send Sec-Fetch-Site or
// Origin on every POST; together with the SameSite=Strict cookie this stands
// in for a CSRF token.
func sameOrigin(r *http.Request) bool {
	if site := r.Header.Get("Sec-Fetch-Site"); site != "" {
		return site == "same-origin" || site == "none"
	}
	if origin := r.Header.Get("Origin"); origin != "" {
		u, err := url.Parse(origin)
		return err == nil && u.Host == r.Host
	}
	return true // not a browser
}

// protect wraps every handler: security headers, the cross-site check, and
// the login requirement for everything except the few public paths.
func (s *Server) protect(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'none'; style-src 'self'; script-src 'self'; img-src 'self' data:; form-action 'self'; base-uri 'none'; frame-ancestors 'none'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Robots-Tag", "noindex, nofollow")
		h.Set("Cache-Control", "no-store")
		if s.cfg.CookieSecure {
			h.Set("Strict-Transport-Security", "max-age=31536000")
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead && !sameOrigin(r) {
			http.Error(w, "cross-site request refused", http.StatusForbidden)
			return
		}
		public := r.URL.Path == "/login" || r.URL.Path == "/healthz" || strings.HasPrefix(r.URL.Path, "/static/")
		if !public && !s.loggedIn(r) {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) loginPage(w http.ResponseWriter, r *http.Request) {
	if s.loggedIn(r) {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	s.render(w, "login.html", map[string]any{"Title": "Sign in", "TOTP": s.cfg.TOTPSecret != "", "Bare": true})
}

func (s *Server) loginSubmit(w http.ResponseWriter, r *http.Request) {
	ctx, ip, now := r.Context(), s.clientIP(r), time.Now()
	fail := func(status int, msg string) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(status)
		s.render(w, "login.html", map[string]any{"Title": "Sign in", "TOTP": s.cfg.TOTPSecret != "", "Bare": true, "Error": msg})
	}
	var fails int
	s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM login_attempts WHERE ip=? AND at>?`, ip, now.Add(-loginWindow).Unix()).Scan(&fails)
	if fails >= loginMaxFails {
		fail(http.StatusTooManyRequests, "Too many attempts. Try again in fifteen minutes.")
		return
	}
	// Always run both checks so a wrong password and a wrong code take the same time.
	passOK := CheckPassword(s.cfg.PasswordHash, r.FormValue("password"))
	codeOK := s.cfg.TOTPSecret == ""
	if step, ok := CheckTOTP(s.cfg.TOTPSecret, r.FormValue("code"), now); ok && passOK && !codeOK {
		// Each code works once: the insert fails if this step was already used.
		_, err := s.db.ExecContext(ctx, `INSERT INTO totp_used(step) VALUES(?)`, step)
		codeOK = err == nil
		s.db.ExecContext(ctx, `DELETE FROM totp_used WHERE step<?`, step-10)
	}
	if !passOK || !codeOK {
		s.db.ExecContext(ctx, `INSERT INTO login_attempts(ip, at) VALUES(?,?)`, ip, now.Unix())
		fail(http.StatusUnauthorized, "That did not match.")
		return
	}
	raw := make([]byte, 32)
	rand.Read(raw)
	token := hex.EncodeToString(raw)
	s.db.ExecContext(ctx, `DELETE FROM sessions WHERE expires_at<?`, now.Unix())
	s.db.ExecContext(ctx, `DELETE FROM login_attempts WHERE at<?`, now.Add(-24*time.Hour).Unix())
	if _, err := s.db.ExecContext(ctx, `INSERT INTO sessions(token_hash, created_at, expires_at) VALUES(?,?,?)`,
		tokenHash(token), now.Unix(), now.Add(sessionLife).Unix()); err != nil {
		http.Error(w, "could not start a session", http.StatusInternalServerError)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: s.cookieName(), Value: token, Path: "/", MaxAge: int(sessionLife.Seconds()),
		HttpOnly: true, Secure: s.cfg.CookieSecure, SameSite: http.SameSiteStrictMode,
	})
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(s.cookieName()); err == nil {
		s.db.ExecContext(r.Context(), `DELETE FROM sessions WHERE token_hash=?`, tokenHash(c.Value))
	}
	http.SetCookie(w, &http.Cookie{Name: s.cookieName(), Path: "/", MaxAge: -1, HttpOnly: true, Secure: s.cfg.CookieSecure, SameSite: http.SameSiteStrictMode})
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}
