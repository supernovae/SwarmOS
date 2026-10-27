package agent

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
)

const recoveryTestPatch = "diff --git a/src/main.go b/src/main.go\nnew file mode 100644\n--- /dev/null\n+++ b/src/main.go\n@@ -0,0 +1 @@\n+package main\n"

var recoveryTestHeader = RecoveryHeader{Issue: 7, PR: 42, Role: "developer", HeadSHA: strings.Repeat("a", 40), Rejected: []string{".swarm/active/VALIDATION.md"}}

func recoveryFiles(t *testing.T, state string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(state, "swarm", "recovery"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		if recoveryFileName.MatchString(entry.Name()) {
			names = append(names, entry.Name())
		}
	}
	return names
}

func TestRecoveryHeaderRoundTrip(t *testing.T) {
	state := useDiagnosticsState(t)
	path, err := SaveRecoveryPatch(t.TempDir(), recoveryTestHeader, []byte(recoveryTestPatch))
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(path) != filepath.Join(state, "swarm", "recovery") || !recoveryFileName.MatchString(filepath.Base(path)) {
		t.Fatalf("saved to %s", path)
	}
	header, body, err := LoadRecoveryPatch(filepath.Base(path))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(header, recoveryTestHeader) || string(body) != recoveryTestPatch {
		t.Fatalf("round trip: %+v %q", header, body)
	}
	// Maintainers can inspect the saved file directly; Git skips the header.
	cmd := exec.Command("git", "apply", "--stat", path)
	cmd.Dir = t.TempDir()
	if out, err := cmd.CombinedOutput(); err != nil || !strings.Contains(string(out), "src/main.go") {
		t.Fatalf("git apply --stat: %v %s", err, out)
	}
}

func TestSaveRecoveryPatchPrivate(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX modes")
	}
	useDiagnosticsState(t)
	path, err := SaveRecoveryPatch(t.TempDir(), recoveryTestHeader, []byte(recoveryTestPatch))
	if err != nil {
		t.Fatal(err)
	}
	for file, want := range map[string]os.FileMode{filepath.Dir(path): 0o700, path: 0o600, filepath.Join(filepath.Dir(path), ".lock"): 0o600} {
		info, err := os.Lstat(file)
		if err != nil || info.Mode().Perm() != want {
			t.Fatalf("%s: %v %v", file, info.Mode(), err)
		}
	}
	for label, tc := range map[string]struct {
		want  diagnosticsUnavailable
		setup func(t *testing.T, state, worktree string)
	}{
		"shared directory": {unavailablePrivate, func(t *testing.T, state, _ string) {
			dir := filepath.Join(state, "swarm", "recovery")
			mustMkdir(t, dir)
			if err := os.Chmod(dir, 0o755); err != nil {
				t.Fatal(err)
			}
		}},
		"symlinked recovery directory": {unavailablePrivate, func(t *testing.T, state, _ string) {
			mustMkdir(t, filepath.Join(state, "swarm"))
			mustSymlink(t, t.TempDir(), filepath.Join(state, "swarm", "recovery"))
		}},
		"inside worktree": {unavailableUnsafe, func(t *testing.T, _, worktree string) {
			diagnosticsStateDir = func() (string, error) { return filepath.Join(worktree, ".state"), nil }
		}},
		"invalid generated name": {unavailableWrite, func(t *testing.T, _, _ string) {
			previous := recoveryNewName
			recoveryNewName = func() string { return "../escape.patch" }
			t.Cleanup(func() { recoveryNewName = previous })
		}},
	} {
		t.Run(label, func(t *testing.T) {
			state := useDiagnosticsState(t)
			worktree := t.TempDir()
			tc.setup(t, state, worktree)
			_, err := SaveRecoveryPatch(worktree, recoveryTestHeader, []byte(recoveryTestPatch))
			if err != tc.want {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			if _, statErr := os.Stat(filepath.Join(worktree, ".state")); statErr == nil {
				t.Fatal("created recovery storage inside the worktree")
			}
		})
	}
}

func TestSaveRecoveryPatchBounded(t *testing.T) {
	state := useDiagnosticsState(t)
	huge := recoveryTestPatch + "+" + strings.Repeat("x", RecoveryPatchMaxBytes) + "\n"
	for label, tc := range map[string]struct {
		header RecoveryHeader
		patch  string
		want   diagnosticsUnavailable
	}{
		"oversized":     {recoveryTestHeader, huge, unavailableOversized},
		"empty":         {recoveryTestHeader, "", unavailableOversized},
		"reviewer role": {RecoveryHeader{Issue: 7, Role: "reviewer", HeadSHA: recoveryTestHeader.HeadSHA, Rejected: recoveryTestHeader.Rejected}, recoveryTestPatch, unavailableHeader},
		"short head":    {RecoveryHeader{Issue: 7, Role: "developer", HeadSHA: "abc", Rejected: recoveryTestHeader.Rejected}, recoveryTestPatch, unavailableHeader},
		"no rejected":   {RecoveryHeader{Issue: 7, Role: "developer", HeadSHA: recoveryTestHeader.HeadSHA}, recoveryTestPatch, unavailableHeader},
		"newline path":  {RecoveryHeader{Issue: 7, Role: "developer", HeadSHA: recoveryTestHeader.HeadSHA, Rejected: []string{"a\n# role tester"}}, recoveryTestPatch, unavailableHeader},
		"missing issue": {RecoveryHeader{Role: "developer", HeadSHA: recoveryTestHeader.HeadSHA, Rejected: recoveryTestHeader.Rejected}, recoveryTestPatch, unavailableHeader},
		"negative pr":   {RecoveryHeader{Issue: 7, PR: -1, Role: "developer", HeadSHA: recoveryTestHeader.HeadSHA, Rejected: recoveryTestHeader.Rejected}, recoveryTestPatch, unavailableHeader},
		"space in path": {RecoveryHeader{Issue: 7, Role: "developer", HeadSHA: recoveryTestHeader.HeadSHA, Rejected: []string{"a b"}}, recoveryTestPatch, unavailableHeader},
	} {
		if _, err := SaveRecoveryPatch(t.TempDir(), tc.header, []byte(tc.patch)); err != tc.want {
			t.Fatalf("%s: err = %v, want %v", label, err, tc.want)
		}
	}
	if files := recoveryFiles(t, state); len(files) != 0 {
		t.Fatalf("saved %v", files)
	}
	for _, name := range []string{"../x", "rejected-0123.patch", "failure-0123456789abcdef0123456789abcdef.log", "rejected-0123456789abcdef0123456789abcdef.patch/../x"} {
		if _, _, err := LoadRecoveryPatch(name); err == nil || !strings.Contains(err.Error(), "invalid recovery patch name") {
			t.Fatalf("%s: %v", name, err)
		}
	}
	dir := filepath.Join(state, "swarm", "recovery")
	mustMkdir(t, dir)
	const name = "rejected-0123456789abcdef0123456789abcdef.patch"
	header := recoveryMagic + "# issue 7\n# pr 0\n# role developer\n# head " + recoveryTestHeader.HeadSHA + "\n# rejected .swarm/active/VALIDATION.md\n"
	mustWrite(t, filepath.Join(dir, name), header+huge)
	if _, _, err := LoadRecoveryPatch(name); err == nil {
		t.Fatal("loaded an oversized patch")
	}
	for label, text := range map[string]string{
		"no magic":       strings.TrimPrefix(header, recoveryMagic) + recoveryTestPatch,
		"unknown key":    header + "# extra x\n" + recoveryTestPatch,
		"duplicate role": strings.Replace(header, "# pr 0\n", "# pr 0\n# role tester\n", 1) + recoveryTestPatch,
		"no diff":        header + "garbage\n" + recoveryTestPatch,
		"missing pr":     strings.Replace(header, "# pr 0\n", "", 1) + recoveryTestPatch,
	} {
		mustWrite(t, filepath.Join(dir, name), text)
		if _, _, err := LoadRecoveryPatch(name); err == nil {
			t.Fatalf("%s: loaded an invalid patch", label)
		}
	}
	if _, _, err := LoadRecoveryPatch("rejected-ffffffffffffffffffffffffffffffff.patch"); err == nil {
		t.Fatal("loaded a missing patch")
	}
}

func TestRecoveryPatchWithholdsCredentials(t *testing.T) {
	state := useDiagnosticsState(t)
	for _, secret := range []string{diagnosticsTestToken, diagnosticsTestBasic} {
		patch := recoveryTestPatch + "diff --git a/.env b/.env\nnew file mode 100644\n--- /dev/null\n+++ b/.env\n@@ -0,0 +1 @@\n+TOKEN=" + secret + "\n"
		_, err := SaveRecoveryPatch(t.TempDir(), recoveryTestHeader, []byte(patch))
		if err != unavailableCredential || strings.Contains(err.Error(), secret) {
			t.Fatalf("err = %v", err)
		}
	}
	if files := recoveryFiles(t, state); len(files) != 0 {
		t.Fatalf("saved a credential: %v", files)
	}
}

func TestRecoveryPatchPrune(t *testing.T) {
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	previousNow := diagnosticsNow
	diagnosticsNow = func() time.Time { return now }
	t.Cleanup(func() { diagnosticsNow = previousNow })
	state := useDiagnosticsState(t)
	dir := filepath.Join(state, "swarm", "recovery")
	diagnostics := filepath.Join(state, "swarm", "diagnostics")
	mustMkdir(t, dir)
	mustMkdir(t, diagnostics)
	patchName := func(i int) string { return fmt.Sprintf("rejected-%032x.patch", i) }
	// 5 expired patches and 25 recent ones.
	for i := range 30 {
		age := time.Duration(i) * time.Minute
		if i < 5 {
			age = diagnosticsMaxAge + time.Hour
		}
		for _, path := range []string{filepath.Join(dir, patchName(i)), filepath.Join(diagnostics, fmt.Sprintf("failure-%032x.log", i))} {
			mustWrite(t, path, "old")
			if err := os.Chtimes(path, now.Add(-age), now.Add(-age)); err != nil {
				t.Fatal(err)
			}
		}
	}
	mustWrite(t, filepath.Join(dir, "unrelated.txt"), "keep")
	saved, err := SaveRecoveryPatch(t.TempDir(), recoveryTestHeader, []byte(recoveryTestPatch))
	if err != nil {
		t.Fatal(err)
	}
	files := recoveryFiles(t, state)
	if len(files) != diagnosticsMaxCount {
		t.Fatalf("kept %d patches: %v", len(files), files)
	}
	kept := map[string]bool{}
	for _, name := range files {
		kept[name] = true
	}
	// The newest 19 old patches (5..23) survive with the new one.
	for i := range 30 {
		if want := i >= 5 && i < 5+diagnosticsMaxCount-1; kept[patchName(i)] != want {
			t.Fatalf("patch %d kept=%v", i, kept[patchName(i)])
		}
	}
	if !kept[filepath.Base(saved)] {
		t.Fatal("pruned the new patch")
	}
	if _, err := os.Stat(filepath.Join(dir, "unrelated.txt")); err != nil {
		t.Fatal("pruned an unrelated file")
	}
	if entries, _ := os.ReadDir(diagnostics); len(entries) != 30 {
		t.Fatalf("recovery pruning touched diagnostics: %d left", len(entries))
	}
}
