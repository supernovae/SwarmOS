// Package gittransport supplies release-controlled coordinator transports for tests.
package gittransport

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/supernovae/swarmos/internal/testutil"
	"github.com/supernovae/swarmos/internal/testutil/gitfixture"
)

type Transport struct {
	t              testing.TB
	Dir, Bare      string
	Repo           string
	stop           chan struct{}
	supervisorDone chan struct{}
	once           sync.Once
	report         func(string, ...any)
}

func New(t testing.TB, bare string) *Transport {
	t.Helper()
	tr := &Transport{t: t, Dir: t.TempDir(), Bare: bare, stop: make(chan struct{}), supervisorDone: make(chan struct{}), report: t.Errorf}
	t.Cleanup(func() { tr.Release(); tr.Join() })
	go tr.superviseHolder()
	tr.Set("upload", "pass")
	tr.Set("receive", "pass")
	script := `#!/bin/sh
D=DIR
exec 3<&0
printf '%s\n' "$$" >> "$D/clients"
trap 'touch "$D/exit-$$"' EXIT
case "$2" in *receive*) service=receive;; *) service=upload;; esac
mode=$(cat "$D/$service")
printf '%s %s %s\n' "$$" "$service" "$mode" >> "$D/services"
case "$mode" in
reject) echo 'fixture rejected' >&2; exit 128;;
hang) while [ ! -f "$D/release" ]; do sleep .05 & wait $!; done; exit 0;;
holder|holder-reject)
 echo "$$" > "$D/holder-request"
 while [ ! -f "$D/master-ready" ] && [ ! -f "$D/release" ]; do sleep .01 & wait $!; done
 ;;

esac
if [ "$mode" = holder-reject ]; then echo "fixture rejected" >&2; exit 128; fi
# Authentication environment stays unchanged in production. The fixture service
# must run its own post-receive hook instead of the coordinator hook policy.
unset GIT_CONFIG_COUNT
sh -c "$2" <&3 &
p=$!
wait "$p"
result=$?
touch "$D/service-exit-$$"
exit "$result"
`
	tr.write(filepath.Join(tr.Dir, "ssh"), strings.ReplaceAll(script, "DIR", quote(tr.Dir)), 0700)
	hook := `#!/bin/sh
D=DIR
if [ "$(cat "$D/receive")" = land-hang ]; then
 cat > "$D/updates"
 touch "$D/landed"
 while [ ! -f "$D/release" ]; do sleep .05 & wait $!; done
 touch "$D/hook-exit"
fi
`
	tr.write(filepath.Join(bare, "hooks", "post-receive"), strings.ReplaceAll(hook, "DIR", quote(tr.Dir)), 0700)
	return tr
}
func quote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }
func (tr *Transport) write(p, s string, mode os.FileMode) {
	tr.t.Helper()
	write := os.WriteFile
	if mode&0111 != 0 {
		write = testutil.WriteExecutable
	}
	if err := write(p, []byte(s), mode); err != nil {
		tr.t.Fatal(err)
	}
}
func (tr *Transport) Configure(t testing.TB, repo, remote string) {
	t.Helper()
	tr.Repo = repo
	gitfixture.Run(t, repo, "config", "core.sshCommand", quote(filepath.Join(tr.Dir, "ssh")))
	gitfixture.Run(t, repo, "config", "ssh.variant", "simple")
	gitfixture.Run(t, repo, "remote", "set-url", remote, "ssh://fake.invalid"+tr.Bare)
}
func (tr *Transport) Set(service, mode string) { tr.write(filepath.Join(tr.Dir, service), mode, 0600) }
func (tr *Transport) Release() {
	tr.write(filepath.Join(tr.Dir, "release"), "", 0600)
	tr.once.Do(func() { close(tr.stop) })
}
func (tr *Transport) Clients() []int {
	b, _ := os.ReadFile(filepath.Join(tr.Dir, "clients"))
	var pids []int
	for _, s := range strings.Fields(string(b)) {
		p, _ := strconv.Atoi(s)
		pids = append(pids, p)
	}
	return pids
}
func (tr *Transport) Receives() int {
	b, _ := os.ReadFile(filepath.Join(tr.Dir, "services"))
	return strings.Count(string(b), "receive")
}
func (tr *Transport) Master() int {
	b, _ := os.ReadFile(filepath.Join(tr.Dir, "master"))
	p, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	return p
}
func (tr *Transport) Landed() bool {
	_, err := os.Stat(filepath.Join(tr.Dir, "landed"))
	return err == nil
}
func Alive(pid int) bool {
	if pid <= 0 || syscall.Kill(pid, 0) != nil {
		return false
	}
	b, _ := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	i := strings.LastIndex(string(b), ")")
	return i >= 0 && !strings.HasPrefix(string(b)[i+1:], " Z")
}
func (tr *Transport) Join() {
	tr.t.Helper()
	select {
	case <-tr.supervisorDone:
	case <-time.After(5 * time.Second):
		tr.report("holder supervisor did not finish")
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		complete := true
		for _, pid := range tr.Clients() {
			if _, err := os.Stat(filepath.Join(tr.Dir, fmt.Sprintf("exit-%d", pid))); err != nil || Alive(pid) {
				complete = false
			}
		}
		services, _ := os.ReadFile(filepath.Join(tr.Dir, "services"))
		for _, line := range strings.Split(strings.TrimSpace(string(services)), "\n") {
			fields := strings.Fields(line)
			if len(fields) != 3 {
				continue
			}
			switch fields[2] {
			case "pass", "holder", "land-hang":
				if _, err := os.Stat(filepath.Join(tr.Dir, "service-exit-"+fields[0])); err != nil {
					complete = false
				}
			}
		}
		if tr.Master() != 0 {
			if _, err := os.Stat(filepath.Join(tr.Dir, "master-exit")); err != nil {
				complete = false
			}
		}
		if tr.Landed() {
			if _, err := os.Stat(filepath.Join(tr.Dir, "hook-exit")); err != nil {
				complete = false
			}
		}
		if complete {
			return
		}
		if time.Now().After(deadline) {
			tr.report("transport teardown acknowledgements missing in %s", tr.Dir)
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// superviseHolder owns Start/Wait of the detached fixture master. The transport
// hands it a copy of stderr through procfs while waiting for master-ready.
func (tr *Transport) superviseHolder() {
	defer close(tr.supervisorDone)
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-tr.stop:
			return
		case <-ticker.C:
		}
		b, err := os.ReadFile(filepath.Join(tr.Dir, "holder-request"))
		if err != nil {
			continue
		}
		pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
		if err != nil {
			continue
		}
		stderr, err := os.OpenFile(fmt.Sprintf("/proc/%d/fd/2", pid), os.O_WRONLY, 0)
		if err != nil {
			tr.report("open holder descriptor: %v", err)
			return
		}
		cmd := exec.Command("sh", "-c", `while [ ! -f "$1/release" ]; do sleep .05 & wait $!; done`, "sh", tr.Dir)
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		cmd.Stderr = stderr
		err = cmd.Start()
		_ = stderr.Close()
		if err != nil {
			tr.report("start holder: %v", err)
			return
		}
		// Directly test-owned child: this goroutine is its sole Wait owner.
		_ = os.WriteFile(filepath.Join(tr.Dir, "master"), []byte(strconv.Itoa(cmd.Process.Pid)), 0600)
		_ = os.WriteFile(filepath.Join(tr.Dir, "master-ready"), nil, 0600)
		if err = cmd.Wait(); err != nil {
			tr.report("wait holder: %v", err)
		}
		_ = os.WriteFile(filepath.Join(tr.Dir, "master-exit"), nil, 0600)
		return
	}
}

// Watch releases the fixture before reporting an outer watchdog failure and
// joins the operation. Assertions in callers run only after settlement.
func (tr *Transport) Watch(budget time.Duration, fn func()) {
	tr.t.Helper()
	done := make(chan struct{})
	go func() { defer close(done); fn() }()
	timer := time.NewTimer(budget)
	defer timer.Stop()
	select {
	case <-done:
		return
	case <-timer.C:
	}
	tr.Release()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		tr.report("operation did not join after watchdog release")
	}
	tr.Join()
	tr.report("operation exceeded %s watchdog", budget)
}
