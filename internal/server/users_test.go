package server

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

// signIn starts a session and returns its token.
func (h *harness) signIn(username, password string) string {
	h.t.Helper()
	req, _ := http.NewRequest("POST", h.srv.URL+"/v1/session",
		strings.NewReader(`{"username":"`+username+`","password":"`+password+`"}`))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		h.t.Fatalf("signing in as %s: %d", username, resp.StatusCode)
	}
	for _, c := range resp.Cookies() {
		if c.Name == SessionCookie {
			if !c.HttpOnly || c.SameSite != http.SameSiteLaxMode || c.Path != "/" || c.MaxAge <= 0 {
				h.t.Errorf("session cookie attributes: %+v", c)
			}
			return c.Value
		}
	}
	h.t.Fatal("no session cookie")
	return ""
}

func TestSessions(t *testing.T) {
	h := newHarness(t, "discard")
	if code, m, _ := h.do("POST", "/v1/session", "", `{"username":"root","password":"wrong password"}`); code != http.StatusUnauthorized || field(m, "error", "code") != "invalid_credentials" {
		t.Errorf("wrong password: %d %v", code, m)
	}
	if code, _, _ := h.do("POST", "/v1/session", "", `{"username":"nobody","password":"wrong password"}`); code != http.StatusUnauthorized {
		t.Errorf("unknown user: %d", code)
	}
	tok := h.signIn("ROOT", rootPassword) // usernames are case-insensitive
	code, m, _ := h.do("GET", "/v1/whoami", tok, "")
	if code != http.StatusOK || m["type"] != "user" || m["username"] != "root" || m["root"] != true {
		t.Errorf("whoami: %d %v", code, m)
	}
	code, m, _ = h.do("GET", "/v1/session", tok, "")
	if exp, _ := time.Parse(time.RFC3339, field(m, "session", "expires_at").(string)); code != http.StatusOK || time.Until(exp) < 59*time.Minute {
		t.Errorf("session: %d %v", code, m)
	}
	if code, _, _ := h.do("GET", "/v1/session", h.admin, ""); code != http.StatusNotFound {
		t.Errorf("session with an API key: %d", code)
	}
	// Jobs and audit events record the user.
	code, m, raw := h.do("POST", "/v1/print/text?wait=5s", tok, "hi")
	if code != http.StatusOK || field(m, "job", "username") != "root" || field(m, "job", "api_key_id") != nil {
		t.Errorf("printing as a user: %d %s", code, raw)
	}
	if _, m, _ := h.do("GET", "/v1/jobs?user=root", tok, ""); len(m["jobs"].([]any)) != 1 {
		t.Errorf("jobs by user: %v", m)
	}
	if _, m, _ := h.do("GET", "/v1/audit?user=root&action=session.created", tok, ""); len(m["events"].([]any)) != 1 {
		t.Errorf("audit by user: %v", m)
	}
	// An API key wins over a cookie.
	reader := h.newKey("read")
	code, _, _ = h.doReq("GET", "/v1/keys", reader, "", http.Header{"Cookie": {SessionCookie + "=" + tok}})
	if code != http.StatusForbidden {
		t.Errorf("key and cookie: %d", code)
	}
	if code, _, _ := h.do("DELETE", "/v1/session", tok, ""); code != http.StatusNoContent {
		t.Errorf("sign out: %d", code)
	}
	if code, _, _ := h.do("GET", "/v1/whoami", tok, ""); code != http.StatusUnauthorized {
		t.Errorf("signed-out session: %d", code)
	}
	if code, _, _ := h.do("GET", "/v1/whoami", "ths_bogus", ""); code != http.StatusUnauthorized {
		t.Errorf("bogus session: %d", code)
	}
}

func TestSessionCrossOrigin(t *testing.T) {
	h := newHarness(t, "discard")
	tok := h.signIn("root", rootPassword)
	cross := http.Header{"Sec-Fetch-Site": {"cross-site"}, "Origin": {"https://evil.example"}}
	if code, m, _ := h.doReq("POST", "/v1/print/text", tok, "hi", cross); code != http.StatusForbidden || field(m, "error", "code") != "cross_origin" {
		t.Errorf("cross-site POST with a cookie: %d %v", code, m)
	}
	if code, _, _ := h.doReq("GET", "/v1/whoami", tok, "", cross); code != http.StatusOK {
		t.Errorf("cross-site GET with a cookie: %d", code)
	}
	if code, _, _ := h.doReq("POST", "/v1/session", "", `{"username":"root","password":"`+rootPassword+`"}`, cross); code != http.StatusForbidden {
		t.Errorf("cross-site sign-in: %d", code)
	}
	same := http.Header{"Sec-Fetch-Site": {"same-origin"}}
	if code, _, _ := h.doReq("POST", "/v1/print/text?dry_run=1", tok, "hi", same); code != http.StatusOK {
		t.Errorf("same-origin POST with a cookie: %d", code)
	}
	// API keys are not sent automatically by browsers, so they are exempt.
	if code, _, _ := h.doReq("POST", "/v1/print/text?dry_run=1", h.admin, "hi", cross); code != http.StatusOK {
		t.Errorf("cross-site POST with an API key: %d", code)
	}
}

func TestLoginThrottle(t *testing.T) {
	h := newHarness(t, "discard")
	for range loginMaxFailures {
		h.do("POST", "/v1/session", "", `{"username":"root","password":"wrong password"}`)
	}
	code, _, _ := h.do("POST", "/v1/session", "", `{"username":"root","password":"`+rootPassword+`"}`)
	if code != http.StatusTooManyRequests {
		t.Errorf("after %d failures: %d", loginMaxFailures, code)
	}
}

func TestUsers(t *testing.T) {
	h := newHarness(t, "discard")
	root := h.signIn("root", rootPassword)
	code, m, raw := h.do("POST", "/v1/users", root, `{"username":"Alice","password":"alice-password","scopes":["print","read"]}`)
	if code != http.StatusCreated || field(m, "user", "username") != "alice" || field(m, "user", "created_by") != "user:root" {
		t.Fatalf("create: %d %s", code, raw)
	}
	if code, _, _ := h.do("POST", "/v1/users", root, `{"username":"alice","password":"alice-password","scopes":["read"]}`); code != http.StatusConflict {
		t.Errorf("duplicate: %d", code)
	}
	if code, _, _ := h.do("POST", "/v1/users", root, `{"username":"bob","password":"short","scopes":["read"]}`); code != http.StatusUnprocessableEntity {
		t.Errorf("short password: %d", code)
	}
	alice := h.signIn("alice", "alice-password")
	if code, _, _ := h.do("GET", "/v1/users", alice, ""); code != http.StatusForbidden {
		t.Errorf("non-admin listing users: %d", code)
	}
	if code, _, _ := h.do("PUT", "/v1/users/root/password", alice, `{"password":"hijacked-password"}`); code != http.StatusForbidden {
		t.Errorf("non-admin changing root's password: %d", code)
	}
	if code, _, _ := h.do("PUT", "/v1/users/alice/password", alice, `{"password":"new-password","current_password":"nope"}`); code != http.StatusUnprocessableEntity {
		t.Errorf("wrong current password: %d", code)
	}
	other := h.signIn("alice", "alice-password")
	if code, _, _ := h.do("PUT", "/v1/users/alice/password", alice, `{"password":"new-password","current_password":"alice-password"}`); code != http.StatusNoContent {
		t.Errorf("changing own password: %d", code)
	}
	if code, _, _ := h.do("GET", "/v1/whoami", alice, ""); code != http.StatusOK {
		t.Errorf("session that changed the password: %d", code)
	}
	if code, _, _ := h.do("GET", "/v1/whoami", other, ""); code != http.StatusUnauthorized {
		t.Errorf("other session after a password change: %d", code)
	}

	// Root cannot be disabled or demoted.
	if code, _, _ := h.do("DELETE", "/v1/users/root", root, ""); code != http.StatusConflict {
		t.Errorf("disabling root: %d", code)
	}
	if code, _, _ := h.do("PATCH", "/v1/users/root", h.admin, `{"scopes":["read"]}`); code != http.StatusConflict {
		t.Errorf("demoting root: %d", code)
	}
	code, m, _ = h.do("PATCH", "/v1/users/alice", root, `{"scopes":["read"]}`)
	if code != http.StatusOK || len(field(m, "user", "scopes").([]any)) != 1 {
		t.Errorf("changing scopes: %d %v", code, m)
	}
	if code, _, _ := h.do("POST", "/v1/print/text", alice, "hi"); code != http.StatusForbidden {
		t.Errorf("printing after losing the print scope: %d", code)
	}
	code, m, _ = h.do("DELETE", "/v1/users/alice", root, "")
	if code != http.StatusOK || field(m, "user", "disabled_at") == nil {
		t.Errorf("disable: %d %v", code, m)
	}
	if code, _, _ := h.do("GET", "/v1/whoami", alice, ""); code != http.StatusUnauthorized {
		t.Errorf("disabled user's session: %d", code)
	}
	if code, _, _ := h.do("POST", "/v1/session", "", `{"username":"alice","password":"new-password"}`); code != http.StatusUnauthorized {
		t.Errorf("disabled user signing in: %d", code)
	}
	if _, m, _ := h.do("GET", "/v1/users", root, ""); len(m["users"].([]any)) != 1 {
		t.Errorf("listing hides disabled users: %v", m)
	}
	h.do("PATCH", "/v1/users/alice", h.admin, `{"disabled":false}`)
	h.signIn("alice", "new-password")
	// An admin resets a password without the current one.
	if code, _, _ := h.do("PUT", "/v1/users/alice/password", root, `{"password":"reset-password"}`); code != http.StatusNoContent {
		t.Errorf("admin reset: %d", code)
	}
	h.signIn("alice", "reset-password")
}

func TestNextPageKeepsFilters(t *testing.T) {
	h := newHarness(t, "discard")
	for range 3 {
		h.do("POST", "/v1/print/text", h.admin, "hi")
	}
	_, m, _ := h.do("GET", "/v1/jobs?kind=text&limit=2", h.admin, "")
	if next, _ := m["next"].(string); next != "/v1/jobs?before=2&kind=text&limit=2" {
		t.Errorf("next: %q", next)
	}
}
