package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const cacheConfig = `"cache": {"listen": "127.0.0.1:8090", "publicURL": "https://cache.example.test",
  "s3": {"bucket": "cache-bucket", "region": "eu-west-1"}, "signingKeyFile": "/s"},
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
	if cc.Issuer != DefaultCacheIssuer || cc.UpstreamResultsURL != DefaultUpstreamResultsURL ||
		cc.DownloadURLTTL.Duration != DefaultCacheDownloadURLTTL || cc.TTL.Duration != DefaultCacheTTL ||
		cc.MaxBytes != 0 || cc.MaxEntryBytes != DefaultCacheMaxEntryBytes || cc.ReapInterval.Duration != DefaultCacheReapInterval {
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
	} {
		if _, err := Parse(withCache(edit[0], edit[1])); err == nil ||
			!(strings.Contains(err.Error(), "cache") || strings.Contains(err.Error(), "unknown field")) {
			t.Errorf("%s: err=%v", name, err)
		}
	}
	if _, err := Parse(withCache(`"region": "eu-west-1"`, `"region": "eu-west-1", "prefix": "ci/gha-cache", "endpoint": "http://127.0.0.1:59240"`)); err != nil {
		t.Errorf("prefix and loopback endpoint: %v", err)
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
