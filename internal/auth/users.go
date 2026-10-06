package auth

import (
	"context"
	"database/sql"
	"errors"
	"regexp"
	"strings"
	"time"

	"github.com/connordoman/thermal/internal/store"
	"github.com/connordoman/thermal/internal/store/dbq"
)

// SessionPrefix starts every session token, so one cannot be mistaken for
// an API key.
const SessionPrefix = "ths_"

const sessionTokenLength = 43 // 256 bits of entropy

// Errors returned by [Users].
var (
	// ErrBadCredentials covers unknown users, wrong passwords and disabled
	// accounts alike, so a login reveals nothing about which accounts exist.
	ErrBadCredentials = errors.New("invalid username or password")
	ErrNoSession      = errors.New("invalid or expired session")
	ErrInvalidName    = errors.New("username must be 1–64 characters: letters, digits, '.', '_' or '-', starting with a letter or digit")
)

var usernamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

// NormalizeUsername lowercases a username and checks it is well formed.
func NormalizeUsername(name string) (string, error) {
	name = strings.ToLower(strings.TrimSpace(name))
	if !usernamePattern.MatchString(name) {
		return "", ErrInvalidName
	}
	return name, nil
}

// Users manages user accounts and their sessions.
type Users struct {
	q *dbq.Queries
	// SessionTTL is how long a session lasts after signing in.
	SessionTTL time.Duration
}

// NewUsers returns a Users backed by q whose sessions last ttl.
func NewUsers(q *dbq.Queries, ttl time.Duration) *Users {
	return &Users{q: q, SessionTTL: ttl}
}

// Create adds a user. The root user always has just the admin scope.
func (u *Users) Create(ctx context.Context, username, password string, scopes []Scope, root bool, createdBy string) (dbq.User, error) {
	if err := CheckPassword(password); err != nil {
		return dbq.User{}, err
	}
	if root {
		scopes = []Scope{ScopeAdmin}
	}
	now := store.Now()
	var r int64
	if root {
		r = 1
	}
	return u.q.CreateUser(ctx, dbq.CreateUserParams{
		Username:          username,
		PasswordHash:      HashPassword(password),
		Scopes:            JoinScopes(scopes),
		Root:              r,
		CreatedBy:         store.NullString(createdBy),
		CreatedAt:         now,
		UpdatedAt:         now,
		PasswordChangedAt: now,
	})
}

// Authenticate checks a username and password.
func (u *Users) Authenticate(ctx context.Context, username, password string) (dbq.User, error) {
	name, err := NormalizeUsername(username)
	if err != nil {
		VerifyPassword(dummyHash(), password)
		return dbq.User{}, ErrBadCredentials
	}
	user, err := u.q.GetUser(ctx, name)
	if errors.Is(err, sql.ErrNoRows) {
		VerifyPassword(dummyHash(), password)
		return dbq.User{}, ErrBadCredentials
	}
	if err != nil {
		return dbq.User{}, err
	}
	ok, err := VerifyPassword(user.PasswordHash, password)
	if err != nil {
		return dbq.User{}, err
	}
	if !ok || user.DisabledAt.Valid {
		return dbq.User{}, ErrBadCredentials
	}
	return user, nil
}

// SetPassword changes a user's password and ends their sessions, except
// the one whose token hash is keep (nil ends them all).
func (u *Users) SetPassword(ctx context.Context, id int64, password string, keep []byte) error {
	if err := CheckPassword(password); err != nil {
		return err
	}
	now := store.Now()
	err := u.q.SetUserPassword(ctx, dbq.SetUserPasswordParams{
		PasswordHash: HashPassword(password), PasswordChangedAt: now, UpdatedAt: now, ID: id,
	})
	if err != nil {
		return err
	}
	_, err = u.q.DeleteUserSessions(ctx, dbq.DeleteUserSessionsParams{UserID: id, Keep: keep})
	return err
}

// Session is a verified session.
type Session struct {
	TokenHash []byte
	CreatedAt time.Time
	ExpiresAt time.Time
	User      dbq.User
}

// HashToken returns the stored form of a session token.
func HashToken(token string) []byte { return hashSecret(token) }

// StartSession signs a user in, returning the token for the session cookie.
func (u *Users) StartSession(ctx context.Context, user dbq.User, clientIP, userAgent string) (string, time.Time, error) {
	token := SessionPrefix + randomString(sessionTokenLength, alphabet)
	now := time.Now()
	expires := now.Add(u.SessionTTL)
	err := u.q.CreateSession(ctx, dbq.CreateSessionParams{
		TokenHash:  HashToken(token),
		UserID:     user.ID,
		CreatedAt:  store.Millis(now),
		ExpiresAt:  store.Millis(expires),
		LastSeenAt: store.Millis(now),
		ClientIp:   store.NullString(clientIP),
		UserAgent:  store.NullString(userAgent),
	})
	if err != nil {
		return "", time.Time{}, err
	}
	// Best effort housekeeping: expired sessions are useless.
	_, _ = u.q.DeleteExpiredSessions(ctx, store.Millis(now))
	_ = u.q.TouchUserLogin(ctx, dbq.TouchUserLoginParams{LastLoginAt: sql.NullInt64{Int64: store.Millis(now), Valid: true}, ID: user.ID})
	return token, expires, nil
}

// VerifySession checks a session token.
func (u *Users) VerifySession(ctx context.Context, token string) (*Session, error) {
	if !strings.HasPrefix(token, SessionPrefix) || len(token) != len(SessionPrefix)+sessionTokenLength {
		return nil, ErrNoSession
	}
	h := HashToken(token)
	row, err := u.q.GetSession(ctx, h)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNoSession
	}
	if err != nil {
		return nil, err
	}
	now := store.Now()
	if now >= row.Session.ExpiresAt || row.User.DisabledAt.Valid {
		_ = u.q.DeleteSession(ctx, h)
		return nil, ErrNoSession
	}
	if now-row.Session.LastSeenAt > touchInterval.Milliseconds() {
		_ = u.q.TouchSession(ctx, dbq.TouchSessionParams{LastSeenAt: now, TokenHash: h})
	}
	return &Session{
		TokenHash: h,
		CreatedAt: time.UnixMilli(row.Session.CreatedAt).UTC(),
		ExpiresAt: time.UnixMilli(row.Session.ExpiresAt).UTC(),
		User:      row.User,
	}, nil
}

// EndSession signs a session out.
func (u *Users) EndSession(ctx context.Context, tokenHash []byte) error {
	return u.q.DeleteSession(ctx, tokenHash)
}

// EndAllSessions signs a user out everywhere.
func (u *Users) EndAllSessions(ctx context.Context, userID int64) error {
	_, err := u.q.DeleteUserSessions(ctx, dbq.DeleteUserSessionsParams{UserID: userID})
	return err
}

// EnsureRoot creates the root user if there is none, like a database
// server's superuser: with password if it is set, otherwise with a random
// password that is returned so it can be shown once. An existing root user
// is left alone.
func (u *Users) EnsureRoot(ctx context.Context, username, password string) (user dbq.User, generated string, created bool, err error) {
	user, err = u.q.GetRootUser(ctx)
	if err == nil {
		return user, "", false, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return user, "", false, err
	}
	name, err := NormalizeUsername(username)
	if err != nil {
		return user, "", false, err
	}
	if password == "" {
		password = GeneratePassword()
		generated = password
	}
	user, err = u.Create(ctx, name, password, nil, true, "server")
	return user, generated, err == nil, err
}

// ResetRootPassword gives the root user a new random password, ends its
// sessions and returns the password.
func (u *Users) ResetRootPassword(ctx context.Context) (dbq.User, string, error) {
	user, err := u.q.GetRootUser(ctx)
	if err != nil {
		return user, "", err
	}
	password := GeneratePassword()
	return user, password, u.SetPassword(ctx, user.ID, password, nil)
}
