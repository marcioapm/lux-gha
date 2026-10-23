package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/marcioapm/lux-gha/cache"
)

const cacheConfig = `"cache": {"listen": "127.0.0.1:8090", "publicURL": "https://cache.example.test",
  "s3": {"bucket": "cache-bucket", "region": "eu-west-1"}, "repositoryIDs": [100], "signingKeyFile": "/s"},
  "listen"`

func withCache(old, new string) []byte {
	return []byte(strings.Replace(string(mutate(`"listen"`, cacheConfig)), old, new, 1))
}

func TestCacheIsOptional(t *testing.T) {
	c, err := Parse([]byte(validConfig))
	if err != nil || c.Cache != nil {
		t.Fatalf("cache %+v err=%v", c.Cache, err)
	}
}

func TestCacheDefaults(t *testing.T) {
	c, err := Parse(withCache("", ""))
	if err != nil {
		t.Fatal(err)
	}
	cc := c.Cache
	if cc.Issuer != cache.DefaultIssuer || cc.UpstreamResultsURL != cache.DefaultUpstreamResultsURL ||
		cc.DownloadURLTTL.Duration != cache.DefaultDownloadURLTTL || cc.TTL.Duration != cache.DefaultTTL ||
		cc.MaxBytes != 0 || cc.MaxEntryBytes != cache.DefaultMaxEntryBytes || cc.ReapInterval.Duration != cache.DefaultReapInterval {
		t.Fatalf("defaults %+v", cc)
	}
	tuned, err := Parse(withCache(`"signingKeyFile": "/s"`, `"signingKeyFile": "/s", "ttl": "48h", "maxBytes": 100000000000,
		"maxEntryBytes": 1000, "downloadURLTTL": "5m", "reapInterval": "10m", "issuer": "http://127.0.0.1:9000",
		"upstreamResultsURL": "http://localhost:9001"`))
	if err != nil || tuned.Cache.TTL.Duration != 48*time.Hour || tuned.Cache.MaxEntryBytes != 1000 {
		t.Fatalf("tuned %+v err=%v", tuned.Cache, err)
	}
}

func TestCacheRejectsInvalid(t *testing.T) {
	for name, edit := range map[string][2]string{
		"no listen":          {`"listen": "127.0.0.1:8090", `, ``},
		"webhook listen":     {`127.0.0.1:8090`, `127.0.0.1:8080`},
		"no public URL":      {`"publicURL": "https://cache.example.test",`, ``},
		"public URL path":    {`https://cache.example.test`, `https://cache.example.test/x`},
		"http public URL":    {`https://cache.example.test`, `http://cache.example.test`},
		"no bucket":          {`"bucket": "cache-bucket", `, ``},
		"bad bucket":         {`cache-bucket`, `Cache_Bucket`},
		"no region":          {`, "region": "eu-west-1"`, ``},
		"bad prefix":         {`"region": "eu-west-1"`, `"region": "eu-west-1", "prefix": "a/../b"`},
		"bad endpoint":       {`"region": "eu-west-1"`, `"region": "eu-west-1", "endpoint": "http://s3.example.test"`},
		"no signing key":     {`, "signingKeyFile": "/s"`, ``},
		"unknown field":      {`"signingKeyFile": "/s"`, `"signingKeyFile": "/s", "accessKey": "x"`},
		"unknown s3 field":   {`"region": "eu-west-1"`, `"region": "eu-west-1", "secretAccessKey": "x"`},
		"http issuer":        {`"signingKeyFile": "/s"`, `"signingKeyFile": "/s", "issuer": "http://token.example.test"`},
		"short ttl":          {`"signingKeyFile": "/s"`, `"signingKeyFile": "/s", "ttl": "1m"`},
		"long download TTL":  {`"signingKeyFile": "/s"`, `"signingKeyFile": "/s", "downloadURLTTL": "200h"`},
		"negative maxBytes":  {`"signingKeyFile": "/s"`, `"signingKeyFile": "/s", "maxBytes": -1`},
		"entry over total":   {`"signingKeyFile": "/s"`, `"signingKeyFile": "/s", "maxBytes": 10, "maxEntryBytes": 11`},
		"short reapInterval": {`"signingKeyFile": "/s"`, `"signingKeyFile": "/s", "reapInterval": "1s"`},
		"no allowlist":       {`"repositoryIDs": [100], `, ``},
		"empty allowlists":   {`"repositoryIDs": [100]`, `"repositoryIDs": [], "repositoryOwnerIDs": []`},
		"zero repository ID": {`"repositoryIDs": [100]`, `"repositoryIDs": [0]`},
		"negative owner ID":  {`"repositoryIDs": [100]`, `"repositoryIDs": [100], "repositoryOwnerIDs": [-1]`},
		"string owner ID":    {`"repositoryIDs": [100]`, `"repositoryOwnerIDs": ["7"]`},
		"fractional ID":      {`"repositoryIDs": [100]`, `"repositoryIDs": [1.5]`},
	} {
		_, err := Parse(withCache(edit[0], edit[1]))
		if err == nil || !(strings.Contains(err.Error(), "cache") || strings.Contains(err.Error(), "unknown field")) {
			t.Errorf("%s: err=%v", name, err)
		}
	}
	if _, err := Parse(withCache(`"region": "eu-west-1"`, `"region": "eu-west-1", "prefix": "ci/gha-cache", "endpoint": "http://127.0.0.1:59240"`)); err != nil {
		t.Errorf("prefix and loopback endpoint: %v", err)
	}
	// An S3-compatible store names its own region; the cache takes it, the registry (AWS only) does not.
	if _, err := Parse(withCache(`"region": "eu-west-1"`, `"region": "auto"`)); err != nil {
		t.Errorf("non-AWS region: %v", err)
	}
}

func TestCacheMaxEntryBytesFollowsMaxBytes(t *testing.T) {
	const gib = int64(1) << 30
	for _, c := range []struct {
		maxBytes, maxEntry string
		want               int64
	}{
		// Only maxBytes, below 10 GiB: 90 % of it.
		{"5368709120", "", 5*gib - 5*gib/10},
		// Only maxBytes, large: the 10 GiB default.
		{"1099511627776", "", 10 * gib},
		// Exactly 90 %.
		{"1000", "900", 900},
	} {
		edit := `"signingKeyFile": "/s", "maxBytes": ` + c.maxBytes
		if c.maxEntry != "" {
			edit += `, "maxEntryBytes": ` + c.maxEntry
		}
		cfg, err := Parse(withCache(`"signingKeyFile": "/s"`, edit))
		if err != nil || cfg.Cache.MaxEntryBytes != c.want {
			t.Errorf("maxBytes %s maxEntryBytes %q: got %+v err=%v, want %d", c.maxBytes, c.maxEntry, cfg, err, c.want)
		}
	}
	for _, edit := range []string{
		`"maxBytes": 1000, "maxEntryBytes": 901`,
		`"maxBytes": 1000, "maxEntryBytes": 1000`,
	} {
		_, err := Parse(withCache(`"signingKeyFile": "/s"`, `"signingKeyFile": "/s", `+edit))
		if err == nil || !strings.Contains(err.Error(), "90 % of cache.maxBytes") || !strings.Contains(err.Error(), "10 GiB") {
			t.Errorf("%s: err=%v", edit, err)
		}
	}
}

func TestCacheAllowlist(t *testing.T) {
	c, err := Parse(withCache(`"repositoryIDs": [100]`, `"repositoryOwnerIDs": [7, 8]`))
	if err != nil || len(c.Cache.RepositoryIDs) != 0 || len(c.Cache.RepositoryOwnerIDs) != 2 || c.Cache.RepositoryOwnerIDs[1] != 8 {
		t.Fatalf("owners only: %+v err=%v", c, err)
	}
	c, err = Parse(withCache(`"repositoryIDs": [100]`, `"repositoryIDs": [100, 200], "repositoryOwnerIDs": [7]`))
	if err != nil || len(c.Cache.RepositoryIDs) != 2 || len(c.Cache.RepositoryOwnerIDs) != 1 {
		t.Fatalf("both: %+v err=%v", c, err)
	}
}

func TestCacheSigningKeys(t *testing.T) {
	dir := t.TempDir()
	c, _ := Parse(withCache("", ""))
	write := func(v string) {
		c.Cache.SigningKeyFile = filepath.Join(dir, "keys")
		if err := os.WriteFile(c.Cache.SigningKeyFile, []byte(v), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	newKey, oldKey := strings.Repeat("n", 32), strings.Repeat("o", 40)
	write(newKey + "\r\n\n" + oldKey + "\n")
	keys, err := c.Cache.LoadSigningKeys()
	if err != nil || len(keys) != 2 || string(keys[0]) != newKey || string(keys[1]) != oldKey {
		t.Fatalf("keys %q err=%v", keys, err)
	}
	write(strings.Repeat("s", 31) + "\n")
	if _, err := c.Cache.LoadSigningKeys(); err == nil || !strings.Contains(err.Error(), "cache.signingKeyFile") ||
		strings.Contains(err.Error(), "sss") {
		t.Fatalf("short key: %v", err)
	}
	write("\n")
	if _, err := c.Cache.LoadSigningKeys(); err == nil {
		t.Fatal("empty file accepted")
	}
}

func TestCacheTLS(t *testing.T) {
	const pair = `"signingKeyFile": "/s", "tlsCertFile": "/c.pem", "tlsKeyFile": "/k.pem"`
	c, err := Parse(withCache(`"signingKeyFile": "/s"`, pair))
	if err != nil || !c.Cache.TLS() || c.Cache.TLSCertFile != "/c.pem" || c.Cache.TLSKeyFile != "/k.pem" {
		t.Fatalf("both: %+v err=%v", c, err)
	}
	if c, err := Parse(withCache("", "")); err != nil || c.Cache.TLS() {
		t.Fatalf("neither: %+v err=%v", c, err)
	}
	for name, edit := range map[string]string{
		"cert only": `"signingKeyFile": "/s", "tlsCertFile": "/c.pem"`,
		"key only":  `"signingKeyFile": "/s", "tlsKeyFile": "/k.pem"`,
	} {
		if _, err := Parse(withCache(`"signingKeyFile": "/s"`, edit)); err == nil || !strings.Contains(err.Error(), "must be set together") {
			t.Errorf("%s: err=%v", name, err)
		}
	}
	// Loopback http publicURL is accepted without TLS but not with it.
	loopback := strings.Replace(string(withCache(`"signingKeyFile": "/s"`, pair)), "https://cache.example.test", "http://127.0.0.1:8090", 1)
	if _, err := Parse([]byte(loopback)); err == nil || !strings.Contains(err.Error(), "must be https when cache.tlsCertFile is set") {
		t.Errorf("http publicURL with TLS: err=%v", err)
	}
	if _, err := Parse(withCache("https://cache.example.test", "http://127.0.0.1:8090")); err != nil {
		t.Errorf("http loopback publicURL without TLS: %v", err)
	}
}

func TestCacheLoopbackListen(t *testing.T) {
	for listen, want := range map[string]bool{
		"127.0.0.1:8090": true, "[::1]:8090": true, "localhost:8090": true,
		"10.0.0.5:8090": false, "0.0.0.0:8090": false, ":8090": false, "cache.example:8090": false,
	} {
		if got := (&Cache{Listen: listen}).LoopbackListen(); got != want {
			t.Errorf("%s: %v", listen, got)
		}
	}
}
