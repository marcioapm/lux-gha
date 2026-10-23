package registry

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/marcioapm/lux-gha/config"
)

// MaxSessionPolicyBytes is STS's plaintext limit for an inline session policy.
const MaxSessionPolicyBytes = 2048

var (
	pullActions = []string{"ecr:BatchCheckLayerAvailability", "ecr:BatchGetImage", "ecr:GetDownloadUrlForLayer"}
	pushActions = []string{"ecr:CompleteLayerUpload", "ecr:InitiateLayerUpload", "ecr:PutImage", "ecr:UploadLayerPart"}
	// denyActions keep a trusted session from reading what pull requests pushed.
	denyActions = []string{"ecr:BatchGetImage", "ecr:GetDownloadUrlForLayer"}
)

type policyDocument struct {
	Version   string            `json:"Version"`
	Statement []policyStatement `json:"Statement"`
}

type policyStatement struct {
	Effect   string   `json:"Effect"`
	Action   []string `json:"Action"`
	Resource string   `json:"Resource"`
}

// SessionPolicy is the inline policy sent with AssumeRole. The session's
// permissions are the intersection of the role's and this, so it limits the
// session to the job's own repositories even if the role policy is broader:
// pull on <owner>/<repo>/*, push on <owner>/<repo>/* (trusted) or
// <owner>/<repo>/pr/* (untrusted), and for trusted an explicit deny of
// pulls from <owner>/<repo>/pr/*.
func SessionPolicy(c *config.Registry, owner, repo, trust string) (string, error) {
	partition, _, _ := strings.Cut(strings.TrimPrefix(c.RoleARN, "arn:"), ":")
	account, _, _ := strings.Cut(c.Host, ".")
	if partition == "" || account == "" {
		return "", errors.New("registry: role ARN partition or registry account missing")
	}
	base := fmt.Sprintf("arn:%s:ecr:%s:%s:repository/%s/%s/", partition, c.Region, account, owner, repo)
	statements := []policyStatement{
		{Effect: "Allow", Action: []string{"ecr:GetAuthorizationToken"}, Resource: "*"},
		{Effect: "Allow", Action: pullActions, Resource: base + "*"},
	}
	switch trust {
	case Trusted:
		statements = append(statements,
			policyStatement{Effect: "Allow", Action: pushActions, Resource: base + "*"},
			policyStatement{Effect: "Deny", Action: denyActions, Resource: base + "pr/*"})
	case Untrusted:
		statements = append(statements, policyStatement{Effect: "Allow", Action: pushActions, Resource: base + "pr/*"})
	case ReadOnly:
	default:
		return "", fmt.Errorf("registry: unknown trust level %q", trust)
	}
	b, err := json.Marshal(policyDocument{Version: "2012-10-17", Statement: statements})
	if err != nil {
		return "", err
	}
	if len(b) > MaxSessionPolicyBytes {
		return "", fmt.Errorf("registry: session policy is %d bytes, over %d", len(b), MaxSessionPolicyBytes)
	}
	return string(b), nil
}
