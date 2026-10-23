package cache

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"unicode/utf8"
)

// The three request and response messages of
// github.actions.results.api.v1.CacheService, with hand-written proto3
// binary and JSON codecs. Field numbers follow results/api/v1/cache.proto as
// generated into @actions/cache. Field 1 of each request (metadata) is
// skipped on input.

type createRequest struct {
	Key, Version string
}

type createResponse struct {
	OK              bool
	SignedUploadURL string
	Message         string
}

type finalizeRequest struct {
	Key       string
	SizeBytes int64
	Version   string
}

type finalizeResponse struct {
	OK      bool
	EntryID int64
	Message string
}

type downloadRequest struct {
	Key         string
	RestoreKeys []string
	Version     string
}

type downloadResponse struct {
	OK                bool
	SignedDownloadURL string
	MatchedKey        string
}

var errMalformed = errors.New("malformed protobuf message")

const (
	wireVarint = 0
	wire64     = 1
	wireBytes  = 2
	wire32     = 5
)

// pbField is one decoded field: a varint value or a length-delimited payload.
type pbField struct {
	num   uint64
	wire  int
	u     uint64
	bytes []byte
}

// pbFields walks a proto3 message. Unknown fields of any valid wire type are
// returned and ignored by the callers; groups are rejected.
func pbFields(b []byte, fn func(pbField) error) error {
	for len(b) > 0 {
		tag, n := binary.Uvarint(b)
		if n <= 0 {
			return errMalformed
		}
		b = b[n:]
		f := pbField{num: tag >> 3, wire: int(tag & 7)}
		if f.num == 0 || f.num > math.MaxInt32 {
			return errMalformed
		}
		switch f.wire {
		case wireVarint:
			v, n := binary.Uvarint(b)
			if n <= 0 {
				return errMalformed
			}
			f.u, b = v, b[n:]
		case wire64:
			if len(b) < 8 {
				return errMalformed
			}
			b = b[8:]
		case wire32:
			if len(b) < 4 {
				return errMalformed
			}
			b = b[4:]
		case wireBytes:
			l, n := binary.Uvarint(b)
			if n <= 0 || l > uint64(len(b)-n) {
				return errMalformed
			}
			f.bytes, b = b[n:n+int(l)], b[n+int(l):]
		default:
			return errMalformed
		}
		if err := fn(f); err != nil {
			return err
		}
	}
	return nil
}

func pbString(f pbField) (string, error) {
	if f.wire != wireBytes {
		return "", errMalformed
	}
	// proto3 string fields must hold valid UTF-8.
	if !utf8.Valid(f.bytes) {
		return "", errMalformed
	}
	return string(f.bytes), nil
}

func (m *createRequest) unmarshalProto(b []byte) error {
	return pbFields(b, func(f pbField) (err error) {
		switch f.num {
		case 2:
			m.Key, err = pbString(f)
		case 3:
			m.Version, err = pbString(f)
		}
		return err
	})
}

func (m *finalizeRequest) unmarshalProto(b []byte) error {
	return pbFields(b, func(f pbField) (err error) {
		switch f.num {
		case 2:
			m.Key, err = pbString(f)
		case 3:
			if f.wire != wireVarint {
				return errMalformed
			}
			m.SizeBytes = int64(f.u)
		case 4:
			m.Version, err = pbString(f)
		}
		return err
	})
}

func (m *downloadRequest) unmarshalProto(b []byte) error {
	return pbFields(b, func(f pbField) (err error) {
		switch f.num {
		case 2:
			m.Key, err = pbString(f)
		case 3:
			var s string
			s, err = pbString(f)
			m.RestoreKeys = append(m.RestoreKeys, s)
		case 4:
			m.Version, err = pbString(f)
		}
		return err
	})
}

// Encoders omit default values, as proto3 does.

func pbAppendBool(b []byte, num uint64, v bool) []byte {
	if !v {
		return b
	}
	return append(binary.AppendUvarint(b, num<<3|wireVarint), 1)
}

func pbAppendInt64(b []byte, num uint64, v int64) []byte {
	if v == 0 {
		return b
	}
	return binary.AppendUvarint(binary.AppendUvarint(b, num<<3|wireVarint), uint64(v))
}

func pbAppendString(b []byte, num uint64, v string) []byte {
	if v == "" {
		return b
	}
	b = binary.AppendUvarint(b, num<<3|wireBytes)
	b = binary.AppendUvarint(b, uint64(len(v)))
	return append(b, v...)
}

func (m createResponse) marshalProto() []byte {
	b := pbAppendBool(nil, 1, m.OK)
	b = pbAppendString(b, 2, m.SignedUploadURL)
	return pbAppendString(b, 3, m.Message)
}

func (m finalizeResponse) marshalProto() []byte {
	b := pbAppendBool(nil, 1, m.OK)
	b = pbAppendInt64(b, 2, m.EntryID)
	return pbAppendString(b, 3, m.Message)
}

func (m downloadResponse) marshalProto() []byte {
	b := pbAppendBool(nil, 1, m.OK)
	b = pbAppendString(b, 2, m.SignedDownloadURL)
	return pbAppendString(b, 3, m.MatchedKey)
}

// JSON follows proto3 JSON mapping: a field may arrive under its proto name
// (what @actions/cache sends, useProtoFieldName) or its lowerCamelCase JSON
// name; int64 is a decimal string, though a JSON number is accepted too.
// Unknown fields are ignored.

func jsonFields(b []byte) (map[string]json.RawMessage, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("malformed JSON: %w", err)
	}
	if m == nil {
		return nil, errors.New("malformed JSON: not an object")
	}
	return m, nil
}

// jsonField returns the raw value under the proto name or the JSON name;
// both present is ambiguous and rejected. JSON null means absent.
func jsonField(m map[string]json.RawMessage, protoName, jsonName string) (json.RawMessage, error) {
	a, okA := m[protoName]
	c, okC := m[jsonName]
	if okA && okC && protoName != jsonName {
		return nil, fmt.Errorf("field %s given twice", protoName)
	}
	if okC {
		a = c
	}
	if string(a) == "null" {
		return nil, nil
	}
	return a, nil
}

func jsonString(m map[string]json.RawMessage, protoName, jsonName string) (string, error) {
	raw, err := jsonField(m, protoName, jsonName)
	if raw == nil || err != nil {
		return "", err
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", fmt.Errorf("field %s: want a string", protoName)
	}
	return s, nil
}

func jsonInt64(m map[string]json.RawMessage, protoName, jsonName string) (int64, error) {
	raw, err := jsonField(m, protoName, jsonName)
	if raw == nil || err != nil {
		return 0, err
	}
	var s string
	if json.Unmarshal(raw, &s) != nil {
		s = string(raw)
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("field %s: want an int64", protoName)
	}
	return v, nil
}

func (m *createRequest) unmarshalJSON(b []byte) error {
	f, err := jsonFields(b)
	if err != nil {
		return err
	}
	if m.Key, err = jsonString(f, "key", "key"); err != nil {
		return err
	}
	m.Version, err = jsonString(f, "version", "version")
	return err
}

func (m *finalizeRequest) unmarshalJSON(b []byte) error {
	f, err := jsonFields(b)
	if err != nil {
		return err
	}
	if m.Key, err = jsonString(f, "key", "key"); err != nil {
		return err
	}
	if m.SizeBytes, err = jsonInt64(f, "size_bytes", "sizeBytes"); err != nil {
		return err
	}
	m.Version, err = jsonString(f, "version", "version")
	return err
}

func (m *downloadRequest) unmarshalJSON(b []byte) error {
	f, err := jsonFields(b)
	if err != nil {
		return err
	}
	if m.Key, err = jsonString(f, "key", "key"); err != nil {
		return err
	}
	raw, err := jsonField(f, "restore_keys", "restoreKeys")
	if err != nil {
		return err
	}
	if raw != nil {
		if err := json.Unmarshal(raw, &m.RestoreKeys); err != nil {
			return errors.New("field restore_keys: want an array of strings")
		}
	}
	m.Version, err = jsonString(f, "version", "version")
	return err
}

// Responses use proto field names, as Twirp's Go server does by default;
// protobuf-ts fromJson accepts either name. Defaults are omitted.

func (m createResponse) marshalJSON() []byte {
	out := map[string]any{}
	if m.OK {
		out["ok"] = true
	}
	if m.SignedUploadURL != "" {
		out["signed_upload_url"] = m.SignedUploadURL
	}
	if m.Message != "" {
		out["message"] = m.Message
	}
	b, _ := json.Marshal(out)
	return b
}

func (m finalizeResponse) marshalJSON() []byte {
	out := map[string]any{}
	if m.OK {
		out["ok"] = true
	}
	if m.EntryID != 0 {
		out["entry_id"] = strconv.FormatInt(m.EntryID, 10)
	}
	if m.Message != "" {
		out["message"] = m.Message
	}
	b, _ := json.Marshal(out)
	return b
}

func (m downloadResponse) marshalJSON() []byte {
	out := map[string]any{}
	if m.OK {
		out["ok"] = true
	}
	if m.SignedDownloadURL != "" {
		out["signed_download_url"] = m.SignedDownloadURL
	}
	if m.MatchedKey != "" {
		out["matched_key"] = m.MatchedKey
	}
	b, _ := json.Marshal(out)
	return b
}
