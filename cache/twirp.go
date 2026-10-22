package cache

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
)

// Twirp error codes used here and their HTTP statuses (twitchtv/twirp spec).
const (
	twirpInvalidArgument  = "invalid_argument"
	twirpMalformed        = "malformed"
	twirpUnauthenticated  = "unauthenticated"
	twirpPermissionDenied = "permission_denied"
	twirpBadRoute         = "bad_route"
	twirpInternal         = "internal"
	twirpUnavailable      = "unavailable"
)

var twirpStatus = map[string]int{
	twirpInvalidArgument:  http.StatusBadRequest,
	twirpMalformed:        http.StatusBadRequest,
	twirpUnauthenticated:  http.StatusUnauthorized,
	twirpPermissionDenied: http.StatusForbidden,
	twirpBadRoute:         http.StatusNotFound,
	twirpInternal:         http.StatusInternalServerError,
	twirpUnavailable:      http.StatusServiceUnavailable,
}

type twirpError struct {
	code, msg string
}

func (e *twirpError) Error() string { return e.code + ": " + e.msg }

func twerr(code, format string, a ...any) *twirpError {
	return &twirpError{code: code, msg: fmt.Sprintf(format, a...)}
}

// Twirp errors are JSON whatever the request's content type.
func writeTwirpError(w http.ResponseWriter, e *twirpError) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(twirpStatus[e.code])
	b, _ := json.Marshal(map[string]string{"code": e.code, "msg": e.msg})
	w.Write(b)
}

// Requests carry at most 10 keys of 512 bytes; this leaves ample room.
const maxTwirpBody = 64 << 10

type codec int

const (
	codecJSON codec = iota
	codecProto
)

func requestCodec(r *http.Request) (codec, *twirpError) {
	mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil {
		return 0, twerr(twirpBadRoute, "unsupported Content-Type %q", r.Header.Get("Content-Type"))
	}
	switch mt {
	case "application/json":
		return codecJSON, nil
	case "application/protobuf":
		return codecProto, nil
	}
	return 0, twerr(twirpBadRoute, "unsupported Content-Type %q", mt)
}

func readTwirpBody(r *http.Request) ([]byte, *twirpError) {
	b, err := io.ReadAll(io.LimitReader(r.Body, maxTwirpBody+1))
	if err != nil {
		return nil, twerr(twirpMalformed, "reading request body: %v", err)
	}
	if len(b) > maxTwirpBody {
		return nil, twerr(twirpMalformed, "request body exceeds %d bytes", maxTwirpBody)
	}
	return b, nil
}

type unmarshaler interface {
	unmarshalJSON([]byte) error
	unmarshalProto([]byte) error
}

func decodeTwirp(r *http.Request, c codec, m unmarshaler) *twirpError {
	b, te := readTwirpBody(r)
	if te != nil {
		return te
	}
	var err error
	if c == codecJSON {
		err = m.unmarshalJSON(b)
	} else {
		err = m.unmarshalProto(b)
	}
	if err != nil {
		return twerr(twirpMalformed, "decoding request: %v", err)
	}
	return nil
}

type marshaler interface {
	marshalJSON() []byte
	marshalProto() []byte
}

func writeTwirp(w http.ResponseWriter, c codec, m marshaler) {
	if c == codecJSON {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write(m.marshalJSON())
		return
	}
	w.Header().Set("Content-Type", "application/protobuf")
	w.WriteHeader(http.StatusOK)
	w.Write(m.marshalProto())
}

func asTwirp(err error) *twirpError {
	var te *twirpError
	if errors.As(err, &te) {
		return te
	}
	return twerr(twirpInternal, "internal error")
}
