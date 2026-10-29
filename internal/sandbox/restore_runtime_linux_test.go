//go:build linux

package sandbox

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"
)

type runtimeFixture struct {
	t    *testing.T
	dir  string
	fs   *restoreRuntimeFS
	tool string
}

func newRuntimeFixture(t *testing.T, tool string) *runtimeFixture {
	t.Helper()
	dir := t.TempDir()
	if e := os.Chmod(dir, 0755); e != nil {
		t.Fatal(e)
	}
	root, e := os.OpenRoot(dir)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { root.Close() })
	i, e := root.Stat(".")
	if e != nil {
		t.Fatal(e)
	}
	f := &runtimeFixture{t, dir, &restoreRuntimeFS{root: root, uid: i.Sys().(*syscall.Stat_t).Uid}, tool}
	launcher := "mvn"
	if tool == "gradle" {
		launcher = "gradle"
	}
	f.file("/usr/share/"+tool+"/bin/"+launcher, "#!/bin/sh\nexit 0\n", 0755)
	if tool == "maven" {
		f.link("/usr/bin/mvn", "/etc/alternatives/mvn")
		f.link("/etc/alternatives/mvn", "/usr/share/maven/bin/mvn")
		f.file("/usr/share/maven/boot/plexus-classworlds-2.x.jar", "boot", 0644)
		f.link("/usr/share/maven/bin/m2.conf", "/etc/maven/m2.conf")
		f.file("/etc/maven/m2.conf", "HOST_SECRET", 0644)
	} else {
		f.link("/usr/bin/gradle", "../share/gradle/bin/gradle")
		f.file("/usr/share/gradle/lib/gradle-launcher-4.4.1.jar", "boot", 0644)
		f.link("/usr/share/gradle/lib/plugins/plugin.jar", "/usr/share/java/plugin.jar")
		f.file("/usr/share/java/plugin.jar", "plugin", 0644)
	}
	f.link("/usr/share/"+tool+"/lib/library.jar", "../../java/library.jar")
	f.link("/usr/share/java/library.jar", "library-1.jar")
	f.file("/usr/share/java/library-1.jar", "library", 0644)
	f.file("/usr/share/java/unrelated.jar", "HOST_SECRET", 0644)
	f.link("/usr/bin/java", "/etc/alternatives/java")
	f.link("/etc/alternatives/java", runtimeJDK+"/bin/java")
	f.file(runtimeJDK+"/bin/java", "#!/bin/sh\nexit 0\n", 0755)
	for _, pair := range runtimeMetadata {
		f.link(runtimeJDK+pair[0], runtimeEtc+pair[1])
		data := "public:" + pair[1]
		if strings.HasSuffix(pair[1], ".cfg") {
			data = "-server KNOWN\n# public VM\n"
		}
		if strings.HasSuffix(pair[1], "java.security") {
			data = "# include example\nsecurity.provider.1=SUN\njdk.tls.disabledAlgorithms=SSLv3, \\\n RC4\n"
		}
		f.file(runtimeEtc+pair[1], data, 0644)
	}
	for _, name := range []string{"net.properties", "logging.properties"} {
		f.link(runtimeJDK+"/conf/"+name, runtimeEtc+"/"+name)
		f.file(runtimeEtc+"/"+name, "HOST_SECRET", 0644)
	}
	for _, p := range []string{runtimeEtc + "/security/policy/unlimited/sibling.policy", runtimeEtc + "/security/policy/limited/default_local.policy", runtimeEtc + "/security/nss.cfg", runtimeEtc + "/management/jmxremote.password", "/etc/ssl/certs/java/cacerts", "/usr/share/maven/conf/settings.xml", "/usr/share/maven/conf/toolchains.xml", "/usr/share/gradle/gradle.properties", "/usr/share/gradle/init.d/private.gradle", "/home/user/.ssh/id_rsa"} {
		f.file(p, "HOST_SECRET", 0644)
	}
	return f
}
func (f *runtimeFixture) host(p string) string { return filepath.Join(f.dir, p) }
func (f *runtimeFixture) file(p, data string, mode os.FileMode) {
	f.t.Helper()
	if e := os.MkdirAll(filepath.Dir(f.host(p)), 0755); e != nil {
		f.t.Fatal(e)
	}
	f.fixParents(p)
	if e := os.WriteFile(f.host(p), []byte(data), mode); e != nil {
		f.t.Fatal(e)
	}
	if e := os.Chmod(f.host(p), mode); e != nil {
		f.t.Fatal(e)
	}
}
func (f *runtimeFixture) link(p, target string) {
	f.t.Helper()
	if e := os.MkdirAll(filepath.Dir(f.host(p)), 0755); e != nil {
		f.t.Fatal(e)
	}
	f.fixParents(p)
	if e := os.Symlink(target, f.host(p)); e != nil {
		f.t.Fatal(e)
	}
}
func (f *runtimeFixture) fixParents(p string) {
	for d := filepath.Dir(f.host(p)); strings.HasPrefix(d, f.dir) && d != f.dir; d = filepath.Dir(d) {
		if e := os.Chmod(d, 0755); e != nil {
			f.t.Fatal(e)
		}
	}
}
func (f *runtimeFixture) replaceLink(p, target string) {
	f.t.Helper()
	if e := os.Remove(f.host(p)); e != nil {
		f.t.Fatal(e)
	}
	f.link(p, target)
}
func (f *runtimeFixture) argv() []string {
	if f.tool == "maven" {
		return []string{"/usr/bin/mvn"}
	}
	return []string{"/usr/bin/gradle"}
}
func (f *runtimeFixture) resolve() *restoreRuntime {
	f.t.Helper()
	r, e := resolveRestoreRuntime(f.fs, f.argv())
	if e != nil {
		f.t.Fatal(e)
	}
	return r
}
func runtimeDestinations(r *restoreRuntime) []string {
	var out []string
	for _, v := range r.inputs {
		out = append(out, v.destination)
	}
	return out
}
func runtimeGolden(tool string) []string {
	out := []string{runtimeJDK + "/bin/java", "/usr/share/" + tool + "/lib/library.jar"}
	for _, p := range runtimeMetadata {
		out = append(out, runtimeEtc+p[1])
	}
	if tool == "maven" {
		out = append(out, "/usr/share/maven/bin/mvn", "/usr/share/maven/bin/m2.conf", "/usr/share/maven/boot/plexus-classworlds-2.x.jar")
	} else {
		out = append(out, "/usr/share/gradle/bin/gradle", "/usr/share/gradle/lib/gradle-launcher-4.4.1.jar", "/usr/share/gradle/lib/plugins/plugin.jar")
	}
	sort.Strings(out)
	return out
}
func TestRestoreRuntimeLayouts(t *testing.T) {
	for _, tool := range []string{"maven", "gradle"} {
		t.Run(tool, func(t *testing.T) {
			f := newRuntimeFixture(t, tool)
			r := f.resolve()
			if !reflect.DeepEqual(runtimeDestinations(r), runtimeGolden(tool)) {
				t.Fatal(runtimeDestinations(r))
			}
			control := t.TempDir()
			mounts, e := stageRestoreRuntime(control, r)
			if e != nil {
				t.Fatal(e)
			}
			for n, m := range mounts {
				if !strings.HasPrefix(m.source, control+"/runtime/") || m.destination != r.inputs[n].destination {
					t.Fatal(m)
				}
				data, e := os.ReadFile(m.source)
				if e != nil || !bytes.Equal(data, r.inputs[n].data) || bytes.Contains(data, []byte("HOST_SECRET")) {
					t.Fatal(m, e)
				}
				i, e := os.Stat(m.source)
				if e != nil || i.Mode().Perm()&0222 != 0 {
					t.Fatal("writable mount", e)
				}
			}
			if tool == "maven" {
				data, e := os.ReadFile(filepath.Join(control, "runtime/usr/share/maven/bin/m2.conf"))
				if e != nil || string(data) != runtimeClassworlds {
					t.Fatal("classworlds", e)
				}
			}
		})
	}
	for _, names := range [][]string{{"maven-resolver-transport-http.jar"}, {"plexus-xml.jar"}, {"maven-resolver-transport-http.jar", "plexus-xml.jar"}} {
		t.Run(strings.Join(names, "+"), func(t *testing.T) {
			f := newRuntimeFixture(t, "maven")
			for _, n := range names {
				f.link("/usr/share/maven/lib/"+n, "../../java/"+n)
			}
			if _, e := resolveRestoreRuntime(f.fs, f.argv()); e == nil || !strings.Contains(e.Error(), "install a supported system layout") {
				t.Fatal(e)
			}
			for _, n := range names {
				f.file("/usr/share/java/"+n, "installed", 0644)
			}
			f.resolve()
		})
	}
	for _, argv := range [][]string{nil, {"/bin/true"}, {"/source/mvn"}, {"mvn"}} {
		r, e := resolveRestoreRuntime(nil, argv)
		if e != nil || r != nil {
			t.Fatal("generic runtime discovery", e)
		}
	}
}
func TestRestoreRuntimePublicMetadata(t *testing.T) {
	for _, pair := range runtimeMetadata {
		t.Run(pair[1], func(t *testing.T) {
			for _, bad := range []string{"missing-link", "terminal-J", "missing-target", "sibling", "extra-hop", "directory", "fifo", "writable", "owner", "ancestor", "swap", "limited", "prefix", "relative-escape", "escape-return"} {
				t.Run(bad, func(t *testing.T) {
					f := newRuntimeFixture(t, "maven")
					j, e := runtimeJDK+pair[0], runtimeEtc+pair[1]
					switch bad {
					case "missing-link":
						os.Remove(f.host(j))
					case "terminal-J":
						os.Remove(f.host(j))
						f.file(j, "public", 0644)
					case "missing-target":
						os.Remove(f.host(e))
					case "sibling":
						f.replaceLink(j, e+".sibling")
						f.file(e+".sibling", "HOST_SECRET", 0644)
					case "extra-hop":
						os.Remove(f.host(e))
						f.link(e, e+".sibling")
						f.file(e+".sibling", "HOST_SECRET", 0644)
					case "directory":
						os.Remove(f.host(e))
						os.Mkdir(f.host(e), 0755)
					case "fifo":
						os.Remove(f.host(e))
						if err := syscall.Mkfifo(f.host(e), 0600); err != nil {
							t.Fatal(err)
						}
					case "writable":
						os.Chmod(f.host(e), 0666)
					case "owner":
						f.fs.uid++
					case "ancestor":
						os.Chmod(filepath.Dir(f.host(e)), 0777)
					case "swap":
						f.replaceLink(j, runtimeEtc+"/security/policy/unlimited/default_local.policy")
						if pair[1] == "/security/policy/unlimited/default_local.policy" {
							f.replaceLink(j, runtimeEtc+"/security/policy/unlimited/default_US_export.policy")
						}
					case "limited":
						f.replaceLink(j, runtimeEtc+"/security/policy/limited/"+path.Base(e))
					case "prefix":
						f.replaceLink(j, runtimeEtc+"-other"+pair[1])
					case "relative-escape":
						f.replaceLink(j, "../../../../../../home/public")
					case "escape-return":
						os.Remove(f.host(e))
						f.link(e, "/home/return")
						f.link("/home/return", e)
					}
					if _, err := resolveRestoreRuntime(f.fs, f.argv()); err == nil || strings.Contains(err.Error(), "HOST_SECRET") {
						t.Fatal("unsafe metadata", err)
					}
				})
			}
		})
	}
	for _, data := range []string{"# comment\rinclude /etc/secret", "# comment\rsecurity.provider.1=External", "include /etc/secret", "incl\\u0075de=/etc/secret", "in\\\n clude=/etc/secret", "security.provider.1=External", "security.provider.1=SUN /etc/key", "security.provider.1=SunPKCS11 /etc/nss.cfg", "security.provider.1=\\u0053UN /etc/key"} {
		if e := validateRuntimeSecurity([]byte(data)); e == nil {
			t.Fatalf("accepted %q", data)
		}
	}
	for _, data := range []string{"# include /etc/secret\nsecurity.provider.1=SUN\n", "! include=/etc/secret\nsecurity.provider.2=SunPKCS11\n", "jdk.tls.disabledAlgorithms=SSLv3, \\\n RC4\n"} {
		if e := validateRuntimeSecurity([]byte(data)); e != nil {
			t.Fatal(e)
		}
	}
}
func TestRestoreRuntimeExcludedHostDefaults(t *testing.T) {
	for _, tool := range []string{"maven", "gradle"} {
		for _, kind := range []string{"missing", "ordinary", "credential", "link", "fifo"} {
			t.Run(tool+"/"+kind, func(t *testing.T) {
				f := newRuntimeFixture(t, tool)
				for _, n := range []string{"net.properties", "logging.properties"} {
					p := runtimeEtc + "/" + n
					os.Remove(f.host(p))
					switch kind {
					case "ordinary":
						f.file(p, "public looking", 0644)
					case "credential":
						f.file(p, "http://user:HOST_SECRET@proxy.invalid custom.handler", 0644)
					case "link":
						f.link(p, "/home/private-key")
					case "fifo":
						if e := syscall.Mkfifo(f.host(p), 0600); e != nil {
							t.Fatal(e)
						}
					}
				}
				f.fs.hook = func(p string) {
					if strings.HasSuffix(p, "net.properties") || strings.HasSuffix(p, "logging.properties") {
						t.Fatal("excluded file opened")
					}
				}
				r := f.resolve()
				if !reflect.DeepEqual(runtimeDestinations(r), runtimeGolden(tool)) {
					t.Fatal(runtimeDestinations(r))
				}
				for _, v := range r.inputs {
					if bytes.Contains(v.data, []byte("HOST_SECRET")) {
						t.Fatal("imported defaults")
					}
				}
			})
		}
	}
}
func TestRestoreRuntimeRejectsUnsafeInputs(t *testing.T) {
	for _, target := range []string{"/etc/settings.xml", "/root/key.jar", "/home/user/a.jar", "/source/a.jar", "/cache/a.jar", "/tmp/a.jar", "/usr/share/java-lookalike/a.jar", "/usr/share/gradle/lib/a.jar", "/usr/share/java/settings.xml", "/usr/share/java/private.key"} {
		t.Run(target, func(t *testing.T) {
			f := newRuntimeFixture(t, "maven")
			f.replaceLink("/usr/share/maven/lib/library.jar", target)
			f.file(target, "HOST_SECRET", 0644)
			if _, e := resolveRestoreRuntime(f.fs, f.argv()); e == nil || strings.Contains(e.Error(), "HOST_SECRET") {
				t.Fatal(e)
			}
		})
	}
	for _, kind := range []string{"directory-link", "fifo", "directory", "writable", "privileged", "loop", "missing-launcher", "missing-java", "missing-boot", "ambiguous-boot", "missing-plugins", "cross-return"} {
		t.Run(kind, func(t *testing.T) {
			tool := "maven"
			if kind == "missing-plugins" {
				tool = "gradle"
			}
			f := newRuntimeFixture(t, tool)
			p := "/usr/share/" + tool + "/lib/library.jar"
			switch kind {
			case "directory-link":
				os.RemoveAll(f.host("/usr/share/maven/lib"))
				f.link("/usr/share/maven/lib", "/usr/share/java")
			case "fifo":
				os.Remove(f.host(p))
				syscall.Mkfifo(f.host(p), 0600)
			case "directory":
				os.Remove(f.host(p))
				os.Mkdir(f.host(p), 0755)
			case "writable":
				os.Chmod(f.host("/usr/share/java/library-1.jar"), 0666)
			case "privileged":
				os.Chmod(f.host("/usr/share/java/library-1.jar"), os.ModeSetuid|0755)
			case "loop":
				f.replaceLink("/usr/share/java/library.jar", "library.jar")
			case "missing-launcher":
				os.Remove(f.host("/usr/share/maven/bin/mvn"))
			case "missing-java":
				os.Remove(f.host(runtimeJDK + "/bin/java"))
			case "missing-boot":
				os.Remove(f.host("/usr/share/maven/boot/plexus-classworlds-2.x.jar"))
			case "ambiguous-boot":
				f.file("/usr/share/maven/boot/plexus-classworlds-3.jar", "boot", 0644)
			case "missing-plugins":
				os.Remove(f.host("/usr/share/gradle/lib/plugins/plugin.jar"))
			case "cross-return":
				f.replaceLink("/usr/share/java/library.jar", "/usr/share/maven/lib/library.jar")
			}
			if _, e := resolveRestoreRuntime(f.fs, f.argv()); e == nil {
				t.Fatal("accepted", kind)
			}
		})
	}
}
func TestRestoreRuntimeSnapshot(t *testing.T) {
	for _, kind := range []string{"mutation", "terminal-replacement", "ancestor-replacement", "link-replacement"} {
		t.Run(kind, func(t *testing.T) {
			f := newRuntimeFixture(t, "maven")
			triggered := false
			f.fs.hook = func(p string) {
				if triggered || p != "/usr/share/java/library-1.jar" {
					return
				}
				triggered = true
				switch kind {
				case "mutation":
					f.file(p, "MUTATED", 0644)
				case "terminal-replacement":
					os.Rename(f.host(p), f.host(p+".old"))
					f.file(p, "MUTATED", 0644)
				case "ancestor-replacement":
					d := "/usr/share/java"
					os.Rename(f.host(d), f.host(d+".old"))
					f.file(p, "MUTATED", 0644)
				case "link-replacement":
					f.replaceLink("/usr/share/java/library.jar", "other.jar")
				}
			}
			if _, e := resolveRestoreRuntime(f.fs, f.argv()); e == nil {
				t.Fatal("concurrent change accepted")
			}
			if !triggered {
				t.Fatal("hook unused")
			}
		})
	}
	f := newRuntimeFixture(t, "maven")
	r := f.resolve()
	control := t.TempDir()
	mounts, e := stageRestoreRuntime(control, r)
	if e != nil {
		t.Fatal(e)
	}
	f.file("/usr/share/java/library-1.jar", "changed after staging", 0644)
	for _, m := range mounts {
		if strings.HasSuffix(m.destination, "/lib/library.jar") {
			data, e := os.ReadFile(m.source)
			if e != nil || string(data) != "library" {
				t.Fatal("snapshot changed", e)
			}
		}
	}
	f = newRuntimeFixture(t, "maven")
	r = f.resolve()
	f.replaceLink(runtimeJDK+runtimeMetadata[4][0], "/home/policy")
	control = t.TempDir()
	if _, e = stageRestoreRuntime(control, r); e == nil {
		t.Fatal("changed input staged")
	}
	if _, e = os.Stat(filepath.Join(control, "runtime")); !os.IsNotExist(e) {
		t.Fatal("failed staging survived", e)
	}
}
func TestRestoreRuntimeLimits(t *testing.T) {
	for _, count := range []int{512, 513} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			f := newRuntimeFixture(t, "maven")
			for i := 1; i < count; i++ {
				f.file(fmt.Sprintf("/usr/share/maven/lib/ignored-%03d", i), "", 0644)
			}
			_, e := resolveRestoreRuntime(f.fs, f.argv())
			if (e != nil) != (count > 512) {
				t.Fatal(e)
			}
		})
	}
	for _, hops := range []int{8, 9} {
		t.Run(fmt.Sprintf("hops-%d", hops), func(t *testing.T) {
			f := newRuntimeFixture(t, "maven")
			f.replaceLink("/usr/share/maven/lib/library.jar", "../../java/h0.jar")
			for i := 0; i < hops-1; i++ {
				f.link(fmt.Sprintf("/usr/share/java/h%d.jar", i), fmt.Sprintf("h%d.jar", i+1))
			}
			f.file(fmt.Sprintf("/usr/share/java/h%d.jar", hops-1), "end", 0644)
			_, e := resolveRestoreRuntime(f.fs, f.argv())
			if (e != nil) != (hops > 8) {
				t.Fatal(e)
			}
		})
	}
	for _, size := range []int64{64 << 20, (64 << 20) + 1} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			f := newRuntimeFixture(t, "maven")
			p := f.host("/usr/share/java/library-1.jar")
			if e := os.Truncate(p, size); e != nil {
				t.Fatal(e)
			}
			_, e := resolveRestoreRuntime(f.fs, f.argv())
			if (e != nil) != (size > 64<<20) {
				t.Fatal(e)
			}
		})
	}
	for _, p := range []string{"/" + strings.Repeat("a", 4096), "/" + strings.Repeat("a/", 32) + "a", "/usr/../etc/a", "relative"} {
		if runtimePathOK(p) {
			t.Fatal("invalid path accepted", p)
		}
	}
	for _, name := range []string{".hidden.jar", "bad name.jar", "bad\nname.jar", "a.jar/key", "a.jar\x00"} {
		if runtimeJarName.MatchString(name) {
			t.Fatal("bad name", name)
		}
	}
}
func TestToolExecutorRejectsRestoreRuntimeWidening(t *testing.T) {
	t.Setenv("JAVA_HOME", "/source/jdk")
	t.Setenv("MAVEN_HOME", "/source/maven")
	t.Setenv("GRADLE_HOME", "/source/gradle")
	flags, e := executorFlags("/work", true, nil, Dependencies{}, "mvn", nil)
	if e != nil {
		t.Fatal(e)
	}
	joined := strings.Join(flags, "\n")
	for _, p := range []string{"/etc/alternatives", "/etc/java-25-openjdk", "/restore-trust", "/source/jdk", "/source/maven", "/source/gradle", "/runtime/"} {
		if strings.Contains(joined, p) {
			t.Fatal("ordinary executor widened", p)
		}
	}
	flags, e = restoreFlags("/control", "/cache", "127.0.0.1:1", []string{"/bin/true"})
	if e != nil {
		t.Fatal(e)
	}
	joined = strings.Join(flags, "\n")
	if strings.Contains(joined, "/usr/share/maven") || strings.Contains(joined, "/etc/alternatives") {
		t.Fatal("generic restore widened")
	}
}

// Exercise the same validator, renderer, resource flags, helper and READY/GO
// protocol with synthetic installed bytes. Generic capability checking retains
// the existing infrastructure-only skip classification.
func TestRestoreRuntimeBoundary(t *testing.T) {
	helper := buildRestoreHelper(t)
	for _, tool := range []string{"maven", "gradle"} {
		t.Run(tool, func(t *testing.T) {
			requireRestoreCapability(t, RestoreBoundary{Helper: helper}, restoreLinuxRequest(t))
			f := newRuntimeFixture(t, tool)
			launcher := "/usr/share/" + tool + "/bin/"
			if tool == "maven" {
				launcher += "mvn"
			} else {
				launcher += "gradle"
			}
			script := "#!/bin/sh\nset -eu\n"
			for _, p := range []string{"/usr/share/" + tool + "/lib/library.jar", "/usr/share/" + tool + "/lib/maven-resolver-transport-http.jar", "/usr/share/" + tool + "/lib/plexus-xml.jar"} {
				if tool == "gradle" && strings.Contains(p, "maven-resolver") {
					continue
				}
				if tool == "gradle" && strings.Contains(p, "plexus-xml") {
					continue
				}
				script += "test -s '" + p + "'\n"
			}
			if tool == "maven" {
				script += "test -s /usr/share/maven/boot/plexus-classworlds-2.x.jar\ntest -s /usr/share/maven/bin/m2.conf\n"
				for _, n := range []string{"maven-resolver-transport-http.jar", "plexus-xml.jar"} {
					f.link("/usr/share/maven/lib/"+n, "../../java/"+n)
					f.file("/usr/share/java/"+n, "repaired", 0644)
				}
			} else {
				script += "test -s /usr/share/gradle/lib/gradle-launcher-4.4.1.jar\ntest -s /usr/share/gradle/lib/plugins/plugin.jar\n"
			}
			script += "exec /usr/bin/java\n"
			f.file(launcher, script, 0755)
			java := "#!/bin/sh\nset -eu\n"
			for _, pair := range runtimeMetadata {
				java += "test -s '" + runtimeJDK + pair[0] + "'\n"
				if strings.HasSuffix(pair[1], ".policy") {
					java += "test \"$(cat '" + runtimeJDK + pair[0] + "')\" = 'public:" + pair[1] + "'\n"
				}
			}
			for _, n := range []string{"net.properties", "logging.properties"} {
				java += "test ! -e '" + runtimeEtc + "/" + n + "'\n"
			}
			java += "test ! -e /usr/share/java\ntest ! -e /etc/maven/m2.conf\ntest ! -e /root\ntest ! -e /home\n"
			for _, p := range []string{runtimeJDK + runtimeMetadata[4][0], runtimeJDK + runtimeMetadata[5][0], runtimeEtc + runtimeMetadata[4][1], launcher, "/source/fixture", "/outside", "/etc/alternatives/java"} {
				java += "if ( echo forbidden > '" + p + "' ) 2>/dev/null; then exit 1; fi\n"
			}
			java += "ln -s /source/fixture /cache/escape\nif (echo forbidden > /cache/escape) 2>/dev/null; then exit 1; fi\nprintf complete > /cache/result\n"
			f.file(runtimeJDK+"/bin/java", java, 0755)
			r := f.resolve()
			control := t.TempDir()
			mounts, e := stageRestoreRuntime(control, r)
			if e != nil {
				t.Fatal(e)
			}
			before := make(map[string][]byte)
			for _, m := range mounts {
				data, err := os.ReadFile(m.source)
				if err != nil {
					t.Fatal(err)
				}
				before[m.source] = data
			}
			// Give the fixture a synthetic JDK while preserving the launcher's original
			// system library bytes for the shell and helper. No extra host input is added.
			systemEntries, e := os.ReadDir("/usr/lib")
			if e != nil {
				t.Fatal(e)
			}
			extra := []string{"--ro-bind", "/usr/lib", "/fixture-system-lib", "--tmpfs", "/usr/lib"}
			for _, v := range systemEntries {
				if v.Name() != "jvm" {
					extra = append(extra, "--symlink", "/fixture-system-lib/"+v.Name(), "/usr/lib/"+v.Name())
				}
			}
			extra = append(extra, "--ro-bind", f.host("/usr/lib/jvm"), "/usr/lib/jvm", "--remount-ro", "/usr/lib")

			binEntries, e := os.ReadDir("/usr/bin")
			if e != nil {
				t.Fatal(e)
			}
			extra = append(extra, "--ro-bind", "/usr/bin", "/fixture-system-bin", "--tmpfs", "/usr/bin")
			for _, v := range binEntries {
				if v.Name() != "java" && v.Name() != "mvn" && v.Name() != "gradle" {
					extra = append(extra, "--symlink", "/fixture-system-bin/"+v.Name(), "/usr/bin/"+v.Name())
				}
			}
			extra = append(extra, "--symlink", "/etc/alternatives/java", "/usr/bin/java")
			alias := f.argv()[0]
			target := "/etc/alternatives/mvn"
			if tool == "gradle" {
				target = "../share/gradle/bin/gradle"
			}
			extra = append(extra, "--symlink", target, alias, "--remount-ro", "/usr/bin")
			extra = append(extra, renderRestoreRuntime(mounts, r.aliases)...)
			for _, d := range []string{"source", "trust/empty", "socket"} {
				if e = os.MkdirAll(filepath.Join(control, d), 0700); e != nil {
					t.Fatal(e)
				}
			}
			data, e := os.ReadFile(helper)
			if e != nil {
				t.Fatal(e)
			}
			if e = os.WriteFile(filepath.Join(control, "helper"), data, 0500); e != nil {
				t.Fatal(e)
			}
			os.WriteFile(filepath.Join(control, "source/fixture"), []byte("approved"), 0400)
			trust, e := newRestoreTrust([]string{restoreCanaryHost, restoreFixtureHost})
			if e != nil {
				t.Fatal(e)
			}
			for name, data := range map[string][]byte{"ca.pem": trust.pem, "ca.jks": trust.jks} {
				if e = os.WriteFile(filepath.Join(control, "trust", name), data, 0400); e != nil {
					t.Fatal(e)
				}
			}
			listener, e := net.Listen("unix", filepath.Join(control, "socket/broker.sock"))
			if e != nil {
				t.Fatal(e)
			}
			broker := newRegistryBroker(t.Context(), listener, registryPolicy{restoreFixtureHost: true}, trust)
			broker.preflight.Store(true)
			broker.serve()
			defer broker.close()
			positive, e := net.Listen("tcp4", "127.0.0.1:0")
			if e != nil {
				t.Fatal(e)
			}
			defer positive.Close()
			cache := t.TempDir()
			for _, d := range []string{"home", "tmp"} {
				if e = os.MkdirAll(filepath.Join(cache, d), 0700); e != nil {
					t.Fatal(e)
				}
			}
			flags, e := restoreFlags(control, cache, positive.Addr().String(), nil)
			if e != nil {
				t.Fatal(e)
			}
			// Insert mounts before the source/control mounts and the final root remount.
			index := 0
			for i := 0; i+2 < len(flags); i++ {
				if flags[i] == "--ro-bind" && flags[i+1] == filepath.Join(control, "source") {
					index = i
					break
				}
			}
			if index == 0 {
				t.Fatal("missing control mount")
			}
			tail := append([]string(nil), flags[index:]...)
			flags = append(flags[:index], extra...)
			flags = append(flags, tail...)
			flags = append(flags, f.argv()[0])
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, "bwrap", flags...)
			cmd.Env = []string{"PATH=/usr/bin:/bin"}
			in, e := cmd.StdinPipe()
			if e != nil {
				t.Fatal(e)
			}
			out, e := cmd.StdoutPipe()
			if e != nil {
				t.Fatal(e)
			}
			var diagnostics bytes.Buffer
			cmd.Stderr = &diagnostics
			if e = cmd.Start(); e != nil {
				t.Fatal(e)
			}
			defer func() {
				if cmd.ProcessState == nil {
					cmd.Process.Kill()
					cmd.Wait()
				}
			}()
			if frame := readRestoreReadyFrame(out); frame != "READY\n" {
				cmd.Process.Kill()
				cmd.Wait()
				t.Fatalf("runtime helper frame %q: %s", frame, diagnostics.String())
			}
			broker.preflight.Store(false)
			if _, e = io.WriteString(in, "GO\n"); e != nil {
				t.Fatal(e)
			}
			in.Close()
			if e = cmd.Wait(); e != nil {
				t.Fatalf("runtime boundary: %v: %s", e, diagnostics.String())
			}
			result, e := os.ReadFile(filepath.Join(cache, "result"))
			if e != nil || string(result) != "complete" {
				t.Fatal("runtime completion", e)
			}
			if err := checkRestoreRuntimeBytes(before); err != nil {
				t.Fatal(err)
			}
			data, e = os.ReadFile(filepath.Join(control, "source/fixture"))
			if e != nil || string(data) != "approved" {
				t.Fatal("source modified", e)
			}
		})
	}
}

// Compare every staged source with bytes captured before the boundary executes.
// Payload text may itself contain the markers used by negative write probes.
func checkRestoreRuntimeBytes(before map[string][]byte) error {
	for source, want := range before {
		got, err := os.ReadFile(source)
		if err != nil {
			return fmt.Errorf("read staged runtime %s: %w", source, err)
		}
		if !bytes.Equal(got, want) {
			return fmt.Errorf("runtime modified: %s", source)
		}
	}
	return nil
}

func TestRestoreRuntimeBoundaryIntegrity(t *testing.T) {
	original := []byte("#!/bin/sh\nif (echo forbidden > /readonly); then exit 1; fi\n")
	for _, tc := range []struct {
		name     string
		after    []byte
		modified bool
	}{
		{"unchanged probe script", original, false},
		{"same length modification", bytes.Replace(original, []byte("readonly"), []byte("modified"), 1), true},
		{"truncated", original[:len(original)-1], true},
		{"appended", append(append([]byte(nil), original...), 'x'), true},
		{"empty", nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source := filepath.Join(t.TempDir(), "staged")
			if err := os.WriteFile(source, original, 0600); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(source)
			if err != nil {
				t.Fatal(err)
			}
			before := map[string][]byte{source: data}
			if err := os.WriteFile(source, tc.after, 0600); err != nil {
				t.Fatal(err)
			}
			if err := checkRestoreRuntimeBytes(before); (err != nil) != tc.modified {
				t.Fatalf("integrity error = %v, want modified = %v", err, tc.modified)
			}
		})
	}
}

func TestRestoreRuntimeDeduplication(t *testing.T) {
	f := newRuntimeFixture(t, "maven")
	f.link("/usr/share/maven/lib/duplicate.jar", "library.jar")
	reads := 0
	f.fs.hook = func(p string) {
		if p == "/usr/share/java/library-1.jar" {
			reads++
		}
	}
	r := f.resolve()
	if reads != 1 {
		t.Fatal("shared target read repeatedly", reads)
	}
	mounts, e := stageRestoreRuntime(t.TempDir(), r)
	if e != nil {
		t.Fatal(e)
	}
	sources := map[string]string{}
	for _, m := range mounts {
		sources[m.destination] = m.source
	}
	if sources["/usr/share/maven/lib/library.jar"] != sources["/usr/share/maven/lib/duplicate.jar"] {
		t.Fatal("shared target staged repeatedly")
	}
	flags := renderRestoreRuntime(mounts, r.aliases)
	var destinations []string
	for i := 0; i < len(flags); i += 3 {
		if flags[i] != "--ro-bind" && flags[i] != "--symlink" {
			t.Fatal(flags)
		}
		destinations = append(destinations, flags[i+2])
		if flags[i] == "--ro-bind" && !strings.Contains(flags[i+1], "/runtime/") {
			t.Fatal("host mount", flags[i+1])
		}
	}
	if !sort.StringsAreSorted(destinations) {
		t.Fatal("nondeterministic flags")
	}
	for _, dest := range destinations {
		if dest == "/etc" || dest == "/usr/share" || dest == "/usr/share/java" || dest == runtimeEtc || dest == runtimeEtc+"/security/policy" {
			t.Fatal("broad mount", dest)
		}
	}
	if e := r.add("/usr/share/maven/lib/library.jar", "/usr/share/java/library-1.jar", 64<<20, false); e == nil {
		t.Fatal("destination collision accepted")
	}
}
func TestRestoreRuntimeAdditionalBudgets(t *testing.T) {
	for _, limit := range []int64{1 << 20, 16 << 20} {
		for _, extra := range []int64{0, 1} {
			t.Run(fmt.Sprintf("file-%d-%d", limit, extra), func(t *testing.T) {
				f := newRuntimeFixture(t, "maven")
				p := runtimeEtc + "/security/blocked.certs"
				if e := os.Truncate(f.host(p), limit+extra); e != nil {
					t.Fatal(e)
				}
				r := &restoreRuntime{fs: f.fs}
				_, _, e := r.read(p, limit)
				if (e != nil) != (extra > 0) {
					t.Fatal(e)
				}
			})
		}
	}
	for _, extra := range []int64{0, 1} {
		t.Run(fmt.Sprintf("closure-%d", extra), func(t *testing.T) {
			f := newRuntimeFixture(t, "maven")
			f.file("/usr/share/java/one.jar", "x", 0644)
			r := &restoreRuntime{fs: f.fs, total: runtimeTotalBytes - 1 + extra}
			e := r.add("/usr/share/maven/lib/one.jar", "/usr/share/java/one.jar", 64<<20, false)
			if (e != nil) != (extra > 0) {
				t.Fatal(e)
			}
		})
	}
	for _, count := range []int{1023, 1024} {
		t.Run(fmt.Sprintf("files-%d", count), func(t *testing.T) {
			f := newRuntimeFixture(t, "maven")
			r := &restoreRuntime{fs: f.fs, files: map[string]runtimeInput{}}
			for i := 0; i < count; i++ {
				r.files[fmt.Sprint(i)] = runtimeInput{}
			}
			e := r.add("/usr/share/maven/lib/one.jar", "/usr/share/java/library-1.jar", 64<<20, false)
			if (e != nil) != (count == 1024) {
				t.Fatal(e)
			}
		})
	}
	for _, count := range []int{2047, 2048} {
		t.Run(fmt.Sprintf("mounts-%d", count), func(t *testing.T) {
			f := newRuntimeFixture(t, "maven")
			r := &restoreRuntime{fs: f.fs, aliases: map[string]string{}}
			for i := 0; i < count; i++ {
				r.aliases[fmt.Sprint(i)] = "unused"
			}
			e := r.add("/usr/share/maven/lib/one.jar", "/usr/share/java/library-1.jar", 64<<20, false)
			if (e != nil) != (count == 2048) {
				t.Fatal(e)
			}
		})
	}
	if !runtimePathOK("/"+strings.Repeat("a", 4095)) || !runtimePathOK("/"+strings.Repeat("a/", 31)+"a") {
		t.Fatal("at-limit pathname rejected")
	}
	for _, p := range []string{"/usr/share/java/../java/library-1.jar", strings.Repeat("../", 40) + "usr/share/java/library-1.jar"} {
		f := newRuntimeFixture(t, "maven")
		f.replaceLink("/usr/share/maven/lib/library.jar", p)
		if _, e := resolveRestoreRuntime(f.fs, f.argv()); e == nil {
			t.Fatal("unsafe intermediate traversal accepted")
		}
	}
}
func TestRestoreRuntimePolicyConcurrentChanges(t *testing.T) {
	for _, pair := range runtimeMetadata[4:] {
		for _, kind := range []string{"J-link", "E-file", "E-parent", "mutation"} {
			t.Run(pair[1]+"/"+kind, func(t *testing.T) {
				f := newRuntimeFixture(t, "maven")
				called := false
				f.fs.hook = func(p string) {
					if called || p != runtimeEtc+pair[1] {
						return
					}
					called = true
					switch kind {
					case "J-link":
						f.replaceLink(runtimeJDK+pair[0], runtimeEtc+pair[1]+".other")
					case "E-file":
						os.Rename(f.host(p), f.host(p+".old"))
						f.file(p, "replaced", 0644)
					case "E-parent":
						d := path.Dir(p)
						os.Rename(f.host(d), f.host(d+".old"))
						f.file(p, "replaced", 0644)
					case "mutation":
						f.file(p, "mutated", 0644)
					}
				}
				if _, e := resolveRestoreRuntime(f.fs, f.argv()); e == nil {
					t.Fatal("policy concurrent change accepted")
				}
				if !called {
					t.Fatal("policy hook unused")
				}
			})
		}
	}
}
