package server

import (
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/connordoman/thermal/internal/auth"
	"github.com/connordoman/thermal/internal/console"
	"github.com/connordoman/thermal/internal/store/dbq"
	"github.com/gin-gonic/gin"
)

// SessionCookie holds a signed-in user's session token.
const SessionCookie = "thermal_session"

// principal is whoever made a request: an API key or a signed-in user.
type principal struct {
	keyID   string        // set when an API key authenticated the request
	user    *dbq.User     // set when a session cookie did
	session *auth.Session // likewise
	name    string
	scopes  []auth.Scope
}

func (p *principal) allows(want auth.Scope) bool { return auth.Allows(p.scopes, want) }

// username is the signed-in user's name, or "" for an API key.
func (p *principal) username() string {
	if p.user != nil {
		return p.user.Username
	}
	return ""
}

// actor names the principal in created_by fields: a key ID, or
// "user:<name>".
func (p *principal) actor() string {
	if p.user != nil {
		return "user:" + p.user.Username
	}
	return p.keyID
}

const principalContext = "thermal.principal"

func currentPrincipal(c *gin.Context) *principal {
	if v, ok := c.Get(principalContext); ok {
		return v.(*principal)
	}
	return nil
}

// authenticate accepts an API key in a header or, failing that, a session
// cookie. API keys take precedence so scripts are unaffected by cookies.
func (s *Server) authenticate(c *gin.Context) {
	token := c.GetHeader("X-API-Key")
	if h := c.GetHeader("Authorization"); token == "" && h != "" {
		scheme, value, _ := strings.Cut(h, " ")
		if strings.EqualFold(scheme, "Bearer") {
			token = strings.TrimSpace(value)
		}
	}
	if token != "" {
		s.authenticateKey(c, token)
		return
	}
	if cookie, err := c.Cookie(SessionCookie); err == nil && cookie != "" {
		s.authenticateSession(c, cookie)
		return
	}
	c.Header("WWW-Authenticate", `Bearer realm="thermal"`)
	abort(c, http.StatusUnauthorized, "unauthorized", "an API key or a session is required")
}

func (s *Server) authenticateKey(c *gin.Context, token string) {
	key, err := s.Keys.Verify(c, token)
	switch {
	case errors.Is(err, auth.ErrInvalidKey), errors.Is(err, auth.ErrRevoked), errors.Is(err, auth.ErrExpired):
		console.Warn("rejected API key from %s: %v", c.ClientIP(), err)
		c.Header("WWW-Authenticate", `Bearer realm="thermal", error="invalid_token"`)
		abort(c, http.StatusUnauthorized, "unauthorized", err.Error())
		return
	case err != nil:
		console.Error("verifying API key: %v", err)
		abort(c, http.StatusInternalServerError, "internal", "could not verify the API key")
		return
	}
	c.Set(principalContext, &principal{keyID: key.ID, name: key.Name, scopes: key.Scopes})
	c.Next()
}

func (s *Server) authenticateSession(c *gin.Context, token string) {
	sess, err := s.Users.VerifySession(c, token)
	if errors.Is(err, auth.ErrNoSession) {
		s.clearSessionCookie(c)
		abort(c, http.StatusUnauthorized, "unauthorized", err.Error())
		return
	}
	if err != nil {
		console.Error("verifying session: %v", err)
		abort(c, http.StatusInternalServerError, "internal", "could not verify the session")
		return
	}
	// A browser sends cookies with cross-site requests, so state-changing
	// requests must come from a page on the same origin (or a trusted one).
	if !s.sameOrigin(c) {
		return
	}
	c.Set(principalContext, &principal{
		user: &sess.User, session: sess, name: sess.User.Username, scopes: auth.SplitScopes(sess.User.Scopes),
	})
	c.Next()
}

// sameOrigin rejects cross-origin browser requests with unsafe methods,
// using the Sec-Fetch-Site and Origin headers. Requests from other clients,
// which send neither, are allowed.
func (s *Server) sameOrigin(c *gin.Context) bool {
	s.csrfOnce.Do(func() {
		s.csrf = http.NewCrossOriginProtection()
		for _, o := range s.TrustedOrigins {
			if err := s.csrf.AddTrustedOrigin(o); err != nil {
				console.Error("trusted origin %q: %v", o, err)
			}
		}
	})
	if err := s.csrf.Check(c.Request); err != nil {
		console.Warn("rejected cross-origin %s %s from %s (origin %q)", c.Request.Method, c.Request.URL.Path, c.ClientIP(), c.GetHeader("Origin"))
		abort(c, http.StatusForbidden, "cross_origin", "cross-origin requests are not allowed with a session cookie")
		return false
	}
	return true
}

func (s *Server) setSessionCookie(c *gin.Context, token string, expires time.Time) {
	http.SetCookie(c.Writer, &http.Cookie{
		Name:     SessionCookie,
		Value:    token,
		Path:     "/",
		Expires:  expires,
		MaxAge:   int(time.Until(expires).Seconds()),
		Secure:   s.SessionSecure,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
}

func (s *Server) clearSessionCookie(c *gin.Context) {
	http.SetCookie(c.Writer, &http.Cookie{
		Name: SessionCookie, Value: "", Path: "/", MaxAge: -1,
		Secure: s.SessionSecure, HttpOnly: true, SameSite: http.SameSiteLaxMode,
	})
}

type sessionJSON struct {
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
}

// createSession signs a user in: POST /v1/session.
func (s *Server) createSession(c *gin.Context) {
	if !s.sameOrigin(c) {
		return
	}
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if !bindJSON(c, &req) {
		return
	}
	throttleKey := strings.ToLower(strings.TrimSpace(req.Username))
	if wait := s.logins.blocked(throttleKey); wait > 0 {
		c.Header("Retry-After", itoa(int64(wait.Seconds()+1)))
		abort(c, http.StatusTooManyRequests, "too_many_attempts", "too many failed sign-ins; try again later")
		return
	}
	user, err := s.Users.Authenticate(c, req.Username, req.Password)
	if errors.Is(err, auth.ErrBadCredentials) {
		s.logins.fail(throttleKey)
		console.Warn("failed sign-in for %q from %s", throttleKey, c.ClientIP())
		s.auditAs(c, "", "", "session.failed", throttleKey, nil)
		abort(c, http.StatusUnauthorized, "invalid_credentials", err.Error())
		return
	}
	if err != nil {
		console.Error("signing in: %v", err)
		abort(c, http.StatusInternalServerError, "internal", "could not sign in")
		return
	}
	s.logins.reset(throttleKey)
	token, expires, err := s.Users.StartSession(c, user, c.ClientIP(), truncate(c.Request.UserAgent(), 512))
	if err != nil {
		console.Error("starting session: %v", err)
		abort(c, http.StatusInternalServerError, "internal", "could not sign in")
		return
	}
	s.auditAs(c, "", user.Username, "session.created", user.Username, nil)
	s.setSessionCookie(c, token, expires)
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusCreated, gin.H{
		"user":    toUserJSON(user),
		"session": sessionJSON{CreatedAt: time.Now().UTC(), ExpiresAt: expires.UTC()},
	})
}

// getSession describes the caller's session: GET /v1/session.
func (s *Server) getSession(c *gin.Context) {
	p := currentPrincipal(c)
	if p.session == nil {
		abort(c, http.StatusNotFound, "no_session", "this request was authenticated with an API key, not a session")
		return
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, gin.H{
		"user":    toUserJSON(*p.user),
		"session": sessionJSON{CreatedAt: p.session.CreatedAt, ExpiresAt: p.session.ExpiresAt},
	})
}

// deleteSession signs out: DELETE /v1/session.
func (s *Server) deleteSession(c *gin.Context) {
	p := currentPrincipal(c)
	if p.session == nil {
		abort(c, http.StatusNotFound, "no_session", "this request was authenticated with an API key, not a session")
		return
	}
	if err := s.Users.EndSession(c, p.session.TokenHash); err != nil {
		abort(c, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	s.audit(c, "session.deleted", p.user.Username, nil)
	s.clearSessionCookie(c)
	c.Status(http.StatusNoContent)
}

// Failed sign-ins are limited per username, since behind a reverse proxy
// every request can come from the same address.
const (
	loginWindow      = 15 * time.Minute
	loginMaxFailures = 10
)

type loginThrottle struct {
	mu    sync.Mutex
	fails map[string][]time.Time
}

func (t *loginThrottle) recent(key string, now time.Time) []time.Time {
	var keep []time.Time
	for _, at := range t.fails[key] {
		if now.Sub(at) < loginWindow {
			keep = append(keep, at)
		}
	}
	if len(keep) == 0 {
		delete(t.fails, key)
	} else {
		t.fails[key] = keep
	}
	return keep
}

// blocked returns how long until key may try again, or 0.
func (t *loginThrottle) blocked(key string) time.Duration {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := time.Now()
	fails := t.recent(key, now)
	if len(fails) < loginMaxFailures {
		return 0
	}
	return loginWindow - now.Sub(fails[len(fails)-loginMaxFailures])
}

func (t *loginThrottle) fail(key string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.fails == nil {
		t.fails = map[string][]time.Time{}
	}
	if len(t.fails) > 10000 { // bound memory under a flood of usernames
		for k := range t.fails {
			t.recent(k, time.Now())
		}
	}
	t.fails[key] = append(t.fails[key], time.Now())
}

func (t *loginThrottle) reset(key string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.fails, key)
}
