package workspace

import (
	"context"
	"testing"
)

func TestAggregateRebasePatchIdentity(t *testing.T) {
	ctx := context.Background()
	root, _, base, head := prFixture(t)
	for _, tc := range []struct{ name, a, b, c, d, want string }{
		{"same", base, head, base, head, "unchanged"},
		{"empty", base, base, head, head, "unchanged"},
		{"changed", base, head, base, base, "changed"},
		{"missing", base, head, base, "0000000000000000000000000000000000000000", "unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := RebasePatchIdentity(ctx, root, tc.a, tc.b, tc.c, tc.d)
			if got != tc.want || (err != nil) != (tc.want == "unavailable") {
				t.Fatalf("identity=%s err=%v", got, err)
			}
		})
	}
}

func TestNormalizeGitOutput(t *testing.T) {
	for _, input := range []string{"one\r\ntwo\rthree", "one\ntwo\nthree"} {
		if got := normalizeGitOutput(input); got != "one\ntwo\nthree" {
			t.Fatalf("output=%q", got)
		}
	}
}
