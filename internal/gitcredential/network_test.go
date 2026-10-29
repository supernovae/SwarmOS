package gitcredential

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/supernovae/swarmos/internal/testutil"
	"github.com/supernovae/swarmos/internal/testutil/gitfixture"
	"github.com/supernovae/swarmos/internal/testutil/gittransport"
)

func networkBudgets(t *testing.T) {
	oldN, oldR, oldI := networkTimeout, reapGrace, ioGrace
	networkTimeout, reapGrace, ioGrace = 300*time.Millisecond, time.Second, 300*time.Millisecond
	t.Cleanup(func() { networkTimeout, reapGrace, ioGrace = oldN, oldR, oldI })
}
func networkFixture(t *testing.T) (string, *gittransport.Transport) {
	bare := gitfixture.Init(t, "", "--bare")
	repo := gitfixture.Init(t, "")
	gitfixture.Run(t, repo, "commit", "--allow-empty", "-m", "candidate")
	gitfixture.Run(t, repo, "remote", "add", "origin", bare)
	gitfixture.Run(t, repo, "push", "origin", "HEAD:refs/heads/main")
	tr := gittransport.New(t, bare)
	tr.Configure(t, repo, "origin")
	return repo, tr
}
func networkArgs(op string) []string {
	a := []string{op, "origin"}
	if op == "push" {
		a = append(a, "HEAD:refs/heads/candidate")
	}
	return a
}
func assertSettled(t *testing.T, r *networkRun) {
	t.Helper()
	if r == nil {
		return
	}
	select {
	case <-r.settledExit:
	case <-time.After(3 * time.Second):
		t.Error("whole run did not settle")
		return
	}
	for _, rd := range r.readers {
		select {
		case <-rd.done:
		default:
			t.Error("reader has not completed")
		}
		if rd.file.Fd() != ^uintptr(0) {
			t.Error("read descriptor still open")
		}
	}
	if _, err := syscall.Wait4(r.cmd.Process.Pid, nil, syscall.WNOHANG, nil); !errors.Is(err, syscall.ECHILD) {
		t.Errorf("leader wait incomplete: %v", err)
	}
	// Completion is a broadcast: every observer receives the same acknowledgement.
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); <-r.settled; <-r.reaped }()
	}
	wg.Wait()
}
func captureRun(t *testing.T, tr *gittransport.Transport) **networkRun {
	var r *networkRun
	lastRun = func(v *networkRun) {
		if v.cmd.Dir == tr.Repo {
			r = v
		}
	}
	t.Cleanup(func() { tr.Release(); tr.Join(); assertSettled(t, r); lastRun = nil })
	return &r
}
func TestNetworkBoundsHangingTransport(t *testing.T) {
	networkBudgets(t)
	for _, op := range []string{"fetch", "push", "ls-remote"} {
		t.Run(op, func(t *testing.T) {
			repo, tr := networkFixture(t)
			tr.Set("upload", "hang")
			tr.Set("receive", "hang")
			r := captureRun(t, tr)
			start := time.Now()
			_, err := Network(context.Background(), repo, true, networkArgs(op)...)
			if time.Since(start) > 3*time.Second {
				t.Error("unbounded return")
			}
			if !errors.Is(err, ErrInterrupted) || !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, ErrCleanupIncomplete) {
				t.Errorf("classification: %v", err)
			}
			if !strings.Contains(err.Error(), "left running") {
				t.Errorf("missing survivor guidance: %v", err)
			}
			if len(tr.Clients()) == 0 {
				t.Error("fixture not invoked")
			}
			for _, pid := range tr.Clients() {
				if !gittransport.Alive(pid) {
					t.Error("transport was signaled")
				}
			}
			assertSettled(t, *r)
			state := (*r).state
			if state == nil || state.Sys().(syscall.WaitStatus).Signal() != syscall.SIGKILL {
				t.Errorf("leader not killed: %v", state)
			}
			if _, e := syscall.Wait4((*r).cmd.Process.Pid, nil, syscall.WNOHANG, nil); !errors.Is(e, syscall.ECHILD) {
				t.Errorf("leader not reaped: %v", e)
			}
		})
	}
}
func TestNetworkWholeRunCompletion(t *testing.T) {
	networkBudgets(t)
	injected := errors.New("injected observation failure")
	for _, row := range []struct {
		name, mode                                 string
		readFail, waitFail, nilState, late, cancel bool
	}{
		{name: "success", mode: "pass"}, {name: "failure", mode: "reject"}, {name: "held success", mode: "holder"},
		{name: "read error", mode: "pass", readFail: true}, {name: "wait error", mode: "pass", waitFail: true},
		{name: "missing state", mode: "pass", nilState: true}, {name: "late waiter", mode: "hang", late: true},
		{name: "explicit cancel", mode: "hang", cancel: true},
		{name: "wait error canceled", mode: "hang", waitFail: true, cancel: true},
		{name: "missing state canceled", mode: "hang", nilState: true, cancel: true},
	} {
		t.Run(row.name, func(t *testing.T) {
			repo, tr := networkFixture(t)
			tr.Set("upload", row.mode)
			r := captureRun(t, tr)
			oldWait, oldRead := waitLeader, readNetwork
			release := make(chan struct{})
			var once sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			t.Cleanup(func() {
				unblock()
				tr.Release()
				tr.Join()
				assertSettled(t, *r)
				waitLeader, readNetwork = oldWait, oldRead
			})
			if row.late || row.waitFail || row.nilState {
				waitLeader = func(c *exec.Cmd) error {
					if row.late {
						<-release
					}
					e := oldWait(c)
					if row.nilState {
						c.ProcessState = nil
					}
					if row.waitFail {
						return injected
					}
					return e
				}
			}
			if row.readFail {
				readNetwork = func(w io.Writer, r io.Reader) (int64, error) { n, _ := io.Copy(w, r); return n, injected }
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if row.cancel {
				cancelTimer := time.AfterFunc(150*time.Millisecond, cancel)
				defer cancelTimer.Stop()
			}
			start := time.Now()
			out, err := Network(ctx, repo, false, "ls-remote", "origin")
			if time.Since(start) > 3*time.Second {
				t.Error("return budget exceeded")
			}
			snapshot := string(out)
			message := fmtError(err)
			if row.waitFail || row.nilState || row.late || row.cancel {
				if !errors.Is(err, ErrInterrupted) {
					t.Errorf("expected interruption: %v", err)
				}
			}
			if row.readFail || row.mode == "holder" {
				var known *KnownResultCleanupError
				if !errors.As(err, &known) || known.ExitCode != 0 || errors.Is(err, ErrInterrupted) {
					t.Errorf("expected known cleanup: %v", err)
				}
			}
			if row.waitFail || row.readFail {
				if !errors.Is(err, injected) {
					t.Errorf("lost injected cause: %v", err)
				}
			}
			if row.mode == "reject" {
				var exit *exec.ExitError
				if !errors.As(err, &exit) || errors.Is(err, ErrInterrupted) || errors.Is(err, ErrCleanupIncomplete) {
					t.Errorf("ordinary rejection: %v", err)
				}
			}
			if row.late {
				select {
				case <-(*r).settled:
					t.Error("late waiter already settled")
				default:
				}
			}
			unblock()
			tr.Release()
			tr.Join()
			// Missing-state injection has already executed real Wait; restore it for the
			// lifecycle assertion without changing the published outcome.
			if row.nilState {
				select {
				case <-(*r).settledExit:
				case <-time.After(time.Second):
					t.Error("missing state run not settled")
				}
			} else {
				assertSettled(t, *r)
			}
			if snapshot != string(out) || message != fmtError(err) {
				t.Error("late completion mutated result")
			}
		})
	}
}
func fmtError(e error) string {
	if e == nil {
		return ""
	}
	return e.Error()
}
func TestNetworkSetupPaths(t *testing.T) {
	networkBudgets(t)
	repo, tr := networkFixture(t)
	for _, row := range []string{"canceled", "first pipe", "second pipe", "start"} {
		t.Run(row, func(t *testing.T) {
			old := newPipe
			var files []*os.File
			calls := 0
			defer func() { newPipe = old }()
			newPipe = func() (*os.File, *os.File, error) {
				calls++
				if row == "first pipe" || row == "second pipe" && calls == 2 {
					return nil, nil, errors.New("pipe failed")
				}
				r, w, e := os.Pipe()
				if e == nil {
					files = append(files, r, w)
				}
				return r, w, e
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if row == "canceled" {
				cancel()
			}
			root := repo
			if row == "start" {
				root += "/absent"
			}
			_, err := Network(ctx, root, false, "fetch", "origin")
			if err == nil || errors.Is(err, ErrInterrupted) {
				t.Errorf("setup classification: %v", err)
			}
			for _, f := range files {
				if f.Fd() != ^uintptr(0) {
					t.Error("setup leaked descriptor")
				}
			}
			if len(tr.Clients()) != 0 {
				t.Error("setup started transport")
			}
		})
	}
}
func TestNetworkRedactsInterruptedDiagnostics(t *testing.T) {
	networkBudgets(t)
	t.Setenv("SWARM_GITHUB_TOKEN", "secret288")
	resetTokenCache()
	t.Cleanup(resetTokenCache)
	repo, tr := networkFixture(t)
	tr.Set("upload", "hang")
	captureRun(t, tr)
	_, err := Network(context.Background(), repo, true, "fetch", "origin")
	if strings.Contains(err.Error(), "secret288") || strings.Contains(err.Error(), repo) || strings.Contains(err.Error(), basicAuth("secret288")) {
		t.Errorf("unsafe diagnostic: %v", err)
	}
}

func TestNetworkClosesHeldPipesWithinBudget(t *testing.T) {
	networkBudgets(t)
	repo, tr := networkFixture(t)
	tr.Set("upload", "hang")
	r := captureRun(t, tr)
	before, _ := os.ReadDir("/proc/self/fd")
	old := newPipe
	var files []*os.File
	newPipe = func() (*os.File, *os.File, error) {
		rd, wr, err := old()
		if err == nil {
			files = append(files, rd, wr)
		}
		return rd, wr, err
	}
	defer func() { newPipe = old }()
	var err error
	tr.Watch(4*time.Second, func() { _, err = Network(context.Background(), repo, false, "fetch", "origin") })
	if !errors.Is(err, ErrCleanupIncomplete) {
		t.Errorf("cleanup classification: %v", err)
	}
	assertSettled(t, *r)
	for _, f := range files {
		if f.Fd() != ^uintptr(0) {
			t.Error("owned descriptor open after return")
		}
	}
	after, _ := os.ReadDir("/proc/self/fd")
	if len(after) != len(before) {
		t.Errorf("fd count %d -> %d", len(before), len(after))
	}
}
func TestNetworkAppliesDefaultBound(t *testing.T) {
	networkBudgets(t)
	for _, early := range []bool{false, true} {
		t.Run(map[bool]string{false: "default", true: "earlier caller"}[early], func(t *testing.T) {
			repo, tr := networkFixture(t)
			tr.Set("upload", "hang")
			r := captureRun(t, tr)
			ctx := context.Background()
			var cancel context.CancelFunc = func() {}
			expected := time.Now().Add(networkTimeout)
			if early {
				expected = time.Now().Add(100 * time.Millisecond)
				ctx, cancel = context.WithDeadline(ctx, expected)
			}
			defer cancel()
			_, err := Network(ctx, repo, true, "fetch", "origin")
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Errorf("deadline: %v", err)
			}
			if (*r).deadline.After(expected.Add(20 * time.Millisecond)) {
				t.Errorf("deadline extended: %s > %s", (*r).deadline, expected)
			}
		})
	}
}
func TestNetworkExceptionalObservation(t *testing.T) {
	networkBudgets(t)
	for _, combined := range []bool{false, true} {
		for _, canceled := range []bool{false, true} {
			t.Run(fmtErrorName(combined, canceled), func(t *testing.T) {
				repo, tr := networkFixture(t)
				if canceled {
					tr.Set("upload", "hang")
				}
				r := captureRun(t, tr)
				old := readNetwork
				cause := errors.New("unexpected read failure")
				readNetwork = func(dst io.Writer, src io.Reader) (int64, error) { n, _ := io.Copy(dst, src); return n, cause }
				t.Cleanup(func() { tr.Release(); tr.Join(); assertSettled(t, *r); readNetwork = old })
				_, err := Network(context.Background(), repo, combined, "ls-remote", "origin")
				if !errors.Is(err, cause) || !errors.Is(err, ErrCleanupIncomplete) {
					t.Errorf("lost observation cause: %v", err)
				}
				if canceled {
					if !errors.Is(err, ErrInterrupted) || !errors.Is(err, context.DeadlineExceeded) {
						t.Errorf("lost interruption: %v", err)
					}
				} else {
					var known *KnownResultCleanupError
					if !errors.As(err, &known) || known.ExitCode != 0 || errors.Is(err, ErrInterrupted) {
						t.Errorf("normal exit misclassified: %v", err)
					}
				}
			})
		}
	}
}
func fmtErrorName(combined, canceled bool) string {
	a := "separate pipes"
	if combined {
		a = "combined pipe"
	}
	if canceled {
		a += " canceled"
	}
	return a
}
func TestNetworkDetachedMasterKnownFailure(t *testing.T) {
	networkBudgets(t)
	repo, tr := networkFixture(t)
	tr.Set("upload", "holder")
	captureRun(t, tr)
	_, err := Network(context.Background(), repo, false, "fetch", "origin", strings.Repeat("a", 40))
	var exit *exec.ExitError
	var known *KnownResultCleanupError
	if !errors.As(err, &exit) || !errors.As(err, &known) || known.ExitCode == 0 || !errors.Is(err, ErrCleanupIncomplete) || errors.Is(err, ErrInterrupted) {
		t.Errorf("known failure: %v", err)
	}
	if !gittransport.Alive(tr.Master()) {
		t.Error("detached shared master was killed")
	}
}
func TestNetworkSharedDirectClientAndConcurrentClient(t *testing.T) {
	networkBudgets(t)
	repo, tr := networkFixture(t)
	tr.Set("upload", "hang")
	captureRun(t, tr)
	other, passing := networkFixture(t)
	unrelated := exec.Command("sleep", "60")
	if err := unrelated.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = unrelated.Process.Kill(); _ = unrelated.Wait() })
	done := make(chan error, 1)
	go func() { _, err := Network(context.Background(), other, true, "ls-remote", "origin"); done <- err }()
	// Independent ordinary Git client has a separate direct Wait owner.
	independent := gitfixture.Command(t, other, "ls-remote", "origin")
	if err := independent.Start(); err != nil {
		t.Fatal(err)
	}
	independentDone := make(chan error, 1)
	go func() { independentDone <- independent.Wait() }()
	t.Cleanup(func() { passing.Release(); passing.Join() })
	_, err := Network(context.Background(), repo, true, "ls-remote", "origin")
	if !errors.Is(err, ErrInterrupted) {
		t.Errorf("hanging client: %v", err)
	}
	if e := <-done; e != nil {
		t.Errorf("concurrent bounded client: %v", e)
	}
	if e := <-independentDone; e != nil {
		t.Errorf("independent client: %v", e)
	}
	if !gittransport.Alive(unrelated.Process.Pid) {
		t.Error("unrelated process killed")
	}
	for _, pid := range tr.Clients() {
		if !gittransport.Alive(pid) {
			t.Error("ambiguous direct child killed")
		}
	}
}

func TestNetworkTokenSetupFailure(t *testing.T) {
	t.Setenv("SWARM_GITHUB_TOKEN", "ghp_conflicting_secret")
	t.Setenv(TokenCommandEnv, "printf two")
	resetTokenCache()
	t.Cleanup(resetTokenCache)
	_, err := Network(context.Background(), t.TempDir(), false, "fetch", "origin")
	if err == nil || errors.Is(err, ErrInterrupted) || strings.Contains(err.Error(), "ghp_conflicting_secret") {
		t.Errorf("token setup failure: %v", err)
	}
}

func TestNetworkOrdinaryFailureIsKnown(t *testing.T) {
	networkBudgets(t)
	for _, op := range []string{"fetch", "push", "ls-remote"} {
		t.Run(op, func(t *testing.T) {
			repo, tr := networkFixture(t)
			tr.Set("upload", "reject")
			tr.Set("receive", "reject")
			captureRun(t, tr)
			_, err := Network(context.Background(), repo, false, networkArgs(op)...)
			var exit *exec.ExitError
			if !errors.As(err, &exit) || errors.Is(err, ErrInterrupted) || errors.Is(err, ErrCleanupIncomplete) {
				t.Errorf("ordinary %s: %v", op, err)
			}
			if exit != nil && !strings.Contains(string(exit.Stderr), "fixture rejected") {
				t.Error("stderr observation missing")
			}
		})
	}
}

func TestNetworkReadErrorsOnEachPipe(t *testing.T) {
	networkBudgets(t)
	for _, stream := range []int{0, 1} {
		for _, failure := range []bool{false, true} {
			t.Run(fmtErrorName(stream == 0, failure), func(t *testing.T) {
				repo, tr := networkFixture(t)
				if failure {
					tr.Set("upload", "reject")
				}
				r := captureRun(t, tr)
				oldPipe, oldRead := newPipe, readNetwork
				var ends []*os.File
				cause := errors.New("stream read failed")
				newPipe = func() (*os.File, *os.File, error) {
					rd, wr, e := oldPipe()
					if e == nil {
						ends = append(ends, rd)
					}
					return rd, wr, e
				}
				readNetwork = func(dst io.Writer, src io.Reader) (int64, error) {
					n, e := io.Copy(dst, src)
					if src == ends[stream] {
						return n, cause
					}
					return n, e
				}
				t.Cleanup(func() { tr.Release(); tr.Join(); assertSettled(t, *r); newPipe, readNetwork = oldPipe, oldRead })
				_, err := Network(context.Background(), repo, false, "ls-remote", "origin")
				var known *KnownResultCleanupError
				if !errors.As(err, &known) || !errors.Is(err, cause) || !errors.Is(err, ErrCleanupIncomplete) || errors.Is(err, ErrInterrupted) {
					t.Errorf("stream observation: %v", err)
				}
				if failure {
					var exit *exec.ExitError
					if !errors.As(err, &exit) {
						t.Error("known failure exit cause lost")
					}
				}
			})
		}
	}
}

// The unexported destination seam runs real Git against an authenticated local
// smart-HTTP server; production ForkRead constructs HTTPS destinations only.
func TestForkReadRealAuthenticatedGit(t *testing.T) {
	root := gitfixture.Init(t, "")
	gitfixture.Run(t, root, "commit", "--allow-empty", "-m", "fixture")
	sha := strings.TrimSpace(gitfixture.Run(t, root, "rev-parse", "HEAD"))
	gitfixture.Run(t, root, "branch", "-M", "main")
	gitfixture.Run(t, root, "config", "http.receivepack", "false")
	parent := filepath.Dir(root)
	name := filepath.Base(root)
	const token = "selected-read-sentinel"
	var requests atomic.Int32
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		if errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.EACCES) {
			t.Skipf("local HTTP transport unavailable: %v", err)
		}
		t.Fatal(err)
	}
	server := &httptest.Server{Listener: listener, Config: &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Header.Get("Authorization") != "basic "+basicAuth(token) {
			t.Error("unexpected authentication")
			w.WriteHeader(401)
			return
		}
		handler := &cgi.Handler{Path: "/usr/bin/git", Args: []string{"http-backend"}, Env: []string{"GIT_PROJECT_ROOT=" + parent, "GIT_HTTP_EXPORT_ALL=1"}}
		handler.ServeHTTP(w, r)
	})}}
	server.Start()
	defer server.Close()
	destination := server.URL + "/" + name
	target := gitfixture.Init(t, "")
	out, err := forkRead(context.Background(), target, destination, token, "ls-remote", "main")
	if err != nil || !strings.Contains(string(out), sha) {
		t.Fatalf("lookup: %s %v", out, err)
	}
	if _, err := forkRead(context.Background(), target, destination, token, "fetch", sha); err != nil {
		t.Fatal(err)
	}
	gitfixture.Run(t, target, "cat-file", "-e", sha)
	if requests.Load() < 2 {
		t.Fatal("missing authenticated requests")
	}
	if _, err := os.Stat(filepath.Join(target, ".git", "FETCH_HEAD")); !os.IsNotExist(err) {
		t.Fatal("fetch persisted FETCH_HEAD")
	}
}

func TestSelectedForkReadUsesBoundedEngine(t *testing.T) {
	networkBudgets(t)
	root := t.TempDir()
	helper := filepath.Join(root, "git")
	if err := testutil.WriteExecutable(helper, []byte("#!/bin/sh\nif [ \"$1\" = config ]; then exit 1; fi\nexec sleep 30\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", root+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("SWARM_GITHUB_TOKEN", "writer-sentinel")
	t.Setenv(TokenCommandEnv, "/nonexistent/no-writer-credential-command")
	t.Setenv("SWARM_GITHUB_FORK_READ_TOKEN", "read-sentinel")
	var run *networkRun
	lastRun = func(r *networkRun) { run = r }
	t.Cleanup(func() { assertSettled(t, run); lastRun = nil })
	start := time.Now()
	_, err := ForkRead(context.Background(), root, "github.com", "alice/repo", "read-sentinel", "ls-remote", "main")
	if !errors.Is(err, ErrInterrupted) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("classification: %v", err)
	}
	if time.Since(start) > 3*time.Second {
		t.Fatal("unbounded selected read")
	}
	if run == nil {
		t.Fatal("shared runner not used")
	}
	for _, entry := range run.cmd.Env {
		if strings.Contains(entry, "writer-sentinel") || strings.HasPrefix(entry, "SWARM_GITHUB_FORK_READ_TOKEN=") {
			t.Fatal("raw credential in read child")
		}
	}
	if strings.Contains(strings.Join(run.cmd.Args, " "), "sentinel") {
		t.Fatal("credential in argv")
	}
}

func TestSelectedForkReadRetainsKnownResultWithHeldPipe(t *testing.T) {
	networkBudgets(t)
	root := t.TempDir()
	done := filepath.Join(root, "holder-done")
	helper := filepath.Join(root, "git")
	if err := testutil.WriteExecutable(helper, []byte("#!/bin/sh\nif [ \"$1\" = config ]; then exit 1; fi\n(sleep 1; touch \"$FORK_READ_HOLDER_DONE\") &\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", root+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FORK_READ_HOLDER_DONE", done)
	t.Cleanup(func() {
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			if _, err := os.Stat(done); err == nil {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Error("held-pipe fixture did not finish")
	})
	_, err := ForkRead(context.Background(), root, "github.com", "alice/repo", "read-sentinel", "ls-remote", "main")
	var known *KnownResultCleanupError
	if !errors.As(err, &known) || known.ExitCode != 0 || !errors.Is(err, ErrCleanupIncomplete) || errors.Is(err, ErrInterrupted) {
		t.Fatalf("lost known outcome: %v", err)
	}
}
