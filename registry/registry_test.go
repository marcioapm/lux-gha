package registry

import (
	"regexp"
	"strings"
	"testing"
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
