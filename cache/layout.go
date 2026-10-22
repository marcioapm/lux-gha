package cache

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"
)

// Object layout, all under the configured prefix:
//
//	v1/<repository_id>/<sha256(scope)>/<version>/<encoded key>   final entries
//	staging/<sha256(reservation)>                                 upload reservations
//
// The scope is hashed to a fixed 64 hex characters, the repository ID is
// digits only and the version is 64 hex characters, so no claim or request
// value can add or shift a path segment. encodeKey never emits '/'.

const (
	maxKeyBytes = 512
	// S3 object keys are at most 1024 bytes of UTF-8.
	maxObjectKeyBytes = 1024
)

var (
	versionPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
	repoIDPattern  = regexp.MustCompile(`^[0-9]{1,20}$`)
)

type layout struct {
	// prefix is empty or ends in '/'.
	prefix string
}

func newLayout(prefix string) layout {
	prefix = strings.Trim(prefix, "/")
	if prefix != "" {
		prefix += "/"
	}
	return layout{prefix: prefix}
}

func scopeHash(scope string) string {
	h := sha256.Sum256([]byte(scope))
	return hex.EncodeToString(h[:])
}

// entriesRoot is the prefix of every final entry, for the reaper.
func (l layout) entriesRoot() string { return l.prefix + "v1/" }

func (l layout) stagingRoot() string { return l.prefix + "staging/" }

// versionDir is the listing prefix of one repository, scope and version.
func (l layout) versionDir(repoID, scope, version string) string {
	return l.entriesRoot() + repoID + "/" + scopeHash(scope) + "/" + version + "/"
}

func (l layout) entryKey(repoID, scope, version, key string) string {
	return l.versionDir(repoID, scope, version) + encodeKey(key)
}

// stagingKey names the reservation record of one save: one repository,
// scope, version and key, made by one runtime token.
func (l layout) stagingKey(repoID, scope, version, key, tokenHash string) string {
	h := sha256.New()
	for _, s := range []string{repoID, scope, version, key, tokenHash} {
		// Length-prefixed, so no two tuples hash the same input.
		fmt.Fprintf(h, "%d:%s|", len(s), s)
	}
	return l.stagingRoot() + hex.EncodeToString(h.Sum(nil))
}

// keyFromEntry returns the cache key of a final entry's object key under dir.
func keyFromEntry(dir, objectKey string) (string, bool) {
	enc, ok := strings.CutPrefix(objectKey, dir)
	if !ok || enc == "" || strings.Contains(enc, "/") {
		return "", false
	}
	k, err := decodeKey(enc)
	if err != nil {
		return "", false
	}
	return k, true
}

func keyByteSafe(c byte, first bool) bool {
	switch {
	case 'a' <= c && c <= 'z', 'A' <= c && c <= 'Z', '0' <= c && c <= '9':
		return true
	case c == '-' || c == '_':
		return true
	case c == '.':
		// A leading dot is escaped: "." and ".." are path names to a
		// filesystem-backed S3 server.
		return !first
	}
	return false
}

// encodeKey escapes every byte outside [A-Za-z0-9._-] as %XX. Each byte maps
// independently of the rest except at offset 0, which is shared by a key
// and every prefix of it, so encodeKey(p) is a prefix of encodeKey(k)
// exactly when p is a prefix of k: a restore-key prefix match is an S3
// prefix listing. The encoding is injective and has no '/'.
func encodeKey(k string) string {
	var b strings.Builder
	for i := 0; i < len(k); i++ {
		c := k[i]
		if keyByteSafe(c, i == 0) {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

var errBadEncoding = errors.New("invalid key encoding")

// decodeKey inverts encodeKey and rejects any string encodeKey cannot
// produce, so one key has exactly one object name.
func decodeKey(s string) (string, error) {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c != '%' {
			if !keyByteSafe(c, b.Len() == 0) {
				return "", errBadEncoding
			}
			b.WriteByte(c)
			continue
		}
		if i+2 >= len(s) {
			return "", errBadEncoding
		}
		v, err := hex.DecodeString(s[i+1 : i+3])
		if err != nil || strings.ToUpper(s[i+1:i+3]) != s[i+1:i+3] || keyByteSafe(v[0], b.Len() == 0) {
			return "", errBadEncoding
		}
		b.WriteByte(v[0])
		i += 2
	}
	return b.String(), nil
}

// validateKey applies the client's own rules (at most 512 characters, no
// comma) plus non-empty and valid UTF-8; the client counts UTF-16 units,
// this counts bytes, which is never fewer.
func validateKey(k string) error {
	switch {
	case k == "":
		return errors.New("key must not be empty")
	case len(k) > maxKeyBytes:
		return fmt.Errorf("key exceeds %d bytes", maxKeyBytes)
	case strings.ContainsRune(k, ','):
		return errors.New("key must not contain a comma")
	case !utf8.ValidString(k):
		return errors.New("key must be valid UTF-8")
	}
	return nil
}

func validateVersion(v string) error {
	if !versionPattern.MatchString(v) {
		return errors.New("version must be 64 lowercase hex characters")
	}
	return nil
}
