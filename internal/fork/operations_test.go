package fork

import (
	"os"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"
)

// The coordinator workflow admits requests before checkout using its own
// operation list; every operation the coordinator executes and the client
// confirms must be admitted, or real requests fail with no result comment.
func TestOperationListsStayInSync(t *testing.T) {
	want := slices.Clone(Operations)
	sort.Strings(want)
	cases := func(path, function string) []string {
		t.Helper()
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		// Windows checkouts may convert source files to CRLF.
		body := strings.ReplaceAll(string(src), "\r\n", "\n")
		if function != "" {
			start := strings.Index(body, function)
			if start < 0 {
				t.Fatalf("%s not found in %s", function, path)
			}
			body = body[start:]
			end := strings.Index(body, "\n}\n")
			if end < 0 {
				t.Fatalf("end of %s not found in %s", function, path)
			}
			body = body[:end]
		}
		var ops []string
		for _, m := range regexp.MustCompile(`case "([a-z-]+)"`).FindAllStringSubmatch(body, -1) {
			ops = append(ops, m[1])
		}
		sort.Strings(ops)
		return ops
	}
	if got := cases("coordinator.go", "func (c *Coordinator) executeRequest("); !slices.Equal(got, want) {
		t.Fatalf("coordinator executes %v, Operations lists %v", got, want)
	}
	if got := cases("transport.go", "func expectedResultPresent("); !slices.Equal(got, want) {
		t.Fatalf("gateway confirms %v, Operations lists %v", got, want)
	}
	admitted := regexp.MustCompile(`operations = \{([^}]*)\}`)
	for _, workflow := range []string{"../harness/templates/github/workflows/swarm-fork-coordinator.yml", "../../.github/workflows/swarm-fork-coordinator.yml"} {
		data, err := os.ReadFile(workflow)
		if err != nil {
			t.Fatal(err)
		}
		body := string(data)
		secret := "SWARM_GITHUB_FORK_READ_TOKEN: ${{ secrets.SWARM_GITHUB_FORK_READ_TOKEN }}"
		execute := strings.Index(body, "- name: Execute fork worker request")
		selected := strings.Index(body, secret)
		if execute < 0 || selected < execute || strings.Count(body, "secrets.SWARM_GITHUB_FORK_READ_TOKEN") != 1 {
			t.Fatalf("%s must expose read secret only at execution", workflow)
		}
		m := admitted.FindSubmatch(data)
		if m == nil {
			t.Fatalf("%s has no admission operation list", workflow)
		}
		var got []string
		for _, quoted := range regexp.MustCompile(`"([a-z-]+)"`).FindAllSubmatch(m[1], -1) {
			got = append(got, string(quoted[1]))
		}
		sort.Strings(got)
		if !slices.Equal(got, want) {
			t.Fatalf("%s admits %v, coordinator executes %v", workflow, got, want)
		}
	}
}
