package cache

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"regexp"
	"strings"
	"time"
)

// An upload token authorises the Azure Blob requests of one reservation. It
// is the base64url of
//
//	version byte (1) | expiry unix seconds (8) | staging key (rest)
//
// followed by '.' and the base64url HMAC-SHA256 of that payload. The staging
// key is a hash of the repository ID, scope, cache version, cache key and
// runtime token, so the token binds all of them.

const uploadTokenVersion = 1

var (
	errBadToken     = errors.New("upload token is invalid")
	errExpiredToken = errors.New("upload token has expired")
	tokenPattern    = regexp.MustCompile(`^[A-Za-z0-9_-]+\.[A-Za-z0-9_-]{43}$`)
)

const maxTokenLen = 2048

// signer mints with keys[0] and accepts any of keys, so a new key can be
// prepended while URLs minted with the old one are still in use.
type signer struct {
	keys [][]byte
}

func (s signer) mac(key, payload []byte) []byte {
	m := hmac.New(sha256.New, key)
	m.Write(payload)
	return m.Sum(nil)
}

func (s signer) mint(stagingKey string, expiry time.Time) string {
	payload := make([]byte, 0, 9+len(stagingKey))
	payload = append(payload, uploadTokenVersion)
	payload = binary.BigEndian.AppendUint64(payload, uint64(expiry.Unix()))
	payload = append(payload, stagingKey...)
	enc := base64.RawURLEncoding
	return enc.EncodeToString(payload) + "." + enc.EncodeToString(s.mac(s.keys[0], payload))
}

// open returns the staging key of a valid, unexpired token.
func (s signer) open(token string, now time.Time) (string, error) {
	if len(token) > maxTokenLen || !tokenPattern.MatchString(token) {
		return "", errBadToken
	}
	p, m, _ := strings.Cut(token, ".")
	enc := base64.RawURLEncoding
	payload, err1 := enc.DecodeString(p)
	sig, err2 := enc.DecodeString(m)
	if err1 != nil || err2 != nil || len(payload) < 10 || payload[0] != uploadTokenVersion {
		return "", errBadToken
	}
	ok := false
	for _, k := range s.keys {
		if hmac.Equal(sig, s.mac(k, payload)) {
			ok = true
		}
	}
	if !ok {
		return "", errBadToken
	}
	exp := int64(binary.BigEndian.Uint64(payload[1:9]))
	if now.Unix() >= exp {
		return "", errExpiredToken
	}
	return string(payload[9:]), nil
}
