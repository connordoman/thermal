package auth

import (
	"strings"
	"testing"
)

func TestFormatParse(t *testing.T) {
	id, secret := randomString(idLength, idAlphabet), randomString(secretLength, alphabet)
	key := Format(id, secret)
	if !strings.HasPrefix(key, "thm_") {
		t.Fatalf("key %q lacks prefix", key)
	}
	gid, gsecret, ok := Parse(key)
	if !ok || gid != id || gsecret != secret {
		t.Fatalf("Parse(%q) = %q, %q, %v", key, gid, gsecret, ok)
	}
	for _, bad := range []string{"", "thm_", "abc_" + id + "_" + secret, "thm_" + id + secret, "thm_short_" + secret, "thm_" + id + "_short"} {
		if _, _, ok := Parse(bad); ok {
			t.Errorf("Parse(%q) accepted", bad)
		}
	}
}

func TestScopes(t *testing.T) {
	if _, err := ParseScopes([]string{"print", "bogus"}); err == nil {
		t.Error("unknown scope accepted")
	}
	s, err := ParseScopes([]string{"Print", "read", "print"})
	if err != nil || JoinScopes(s) != "print read" {
		t.Errorf("got %v %v", s, err)
	}
	if !Allows([]Scope{ScopeAdmin}, ScopePrint) || Allows([]Scope{ScopeRead}, ScopePrint) {
		t.Error("Allows is wrong")
	}
}

func TestPasswords(t *testing.T) {
	h := HashPassword("hunter2hunter2")
	if !strings.HasPrefix(h, "$argon2id$v=19$m=19456,t=2,p=1$") {
		t.Errorf("hash format: %s", h)
	}
	if h == HashPassword("hunter2hunter2") {
		t.Error("hashes are not salted")
	}
	if ok, err := VerifyPassword(h, "hunter2hunter2"); !ok || err != nil {
		t.Errorf("right password: %v %v", ok, err)
	}
	if ok, _ := VerifyPassword(h, "hunter2hunter3"); ok {
		t.Error("wrong password accepted")
	}
	if CheckPassword("short") == nil || CheckPassword(strings.Repeat("x", MaxPasswordLength+1)) == nil {
		t.Error("CheckPassword accepted a bad length")
	}
	for in, want := range map[string]string{"Root": "root", " a.b-c_d ": "a.b-c_d"} {
		if got, err := NormalizeUsername(in); got != want || err != nil {
			t.Errorf("NormalizeUsername(%q) = %q, %v", in, got, err)
		}
	}
	for _, bad := range []string{"", "-x", "a b", "émile", strings.Repeat("a", 65)} {
		if _, err := NormalizeUsername(bad); err == nil {
			t.Errorf("NormalizeUsername(%q) accepted", bad)
		}
	}
}
