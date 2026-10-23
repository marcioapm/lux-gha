// Package identity mints and verifies the per-Run identity token a runner
// presents to the bridge's runner listener:
//
//	v1.<runner name>.<base64url(HMAC-SHA256(key, "v1." + runner name))>
//
// The token only proves "I am runner R"; the bridge derives everything else
// from Lux and GitHub. It is deterministic, so a replayed Lux submission
// carries the same token.
package identity

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"regexp"
	"strings"
)

const version = "v1"

// Runner names are lux-gha-<instance>-<repo id>-<job id>-<gen>: no dots, so
// the token splits unambiguously.
var runnerNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,99}$`)

var ErrInvalid = errors.New("identity: invalid token")

// Token returns the token for runner. The MAC is unpadded base64url.
func Token(key []byte, runner string) string {
	return version + "." + runner + "." + base64.RawURLEncoding.EncodeToString(mac(key, runner))
}

// Verify returns the runner name a token was minted for, comparing the MAC
// in constant time. Every failure is ErrInvalid, without detail.
func Verify(key []byte, token string) (string, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 || parts[0] != version || !runnerNameRe.MatchString(parts[1]) {
		return "", ErrInvalid
	}
	// Strict: a MAC string with non-zero trailing bits does not decode, so
	// each runner has exactly one valid token.
	got, err := base64.RawURLEncoding.Strict().DecodeString(parts[2])
	if err != nil || !hmac.Equal(got, mac(key, parts[1])) {
		return "", ErrInvalid
	}
	return parts[1], nil
}

func mac(key []byte, runner string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(version + "." + runner))
	return m.Sum(nil)
}
