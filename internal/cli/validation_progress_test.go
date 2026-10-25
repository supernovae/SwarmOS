package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/supernovae/swarmos/internal/progress"
)

func runProgressCommand(t *testing.T, f *cliFixture, args ...string) (string, string, error) {
	t.Helper()
	cmd := NewRootCmd()
	var out, diag bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&diag)
	cmd.SetArgs(append([]string{"--root", f.root, "--backend", "local"}, args...))
	err := cmd.Execute()
	return out.String(), diag.String(), err
}
func assertProgressOrder(t *testing.T, text string, parts ...string) {
	t.Helper()
	pos := 0
	for _, part := range parts {
		i := strings.Index(text[pos:], part)
		if i < 0 {
			t.Fatalf("missing ordered %q in %s", part, text)
		}
		pos += i + len(part)
	}
}
func TestValidationSubmissionProgressOutput(t *testing.T) {
	for _, family := range []string{"issue", "pr"} {
		for _, operation := range []string{"validate", "submit"} {
			for _, mode := range []string{"default", "true", "false"} {
				for _, asJSON := range []bool{false, true} {
					t.Run(family+"/"+operation+"/"+mode+"/"+strconv.FormatBool(asJSON), func(t *testing.T) {
						var f *cliFixture
						var number int
						if family == "issue" {
							f = newCLIFixture(t)
							number = f.create(t, "agent")
							n := strconv.Itoa(number)
							f.must(t, "issue", "approve", n)
							f.must(t, "issue", "work", n, "--agent", "mock", "--progress=false")
						} else {
							var prNumber int
							f, _, prNumber = submittedPRFixture(t, "agent")
							number = prNumber
							t.Setenv("SWARM_MOCK_DEVELOPER_CMD", "printf 'revision\\n' > revision.txt")
							f.must(t, "pr", "work", strconv.Itoa(number), "--agent", "mock", "--progress=false")
						}
						args := []string{family, operation, strconv.Itoa(number)}
						if mode != "default" {
							args = append(args, "--progress="+mode)
						}
						if asJSON {
							args = append(args, "--json")
						}
						out, diag, err := runProgressCommand(t, f, args...)
						if err != nil {
							t.Fatalf("%v\n%s", err, diag)
						}
						if mode == "false" {
							if diag != "" {
								t.Fatalf("quiet progress: %s", diag)
							}
						} else {
							assertProgressOrder(t, diag, "preflight started", "validation started", "Check ", " started", "Check ", " PASS")
							if operation == "submit" {
								if family == "issue" {
									assertProgressOrder(t, diag, "checkpoint started", "validation started", "checkpoint push/record started", "draft PR create/update started", "submission record started")
								} else {
									assertProgressOrder(t, diag, "validation started", "publish started", "submission record started", "review evidence update started", "rebase metadata update started")
								}
							}
						}
						if strings.Contains(out, "preflight started") || strings.Contains(out, "\nCheck ") {
							t.Fatalf("progress in stdout: %s", out)
						}
						if asJSON {
							d := json.NewDecoder(strings.NewReader(out))
							var v map[string]any
							if err := d.Decode(&v); err != nil {
								t.Fatal(err)
							}
							var extra any
							if err := d.Decode(&extra); err != io.EOF {
								t.Fatalf("extra stdout %v", err)
							}
							if v["status"] == "failed" {
								t.Fatalf("unexpected failure %s", out)
							}
						} else if operation == "validate" && !strings.Contains(out, "PASS ") {
							t.Fatalf("missing existing result %s", out)
						}
					})
				}
			}
		}
	}
}

func TestProgressContainsOnlyMetadata(t *testing.T) {
	var out bytes.Buffer
	operationProgressPrinter(&out)(progress.Event{Check: "trusted\n\x1b[31m\t", Status: "PASS"})
	if strings.Count(out.String(), "\n") != 1 || strings.ContainsAny(strings.TrimSuffix(out.String(), "\n"), "\x1b\t") {
		t.Fatalf("unescaped check: %q", out.String())
	}
}
func TestImmediateProgressOnEarlyFailure(t *testing.T) {
	for _, family := range []string{"issue", "pr"} {
		for _, op := range []string{"validate", "submit"} {
			f := newCLIFixture(t)
			_, diag, err := runProgressCommand(t, f, family, op, "999")
			if err == nil {
				t.Fatal("missing early failure")
			}
			if !strings.HasPrefix(diag, "preflight started") || !strings.Contains(diag, "preflight failed") {
				t.Fatalf("missing preflight: %s", diag)
			}
		}
	}
}

func TestSubmissionRetryAnnouncesOnlyExecutedPhases(t *testing.T) {
	f, issue, number := submittedPRFixture(t, "agent")
	for _, tc := range []struct {
		family string
		number int
	}{{"issue", issue}, {"pr", number}} {
		out, diag, err := runProgressCommand(t, f, tc.family, "submit", strconv.Itoa(tc.number), "--json")
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out, "local://pulls/") {
			t.Fatalf("lost PR URL: %s", out)
		}
		for _, phase := range []string{"checkpoint started", "validation started", "publish started", "submission record started", "draft PR create/update started"} {
			if strings.Contains(diag, phase) {
				t.Fatalf("retry fabricated %q: %s", phase, diag)
			}
		}
		if tc.family == "pr" {
			assertProgressOrder(t, diag, "review evidence update started", "rebase metadata update started")
		}
	}
}

func TestValidationFailureDoesNotAnnouncePublication(t *testing.T) {
	f := newCLIFixture(t)
	policy := "required_checks:\n  - name: trusted-failure\n    command: printf PRIVATE_CHECK_OUTPUT; exit 7\n    timeout: 1m\nprotected_paths: ['.swarm/**', '.github/**']\n"
	if err := os.WriteFile(filepath.Join(f.root, ".swarm/validation/required-checks.yaml"), []byte(policy), 0600); err != nil {
		t.Fatal(err)
	}
	prGit(t, f.root, "add", ".")
	prGit(t, f.root, "commit", "-m", "failing trusted check")
	n := strconv.Itoa(f.create(t, "none"))
	f.must(t, "issue", "approve", n)
	// Work records an implementation checkpoint even when its required check fails.
	if _, err := f.run("issue", "work", n, "--agent", "mock", "--progress=false"); err == nil {
		t.Fatal("expected failed check")
	}
	for _, operation := range []string{"validate", "submit"} {
		out, diag, err := runProgressCommand(t, f, "issue", operation, n, "--json")
		if err == nil {
			t.Fatal("failure accepted")
		}
		assertProgressOrder(t, diag, "validation started", "Check \"trusted-failure\" started", "Check \"trusted-failure\" FAIL")
		for _, private := range []string{"PRIVATE_CHECK_OUTPUT", "printf", "checkpoint push/record started", "draft PR create/update started", "submission record started"} {
			if strings.Contains(diag, private) {
				t.Fatalf("unexpected %q in progress: %s", private, diag)
			}
		}
		d := json.NewDecoder(strings.NewReader(out))
		var v any
		if err := d.Decode(&v); err != nil {
			t.Fatal(err)
		}
		if err := d.Decode(&v); err != io.EOF {
			t.Fatal("extra JSON stdout")
		}
	}
}

func TestProgressObserverDoesNotBlockLeaseRenewal(t *testing.T) {
	f := newCLIFixture(t)
	number := f.create(t, "none")
	f.must(t, "issue", "approve", strconv.Itoa(number))
	_, err := f.manager.Acquire(t.Context(), number, "alice", "mock", "plan", 3*time.Second, 3)
	if err != nil {
		t.Fatal(err)
	}
	heartbeat := f.manager.StartHeartbeat(t.Context(), number, "alice", 3*time.Second)
	defer heartbeat.Stop()
	entered, release, returned := make(chan struct{}), make(chan struct{}), make(chan struct{})
	session := progress.New(t.Context(), number, 0, func(e progress.Event) {
		if e.Stage == "validation" && e.Status == "started" {
			close(entered)
			<-release
		}
	}, nil, time.Now, nil)
	go func() { defer close(returned); session.Next("validation") }()
	<-entered
	deadline := time.NewTimer(4 * time.Second)
	defer deadline.Stop()
	poll := time.NewTicker(20 * time.Millisecond)
	defer poll.Stop()
	renewed := false
wait:
	for {
		select {
		case <-poll.C:
			state, e := f.manager.GetCurrentState(t.Context(), number)
			if e != nil {
				break wait
			}
			if state.Lease.Renewals > 0 {
				renewed = true
				break wait
			}
		case <-deadline.C:
			break wait
		}
	}
	close(release)
	<-returned
	session.Finish("completed")
	if !renewed || heartbeat.Err() != nil {
		t.Fatalf("observer blocked renewal: %v", heartbeat.Err())
	}
}
