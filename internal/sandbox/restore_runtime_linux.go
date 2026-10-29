//go:build linux

package sandbox

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"

	"github.com/supernovae/swarmos/internal/safefs"
)

const runtimeJDK = "/usr/lib/jvm/java-25-openjdk-amd64"
const runtimeEtc = "/etc/java-25-openjdk"
const runtimeClassworlds = "main is org.apache.maven.cli.MavenCli from plexus.core\n\n[plexus.core]\nload ${maven.home}/lib/*.jar\n"
const runtimeTotalBytes int64 = 512 << 20

var runtimeJarName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]*\.jar$`)
var runtimeBootName = regexp.MustCompile(`^plexus-classworlds-[A-Za-z0-9._-]+\.jar$`)

// This seam is private to Linux fixtures. Production always opens / and requires
// root ownership. No request field can select roots, manifests or trust policy.
type restoreRuntimeFS struct {
	root *os.Root
	uid  uint32
	hook func(string)
}
type runtimeIdentity struct {
	name string
	info os.FileInfo
}
type runtimeMount struct{ source, destination string }
type runtimeInput struct {
	destination string
	source      string
	data        []byte
	mode        os.FileMode
}
type restoreRuntime struct {
	fs         *restoreRuntimeFS
	inputs     []runtimeInput
	files      map[string]runtimeInput
	aliases    map[string]string
	identities []runtimeIdentity
	total      int64
}

func runtimePathOK(p string) bool {
	return strings.HasPrefix(p, "/") && path.Clean(p) == p && len(p) <= 4096 && len(strings.Split(strings.TrimPrefix(p, "/"), "/")) <= 32
}
func (f *restoreRuntimeFS) trusted(i os.FileInfo) bool {
	s, ok := i.Sys().(*syscall.Stat_t)
	return ok && s.Uid == f.uid && i.Mode().Perm()&0022 == 0 && i.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) == 0
}
func sameRuntimeInfo(a, b os.FileInfo) bool {
	x, xok := a.Sys().(*syscall.Stat_t)
	y, yok := b.Sys().(*syscall.Stat_t)
	return xok && yok && os.SameFile(a, b) && a.Mode() == b.Mode() && a.Size() == b.Size() && a.ModTime() == b.ModTime() && x.Ctim == y.Ctim && x.Uid == y.Uid && x.Gid == y.Gid
}

// Pin every directory, disallow directory links, and remember the complete
// ancestry for revalidation after copying. os.Root prevents escape on rename.
func (r *restoreRuntime) parent(p string) (*os.Root, string, error) {
	if !runtimePathOK(p) {
		return nil, "", fmt.Errorf("invalid runtime pathname")
	}
	d, err := r.fs.root.OpenRoot(".")
	if err != nil {
		return nil, "", err
	}
	rootInfo, err := d.Stat(".")
	if err != nil || !r.fs.trusted(rootInfo) {
		d.Close()
		return nil, "", fmt.Errorf("untrusted runtime root")
	}
	r.identities = append(r.identities, runtimeIdentity{".", rootInfo})
	parts := strings.Split(strings.TrimPrefix(p, "/"), "/")
	prefix := ""
	for _, part := range parts[:len(parts)-1] {
		prefix = path.Join(prefix, part)
		before, e := d.Lstat(part)
		if e != nil {
			d.Close()
			return nil, "", e
		}
		if !before.IsDir() || !r.fs.trusted(before) {
			d.Close()
			return nil, "", fmt.Errorf("unsafe runtime ancestor")
		}
		next, e := d.OpenRoot(part)
		if e != nil {
			d.Close()
			return nil, "", e
		}
		actual, e := next.Stat(".")
		after, ae := d.Lstat(part)
		d.Close()
		if e != nil || ae != nil || !sameRuntimeInfo(before, actual) || !sameRuntimeInfo(after, actual) {
			next.Close()
			return nil, "", fmt.Errorf("runtime ancestor changed")
		}
		r.identities = append(r.identities, runtimeIdentity{prefix, actual})
		d = next
	}
	return d, parts[len(parts)-1], nil
}
func (r *restoreRuntime) entry(p string) (os.FileInfo, string, error) {
	d, b, err := r.parent(p)
	if err != nil {
		return nil, "", err
	}
	defer d.Close()
	i, err := d.Lstat(b)
	if err != nil {
		return nil, "", err
	}
	// System symlink permission bits are always 0777; only owner is relevant.
	if i.Mode()&os.ModeSymlink != 0 {
		s, ok := i.Sys().(*syscall.Stat_t)
		if !ok || s.Uid != r.fs.uid {
			return nil, "", fmt.Errorf("untrusted runtime link")
		}
		target, e := d.Readlink(b)
		if e != nil {
			return nil, "", e
		}

		if len(target) > 4096 || len(strings.Split(strings.TrimPrefix(target, "/"), "/")) > 32 {
			return nil, "", fmt.Errorf("runtime link pathname budget exceeded")
		}
		named := false
		for _, part := range strings.Split(target, "/") {
			if part == ".." && named {
				return nil, "", fmt.Errorf("unsupported runtime link traversal")
			}
			if part != "" && part != "." && part != ".." {
				named = true
			}
		}
		r.identities = append(r.identities, runtimeIdentity{strings.TrimPrefix(p, "/"), i})
		if !path.IsAbs(target) {
			target = path.Join(path.Dir(p), target)
		}
		if !runtimePathOK(target) {
			return nil, "", fmt.Errorf("invalid runtime link target")
		}
		return i, target, nil
	}
	if !r.fs.trusted(i) {
		return nil, "", fmt.Errorf("untrusted runtime input")
	}
	r.identities = append(r.identities, runtimeIdentity{strings.TrimPrefix(p, "/"), i})
	return i, "", nil
}
func (r *restoreRuntime) verify() error {
	for _, v := range r.identities {
		i, e := r.fs.root.Lstat(v.name)
		if e != nil || !sameRuntimeInfo(v.info, i) {
			return fmt.Errorf("runtime input changed")
		}
	}
	return nil
}
func (r *restoreRuntime) read(p string, limit int64) ([]byte, os.FileMode, error) {
	d, b, e := r.parent(p)
	if e != nil {
		return nil, 0, e
	}
	defer d.Close()
	before, e := d.Lstat(b)
	if e != nil {
		return nil, 0, e
	}
	if !before.Mode().IsRegular() || !r.fs.trusted(before) || before.Size() > limit {
		return nil, 0, fmt.Errorf("unsafe runtime file or file budget exceeded")
	}
	// safefs supplies O_NOFOLLOW and nonblocking regular-file opening.
	directory, e := d.Open(".")
	if e != nil {
		return nil, 0, e
	}
	defer directory.Close()
	sr, e := safefs.Open("/proc/self/fd/" + strconv.Itoa(int(directory.Fd())))
	if e != nil {
		return nil, 0, e
	}
	defer sr.Close()
	f, e := sr.OpenRegular(b)
	if e != nil {
		return nil, 0, e
	}
	defer f.Close()
	actual, e := f.Stat()
	if e != nil || !sameRuntimeInfo(before, actual) {
		return nil, 0, fmt.Errorf("runtime file changed")
	}
	if r.fs.hook != nil {
		r.fs.hook(p)
	}
	data, e := io.ReadAll(io.LimitReader(f, limit+1))
	if e != nil {
		return nil, 0, e
	}
	after, e := f.Stat()
	entry, ee := d.Lstat(b)
	if e != nil || ee != nil || int64(len(data)) != before.Size() || int64(len(data)) > limit || !sameRuntimeInfo(before, after) || !sameRuntimeInfo(before, entry) {
		return nil, 0, fmt.Errorf("runtime file changed during copy")
	}
	r.identities = append(r.identities, runtimeIdentity{strings.TrimPrefix(p, "/"), before})
	return data, before.Mode().Perm(), nil
}
func (r *restoreRuntime) add(dest, src string, limit int64, executable bool) error {
	for _, v := range r.inputs {
		if v.destination == dest {
			return fmt.Errorf("conflicting runtime destination")
		}
	}
	if len(r.inputs)+len(r.aliases) >= 2048 {
		return fmt.Errorf("runtime mount budget exceeded")
	}
	if r.files == nil {
		r.files = map[string]runtimeInput{}
	}
	if cached, ok := r.files[src]; ok {
		cached.destination = dest
		r.inputs = append(r.inputs, cached)
		return nil
	}
	if len(r.files) >= 1024 {
		return fmt.Errorf("runtime file count exceeded")
	}
	if remaining := runtimeTotalBytes - r.total; limit > remaining {
		limit = remaining
	}
	data, mode, e := r.read(src, limit)
	if e != nil {
		return e
	}
	if executable && mode&0111 == 0 {
		return fmt.Errorf("runtime launcher is not executable")
	}
	r.total += int64(len(data))
	input := runtimeInput{destination: dest, source: src, data: data, mode: mode & 0555}
	r.files[src] = input
	r.inputs = append(r.inputs, input)
	return nil
}
func (r *restoreRuntime) exactLink(from, to string) error {
	i, t, e := r.entry(from)
	if e != nil {
		return e
	}
	if i.Mode()&os.ModeSymlink == 0 || t != to {
		return fmt.Errorf("unsupported runtime alias")
	}
	return nil
}
func (r *restoreRuntime) jars(dir string, boot bool) error {
	// parent() checks and pins the enumeration directory without following links.
	d, _, e := r.parent(dir + "/entry")
	if e != nil {
		return e
	}
	defer d.Close()
	f, e := d.Open(".")
	if e != nil {
		return e
	}
	defer f.Close()
	entries, e := f.ReadDir(513)
	if e != nil && e != io.EOF {
		return e
	}
	if len(entries) > 512 {
		return fmt.Errorf("runtime directory scan budget exceeded")
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	count := 0
	for _, v := range entries {
		name := v.Name()
		match := runtimeJarName.MatchString(name) && len(name) <= 255
		if boot {
			match = runtimeBootName.MatchString(name) && len(name) <= 255
		}
		if !match {
			continue
		}
		count++
		selected := dir + "/" + name
		current := selected
		seen := map[string]bool{}
		for hops := 0; ; hops++ {
			if seen[current] || hops > 8 {
				return fmt.Errorf("runtime symlink budget exceeded")
			}
			seen[current] = true
			i, target, e := r.entry(current)
			if e != nil {
				return e
			}
			if i.Mode()&os.ModeSymlink == 0 {
				if !i.Mode().IsRegular() {
					return fmt.Errorf("nonregular runtime library")
				}
				break
			}
			td := path.Dir(target)
			allowed := td == "/usr/share/java" || (path.Dir(current) == dir && td == dir)
			if !allowed || !runtimeJarName.MatchString(path.Base(target)) || len(path.Base(target)) > 255 {
				return fmt.Errorf("unsupported runtime library edge")
			}
			current = target
		}
		if e := r.add(selected, current, 64<<20, false); e != nil {
			return e
		}
	}
	if count == 0 || boot && count != 1 {
		return fmt.Errorf("missing or ambiguous runtime bootstrap/libraries")
	}
	return nil
}

var runtimeMetadata = [][2]string{
	{"/lib/jvm.cfg", "/jvm-amd64.cfg"},
	{"/conf/security/java.security", "/security/java.security"},
	{"/lib/security/blocked.certs", "/security/blocked.certs"},
	{"/lib/security/public_suffix_list.dat", "/security/public_suffix_list.dat"},
	{"/conf/security/policy/unlimited/default_US_export.policy", "/security/policy/unlimited/default_US_export.policy"},
	{"/conf/security/policy/unlimited/default_local.policy", "/security/policy/unlimited/default_local.policy"},
}

func validateRuntimeSecurity(data []byte) error {
	// Java Properties joins continuations before interpreting key escapes.
	lines := strings.Split(strings.ReplaceAll(strings.ReplaceAll(string(data), "\r\n", "\n"), "\r", "\n"), "\n")
	logical := ""
	providers := map[string]bool{}
	for _, p := range strings.Fields("SUN SunRsaSign SunEC SunJSSE SunJCE SunJGSS SunSASL XMLDSig SunPCSC JdkLDAP JdkSASL SunPKCS11") {
		providers[p] = true
	}
	for _, line := range lines {
		line = strings.TrimLeft(line, " \t\f")
		if logical == "" && (strings.HasPrefix(line, "#") || strings.HasPrefix(line, "!")) {
			continue
		}
		logical += line
		n := 0
		for i := len(logical) - 1; i >= 0 && logical[i] == '\\'; i-- {
			n++
		}
		if n%2 == 1 {
			logical = logical[:len(logical)-1]
			continue
		}
		decoded, e := decodeRuntimeProperty(logical)
		logical = ""
		if e != nil {
			return e
		}
		fields := strings.FieldsFunc(decoded, func(c rune) bool { return c == '=' || c == ':' || c == ' ' || c == '\t' || c == '\f' })
		if len(fields) == 0 {
			continue
		}
		key := fields[0]
		if strings.EqualFold(key, "include") {
			return fmt.Errorf("unsupported Java security include")
		}
		if strings.HasPrefix(key, "security.provider.") && (len(fields) != 2 || !providers[fields[1]]) {
			return fmt.Errorf("unsupported Java security provider")
		}
	}
	if logical != "" {
		return fmt.Errorf("incomplete Java security property")
	}
	return nil
}
func decodeRuntimeProperty(s string) (string, error) {
	var out strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' {
			out.WriteByte(s[i])
			continue
		}
		i++
		if i == len(s) {
			return "", fmt.Errorf("invalid Java property escape")
		}
		if s[i] == 'u' {
			if i+4 >= len(s) {
				return "", fmt.Errorf("invalid Java property escape")
			}
			v, e := strconv.ParseUint(s[i+1:i+5], 16, 16)
			if e != nil {
				return "", fmt.Errorf("invalid Java property escape")
			}
			out.WriteRune(rune(v))
			i += 4
		} else {
			switch s[i] {
			case 't':
				out.WriteByte('\t')
			case 'n':
				out.WriteByte('\n')
			case 'r':
				out.WriteByte('\r')
			case 'f':
				out.WriteByte('\f')
			default:
				out.WriteByte(s[i])
			}
		}
	}
	return out.String(), nil
}
func (r *restoreRuntime) java() error {
	if e := r.exactLink("/usr/bin/java", "/etc/alternatives/java"); e != nil {
		return e
	}
	if e := r.exactLink("/etc/alternatives/java", runtimeJDK+"/bin/java"); e != nil {
		return e
	}
	i, t, e := r.entry(runtimeJDK + "/bin/java")
	if e != nil {
		return e
	}
	if t != "" || !i.Mode().IsRegular() || i.Mode().Perm()&0111 == 0 {
		return fmt.Errorf("unsupported Java executable")
	}
	if e := r.add(runtimeJDK+"/bin/java", runtimeJDK+"/bin/java", 1<<20, true); e != nil {
		return e
	}
	r.aliases["/etc/alternatives/java"] = runtimeJDK + "/bin/java"
	for _, pair := range runtimeMetadata {
		src := runtimeEtc + pair[1]
		if e := r.exactLink(runtimeJDK+pair[0], src); e != nil {
			return e
		}
		limit := int64(16 << 20)
		if strings.HasSuffix(src, "java.security") || strings.HasSuffix(src, ".policy") || strings.HasSuffix(src, ".cfg") {
			limit = 1 << 20
		}
		if e := r.add(src, src, limit, false); e != nil {
			return e
		}
		data := r.inputs[len(r.inputs)-1].data
		if strings.HasSuffix(src, "java.security") {
			if e := validateRuntimeSecurity(data); e != nil {
				return e
			}
		}
		if strings.HasSuffix(src, ".cfg") {
			for _, line := range strings.Split(string(data), "\n") {
				line = strings.TrimSpace(line)
				if line == "" || strings.HasPrefix(line, "#") {
					continue
				}
				if line != "-server KNOWN" && line != "-client IGNORE" && line != "-zero KNOWN" && line != "-dcevm KNOWN" {
					return fmt.Errorf("unsupported Java VM configuration")
				}
			}
		}
	}
	return nil
}
func resolveRestoreRuntime(fs *restoreRuntimeFS, argv []string) (*restoreRuntime, error) {
	if len(argv) == 0 {
		return nil, nil
	}
	tool := ""
	switch argv[0] {
	case "/usr/bin/mvn", "/usr/share/maven/bin/mvn":
		tool = "maven"
	case "/usr/bin/gradle", "/usr/share/gradle/bin/gradle":
		tool = "gradle"
	default:
		return nil, nil
	}
	r := &restoreRuntime{fs: fs, aliases: map[string]string{}}
	err := func() error {
		launcher := "/usr/share/" + tool + "/bin/"
		alias := "/usr/bin/gradle"
		if tool == "maven" {
			launcher += "mvn"
			alias = "/usr/bin/mvn"
		} else {
			launcher += "gradle"
		}
		_, target, e := r.entry(alias)
		if e != nil {
			return e
		}
		if tool == "maven" && target == "/etc/alternatives/mvn" {
			if e := r.exactLink(target, launcher); e != nil {
				return e
			}
			r.aliases[target] = launcher
		} else if target != launcher {
			return fmt.Errorf("unsupported launcher layout")
		}
		if e := r.add(launcher, launcher, 1<<20, true); e != nil {
			return e
		}
		base := "/usr/share/" + tool
		if tool == "maven" {
			if e := r.jars(base+"/boot", true); e != nil {
				return e
			}
		}
		if e := r.jars(base+"/lib", false); e != nil {
			return e
		}
		if tool == "gradle" {
			found := false
			for _, v := range r.inputs {
				if v.destination == base+"/lib/gradle-launcher-4.4.1.jar" {
					found = true
				}
			}
			if !found {
				return fmt.Errorf("missing Gradle 4.4.1 bootstrap")
			}
			if e := r.jars(base+"/lib/plugins", false); e != nil {
				return e
			}
		}
		if e := r.java(); e != nil {
			return e
		}
		if tool == "maven" {
			r.inputs = append(r.inputs, runtimeInput{destination: base + "/bin/m2.conf", source: "generated:m2.conf", data: []byte(runtimeClassworlds), mode: 0444})
			r.total += int64(len(runtimeClassworlds))
		}
		generated := 0
		if tool == "maven" {
			generated = 1
		}
		if len(r.files)+generated > 1024 || len(r.inputs)+len(r.aliases) > 2048 || r.total > runtimeTotalBytes {
			return fmt.Errorf("runtime manifest budget exceeded")
		}
		return r.verify()
	}()
	if err != nil {
		return nil, fmt.Errorf("restore %s runtime: %w; install a supported system layout", tool, err)
	}
	sort.Slice(r.inputs, func(i, j int) bool { return r.inputs[i].destination < r.inputs[j].destination })
	return r, nil
}
func stageRestoreRuntime(control string, r *restoreRuntime) (mounts []runtimeMount, err error) {
	if r == nil {
		return nil, nil
	}
	root, e := openRestoreDirectory(control)
	if e != nil {
		return nil, e
	}
	defer root.Close()
	directory, e := root.OpenDirectory(".")
	if e != nil {
		return nil, e
	}
	defer directory.Close()
	cleanup, e := os.OpenRoot("/proc/self/fd/" + strconv.Itoa(int(directory.Fd())))
	if e != nil {
		return nil, e
	}
	defer cleanup.Close()
	if e = root.MkdirAll("runtime", 0700); e != nil {
		return nil, e
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, cleanup.RemoveAll("runtime"))
		}
	}()
	staged := map[string]string{}
	for _, v := range r.inputs {
		if source, ok := staged[v.source]; ok {
			mounts = append(mounts, runtimeMount{source, v.destination})
			continue
		}
		name := "runtime" + v.destination
		if e = root.WriteFile(name, v.data, v.mode); e != nil {
			return nil, e
		}
		source := filepath.Join(control, name)
		staged[v.source] = source
		mounts = append(mounts, runtimeMount{source, v.destination})
	}
	if e = r.verify(); e != nil {
		return nil, e
	}
	return mounts, nil
}
func restoreRuntimeFlags(control string, argv []string) ([]string, error) {
	if len(argv) == 0 {
		return nil, nil
	}
	// Generic payloads do not even open the host runtime filesystem.
	switch argv[0] {
	case "/usr/bin/mvn", "/usr/share/maven/bin/mvn", "/usr/bin/gradle", "/usr/share/gradle/bin/gradle":
	default:
		return nil, nil
	}
	root, e := os.OpenRoot("/")
	if e != nil {
		return nil, e
	}
	defer root.Close()
	r, e := resolveRestoreRuntime(&restoreRuntimeFS{root: root}, argv)
	if e != nil {
		return nil, e
	}
	mounts, e := stageRestoreRuntime(control, r)
	if e != nil {
		return nil, e
	}
	return renderRestoreRuntime(mounts, r.aliases), nil
}

func renderRestoreRuntime(mounts []runtimeMount, links map[string]string) []string {
	var flags []string
	// bwrap creates empty parents for individual bind destinations.
	entries := make(map[string][]string, len(mounts)+len(links))
	for _, m := range mounts {
		entries[m.destination] = []string{"--ro-bind", m.source, m.destination}
	}
	for dest, target := range links {
		entries[dest] = []string{"--symlink", target, dest}
	}
	destinations := make([]string, 0, len(entries))
	for dest := range entries {
		destinations = append(destinations, dest)
	}
	sort.Strings(destinations)
	for _, dest := range destinations {
		flags = append(flags, entries[dest]...)
	}
	return flags
}
