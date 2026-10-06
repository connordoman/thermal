package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"sync"
	"unicode/utf8"

	"golang.org/x/crypto/argon2"
)

// Argon2id parameters: OWASP's minimum (19 MiB, 2 passes), which keeps a
// login well under a second on a Raspberry Pi.
const (
	argonMemory  = 19 * 1024 // KiB
	argonTime    = 2
	argonThreads = 1
	argonKeyLen  = 32
	argonSaltLen = 16
)

// Password length limits. The upper bound keeps hashing cheap for
// attackers' oversized inputs.
const (
	MinPasswordLength = 8
	MaxPasswordLength = 256
)

// ErrWeakPassword is returned for passwords outside the length limits.
var ErrWeakPassword = fmt.Errorf("password must be %d–%d characters", MinPasswordLength, MaxPasswordLength)

// CheckPassword reports whether a new password is acceptable.
func CheckPassword(password string) error {
	if n := utf8.RuneCountInString(password); n < MinPasswordLength || len(password) > MaxPasswordLength {
		return ErrWeakPassword
	}
	return nil
}

var b64 = base64.RawStdEncoding

// HashPassword returns an Argon2id hash of password in PHC string format.
func HashPassword(password string) string {
	salt := make([]byte, argonSaltLen)
	rand.Read(salt)
	key := argon2.IDKey([]byte(password), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, argonMemory, argonTime, argonThreads, b64.EncodeToString(salt), b64.EncodeToString(key))
}

// VerifyPassword reports whether password matches a hash from
// [HashPassword]. The hash's own parameters are used, so they can change
// without invalidating stored passwords.
func VerifyPassword(hash, password string) (bool, error) {
	parts := strings.Split(hash, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false, errors.New("auth: unsupported password hash")
	}
	var version int
	var memory, time uint32
	var threads uint8
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil || version != argon2.Version {
		return false, errors.New("auth: unsupported argon2 version")
	}
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &memory, &time, &threads); err != nil {
		return false, fmt.Errorf("auth: bad password hash parameters: %w", err)
	}
	salt, err := b64.DecodeString(parts[4])
	if err != nil {
		return false, err
	}
	want, err := b64.DecodeString(parts[5])
	if err != nil {
		return false, err
	}
	if len(password) > MaxPasswordLength {
		return false, nil
	}
	got := argon2.IDKey([]byte(password), salt, time, memory, threads, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}

// dummyHash is verified against when a username does not exist, so a
// failed login takes as long whether or not the user exists.
var dummyHash = sync.OnceValue(func() string { return HashPassword("not a real password") })

// GeneratePassword returns a random password for the root user.
func GeneratePassword() string { return randomString(24, alphabet) }
