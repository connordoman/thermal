// Package auth issues and verifies API keys.
//
// A key looks like thm_k3h9x2qa_4f6Zt0... : a fixed prefix that secret
// scanners and people can recognise, a public key ID that appears in logs and
// the audit trail, and a random secret. Only a SHA-256 hash of the secret is
// stored. The secret has 190 bits of entropy, so a fast hash is appropriate.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/connordoman/thermal/internal/store"
	"github.com/connordoman/thermal/internal/store/dbq"
)

// Prefix starts every key.
const Prefix = "thm_"

const (
	idLength     = 8
	secretLength = 32
	alphabet     = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
	idAlphabet   = "0123456789abcdefghijklmnopqrstuvwxyz"
)

// Scope is a permission granted to a key.
type Scope string

const (
	// ScopeAdmin manages keys and reads the audit log. It implies every
	// other scope.
	ScopeAdmin Scope = "admin"
	// ScopePrint submits, cancels and retries print jobs.
	ScopePrint Scope = "print"
	// ScopeRead reads printer information, jobs and the queue.
	ScopeRead Scope = "read"
)

// Scopes lists every scope.
var Scopes = []Scope{ScopeAdmin, ScopePrint, ScopeRead}

// ParseScopes parses scope names, rejecting unknown ones and duplicates.
func ParseScopes(names []string) ([]Scope, error) {
	if len(names) == 0 {
		return nil, errors.New("at least one scope is required")
	}
	var out []Scope
	for _, n := range names {
		s := Scope(strings.ToLower(strings.TrimSpace(n)))
		if !slices.Contains(Scopes, s) {
			return nil, fmt.Errorf("unknown scope %q (want admin, print or read)", n)
		}
		if !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	return out, nil
}

// JoinScopes formats scopes for storage.
func JoinScopes(scopes []Scope) string {
	s := make([]string, len(scopes))
	for i, sc := range scopes {
		s[i] = string(sc)
	}
	return strings.Join(s, " ")
}

// SplitScopes parses stored scopes.
func SplitScopes(s string) []Scope {
	var out []Scope
	for f := range strings.FieldsSeq(s) {
		out = append(out, Scope(f))
	}
	return out
}

// Allows reports whether scopes grant want.
func Allows(scopes []Scope, want Scope) bool {
	return slices.Contains(scopes, ScopeAdmin) || slices.Contains(scopes, want)
}

func randomString(n int, chars string) string {
	// Rejection sampling keeps the distribution uniform.
	limit := byte(256 - 256%len(chars))
	out := make([]byte, 0, n)
	buf := make([]byte, n*2)
	for len(out) < n {
		rand.Read(buf)
		for _, b := range buf {
			if b < limit && len(out) < n {
				out = append(out, chars[int(b)%len(chars)])
			}
		}
	}
	return string(out)
}

func hashSecret(secret string) []byte {
	h := sha256.Sum256([]byte(secret))
	return h[:]
}

// Format assembles a key from its ID and secret.
func Format(id, secret string) string { return Prefix + id + "_" + secret }

// Parse splits a key into its ID and secret.
func Parse(key string) (id, secret string, ok bool) {
	rest, ok := strings.CutPrefix(key, Prefix)
	if !ok {
		return "", "", false
	}
	id, secret, ok = strings.Cut(rest, "_")
	if !ok || len(id) != idLength || len(secret) != secretLength {
		return "", "", false
	}
	return id, secret, true
}

// Manager issues and verifies keys stored in the database.
type Manager struct {
	q *dbq.Queries
}

// NewManager returns a Manager backed by q.
func NewManager(q *dbq.Queries) *Manager { return &Manager{q: q} }

// Errors returned by [Manager.Verify].
var (
	ErrInvalidKey = errors.New("invalid API key")
	ErrRevoked    = errors.New("API key has been revoked")
	ErrExpired    = errors.New("API key has expired")
)

// Key is a verified key.
type Key struct {
	ID     string
	Name   string
	Scopes []Scope
}

// Allows reports whether the key grants want.
func (k *Key) Allows(want Scope) bool { return Allows(k.Scopes, want) }

// Create issues a new key and returns it in full. The full key cannot be
// recovered later.
func (m *Manager) Create(ctx context.Context, name string, scopes []Scope, expires *time.Time, createdBy string) (string, dbq.ApiKey, error) {
	id := randomString(idLength, idAlphabet)
	secret := randomString(secretLength, alphabet)
	var exp sql.NullInt64
	if expires != nil {
		exp = sql.NullInt64{Int64: store.Millis(*expires), Valid: true}
	}
	err := m.q.CreateAPIKey(ctx, dbq.CreateAPIKeyParams{
		ID:         id,
		Name:       name,
		Scopes:     JoinScopes(scopes),
		SecretHash: hashSecret(secret),
		CreatedBy:  store.NullString(createdBy),
		CreatedAt:  store.Now(),
		ExpiresAt:  exp,
	})
	if err != nil {
		return "", dbq.ApiKey{}, err
	}
	k, err := m.q.GetAPIKey(ctx, id)
	return Format(id, secret), k, err
}

// Rotate replaces a key's secret and returns the new full key. With a
// positive grace period the old secret keeps working until it ends, so
// clients can be updated without downtime.
func (m *Manager) Rotate(ctx context.Context, id string, grace time.Duration) (string, dbq.ApiKey, error) {
	k, err := m.q.GetAPIKey(ctx, id)
	if err != nil {
		return "", k, err
	}
	if k.RevokedAt.Valid {
		return "", k, ErrRevoked
	}
	secret := randomString(secretLength, alphabet)
	now := time.Now()
	var until sql.NullInt64
	if grace > 0 {
		until = sql.NullInt64{Int64: store.Millis(now.Add(grace)), Valid: true}
	}
	err = m.q.RotateAPIKey(ctx, dbq.RotateAPIKeyParams{
		GraceUntil: until,
		SecretHash: hashSecret(secret),
		RotatedAt:  sql.NullInt64{Int64: store.Millis(now), Valid: true},
		ID:         id,
	})
	if err != nil {
		return "", k, err
	}
	k, err = m.q.GetAPIKey(ctx, id)
	return Format(id, secret), k, err
}

// touchInterval limits how often last_used_at is written.
const touchInterval = time.Minute

// Verify checks a full key and returns its details.
func (m *Manager) Verify(ctx context.Context, key string) (*Key, error) {
	id, secret, ok := Parse(key)
	if !ok {
		return nil, ErrInvalidKey
	}
	k, err := m.q.GetAPIKey(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrInvalidKey
	}
	if err != nil {
		return nil, err
	}
	now := store.Now()
	h := hashSecret(secret)
	match := subtle.ConstantTimeCompare(h, k.SecretHash) == 1
	if !match && k.PreviousSecretHash != nil && k.PreviousExpiresAt.Valid && now < k.PreviousExpiresAt.Int64 {
		match = subtle.ConstantTimeCompare(h, k.PreviousSecretHash) == 1
	}
	if !match {
		return nil, ErrInvalidKey
	}
	if k.RevokedAt.Valid {
		return nil, ErrRevoked
	}
	if k.ExpiresAt.Valid && now >= k.ExpiresAt.Int64 {
		return nil, ErrExpired
	}
	if !k.LastUsedAt.Valid || now-k.LastUsedAt.Int64 > touchInterval.Milliseconds() {
		// Best effort: a failed write must not fail the request.
		_ = m.q.TouchAPIKey(ctx, dbq.TouchAPIKeyParams{LastUsedAt: sql.NullInt64{Int64: now, Valid: true}, ID: id})
	}
	return &Key{ID: k.ID, Name: k.Name, Scopes: SplitScopes(k.Scopes)}, nil
}
