package identity

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"strings"
	"testing"
)

var key = []byte("0123456789abcdef0123456789abcdef")

func TestTokenIsTheContractFormat(t *testing.T) {
	const name = "lux-gha-prod-1001-7-0"
	m := hmac.New(sha256.New, key)
	m.Write([]byte("v1." + name))
	want := "v1." + name + "." + base64.RawURLEncoding.EncodeToString(m.Sum(nil))
	if got := Token(key, name); got != want {
		t.Fatalf("token %q, want %q", got, want)
	}
	if got, err := Verify(key, want); err != nil || got != name {
		t.Fatalf("verify: %q %v", got, err)
	}
}

func TestVerifyRejectsForgedAndMalformedTokens(t *testing.T) {
	good := Token(key, "lux-gha-prod-1001-7-0")
	other := Token([]byte("another-key-another-key-another-k"), "lux-gha-prod-1001-7-0")
	mac := good[strings.LastIndex(good, ".")+1:]
	for name, tok := range map[string]string{
		"other key":         other,
		"renamed runner":    strings.Replace(good, "-7-0", "-8-0", 1),
		"flipped mac":       good[:len(good)-1] + map[bool]string{true: "A", false: "B"}[good[len(good)-1] != 'A'],
		"padded mac":        good + "=",
		"wrong version":     "v2" + good[2:],
		"extra part":        good + ".x",
		"no mac":            "v1.lux-gha-prod-1001-7-0",
		"empty":             "",
		"dotted name":       "v1.lux.gha." + mac,
		"uppercase name":    "v1.LUX-gha-prod-1001-7-0." + mac,
		"truncated mac":     good[:len(good)-4],
		"std base64 symbol": "v1.lux-gha-prod-1001-7-0.+/" + mac[2:],
	} {
		if _, err := Verify(key, tok); err != ErrInvalid {
			t.Errorf("%s: err=%v", name, err)
		}
	}
}
