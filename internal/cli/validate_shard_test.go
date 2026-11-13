package cli

import (
	"fmt"
	"testing"

	"github.com/supernovae/swarmos/internal/validation"
)

func TestParseValidationShard(t *testing.T) {
	for in, want := range map[string][2]int{"": {0, 0}, "1/3": {1, 3}, "3/3": {3, 3}, "1/1": {1, 1}} {
		i, n, err := parseValidationShard(in)
		if err != nil || i != want[0] || n != want[1] {
			t.Fatalf("%q: %d/%d %v", in, i, n, err)
		}
	}
	for _, bad := range []string{"0/3", "4/3", "1/0", "3", "1/", "/3", "a/b", "1/3/5", "-1/3", fmt.Sprintf("1/%d", validation.MaxValidationShards+1)} {
		if _, _, err := parseValidationShard(bad); err == nil {
			t.Fatalf("accepted %q", bad)
		}
	}
}
