package registry

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"

	"github.com/marcioapm/lux-gha/config"
)

func TestSessionNameFitsSTS(t *testing.T) {
	valid := regexp.MustCompile(`^[\w+=,.@-]{2,64}$`)
	if got := SessionName(1001, 7, 1); got != "lux-gha-1001-7-1" {
		t.Fatalf("session name %q", got)
	}
	long := SessionName(1<<62, 1<<62, 1<<62)
	if !valid.MatchString(long) || !strings.HasPrefix(long, "lux-gha-") {
		t.Fatalf("session name %q", long)
	}
}

func testRegistry() *config.Registry {
	return &config.Registry{RoleARN: "arn:aws:iam::123456789012:role/lux-gha-registry", Region: "eu-west-1",
		Host: "123456789012.dkr.ecr.eu-west-1.amazonaws.com"}
}

// The session policy per trust level, byte for byte: AWS evaluates exactly
// this document against every request the session makes.
func TestSessionPolicyPerTrustLevel(t *testing.T) {
	const (
		head = `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":["ecr:GetAuthorizationToken"],"Resource":"*"},`
		pull = `{"Effect":"Allow","Action":["ecr:BatchCheckLayerAvailability","ecr:BatchGetImage","ecr:GetDownloadUrlForLayer"],"Resource":"arn:aws:ecr:eu-west-1:123456789012:repository/acme/widgets/*"}`
		push = `{"Effect":"Allow","Action":["ecr:CompleteLayerUpload","ecr:InitiateLayerUpload","ecr:PutImage","ecr:UploadLayerPart"],"Resource":"arn:aws:ecr:eu-west-1:123456789012:repository/acme/widgets/`
		deny = `{"Effect":"Deny","Action":["ecr:BatchGetImage","ecr:GetDownloadUrlForLayer"],"Resource":"arn:aws:ecr:eu-west-1:123456789012:repository/acme/widgets/pr/*"}`
		end  = `]}`
	)
	for trust, want := range map[string]string{
		Trusted:   head + pull + "," + push + `*"},` + deny + end,
		Untrusted: head + pull + "," + push + `pr/*"}` + end,
		ReadOnly:  head + pull + end,
	} {
		got, err := SessionPolicy(testRegistry(), "acme", "widgets", trust)
		if err != nil || got != want {
			t.Errorf("%s:\n got %s\nwant %s\nerr %v", trust, got, want, err)
		}
		if !json.Valid([]byte(got)) {
			t.Errorf("%s: invalid JSON", trust)
		}
	}
	if _, err := SessionPolicy(testRegistry(), "acme", "widgets", "admin"); err == nil {
		t.Fatal("unknown trust level accepted")
	}
}

// The partition comes from the role ARN; the account from the host.
func TestSessionPolicyPartitionAndAccount(t *testing.T) {
	c := &config.Registry{RoleARN: "arn:aws-cn:iam::210987654321:role/r", Region: "cn-north-1",
		Host: "210987654321.dkr.ecr.cn-north-1.amazonaws.com.cn"}
	got, err := SessionPolicy(c, "acme", "widgets", ReadOnly)
	if err != nil || !strings.Contains(got, `"Resource":"arn:aws-cn:ecr:cn-north-1:210987654321:repository/acme/widgets/*"`) {
		t.Fatalf("%s %v", got, err)
	}
}

// The longest names the registry accepts (GitHub caps owners at 39 and
// repositories at 100 characters) stay within STS's 2048-byte limit.
func TestSessionPolicyFitsTheSTSLimit(t *testing.T) {
	owner, repo := strings.Repeat("o", 39), strings.Repeat("r", 100)
	got, err := SessionPolicy(testRegistry(), owner, repo, Trusted)
	if err != nil || len(got) > MaxSessionPolicyBytes {
		t.Fatalf("%d bytes, err %v", len(got), err)
	}
	t.Logf("longest trusted policy: %d bytes", len(got))
	if _, err := SessionPolicy(testRegistry(), strings.Repeat("o", 600), repo, Trusted); err == nil {
		t.Fatal("over-long policy accepted")
	}
}
