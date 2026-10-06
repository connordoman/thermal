package server

import (
	"database/sql"
	"errors"
	"net/http"
	"time"

	"github.com/connordoman/thermal/internal/auth"
	"github.com/connordoman/thermal/internal/store"
	"github.com/connordoman/thermal/internal/store/dbq"
	"github.com/gin-gonic/gin"
)

type userJSON struct {
	Username          string       `json:"username"`
	Scopes            []auth.Scope `json:"scopes"`
	Root              bool         `json:"root"`
	CreatedBy         string       `json:"created_by,omitempty"`
	CreatedAt         time.Time    `json:"created_at"`
	UpdatedAt         time.Time    `json:"updated_at"`
	PasswordChangedAt time.Time    `json:"password_changed_at"`
	LastLoginAt       *time.Time   `json:"last_login_at,omitempty"`
	DisabledAt        *time.Time   `json:"disabled_at,omitempty"`
}

func toUserJSON(u dbq.User) userJSON {
	return userJSON{
		Username: u.Username, Scopes: auth.SplitScopes(u.Scopes), Root: u.Root != 0,
		CreatedBy: u.CreatedBy.String, CreatedAt: time.UnixMilli(u.CreatedAt).UTC(),
		UpdatedAt: time.UnixMilli(u.UpdatedAt).UTC(), PasswordChangedAt: time.UnixMilli(u.PasswordChangedAt).UTC(),
		LastLoginAt: store.Time(u.LastLoginAt), DisabledAt: store.Time(u.DisabledAt),
	}
}

// lookupUser loads the user named in the URL.
func (s *Server) lookupUser(c *gin.Context) (dbq.User, bool) {
	name, err := auth.NormalizeUsername(c.Param("username"))
	if err != nil {
		abort(c, http.StatusNotFound, "not_found", "user not found")
		return dbq.User{}, false
	}
	u, err := s.Store.GetUser(c, name)
	if err != nil {
		notFoundOr(c, err, "user")
		return u, false
	}
	return u, true
}

func (s *Server) listUsers(c *gin.Context) {
	q := &query{c: c}
	users, err := s.Store.ListUsers(c, q.bool("include_disabled", false))
	if err != nil {
		abort(c, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	out := make([]userJSON, len(users))
	for i, u := range users {
		out[i] = toUserJSON(u)
	}
	c.JSON(http.StatusOK, gin.H{"users": out})
}

func (s *Server) getUser(c *gin.Context) {
	u, ok := s.lookupUser(c)
	if !ok {
		return
	}
	c.JSON(http.StatusOK, gin.H{"user": toUserJSON(u)})
}

func (s *Server) createUser(c *gin.Context) {
	var req struct {
		Username string   `json:"username"`
		Password string   `json:"password"`
		Scopes   []string `json:"scopes"`
	}
	if !bindJSON(c, &req) {
		return
	}
	name, err := auth.NormalizeUsername(req.Username)
	if err != nil {
		abort(c, http.StatusUnprocessableEntity, "invalid_body", err.Error())
		return
	}
	scopes, err := auth.ParseScopes(req.Scopes)
	if err != nil {
		abort(c, http.StatusUnprocessableEntity, "invalid_body", err.Error())
		return
	}
	if err := auth.CheckPassword(req.Password); err != nil {
		abort(c, http.StatusUnprocessableEntity, "invalid_body", err.Error())
		return
	}
	if _, err := s.Store.GetUser(c, name); err == nil {
		abort(c, http.StatusConflict, "exists", "a user with that name already exists")
		return
	} else if !errors.Is(err, sql.ErrNoRows) {
		abort(c, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	u, err := s.Users.Create(c, name, req.Password, scopes, false, currentPrincipal(c).actor())
	if err != nil {
		abort(c, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	s.audit(c, "user.created", name, gin.H{"scopes": scopes})
	c.Header("Location", "/v1/users/"+name)
	c.JSON(http.StatusCreated, gin.H{"user": toUserJSON(u)})
}

// updateUser changes a user's scopes or disables (or re-enables) them.
func (s *Server) updateUser(c *gin.Context) {
	u, ok := s.lookupUser(c)
	if !ok {
		return
	}
	var req struct {
		Scopes   []string `json:"scopes"`
		Disabled *bool    `json:"disabled"`
	}
	if !bindJSON(c, &req) {
		return
	}
	p := dbq.UpdateUserParams{ID: u.ID, Scopes: u.Scopes, DisabledAt: u.DisabledAt, UpdatedAt: store.Now()}
	if req.Scopes != nil {
		scopes, err := auth.ParseScopes(req.Scopes)
		if err != nil {
			abort(c, http.StatusUnprocessableEntity, "invalid_body", err.Error())
			return
		}
		if u.Root != 0 && auth.JoinScopes(scopes) != u.Scopes {
			abort(c, http.StatusConflict, "root", "the root user's scopes cannot be changed")
			return
		}
		p.Scopes = auth.JoinScopes(scopes)
	}
	if req.Disabled != nil {
		if *req.Disabled && u.Root != 0 {
			abort(c, http.StatusConflict, "root", "the root user cannot be disabled")
			return
		}
		switch {
		case *req.Disabled && !u.DisabledAt.Valid:
			p.DisabledAt = sql.NullInt64{Int64: store.Now(), Valid: true}
		case !*req.Disabled:
			p.DisabledAt = sql.NullInt64{}
		}
	}
	if !s.saveUser(c, u, p) {
		return
	}
	s.getUser(c)
}

// disableUser is DELETE /v1/users/:username. Like revoked keys, disabled
// users are kept so the jobs and audit events they left stay attributable.
func (s *Server) disableUser(c *gin.Context) {
	u, ok := s.lookupUser(c)
	if !ok {
		return
	}
	if u.Root != 0 {
		abort(c, http.StatusConflict, "root", "the root user cannot be disabled")
		return
	}
	if !u.DisabledAt.Valid {
		now := store.Now()
		p := dbq.UpdateUserParams{ID: u.ID, Scopes: u.Scopes, DisabledAt: sql.NullInt64{Int64: now, Valid: true}, UpdatedAt: now}
		if !s.saveUser(c, u, p) {
			return
		}
	}
	s.getUser(c)
}

func (s *Server) saveUser(c *gin.Context, u dbq.User, p dbq.UpdateUserParams) bool {
	if err := s.Store.UpdateUser(c, p); err != nil {
		abort(c, http.StatusInternalServerError, "internal", err.Error())
		return false
	}
	disabled := p.DisabledAt.Valid && !u.DisabledAt.Valid
	if disabled {
		if err := s.Users.EndAllSessions(c, u.ID); err != nil {
			abort(c, http.StatusInternalServerError, "internal", err.Error())
			return false
		}
	}
	detail := gin.H{"scopes": p.Scopes}
	switch {
	case disabled:
		s.audit(c, "user.disabled", u.Username, nil)
	case !p.DisabledAt.Valid && u.DisabledAt.Valid:
		s.audit(c, "user.enabled", u.Username, nil)
	}
	if p.Scopes != u.Scopes {
		s.audit(c, "user.updated", u.Username, detail)
	}
	return true
}

// setPassword is PUT /v1/users/:username/password. Users change their own
// password by giving the current one; admins can set anyone's.
func (s *Server) setPassword(c *gin.Context) {
	caller := currentPrincipal(c)
	u, ok := s.lookupUser(c)
	if !ok {
		return
	}
	self := caller.user != nil && caller.user.ID == u.ID
	if !self && !caller.allows(auth.ScopeAdmin) {
		abort(c, http.StatusForbidden, "forbidden", "only admins can change other users' passwords")
		return
	}
	var req struct {
		Password        string `json:"password"`
		CurrentPassword string `json:"current_password"`
	}
	if !bindJSON(c, &req) {
		return
	}
	if self {
		ok, err := auth.VerifyPassword(u.PasswordHash, req.CurrentPassword)
		if err != nil {
			abort(c, http.StatusInternalServerError, "internal", err.Error())
			return
		}
		if !ok {
			abort(c, http.StatusUnprocessableEntity, "invalid_credentials", "current_password is wrong")
			return
		}
	}
	if err := auth.CheckPassword(req.Password); err != nil {
		abort(c, http.StatusUnprocessableEntity, "invalid_body", err.Error())
		return
	}
	// Changing your own password keeps you signed in here and signs you
	// out everywhere else; an admin's reset signs the user out everywhere.
	var keep []byte
	if self && caller.session != nil {
		keep = caller.session.TokenHash
	}
	if err := s.Users.SetPassword(c, u.ID, req.Password, keep); err != nil {
		abort(c, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	s.audit(c, "user.password_changed", u.Username, nil)
	c.Status(http.StatusNoContent)
}

// whoami describes the caller, whether an API key or a user.
func (s *Server) whoami(c *gin.Context) {
	p := currentPrincipal(c)
	if p.user != nil {
		c.JSON(http.StatusOK, gin.H{
			"type": "user", "name": p.name, "username": p.user.Username, "scopes": p.scopes, "root": p.user.Root != 0,
		})
		return
	}
	c.JSON(http.StatusOK, gin.H{"type": "key", "id": p.keyID, "name": p.name, "scopes": p.scopes})
}
