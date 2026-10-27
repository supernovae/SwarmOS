package sandbox

import (
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// TestFinishGoToolchainInReadOnlyLayout guards the read-only cache
// regression: go mod download extracts golang.org/toolchain read-only, without
// execute bits and with go.mod files still named _go.mod. The go command
// finishes that on its first switch, which fails once the per-repository
// module cache is mounted read-only in the sandbox.
func TestFinishGoToolchainInReadOnlyLayout(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("execute bits are not used on Windows")
	}
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "toolchain@v0.0.1-go1.99.0.linux-amd64")
	files := map[string]string{
		"bin/go": "go", "bin/gofmt": "gofmt", "pkg/tool/linux_amd64/asm": "asm",
		"lib/wasm/go_js_wasm_exec": "exec", "lib/time/zoneinfo.zip": "zip",
		"src/_go.mod": "module std\n", "src/cmd/_go.mod": "module cmd\n", "src/fmt/print.go": "package fmt\n",
	}
	for name, body := range files {
		path := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o444); err != nil {
			t.Fatal(err)
		}
	}
	// Match the module cache: every extracted directory is read-only.
	var dirs []string
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			dirs = append(dirs, path)
		}
		return err
	})
	for i := len(dirs) - 1; i >= 0; i-- {
		if err := os.Chmod(dirs[i], 0o555); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		for _, d := range dirs {
			_ = os.Chmod(d, 0o755)
		}
	})
	for round := 0; round < 2; round++ { // the second round must be a no-op
		if err := finishGoToolchain(dir); err != nil {
			t.Fatalf("round %d: %v", round, err)
		}
	}
	for name, wantExec := range map[string]bool{"bin/go": true, "bin/gofmt": true, "pkg/tool/linux_amd64/asm": true, "lib/wasm/go_js_wasm_exec": true, "lib/time/zoneinfo.zip": false, "src/fmt/print.go": false} {
		info, err := os.Stat(filepath.Join(dir, filepath.FromSlash(name)))
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode()&0o111 == 0o111; got != wantExec {
			t.Fatalf("%s executable=%v, want %v (mode %v)", name, got, wantExec, info.Mode())
		}
	}
	for _, sub := range []string{"src", "src/cmd"} {
		got, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(sub), "go.mod"))
		if err != nil || string(got) != files[sub+"/_go.mod"] {
			t.Fatalf("%s/go.mod = %q, %v", sub, got, err)
		}
		if info, _ := os.Stat(filepath.Join(dir, filepath.FromSlash(sub))); info.Mode().Perm() != 0o555 {
			t.Fatalf("%s mode restored to %v, want 0555", sub, info.Mode().Perm())
		}
	}
	if err := finishGoToolchain(filepath.Join(t.TempDir(), "missing")); err != nil {
		t.Fatalf("missing toolchain dir: %v", err)
	}
}
