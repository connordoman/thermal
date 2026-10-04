package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/connordoman/thermal/internal/auth"
	"github.com/connordoman/thermal/internal/console"
	"github.com/connordoman/thermal/internal/store"
	"github.com/connordoman/thermal/internal/store/dbq"
	"github.com/gin-gonic/gin"
)

type keyJSON struct {
	ID         string       `json:"id"`
	Name       string       `json:"name"`
	Scopes     []auth.Scope `json:"scopes"`
	Prefix     string       `json:"prefix"` // what the full key starts with, for recognising it
	CreatedBy  string       `json:"created_by,omitempty"`
	CreatedAt  time.Time    `json:"created_at"`
	RotatedAt  *time.Time   `json:"rotated_at,omitempty"`
	LastUsedAt *time.Time   `json:"last_used_at,omitempty"`
	ExpiresAt  *time.Time   `json:"expires_at,omitempty"`
	RevokedAt  *time.Time   `json:"revoked_at,omitempty"`
	// PreviousValidUntil is when the secret replaced by the last rotation
	// stops working.
	PreviousValidUntil *time.Time `json:"previous_valid_until,omitempty"`
}

func toKeyJSON(k dbq.ApiKey) keyJSON {
	out := keyJSON{
		ID: k.ID, Name: k.Name, Scopes: auth.SplitScopes(k.Scopes), Prefix: auth.Format(k.ID, ""),
		CreatedBy: k.CreatedBy.String, CreatedAt: time.UnixMilli(k.CreatedAt).UTC(),
		RotatedAt: store.Time(k.RotatedAt), LastUsedAt: store.Time(k.LastUsedAt),
		ExpiresAt: store.Time(k.ExpiresAt), RevokedAt: store.Time(k.RevokedAt),
	}
	if t := store.Time(k.PreviousExpiresAt); t != nil && t.After(time.Now()) {
		out.PreviousValidUntil = t
	}
	return out
}

func (s *Server) listKeys(c *gin.Context) {
	q := &query{c: c}
	keys, err := s.Store.ListAPIKeys(c, q.bool("include_revoked", false))
	if err != nil {
		abort(c, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	out := make([]keyJSON, len(keys))
	for i, k := range keys {
		out[i] = toKeyJSON(k)
	}
	c.JSON(http.StatusOK, gin.H{"keys": out})
}

func (s *Server) getKey(c *gin.Context) {
	k, err := s.Store.GetAPIKey(c, c.Param("id"))
	if err != nil {
		notFoundOr(c, err, "key")
		return
	}
	c.JSON(http.StatusOK, gin.H{"key": toKeyJSON(k)})
}

// expiry parses "expires_in" (a duration) or "expires_at" (RFC 3339).
func expiry(in, at string) (*time.Time, error) {
	switch {
	case in != "" && at != "":
		return nil, errors.New("give expires_in or expires_at, not both")
	case in != "":
		d, err := time.ParseDuration(in)
		if err != nil || d <= 0 {
			return nil, errors.New("expires_in must be a positive duration such as 720h")
		}
		t := time.Now().Add(d)
		return &t, nil
	case at != "":
		t, err := time.Parse(time.RFC3339, at)
		if err != nil {
			return nil, errors.New("expires_at must be an RFC 3339 time")
		}
		if !t.After(time.Now()) {
			return nil, errors.New("expires_at must be in the future")
		}
		return &t, nil
	}
	return nil, nil
}

func validName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" || utf8.RuneCountInString(name) > 100 {
		return "", errors.New("name must be 1–100 characters")
	}
	return name, nil
}

func bindJSON(c *gin.Context, v any) bool {
	d := json.NewDecoder(c.Request.Body)
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		abort(c, http.StatusBadRequest, "invalid_body", "invalid JSON body: "+err.Error())
		return false
	}
	return true
}

func (s *Server) createKey(c *gin.Context) {
	var req struct {
		Name      string   `json:"name"`
		Scopes    []string `json:"scopes"`
		ExpiresIn string   `json:"expires_in"`
		ExpiresAt string   `json:"expires_at"`
	}
	if !bindJSON(c, &req) {
		return
	}
	name, err := validName(req.Name)
	if err != nil {
		abort(c, http.StatusUnprocessableEntity, "invalid_body", err.Error())
		return
	}
	scopes, err := auth.ParseScopes(req.Scopes)
	if err != nil {
		abort(c, http.StatusUnprocessableEntity, "invalid_body", err.Error())
		return
	}
	exp, err := expiry(req.ExpiresIn, req.ExpiresAt)
	if err != nil {
		abort(c, http.StatusUnprocessableEntity, "invalid_body", err.Error())
		return
	}
	full, k, err := s.Keys.Create(c, name, scopes, exp, currentKey(c).ID)
	if err != nil {
		abort(c, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	s.audit(c, "key.created", k.ID, gin.H{"name": name, "scopes": scopes})
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusCreated, gin.H{
		"secret":  full,
		"key":     toKeyJSON(k),
		"warning": "store this key now; it cannot be shown again",
	})
}

// activeAdmins counts usable admin keys other than except.
func (s *Server) activeAdmins(ctx context.Context, except string) (int, error) {
	keys, err := s.Store.ListAPIKeys(ctx, false)
	if err != nil {
		return 0, err
	}
	n := 0
	now := store.Now()
	for _, k := range keys {
		if k.ID != except && (!k.ExpiresAt.Valid || k.ExpiresAt.Int64 > now) &&
			slices.Contains(auth.SplitScopes(k.Scopes), auth.ScopeAdmin) {
			n++
		}
	}
	return n, nil
}

func (s *Server) updateKey(c *gin.Context) {
	k, err := s.Store.GetAPIKey(c, c.Param("id"))
	if err != nil {
		notFoundOr(c, err, "key")
		return
	}
	if k.RevokedAt.Valid {
		abort(c, http.StatusConflict, "revoked", "revoked keys cannot be changed")
		return
	}
	var req struct {
		Name      *string         `json:"name"`
		Scopes    []string        `json:"scopes"`
		ExpiresIn string          `json:"expires_in"`
		ExpiresAt json.RawMessage `json:"expires_at"` // null clears
	}
	if !bindJSON(c, &req) {
		return
	}
	p := dbq.UpdateAPIKeyParams{ID: k.ID, Name: k.Name, Scopes: k.Scopes, ExpiresAt: k.ExpiresAt}
	if req.Name != nil {
		if p.Name, err = validName(*req.Name); err != nil {
			abort(c, http.StatusUnprocessableEntity, "invalid_body", err.Error())
			return
		}
	}
	if req.Scopes != nil {
		scopes, err := auth.ParseScopes(req.Scopes)
		if err != nil {
			abort(c, http.StatusUnprocessableEntity, "invalid_body", err.Error())
			return
		}
		if !slices.Contains(scopes, auth.ScopeAdmin) && slices.Contains(auth.SplitScopes(k.Scopes), auth.ScopeAdmin) {
			if n, err := s.activeAdmins(c, k.ID); err != nil || n == 0 {
				abort(c, http.StatusConflict, "last_admin", "this is the last admin key; create another before removing its admin scope")
				return
			}
		}
		p.Scopes = auth.JoinScopes(scopes)
	}
	if string(req.ExpiresAt) == "null" {
		p.ExpiresAt = sql.NullInt64{}
	} else {
		var at string
		if len(req.ExpiresAt) > 0 {
			if err := json.Unmarshal(req.ExpiresAt, &at); err != nil {
				abort(c, http.StatusUnprocessableEntity, "invalid_body", "expires_at must be an RFC 3339 string or null")
				return
			}
		}
		exp, err := expiry(req.ExpiresIn, at)
		if err != nil {
			abort(c, http.StatusUnprocessableEntity, "invalid_body", err.Error())
			return
		}
		if exp != nil {
			p.ExpiresAt = sql.NullInt64{Int64: store.Millis(*exp), Valid: true}
		}
	}
	if err := s.Store.UpdateAPIKey(c, p); err != nil {
		abort(c, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	s.audit(c, "key.updated", k.ID, gin.H{"name": p.Name, "scopes": p.Scopes})
	s.getKey(c)
}

func (s *Server) rotateKey(c *gin.Context) {
	var req struct {
		Grace string `json:"grace"`
	}
	if c.Request.ContentLength != 0 && !bindJSON(c, &req) {
		return
	}
	var grace time.Duration
	if req.Grace != "" {
		var err error
		grace, err = time.ParseDuration(req.Grace)
		if err != nil || grace < 0 || grace > 30*24*time.Hour {
			abort(c, http.StatusUnprocessableEntity, "invalid_body", "grace must be a duration up to 720h, such as 24h")
			return
		}
	}
	full, k, err := s.Keys.Rotate(c, c.Param("id"), grace)
	if errors.Is(err, auth.ErrRevoked) {
		abort(c, http.StatusConflict, "revoked", "revoked keys cannot be rotated")
		return
	}
	if err != nil {
		notFoundOr(c, err, "key")
		return
	}
	s.audit(c, "key.rotated", k.ID, gin.H{"grace": grace.String()})
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, gin.H{
		"secret":  full,
		"key":     toKeyJSON(k),
		"warning": "store this key now; it cannot be shown again",
	})
}

func (s *Server) revokeKey(c *gin.Context) {
	k, err := s.Store.GetAPIKey(c, c.Param("id"))
	if err != nil {
		notFoundOr(c, err, "key")
		return
	}
	if slices.Contains(auth.SplitScopes(k.Scopes), auth.ScopeAdmin) && !k.RevokedAt.Valid {
		if n, err := s.activeAdmins(c, k.ID); err != nil || n == 0 {
			abort(c, http.StatusConflict, "last_admin", "this is the last admin key; create another before revoking it")
			return
		}
	}
	n, err := s.Store.RevokeAPIKey(c, dbq.RevokeAPIKeyParams{RevokedAt: sql.NullInt64{Int64: store.Now(), Valid: true}, ID: k.ID})
	if err != nil {
		abort(c, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	if n > 0 {
		s.audit(c, "key.revoked", k.ID, nil)
	}
	s.getKey(c)
}

// audit records an action. Failures are logged, not returned: the action
// itself has already happened.
func (s *Server) audit(c *gin.Context, action, target string, detail any) {
	var d sql.NullString
	if detail != nil {
		if b, err := json.Marshal(detail); err == nil {
			d = sql.NullString{String: string(b), Valid: true}
		}
	}
	actor := ""
	if k := currentKey(c); k != nil {
		actor = k.ID
	}
	err := s.Store.InsertAuditEvent(context.WithoutCancel(c), dbq.InsertAuditEventParams{
		At: store.Now(), ActorKeyID: store.NullString(actor), Action: action,
		Target: store.NullString(target), Detail: d, ClientIp: store.NullString(c.ClientIP()),
	})
	if err != nil {
		console.Error("recording audit event %s: %v", action, err)
	}
}

func (s *Server) listAudit(c *gin.Context) {
	q := &query{c: c}
	limit := q.int("limit", 100, 1, 1000)
	p := dbq.ListAuditEventsParams{
		Action:     store.NullString(q.str("action", "")),
		ActorKeyID: store.NullString(q.str("actor", "")),
		BeforeID:   store.NullInt(int64(q.int("before", 0, 0, math.MaxInt))),
		Limit:      int64(limit),
	}
	if q.err != nil {
		renderError(c, q.err)
		return
	}
	rows, err := s.Store.ListAuditEvents(c, p)
	if err != nil {
		abort(c, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	type event struct {
		ID       int64           `json:"id"`
		At       time.Time       `json:"at"`
		Actor    string          `json:"actor_key_id,omitempty"`
		Action   string          `json:"action"`
		Target   string          `json:"target,omitempty"`
		Detail   json.RawMessage `json:"detail,omitempty"`
		ClientIP string          `json:"client_ip,omitempty"`
	}
	out := make([]event, len(rows))
	for i, r := range rows {
		out[i] = event{r.ID, time.UnixMilli(r.At).UTC(), r.ActorKeyID.String, r.Action, r.Target.String, nil, r.ClientIp.String}
		if r.Detail.Valid {
			out[i].Detail = json.RawMessage(r.Detail.String)
		}
	}
	resp := gin.H{"events": out}
	if len(rows) == limit {
		resp["next"] = "/v1/audit?before=" + itoa(rows[len(rows)-1].ID)
	}
	c.JSON(http.StatusOK, resp)
}
