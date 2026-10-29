package packaging

import (
	"debug/buildinfo"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const module = "github.com/supernovae/swarmos"

type build struct {
	ID, Main, Binary           string
	Env, Goos, Goarch, Ldflags []string
}
type archive struct {
	ID        string
	IDs       []string `yaml:"ids"`
	Builds    []string `yaml:"builds"`
	Different bool     `yaml:"allow_different_binary_count"`
	Formats   []string
	Overrides []struct {
		Goos    string
		Formats []string
	} `yaml:"format_overrides"`
	Name  string `yaml:"name_template"`
	Files []string
}
type config struct {
	Version  int
	Project  string `yaml:"project_name"`
	Builds   []build
	Archives []archive
	Checksum struct {
		Name      string `yaml:"name_template"`
		Algorithm string
	}
}

func root(t *testing.T) string {
	t.Helper()
	p, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	return p
}
func read(t *testing.T, p string) []byte {
	t.Helper()
	b, e := os.ReadFile(p)
	if e != nil {
		t.Fatal(e)
	}
	return b
}
func load(t *testing.T) config {
	t.Helper()
	var c config
	if e := yaml.Unmarshal(read(t, filepath.Join(root(t), ".goreleaser.yaml")), &c); e != nil {
		t.Fatal(e)
	}
	return c
}
func validate(c config) error {
	want := config{Version: 2, Project: "swarm",
		Builds: []build{
			{ID: "swarm", Main: "./cmd/swarm", Binary: "swarm", Env: []string{"CGO_ENABLED=0"}, Goos: []string{"linux", "darwin", "windows"}, Goarch: []string{"amd64", "arm64"}, Ldflags: []string{"-s -w", "-X " + module + "/internal/cli.Version={{.Version}}", "-X " + module + "/internal/cli.Commit={{.Commit}}", "-X " + module + "/internal/cli.Date={{.Date}}"}},
			{ID: "swarm-restore-helper", Main: "./cmd/swarm-restore-helper", Binary: "swarm-restore-helper", Env: []string{"CGO_ENABLED=0"}, Goos: []string{"linux"}, Goarch: []string{"amd64", "arm64"}},
		}}
	if c.Version != want.Version || c.Project != want.Project || !reflect.DeepEqual(c.Builds, want.Builds) {
		return fmt.Errorf("build contract mismatch: %+v", c.Builds)
	}
	if len(c.Archives) != 1 {
		return fmt.Errorf("archive count")
	}
	a := c.Archives[0]
	if a.ID != "default" || !reflect.DeepEqual(a.IDs, []string{"swarm", "swarm-restore-helper"}) || len(a.Builds) != 0 || !a.Different || !reflect.DeepEqual(a.Formats, []string{"tar.gz"}) || a.Name != "{{ .ProjectName }}_{{ .Version }}_{{ .Os }}_{{ .Arch }}" || !reflect.DeepEqual(a.Files, []string{"README.md", "LICENSE", "docs/*"}) {
		return fmt.Errorf("archive contract mismatch")
	}
	if len(a.Overrides) != 1 || a.Overrides[0].Goos != "windows" || !reflect.DeepEqual(a.Overrides[0].Formats, []string{"zip"}) || c.Checksum.Algorithm != "sha256" || c.Checksum.Name != "{{ .ProjectName }}_{{ .Version }}_checksums.txt" {
		return fmt.Errorf("format/checksum mismatch")
	}
	return nil
}
func run(t *testing.T, dir string, env []string, name string, args ...string) string {
	t.Helper()
	c := exec.Command(name, args...)
	c.Dir = dir
	c.Env = append(os.Environ(), env...)
	b, e := c.CombinedOutput()
	if e != nil {
		t.Fatalf("%s %v: %v\n%s", name, args, e, b)
	}
	return string(b)
}
func helperInfo(t *testing.T, p, arch string) {
	t.Helper()
	b, e := buildinfo.ReadFile(p)
	if e != nil {
		t.Fatal(e)
	}
	if b.Path != module+"/cmd/swarm-restore-helper" {
		t.Fatalf("main: %s", b.Path)
	}
	settings := map[string]string{}
	for _, s := range b.Settings {
		settings[s.Key] = s.Value
	}
	for k, v := range map[string]string{"CGO_ENABLED": "0", "GOOS": "linux", "GOARCH": arch} {
		if settings[k] != v {
			t.Fatalf("%s=%q want %q", k, settings[k], v)
		}
	}
	st, e := os.Stat(p)
	if e != nil || st.Mode().Perm()&0111 == 0 {
		t.Fatalf("not executable: %s %v", p, e)
	}
}
func TestReleasePackaging(t *testing.T) {
	c := load(t)
	if e := validate(c); e != nil {
		t.Fatal(e)
	}
	for _, arch := range c.Builds[1].Goarch {
		t.Run(arch, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "helper")
			env := append([]string{}, c.Builds[1].Env...)
			env = append(env, "GOOS="+c.Builds[1].Goos[0], "GOARCH="+arch)
			run(t, root(t), env, "go", "build", "-o", p, c.Builds[1].Main)
			helperInfo(t, p, arch)
		})
	}
}
func TestReleasePackagingRejectsInvalidConfig(t *testing.T) {
	mutations := map[string]func(*config){
		"missing helper":     func(c *config) { c.Builds = c.Builds[:1] },
		"missing archive ID": func(c *config) { c.Archives[0].IDs = c.Archives[0].IDs[:1] },
		"missing cgo":        func(c *config) { c.Builds[1].Env = nil },
		"enabled cgo":        func(c *config) { c.Builds[1].Env = []string{"CGO_ENABLED=1"} },
		"wrong OS":           func(c *config) { c.Builds[1].Goos = []string{"darwin"} },
		"wrong arch":         func(c *config) { c.Builds[1].Goarch = []string{"386"} },
		"deprecated builds":  func(c *config) { c.Archives[0].Builds = c.Archives[0].IDs; c.Archives[0].IDs = nil },
	}
	for n, m := range mutations {
		t.Run(n, func(t *testing.T) {
			c := load(t)
			m(&c)
			if validate(c) == nil {
				t.Fatal("mutation accepted")
			}
		})
	}
}
func assertRecording(log, target, goos, dest string) error {
	lines := strings.Split(strings.TrimSpace(log), "\n")
	var cli, helper bool
	for _, line := range lines {
		if strings.Contains(line, "./cmd/swarm|") {
			cli = true
			if !strings.Contains(line, "cgo=1") {
				return fmt.Errorf("CLI cgo changed")
			}
		}
		if strings.Contains(line, "./cmd/swarm-restore-helper|") {
			helper = true
			if !strings.Contains(line, "cgo=0") {
				return fmt.Errorf("helper cgo not disabled")
			}
		}
		if strings.HasPrefix(line, target+"|") && !strings.Contains(line, dest) {
			return fmt.Errorf("destination lost: %s", line)
		}
	}
	if !cli || helper != (goos == "linux") {
		return fmt.Errorf("missing/unexpected invocation: %s", log)
	}
	return nil
}

// The wrapper records argument boundaries and environment; it never runs restore.
func recording(t *testing.T, makefile, target, goos string, defaults, fail bool) (string, string, error) {
	t.Helper()
	dir := t.TempDir()
	dest := filepath.Join(dir, "output with spaces")
	os.MkdirAll(dest, 0755)
	wrapper := filepath.Join(dir, "go wrapper")
	log := filepath.Join(dir, "calls")
	script := `#!/bin/sh
if [ "$1" = env ]; then
 case "$2" in GOOS) echo "$TEST_GOOS";; GOPATH) echo "$TEST_GOPATH";; esac
 exit 0
fi
printf '%s|' "$@" >> "$TEST_LOG"
printf 'cgo=%s|gobin=%s\n' "$CGO_ENABLED" "$GOBIN" >> "$TEST_LOG"
case "$*" in *./cmd/swarm-restore-helper*) [ "$FAIL_HELPER" != 1 ] || exit 42;; esac
`
	os.WriteFile(wrapper, []byte(script), 0755)
	os.WriteFile(filepath.Join(dir, "Makefile"), []byte(makefile), 0644)
	// install's final version invocation remains executable in this fixture.
	bin := dest
	if defaults {
		bin = filepath.Join(dest, "bin")
	}
	os.MkdirAll(bin, 0755)
	os.WriteFile(filepath.Join(bin, "swarm"), []byte("#!/bin/sh\nexit 0\n"), 0755)
	args := []string{target, "GO=" + wrapper}
	if !defaults {
		args = append(args, "BIN_DIR="+dest, "GOBIN="+dest)
	}
	c := exec.Command("make", args...)
	c.Dir = dir
	c.Env = append(os.Environ(), "CGO_ENABLED=1", "TEST_GOOS="+goos, "TEST_GOPATH="+dest, "TEST_LOG="+log, fmt.Sprintf("FAIL_HELPER=%d", map[bool]int{true: 1}[fail]))
	out, e := c.CombinedOutput()
	if e != nil {
		return "", dest, fmt.Errorf("%w: %s", e, out)
	}
	b := read(t, log)
	if defaults {
		if target == "install" {
			dest = bin
		} else {
			dest = "bin/"
		}
	}
	return string(b), dest, nil
}
func TestMakePackaging(t *testing.T) {
	source := string(read(t, filepath.Join(root(t), "Makefile")))
	for _, target := range []string{"build", "install"} {
		for _, goos := range []string{"linux", "darwin", "windows"} {
			for _, defaults := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/default=%v", target, goos, defaults), func(t *testing.T) {
					log, dest, e := recording(t, source, target, goos, defaults, false)
					if e != nil {
						t.Fatal(e)
					}
					if e = assertRecording(log, target, goos, dest); e != nil {
						t.Fatal(e)
					}
				})
			}
		}
		t.Run(target+"/failure", func(t *testing.T) {
			if _, _, e := recording(t, source, target, "linux", false, true); e == nil {
				t.Fatal("helper failure swallowed")
			}
		})
		for _, mutation := range []string{"omit", "remove cgo", "enable cgo"} {
			t.Run(target+"/"+mutation, func(t *testing.T) {
				lines := strings.Split(source, "\n")
				inTarget := false
				for i, line := range lines {
					if line == target+":" {
						inTarget = true
						continue
					}
					if inTarget && line != "" && !strings.HasPrefix(line, "\t") {
						inTarget = false
					}
					if inTarget && strings.Contains(line, "./cmd/swarm-restore-helper") {
						switch mutation {
						case "omit":
							lines[i] = ""
						case "remove cgo":
							lines[i] = strings.Replace(line, "CGO_ENABLED=0 ", "", 1)
						case "enable cgo":
							lines[i] = strings.Replace(line, "CGO_ENABLED=0", "CGO_ENABLED=1", 1)
						}
					}
				}
				log, dest, e := recording(t, strings.Join(lines, "\n"), target, "linux", false, false)
				if e != nil {
					t.Fatalf("incidental command failure: %v", e)
				}
				if assertRecording(log, target, "linux", dest) == nil {
					t.Fatal("mutation accepted")
				}
			})
		}
	}
	t.Run("real Linux artifacts", func(t *testing.T) {
		if runtime.GOOS != "linux" {
			t.Skip("native Linux artifact check")
		}
		control := filepath.Join(t.TempDir(), "control")
		run(t, root(t), nil, "go", "build", "-o", control, "./cmd/swarm")
		want, e := buildinfo.ReadFile(control)
		if e != nil {
			t.Fatal(e)
		}
		for _, target := range []string{"build", "install"} {
			dest := filepath.Join(t.TempDir(), "real output")
			run(t, root(t), nil, "make", target, "BIN_DIR="+dest, "GOBIN="+dest)
			helperInfo(t, filepath.Join(dest, "swarm-restore-helper"), runtime.GOARCH)
			cli := filepath.Join(dest, "swarm")
			got, e := buildinfo.ReadFile(cli)
			if e != nil {
				t.Fatal(e)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("CLI metadata differs from same-checkout control:\ngot %v\nwant %v", got, want)
			}
			run(t, root(t), nil, cli, "version")
		}
	})
}
