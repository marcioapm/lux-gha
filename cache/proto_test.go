package cache

import (
	"encoding/hex"
	"reflect"
	"strings"
	"testing"
)

const testVersion = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// The golden encodings below were produced by @actions/cache 6.3.0's
// generated protobuf-ts messages (toBinary / toJson with useProtoFieldName).

func TestDecodesClientProtobufRequests(t *testing.T) {
	var c createRequest
	// Includes field 1, CacheMetadata, which is skipped.
	if err := c.unmarshalProto(mustHex(t, "0a17080712130a0f726566732f68656164732f6d61696e100312046b2dc3bc1a40"+hex.EncodeToString([]byte(testVersion)))); err != nil {
		t.Fatal(err)
	}
	if c != (createRequest{Key: "k-ü", Version: testVersion}) {
		t.Fatalf("create = %+v", c)
	}
	var f finalizeRequest
	if err := f.unmarshalProto(mustHex(t, "12016b1880f092cbdd082240"+hex.EncodeToString([]byte(testVersion)))); err != nil {
		t.Fatal(err)
	}
	if f != (finalizeRequest{Key: "k", SizeBytes: 300000000000, Version: testVersion}) {
		t.Fatalf("finalize = %+v", f)
	}
	var d downloadRequest
	if err := d.unmarshalProto(mustHex(t, "12016b1a01611a01622240"+hex.EncodeToString([]byte(testVersion)))); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(d, downloadRequest{Key: "k", RestoreKeys: []string{"a", "b"}, Version: testVersion}) {
		t.Fatalf("download = %+v", d)
	}
}

func TestDecodesClientJSONRequests(t *testing.T) {
	var f finalizeRequest
	if err := f.unmarshalJSON([]byte(`{"key":"k","size_bytes":"300000000000","version":"` + testVersion + `"}`)); err != nil {
		t.Fatal(err)
	}
	if f != (finalizeRequest{Key: "k", SizeBytes: 300000000000, Version: testVersion}) {
		t.Fatalf("finalize = %+v", f)
	}
	var d downloadRequest
	if err := d.unmarshalJSON([]byte(`{"key":"k","restore_keys":["a","b"],"version":"` + testVersion + `"}`)); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(d, downloadRequest{Key: "k", RestoreKeys: []string{"a", "b"}, Version: testVersion}) {
		t.Fatalf("download = %+v", d)
	}
}

func TestJSONAcceptsCamelCaseNumbersAndUnknownFields(t *testing.T) {
	var f finalizeRequest
	if err := f.unmarshalJSON([]byte(`{"key":"k","sizeBytes":42,"version":"v","metadata":{"repositoryId":"1"},"extra":true}`)); err != nil {
		t.Fatal(err)
	}
	if f != (finalizeRequest{Key: "k", SizeBytes: 42, Version: "v"}) {
		t.Fatalf("finalize = %+v", f)
	}
	var d downloadRequest
	if err := d.unmarshalJSON([]byte(`{"key":"k","restoreKeys":["x"],"version":null}`)); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(d, downloadRequest{Key: "k", RestoreKeys: []string{"x"}}) {
		t.Fatalf("download = %+v", d)
	}
}

func TestJSONRejectsMalformed(t *testing.T) {
	for _, body := range []string{
		``, `[]`, `null`, `{"key":1}`, `{"key":"k","size_bytes":"1.5"}`, `{"size_bytes":"1","sizeBytes":"2"}`,
		`{"restore_keys":"a"}`, `{"key":"k"} trailing`,
	} {
		var f finalizeRequest
		var d downloadRequest
		if f.unmarshalJSON([]byte(body)) == nil && d.unmarshalJSON([]byte(body)) == nil {
			t.Errorf("%q decoded without error", body)
		}
	}
}

func TestProtoRejectsMalformed(t *testing.T) {
	for _, h := range []string{
		"12",           // tag without a length
		"1205616263",   // length past the end
		"2202c328",     // invalid UTF-8 in a string field (version)
		"0b",           // start-group wire type
		"00",           // field number 0
		"12016b18",     // size_bytes varint truncated
		"1001",         // field 2 as varint, not a string
		"1a0101",       // size_bytes as bytes, not a varint
		"ffffffffffff", // overlong varint tag
	} {
		var f finalizeRequest
		if err := f.unmarshalProto(mustHex(t, h)); err == nil {
			t.Errorf("%s decoded without error: %+v", h, f)
		}
	}
}

func TestProtoSkipsUnknownFieldsOfEveryWireType(t *testing.T) {
	var c createRequest
	// field 9 varint, field 10 fixed64, field 11 bytes, field 12 fixed32, then key.
	b := mustHex(t, "4801"+"510102030405060708"+"5a0178"+"6501020304"+"12016b")
	if err := c.unmarshalProto(b); err != nil || c.Key != "k" {
		t.Fatalf("%+v, %v", c, err)
	}
}

func TestResponsesEncodeAsTheClientParsesThem(t *testing.T) {
	// Protobuf: field 1 bool, 2 string/int64, 3 string, defaults omitted.
	cases := []struct {
		m    marshaler
		hex  string
		json string
	}{
		{createResponse{OK: true, SignedUploadURL: "u"}, "0801120175", `{"ok":true,"signed_upload_url":"u"}`},
		{createResponse{Message: "m"}, "1a016d", `{"message":"m"}`},
		{finalizeResponse{OK: true, EntryID: 9007199254740991}, "080110ffffffffffffff0f", `{"entry_id":"9007199254740991","ok":true}`},
		{downloadResponse{OK: true, SignedDownloadURL: "u", MatchedKey: "k"}, "08011201751a016b", `{"matched_key":"k","ok":true,"signed_download_url":"u"}`},
		{downloadResponse{}, "", `{}`},
	}
	for _, c := range cases {
		if got := hex.EncodeToString(c.m.marshalProto()); got != c.hex {
			t.Errorf("%+v proto = %s, want %s", c.m, got, c.hex)
		}
		if got := string(c.m.marshalJSON()); got != c.json {
			t.Errorf("%+v json = %s, want %s", c.m, got, c.json)
		}
	}
}

func TestKeyEncodingRoundTripsEveryByte(t *testing.T) {
	var all strings.Builder
	for c := 0; c < 256; c++ {
		all.WriteByte(byte(c))
	}
	for _, k := range []string{all.String(), ".", "..", "./x", "a/b", "%41", "A", "x.y", "é", "a b,c"} {
		enc := encodeKey(k)
		if strings.Contains(enc, "/") {
			t.Errorf("encodeKey(%q) = %q contains a slash", k, enc)
		}
		if enc == "." || enc == ".." || strings.HasPrefix(enc, ".") {
			t.Errorf("encodeKey(%q) = %q starts with a dot", k, enc)
		}
		got, err := decodeKey(enc)
		if err != nil || got != k {
			t.Errorf("decodeKey(encodeKey(%q)) = %q, %v", k, got, err)
		}
	}
}

func TestKeyEncodingIsInjectiveAndCanonical(t *testing.T) {
	// "%41" and "A" must not collide, and only the canonical form decodes.
	if encodeKey("%41") == encodeKey("A") {
		t.Fatal("collision")
	}
	for _, s := range []string{"%41", "%2e", "%2Ex", "a%", "a%4", "a%zz", "a/b", ".x", "a b"} {
		if _, err := decodeKey(s); err == nil && s != "%2Ex" {
			t.Errorf("decodeKey(%q) accepted a non-canonical encoding", s)
		}
	}
	if k, err := decodeKey("%2Ex"); err != nil || k != ".x" {
		t.Errorf("decodeKey(%%2Ex) = %q, %v", k, err)
	}
}

func TestKeyEncodingPreservesPrefixes(t *testing.T) {
	keys := []string{"node-linux-abc", ".cache-x", "a b", "日本語-キー", "x.y.z", "%"}
	for _, k := range keys {
		for i := 0; i <= len(k); i++ {
			p := k[:i]
			if !strings.HasPrefix(encodeKey(k), encodeKey(p)) {
				t.Errorf("encodeKey(%q) does not start with encodeKey(%q)", k, p)
			}
		}
	}
	// And a non-prefix never encodes as one.
	if strings.HasPrefix(encodeKey("ab"), encodeKey("b")) || strings.HasPrefix(encodeKey("a"), encodeKey("a.")) {
		t.Error("non-prefix matched")
	}
}

func TestPartitionsCannotBeShiftedByScopeOrKey(t *testing.T) {
	l := newLayout("/cache/")
	a := l.entryKey("1", "refs/heads/main", testVersion, "k")
	if !strings.HasPrefix(a, "cache/v1/1/") {
		t.Fatalf("entry key %q", a)
	}
	parts := strings.Split(a, "/")
	if len(parts) != 6 || len(parts[3]) != 64 || parts[4] != testVersion {
		t.Fatalf("entry key %q has segments %q", a, parts)
	}
	// A scope or key containing the other's separator lands elsewhere.
	if l.entryKey("1", "refs/heads/main/"+testVersion, testVersion, "k") == a ||
		strings.Count(l.entryKey("1", "s", testVersion, "x/../../2/y"), "/") != 5 {
		t.Fatal("scope or key shifted a partition")
	}
	// The staging area is disjoint from entries.
	if strings.HasPrefix(l.stagingKey("1", "s", testVersion, "k", "t"), l.entriesRoot()) {
		t.Fatal("staging under entries")
	}
	if newLayout("").entryKey("1", "s", testVersion, "k")[:3] != "v1/" {
		t.Fatal("empty prefix")
	}
}

func TestStagingKeyBindsEveryComponent(t *testing.T) {
	l := newLayout("p")
	base := l.stagingKey("1", "s", testVersion, "k", "t")
	for _, other := range []string{
		l.stagingKey("2", "s", testVersion, "k", "t"),
		l.stagingKey("1", "s2", testVersion, "k", "t"),
		l.stagingKey("1", "s", strings.Repeat("b", 64), "k", "t"),
		l.stagingKey("1", "s", testVersion, "k2", "t"),
		l.stagingKey("1", "s", testVersion, "k", "t2"),
		l.stagingKey("1", "s|", testVersion, "k", "t"),
	} {
		if other == base {
			t.Fatal("staging key collision")
		}
	}
}

func TestValidation(t *testing.T) {
	if validateKey(strings.Repeat("x", 512)) != nil || validateKey(strings.Repeat("x", 513)) == nil {
		t.Error("512-byte bound")
	}
	for _, k := range []string{"", "a,b", "\xff"} {
		if validateKey(k) == nil {
			t.Errorf("key %q accepted", k)
		}
	}
	for _, v := range []string{"", strings.Repeat("A", 64), strings.Repeat("a", 63), strings.Repeat("a", 65), "../" + strings.Repeat("a", 61)} {
		if validateVersion(v) == nil {
			t.Errorf("version %q accepted", v)
		}
	}
}
