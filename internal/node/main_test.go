package node

import (
	"context"
	"fmt"
	"github.com/supernovae/swarmos/internal/storageuse"
	"github.com/supernovae/swarmos/internal/testutil"
	"os"
	"path/filepath"
	"testing"
)

func TestMain(m *testing.M) {
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
		os.RemoveAll(ledgerRoot)
		os.Exit(code)
	}
	if err := checkLedger(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		ledgerExit(1)
	}

	root, err := os.MkdirTemp("", "swarm-node-test-*")
	if err != nil {
		panic(err)
	}

	code := m.Run()
	if err := checkLedger(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		code = 1
	}
	os.RemoveAll(root)
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
