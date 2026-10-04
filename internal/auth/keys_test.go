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
