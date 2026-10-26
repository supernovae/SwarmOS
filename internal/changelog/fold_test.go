package changelog

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const history = "## old\n\n- History.  \n\n## older\n\nLegacy\n"
const baseline = "# Changelog\n\n## Unreleased\n\n" + history

func put(t *testing.T, root, name, text string) {
	t.Helper()
	p := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(text), 0644); err != nil {
		t.Fatal(err)
	}
}
func read(t *testing.T, root, name string) string {
	t.Helper()
	b, e := os.ReadFile(filepath.Join(root, name))
	if e != nil {
		t.Fatal(e)
	}
	return string(b)
}
func fixture(t *testing.T) string {
	t.Helper()
	r := t.TempDir()
	put(t, r, "CHANGELOG.md", baseline)
	return r
}
func TestFoldOrdering(t *testing.T) {
	r := fixture(t)
	for _, n := range []string{"20", "03", "10"} {
		put(t, r, "changes/unreleased/"+n+".md", "- "+n+"\n")
	}
	put(t, r, "changes/unreleased/keep.txt", "keep")
	if e := Fold(r, "1.2.3"); e != nil {
		t.Fatal(e)
	}
	want := "# Changelog\n\n## Unreleased\n\n## 1.2.3\n\n- 03\n\n- 10\n\n- 20\n\n" + history
	if got := read(t, r, "CHANGELOG.md"); got != want {
		t.Fatalf("got %q want %q", got, want)
	}
	for _, n := range []string{"20", "03", "10"} {
		if _, e := os.Stat(filepath.Join(r, "changes/unreleased/"+n+".md")); !os.IsNotExist(e) {
			t.Fatal(e)
		}
	}
	if read(t, r, "changes/unreleased/keep.txt") != "keep" {
		t.Fatal("unrelated file changed")
	}
	assertNoop(t, r)
}
func assertNoop(t *testing.T, r string) {
	t.Helper()
	before := read(t, r, "CHANGELOG.md")
	a, _ := os.Stat(filepath.Join(r, "CHANGELOG.md"))
	if e := Fold(r, "1.2.3"); e != nil {
		t.Fatal(e)
	}
	b, _ := os.Stat(filepath.Join(r, "CHANGELOG.md"))
	if before != read(t, r, "CHANGELOG.md") || !a.ModTime().Equal(b.ModTime()) {
		t.Fatal("no-op changed changelog")
	}
}
func TestFoldNoFragments(t *testing.T) {
	r := fixture(t)
	assertNoop(t, r)
	if e := os.MkdirAll(filepath.Join(r, "changes/unreleased"), 0755); e != nil {
		t.Fatal(e)
	}
	assertNoop(t, r)
}
func TestFoldPreservesReleasedHistory(t *testing.T) {
	r := fixture(t)
	put(t, r, "changes/unreleased/1.md", "- New\n")
	if e := Fold(r, "v1"); e != nil {
		t.Fatal(e)
	}
	if !strings.HasSuffix(read(t, r, "CHANGELOG.md"), history) {
		t.Fatal("history changed")
	}
}
func TestFoldRejectsInvalidFragments(t *testing.T) {
	for _, s := range []string{"", " \n", "- one\n- two", "- one\n  - two", "- one\n  * two", "- one\n  + two", "- one\n  1. two", "paragraph", "- one\nparagraph", "- one\n  # heading"} {
		t.Run(s, func(t *testing.T) {
			r := fixture(t)
			put(t, r, "changes/unreleased/01.md", "- Valid")
			put(t, r, "changes/unreleased/02.md", s)
			if e := Fold(r, "1"); e == nil || !strings.Contains(e.Error(), "02.md") {
				t.Fatalf("error %v", e)
			}
			if read(t, r, "CHANGELOG.md") != baseline || read(t, r, "changes/unreleased/02.md") != s || read(t, r, "changes/unreleased/01.md") != "- Valid" {
				t.Fatal("mutated on rejection")
			}
		})
	}
}
func TestFoldFragmentFormat(t *testing.T) {
	for _, s := range []string{"- Plain", "\n- **Unicode** café\n  continued prose\n", "- Text\n\n\tMore text"} {
		if _, e := fragment(s); e != nil {
			t.Fatal(e)
		}
	}
}
func TestFoldInvalidVersionAndAnchor(t *testing.T) {
	for _, v := range []string{"", " ", "1\n2", "#bad", "1 2"} {
		r := fixture(t)
		put(t, r, "changes/unreleased/1.md", "- New")
		if e := Fold(r, v); e == nil {
			t.Fatalf("accepted %q", v)
		}
		if read(t, r, "CHANGELOG.md") != baseline {
			t.Fatal("changed")
		}
	}
	for _, s := range []string{"# No anchor\n", baseline + "## Unreleased\n", strings.Replace(baseline, "## Unreleased\n", "## Unreleased\n\n- old\n", 1)} {
		r := fixture(t)
		put(t, r, "CHANGELOG.md", s)
		put(t, r, "changes/unreleased/1.md", "- New")
		if e := Fold(r, "1"); e == nil {
			t.Fatal("accepted invalid anchor")
		}
		if read(t, r, "CHANGELOG.md") != s {
			t.Fatal("changed")
		}
	}
}
func TestFoldRejectsUnsafeFiles(t *testing.T) {
	for _, name := range []string{"CHANGELOG.md", "changes/unreleased/1.md"} {
		t.Run(name, func(t *testing.T) {
			r := fixture(t)
			put(t, r, "changes/unreleased/1.md", "- New")
			target := filepath.Join(t.TempDir(), "target")
			put(t, filepath.Dir(target), "target", "- Outside")
			if e := os.Remove(filepath.Join(r, name)); e != nil {
				t.Fatal(e)
			}
			if e := os.Symlink(target, filepath.Join(r, name)); e != nil {
				t.Skip(e)
			}
			if e := Fold(r, "1"); e == nil {
				t.Fatal("accepted symlink")
			}
			if read(t, filepath.Dir(target), "target") != "- Outside" {
				t.Fatal("outside changed")
			}
		})
	}
	r := fixture(t)
	if e := os.MkdirAll(filepath.Join(r, "changes/unreleased/directory.md"), 0755); e != nil {
		t.Fatal(e)
	}
	if e := Fold(r, "1"); e == nil {
		t.Fatal("accepted directory")
	}
}
func TestMakeChangelog(t *testing.T) {
	if _, e := exec.LookPath("make"); e != nil {
		t.Skip(e)
	}
	r := fixture(t)
	binary := filepath.Join(t.TempDir(), "changelog")
	cmd := exec.Command("go", "build", "-o", binary, "../../cmd/changelog")
	if out, e := cmd.CombinedOutput(); e != nil {
		t.Fatalf("%v: %s", e, out)
	}
	mf, e := os.ReadFile("../../Makefile")
	if e != nil {
		t.Fatal(e)
	}
	put(t, r, "Makefile", string(mf))
	put(t, r, "changes/unreleased/1.md", "- New")
	wrapper := filepath.Join(t.TempDir(), "go-wrapper")
	put(t, filepath.Dir(wrapper), filepath.Base(wrapper), "#!/bin/sh\nexec '"+strings.ReplaceAll(binary, "'", "'\\''")+"' \"$3\"\n")
	if e := os.Chmod(wrapper, 0755); e != nil {
		t.Fatal(e)
	}
	run := func(version string) ([]byte, error) {
		c := exec.Command("make", "changelog", "VERSION="+version, "GO="+wrapper)
		c.Dir = r
		return c.CombinedOutput()
	}
	if out, e := run("1.2.3"); e != nil {
		t.Fatalf("%v: %s", e, out)
	}
	if !strings.Contains(read(t, r, "CHANGELOG.md"), "## 1.2.3\n") {
		t.Fatal("missing release")
	}
	before := read(t, r, "CHANGELOG.md")
	if out, e := run("1.2.3"); e != nil {
		t.Fatalf("%v: %s", e, out)
	}
	if read(t, r, "CHANGELOG.md") != before {
		t.Fatal("second run changed")
	}
	put(t, r, "changes/unreleased/bad.md", "")
	if out, e := run("2"); e == nil || !strings.Contains(string(out), "bad.md") {
		t.Fatalf("%v: %s", e, out)
	}
}
func TestFragmentBranchesMerge(t *testing.T) {
	if _, e := exec.LookPath("git"); e != nil {
		t.Skip(e)
	}
	r := fixture(t)
	git := func(args ...string) string {
		t.Helper()
		c := exec.Command("git", args...)
		c.Dir = r
		c.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_ATTR_NOSYSTEM=1", "GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=core.attributesFile", "GIT_CONFIG_VALUE_0=/dev/null")
		out, e := c.CombinedOutput()
		if e != nil {
			t.Fatalf("git %v: %v: %s", args, e, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init")
	git("config", "user.name", "Test")
	git("config", "user.email", "test@example.invalid")
	git("add", ".")
	git("commit", "-m", "base")
	base := git("rev-parse", "HEAD")
	git("checkout", "-b", "a")
	put(t, r, "changes/unreleased/101.md", "- A\n")
	git("add", ".")
	git("commit", "-m", "A")
	git("checkout", "-b", "b", base)
	put(t, r, "changes/unreleased/102.md", "- B\n")
	git("add", ".")
	git("commit", "-m", "B")
	tree := git("merge-tree", "--write-tree", "a", "b")
	for n, w := range map[string]string{"101": "- A", "102": "- B"} {
		if git("show", tree+":changes/unreleased/"+n+".md") != w {
			t.Fatal("missing fragment")
		}
	}
	if git("show", tree+":CHANGELOG.md") != strings.TrimSpace(baseline) {
		t.Fatal("changelog changed")
	}
}

func TestFoldRejectsSpecialFile(t *testing.T) {
	mkfifo, err := exec.LookPath("mkfifo")
	if err != nil {
		t.Skip("mkfifo unavailable")
	}
	for _, name := range []string{"CHANGELOG.md", "changes/unreleased/1.md"} {
		t.Run(name, func(t *testing.T) {
			r := fixture(t)
			put(t, r, "changes/unreleased/1.md", "- New")
			if err := os.Remove(filepath.Join(r, name)); err != nil {
				t.Fatal(err)
			}
			if out, err := exec.Command(mkfifo, filepath.Join(r, name)).CombinedOutput(); err != nil {
				t.Fatalf("mkfifo: %v: %s", err, out)
			}
			if err := Fold(r, "1"); err == nil {
				t.Fatal("accepted FIFO")
			}
			info, err := os.Lstat(filepath.Join(r, name))
			if err != nil || info.Mode()&os.ModeNamedPipe == 0 {
				t.Fatal("FIFO was changed")
			}
		})
	}
}
