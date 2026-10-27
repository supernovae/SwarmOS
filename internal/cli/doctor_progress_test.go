package cli

import (
	"bytes"
	"context"
	"fmt"
	"github.com/supernovae/swarmos/internal/contract"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/supernovae/swarmos/internal/harness"
	"github.com/supernovae/swarmos/internal/progress"
)

func TestDoctorProgressOutput(t *testing.T) {
	for _, jsonMode := range []bool{false, true} {
		for _, failing := range []bool{false, true} {
			t.Run(fmt.Sprintf("json=%v/fail=%v", jsonMode, failing), func(t *testing.T) {
				statuses := []string{"OK", "WARN", "OPTIONAL", "OK", "OK", "OK", "OK"}
				if failing {
					statuses[3] = "FAIL"
				}
				names := []string{"git", "github-auth", ".swarm/config.yaml", ".swarm/validation/required-checks.yaml", "reviewer-runtime", "git-identity", "dependency-caches"}
				categories := []string{"system", "tracker", "harness", "harness", "sandbox", "system", "execution"}
				var stdout, stderr bytes.Buffer
				count := 0
				factory := func(ctx context.Context, observe func(progress.Event)) harness.DoctorRunner {
					real := harness.NewDoctorRunner(ctx, observe, nil, func() time.Time { return time.Unix(0, 0) })
					return func(category, name string, check func() harness.DoctorCheck) harness.DoctorCheck {
						if count >= len(names) || name != names[count] || category != categories[count] {
							t.Fatalf("unexpected boundary %s/%s", category, name)
						}
						if name == ".swarm/config.yaml" {
							check()
						}
						result := harness.DoctorCheck{Category: category, Name: name, Status: statuses[count], Detail: "fixture: café"}
						count++
						return real.Run(category, name, func() harness.DoctorCheck { return result })
					}
				}
				cmd := newDoctorCmdWithRunner(&RootOptions{RepoRoot: t.TempDir(), Backend: "local"}, factory)
				cmd.SetContext(context.Background())
				cmd.SetOut(&stdout)
				cmd.SetErr(&stderr)
				if jsonMode {
					if err := cmd.Flags().Set("json", "true"); err != nil {
						t.Fatal(err)
					}
				}
				err := cmd.RunE(cmd, nil)
				if failing {
					if err == nil || err.Error() != "doctor detected 1 failing check(s)" {
						t.Fatalf("error: %v", err)
					}
				} else if err != nil {
					t.Fatal(err)
				}
				if count != len(names) {
					t.Fatal(count)
				}
				var expected strings.Builder
				if jsonMode {
					expected.WriteString("{\"checks\":[")
					for i, name := range names {
						if i > 0 {
							expected.WriteString(",")
						}
						fmt.Fprintf(&expected, `{"category":%q,"name":%q,"status":%q,"detail":"fixture: café"}`, categories[i], name, statuses[i])
					}
					status := "passed"
					if failing {
						status = "failed"
					}
					fmt.Fprintf(&expected, "],\"kind\":\"doctor\",\"status\":%q}\n", status)
					if stderr.Len() != 0 {
						t.Fatalf("JSON stderr: %s", stderr.String())
					}
				} else {
					expected.WriteString("CATEGORY   CHECK                                    STATUS   DETAIL\n--------   -----                                    ------   ------\n")
					for i, name := range names {
						fmt.Fprintf(&expected, "%-10s %-40s %-8s fixture: café\n", categories[i], name, statuses[i])
						for _, status := range []string{"started", statuses[i]} {
							if !strings.Contains(stderr.String(), fmt.Sprintf("Check %q %s (elapsed 0s)\n", name, status)) {
								t.Fatalf("stderr: %s", stderr.String())
							}
						}
					}
				}
				if stdout.String() != expected.String() {
					t.Fatalf("stdout:\n%q\nwant:\n%q", stdout.String(), expected.String())
				}
			})
		}
	}
}

type doctorFailWriter struct{}

func (doctorFailWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

func TestDoctorProgressQuotesAndIgnoresWriterErrors(t *testing.T) {
	for _, out := range []io.Writer{&bytes.Buffer{}, doctorFailWriter{}} {
		run := harness.NewDoctorRunner(context.Background(), operationProgressPrinter(out), nil, nil)
		want := harness.DoctorCheck{Status: "WARN", Detail: "secret detail"}
		if got := run.Run("fixture", "a\nb\x00", func() harness.DoctorCheck { return want }); got != want {
			t.Fatal(got)
		}
		if b, ok := out.(*bytes.Buffer); ok {
			if strings.Count(b.String(), "\n") != 2 || strings.Contains(b.String(), want.Detail) {
				t.Fatal(b.String())
			}
		}
	}
}

func TestDoctorAgentChecksRunInsideBoundary(t *testing.T) {
	cfg := &contract.SwarmConfig{DefaultAgent: "z", Agents: map[string]contract.AgentConfig{"z": {}, "a": {Roles: []string{"developer"}}}}
	active := ""
	var names []string
	run := harness.DoctorRunner(func(category, name string, check func() harness.DoctorCheck) harness.DoctorCheck {
		if active != "" {
			t.Fatal("nested check")
		}
		active = name
		names = append(names, name)
		result := check()
		active = ""
		return result
	})
	checks := doctorAgentChecksWithRunner(cfg, true, func(name string, _ *contract.SwarmConfig, local bool, role string) (string, error) {
		if !local || active != name+"/"+role {
			t.Fatal("preflight outside boundary")
		}
		return "fixture", fmt.Errorf("unavailable")
	}, run)
	want := []string{"a/developer", "z/planner", "z/developer", "z/tester", "z/reviewer"}
	if !reflect.DeepEqual(names, want) {
		t.Fatal(names)
	}
	for i, c := range checks {
		status := "FAIL"
		if i == 0 {
			status = "OPTIONAL"
		}
		if c.Status != status {
			t.Fatal(c)
		}
	}
}
