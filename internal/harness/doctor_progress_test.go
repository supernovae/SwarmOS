package harness

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/supernovae/swarmos/internal/progress"
	"github.com/supernovae/swarmos/internal/testutil"
)

func TestDoctorRunnerLifecycle(t *testing.T) {
	for _, status := range []string{"OK", "WARN", "FAIL", "OPTIONAL"} {
		t.Run(status, func(t *testing.T) {
			var seconds atomic.Int64
			ticks := make(chan time.Time)
			events := make(chan progress.Event, 10)
			run := NewDoctorRunner(context.Background(), func(e progress.Event) { events <- e }, func() <-chan time.Time { return ticks }, func() time.Time { return time.Unix(seconds.Load(), 0) })
			want := DoctorCheck{"category", "name\ncontrol", status, "private detail"}
			result := make(chan DoctorCheck)
			release := make(chan struct{})
			go func() { result <- run.Run(want.Category, want.Name, func() DoctorCheck { <-release; return want }) }()
			read := func(status string, elapsed time.Duration) {
				t.Helper()
				select {
				case e := <-events:
					if e.Check != want.Name || e.Status != status || e.StageElapsed != elapsed {
						t.Fatalf("event: %+v", e)
					}
				case <-time.After(time.Second):
					t.Fatal("missing event")
				}
			}
			read("started", 0)
			for _, second := range []int64{15, 30} {
				seconds.Store(second)
				ticks <- time.Unix(second, 0)
				read("running", time.Duration(second)*time.Second)
			}
			close(release)
			if got := <-result; got != want {
				t.Fatalf("result = %+v", got)
			}
			read(status, 30*time.Second)
			// A fresh invocation must not inherit the previous elapsed time.
			got := run.Run(want.Category, want.Name, func() DoctorCheck { return want })
			if got != want {
				t.Fatal(got)
			}
			read("started", 0)
			read(status, 0)
			select {
			case e := <-events:
				t.Fatalf("late event: %+v", e)
			default:
			}
		})
	}
}

func TestDoctorRunnerCancellationAndPanicCleanup(t *testing.T) {
	for _, panicCheck := range []bool{false, true} {
		ctx, cancel := context.WithCancel(context.Background())
		ticks := make(chan time.Time)
		var events []progress.Event
		run := NewDoctorRunner(ctx, func(e progress.Event) { events = append(events, e) }, func() <-chan time.Time { return ticks }, nil)
		func() {
			defer func() {
				if r := recover(); (r != nil) != panicCheck {
					t.Fatalf("panic: %v", r)
				}
			}()
			run.Run("c", "n", func() DoctorCheck {
				cancel()
				if panicCheck {
					panic("fixture")
				}
				return DoctorCheck{Status: "WARN"}
			})
		}()
		want := 1
		if !panicCheck {
			want = 2
		}
		if len(events) != want {
			t.Fatalf("events: %+v", events)
		}
		cancel()
	}
}

func TestRunDoctorObservedMatchesSilent(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	t.Setenv("SWARM_GITHUB_TOKEN_COMMAND", "")
	t.Setenv("SWARM_GITHUB_TOKEN", "fixture")
	root := t.TempDir()
	silent := RunDoctor(root, false)
	var names []string
	var observed []DoctorCheck
	run := DoctorRunner(func(category, name string, check func() DoctorCheck) DoctorCheck {
		names = append(names, name)
		result := check()
		if result.Category != category {
			t.Fatal(result)
		}
		observed = append(observed, result)
		return result
	})
	got := RunDoctorWithRunner(root, false, run)
	if !reflect.DeepEqual(got, silent) || !reflect.DeepEqual(got, observed) {
		t.Fatalf("got %+v; want %+v", got, silent)
	}
	if !reflect.DeepEqual(names, []string{"git", "github-auth", ".swarm/config.yaml", ".swarm/validation/required-checks.yaml", "reviewer-runtime"}) {
		t.Fatal(names)
	}
}

func TestRunDoctorAuthAndConfiguredChecks(t *testing.T) {
	for _, auth := range []string{"token", "gh-success", "gh-failure", "gh-local", "missing", "command-failure"} {
		t.Run(auth, func(t *testing.T) {
			if os.Getenv("SWARM_DOCTOR_AUTH_FIXTURE") != auth {
				binary, err := os.Executable()
				if err != nil {
					t.Fatal(err)
				}
				cmd := exec.Command(binary, "-test.run=^TestRunDoctorAuthAndConfiguredChecks$/^"+auth+"$")
				cmd.Env = append(os.Environ(), "SWARM_DOCTOR_AUTH_FIXTURE="+auth)
				if out, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("fixture: %v\n%s", err, out)
				}
				return
			}
			bin := t.TempDir()
			root := t.TempDir()
			t.Setenv("PATH", bin)
			t.Setenv("SWARM_GITHUB_TOKEN_COMMAND", "")
			t.Setenv("SWARM_GITHUB_TOKEN", "")
			t.Setenv("GH_TOKEN", "")
			t.Setenv("GITHUB_TOKEN", "")
			if auth == "token" {
				t.Setenv("SWARM_GITHUB_TOKEN", "fixture")
			}
			if strings.HasPrefix(auth, "gh-") {
				code := "0"
				if auth == "gh-failure" {
					code = "1"
				}
				if err := testutil.WriteExecutable(filepath.Join(bin, "gh"), []byte("#!/bin/sh\necho fixture\nexit "+code+"\n"), 0700); err != nil {
					t.Fatal(err)
				}
			}
			if auth == "command-failure" {
				t.Setenv("SWARM_GITHUB_TOKEN_COMMAND", "/missing-fixture-command")
			}
			if err := os.MkdirAll(filepath.Join(root, ".swarm"), 0700); err != nil {
				t.Fatal(err)
			}
			data, err := embeddedTemplates.ReadFile("templates/swarm/config.yaml")
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, ".swarm/config.yaml"), data, 0600); err != nil {
				t.Fatal(err)
			}
			var observed []DoctorCheck
			run := DoctorRunner(func(category, name string, check func() DoctorCheck) DoctorCheck {
				result := check()
				if name != result.Name && name != "github-auth" {
					t.Fatalf("label %s result %+v", name, result)
				}
				observed = append(observed, result)
				return result
			})
			ghAuth := auth != "gh-local"
			got := RunDoctorWithRunner(root, ghAuth, run)
			if !reflect.DeepEqual(got, RunDoctor(root, ghAuth)) || !reflect.DeepEqual(got, observed) {
				t.Fatal("reporting changed results")
			}
			wantName, wantStatus := "github-auth", "WARN"
			switch auth {
			case "token":
				wantName, wantStatus = "github-token", "OK"
			case "gh-success":
				wantName, wantStatus = "gh-auth", "OK"
			case "gh-failure":
				wantName = "gh-auth"
			case "gh-local":
				wantName, wantStatus = "gh-cli", "OK"
			case "command-failure":
				wantName, wantStatus = "github-token", "FAIL"
			}
			if got[1].Name != wantName || got[1].Status != wantStatus {
				t.Fatal(got[1])
			}
			timeouts, binaries := 0, 0
			for _, c := range got {
				if strings.HasSuffix(c.Name, " timeout") {
					timeouts++
				}
				if c.Category == "binary" {
					binaries++
				}
			}
			if timeouts != 4 || binaries == 0 {
				t.Fatalf("timeouts %d binaries %d", timeouts, binaries)
			}
		})
	}
}
