package validation

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/supernovae/swarmos/internal/contract"
)

// Shards split CI validation across runners; together they must run every
// required check exactly once, whatever the shard count.
func TestShardSpecCoversEveryCheckOnce(t *testing.T) {
	for total := 0; total <= 13; total++ {
		spec := &contract.ValidationSpec{}
		for k := 0; k < total; k++ {
			spec.RequiredChecks = append(spec.RequiredChecks, contract.CheckSpec{Name: fmt.Sprintf("c%d", k), Command: "true"})
		}
		for n := 1; n <= MaxValidationShards; n++ {
			seen := map[string]int{}
			for i := 1; i <= n; i++ {
				sharded, note, err := shardSpec(spec, ValidateOptions{CIMode: true, ShardIndex: i, ShardCount: n})
				if err != nil {
					t.Fatal(err)
				}
				if !strings.HasPrefix(note, fmt.Sprintf("Shard %d/%d: %d of %d required checks", i, n, len(sharded.RequiredChecks), total)) {
					t.Fatalf("note %q", note)
				}
				for _, c := range sharded.RequiredChecks {
					seen[c.Name]++
				}
			}
			if len(seen) != total {
				t.Fatalf("%d checks, %d shards: covered %d", total, n, len(seen))
			}
			for name, count := range seen {
				if count != 1 {
					t.Fatalf("%s ran %d times", name, count)
				}
			}
		}
		if len(spec.RequiredChecks) != total {
			t.Fatal("sharding mutated the trusted policy")
		}
	}
}

func TestShardSpecRejectsOutsideCIAndBadValues(t *testing.T) {
	spec := &contract.ValidationSpec{RequiredChecks: []contract.CheckSpec{{Name: "a", Command: "true"}}}
	if _, _, err := shardSpec(spec, ValidateOptions{ShardIndex: 1, ShardCount: 2}); err == nil {
		t.Fatal("coordinator validation accepted a shard")
	}
	for _, bad := range [][2]int{{0, 2}, {3, 2}, {1, 0}, {1, MaxValidationShards + 1}, {-1, 2}} {
		if _, _, err := shardSpec(spec, ValidateOptions{CIMode: true, ShardIndex: bad[0], ShardCount: bad[1]}); err == nil {
			t.Fatalf("accepted shard %d/%d", bad[0], bad[1])
		}
	}
}

func TestShardedValidateRunsOnlyItsChecks(t *testing.T) {
	var checks string
	for _, name := range []string{"a", "b", "c", "d"} {
		checks += "  - name: " + name + "\n    command: 'true'\n    timeout: 1m\n"
	}
	repo, base := setupGoFastPassRepo(t, checks, nil)
	report, err := Validate(t.Context(), repo, nil, ValidateOptions{CIMode: true, BaseSHA: base, ShardIndex: 2, ShardCount: 3})
	if err != nil {
		t.Fatal(err)
	}
	var ran []string
	for _, o := range report.CheckOutcomes {
		ran = append(ran, o.Name)
	}
	if !slices.Equal(ran, []string{"b"}) || !strings.Contains(report.Shard, "Shard 2/3: 1 of 4 required checks (b)") || !strings.Contains(report.Artifact.Body, report.Shard) {
		t.Fatalf("ran %v, shard %q", ran, report.Shard)
	}
}
