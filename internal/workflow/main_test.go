package workflow

import (
	"context"
	"fmt"
	"github.com/supernovae/swarmos/internal/sandbox"
	"github.com/supernovae/swarmos/internal/storageuse"
	"github.com/supernovae/swarmos/internal/testutil"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// TestMain keeps the persistent Go build cache that host validation creates
// under XDG_CACHE_HOME out of the real user cache directory.
func TestMain(m *testing.M) {
	if len(os.Args) == 3 && os.Args[1] == "internal-native-exec" {
		if err := sandbox.ExecNativeScope(os.Args[2]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}

	ledgerRoot, ledgerErr := os.MkdirTemp("", "swarm-test-ledger-*")
	if ledgerErr != nil {
		panic(ledgerErr)
	}
	ledgerRoot, ledgerErr = filepath.EvalSymlinks(ledgerRoot)
	if ledgerErr != nil {
		panic(ledgerErr)
	}
	storageuse.UserCacheDir = func() (string, error) { return ledgerRoot, nil }
	checkLedger := func() error {
		control, err := storageuse.ControlRoot()
		return testutil.CheckStorageLedger(ledgerRoot, control, err)
	}
	ledgerExit := func(code int) {
		if err := checkLedger(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			code = 1
		}
		if err := os.RemoveAll(ledgerRoot); err != nil {
			fmt.Fprintln(os.Stderr, "cleanup test ledger:", err)
			code = 1
		}
		os.Exit(code)
	}
	if err := checkLedger(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		ledgerExit(1)
	}

	dir, err := os.MkdirTemp("", "swarm-test-cache-*")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		ledgerExit(1)
	}
	os.Setenv("XDG_CACHE_HOME", dir)
	os.Setenv("HOME", dir)

	code := m.Run()
	if err := checkLedger(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		code = 1
	}
	os.RemoveAll(dir)
	ledgerExit(code)
}

// Exercise the shared scratch producer used by coordinator workflows after a
// test changes user directories; the package resolver must remain fixed.
func TestStorageLedgerProducerIgnoresEnvironment(t *testing.T) {
	control, err := storageuse.ControlRoot()
	if err != nil {
		t.Fatal(err)
	}
	decoy := t.TempDir()
	t.Setenv("HOME", decoy)
	t.Setenv("XDG_CACHE_HOME", decoy)
	ctx, scope, err := storageuse.Ensure(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	defer scope.Finish()
	path, err := scope.TempDir(ctx, t.TempDir(), "producer-*", "review-scratch")
	if err != nil {
		t.Fatal(err)
	}
	if err = scope.Finish(); err != nil {
		t.Fatal(err)
	}
	resource := storageuse.ResourceFor(scope.Repository(), "review-scratch", path)
	observation, err := storageuse.Observe(ctx, control, resource, 16<<10, func(int64) error { return nil })
	if err != nil || !observation.CompleteCoverage || observation.Active == nil || *observation.Active {
		t.Fatalf("missing finished producer record: %+v %v", observation, err)
	}
	for _, candidate := range []string{filepath.Join(decoy, "swarm-use"), filepath.Join(decoy, "Library", "Caches", "swarm-use")} {
		if _, err := os.Stat(candidate); !os.IsNotExist(err) {
			t.Fatalf("producer wrote decoy ledger: %s %v", candidate, err)
		}
	}
	restored, err := storageuse.ControlRoot()
	if err != nil || restored != control {
		t.Fatal("resolver moved", restored, err)
	}
}

// TestNativeHelperDispatch exercises the helper protocol without a live manager.
func TestNativeHelperDispatch(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, malformed := range []bool{false, true} {
		t.Run(fmt.Sprintf("malformed=%v", malformed), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, executable, "internal-native-exec", "swarm-native-test.scope")
			if malformed {
				file, err := os.Open(os.DevNull)
				if err != nil {
					t.Fatal(err)
				}
				defer file.Close()
				cmd.ExtraFiles = []*os.File{file, file, file}
			}
			output, err := cmd.CombinedOutput()
			if ctx.Err() != nil {
				t.Fatalf("helper did not terminate: %v\n%s", ctx.Err(), output)
			}
			if err == nil {
				t.Fatalf("helper accepted invalid control descriptors: %s", output)
			}
			want := "native control pipe"
			if runtime.GOOS != "linux" {
				want = "native storage scope unavailable"
			}
			if !strings.Contains(string(output), want) {
				t.Fatalf("helper validation not reached: %v\n%s", err, output)
			}
		})
	}
}
