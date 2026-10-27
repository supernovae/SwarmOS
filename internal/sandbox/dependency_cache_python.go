package sandbox

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// pythonUVExportArgv turns the base's uv.lock into pinned, hashed
// requirements without resolving, building or reading any configuration.
// --no-build stops uv from running the project's build backend to read
// dynamic metadata.
var pythonUVExportArgv = []string{
	"uv", "export", "--frozen", "--offline", "--no-build", "--no-config", "--no-python-downloads",
	"--no-emit-project", "--no-emit-workspace", "--all-extras", "--all-groups", "--format", "requirements-txt",
}

// pythonPipDownloadArgv only downloads wheels: --only-binary=:all: means pip
// never builds an sdist or runs its setup.py to read metadata.
var pythonPipDownloadArgv = []string{
	"python3", "-I", "-m", "pip", "download", "--only-binary=:all:", "--no-input",
	"--disable-pip-version-check", "--progress-bar", "off",
}

const pythonExportName = "uv.lock (exported)"

// pythonWheelDownload fills req.Dir with wheels for the base's uv.lock and
// requirements*.txt files. Every requirements file is checked in Go first,
// because a line such as -e, a path, a URL or --no-binary would make pip
// build package code. An sdist-only dependency is reported, never built.
func pythonWheelDownload(ctx context.Context, req prefetchRequest) error {
	var reqFiles []string
	for name := range req.Files {
		if ok, _ := path.Match("requirements*.txt", name); ok {
			reqFiles = append(reqFiles, name)
		}
	}
	slices.Sort(reqFiles)
	for _, name := range reqFiles {
		if err := checkRequirements(name, req.Files[name], req.Files); err != nil {
			return err
		}
	}
	_, uvLock := req.Files["uv.lock"]
	if _, ok := req.Files["pyproject.toml"]; uvLock && !ok {
		return errors.New("approved base has uv.lock but no pyproject.toml")
	}
	if !uvLock && len(reqFiles) == 0 {
		return errors.New("approved base has no uv.lock or requirements*.txt")
	}

	tmp, err := os.MkdirTemp("", "swarm-prefetch-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	project, outside := filepath.Join(tmp, "project"), filepath.Join(tmp, "outside")
	for _, dir := range []string{project, outside} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			return err
		}
	}
	for name, data := range req.Files {
		if filepath.Base(name) != name {
			return fmt.Errorf("lockfile %q is not at the repository root", name)
		}
		if err := os.WriteFile(filepath.Join(project, name), data, 0o600); err != nil {
			return err
		}
	}
	env := pythonPrefetchEnv(os.Getenv, tmp)
	redact := func(s string) string {
		for _, p := range []string{req.Dir, tmp} {
			s = strings.ReplaceAll(s, p, "<"+filepath.Base(p)+">")
		}
		return s
	}

	type download struct {
		label, file string
		extra       []string
	}
	var downloads []download
	if uvLock {
		exported := filepath.Join(tmp, "uv-requirements.txt")
		argv := append(slices.Clone(pythonUVExportArgv), "-o", exported)
		stdout, stderr, err := runPrefetchCommand(ctx, project, env, argv)
		if err != nil {
			return fmt.Errorf("export uv.lock: %s", redact(pythonPrefetchFailure(stdout, stderr, err)))
		}
		data, err := os.ReadFile(exported)
		if err != nil {
			return fmt.Errorf("export uv.lock: %w", err)
		}
		if err := checkRequirements(pythonExportName, data, nil); err != nil {
			return err
		}
		downloads = append(downloads, download{pythonExportName, exported, []string{"--no-deps", "--require-hashes"}})
	}
	for _, name := range reqFiles {
		downloads = append(downloads, download{name, filepath.Join(project, name), nil})
	}
	// One file at a time, so pins in different files cannot conflict.
	for _, d := range downloads {
		argv := append(slices.Clone(pythonPipDownloadArgv), "--dest", req.Dir)
		argv = append(append(argv, d.extra...), "-r", d.file)
		stdout, stderr, err := runPrefetchCommand(ctx, outside, env, argv)
		if err != nil {
			return fmt.Errorf("%s: %s", d.label, redact(pythonPrefetchFailure(stdout, stderr, err)))
		}
	}
	return nil
}

// pythonPrefetchEnv builds the prefetch environment from scratch. It copies
// only PATH and the host's package index settings, so no token, credential
// helper, SSH agent, pip or uv configuration, or PYTHONPATH reaches pip or uv.
func pythonPrefetchEnv(getenv func(string) string, tmp string) []string {
	env := []string{
		"PATH=" + getenv("PATH"), "TMPDIR=" + tmp, "HOME=" + tmp,
		"PIP_CONFIG_FILE=" + os.DevNull, "PIP_NO_INPUT=1", "PYTHONNOUSERSITE=1",
		"UV_NO_CONFIG=1", "UV_OFFLINE=1", "UV_NO_BUILD=1", "UV_PYTHON_DOWNLOADS=never",
		"UV_CACHE_DIR=" + filepath.Join(tmp, "uv"),
		"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + os.DevNull, "GIT_TERMINAL_PROMPT=0",
	}
	for _, key := range []string{"PIP_INDEX_URL", "PIP_EXTRA_INDEX_URL", "PIP_TRUSTED_HOST", "PIP_CERT", "SSL_CERT_FILE"} {
		if value := strings.TrimSpace(getenv(key)); value != "" {
			env = append(env, key+"="+value)
		}
	}
	return env
}

// pythonRequirement is a PEP 508 name with optional extras and version
// specifiers: nothing pip could treat as a path, URL or archive.
var pythonRequirement = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9._-]*[A-Za-z0-9])?\s*(\[\s*[A-Za-z0-9._-]+(\s*,\s*[A-Za-z0-9._-]+)*\s*\])?\s*(\(?\s*(===|==|!=|~=|<=|>=|<|>)\s*[A-Za-z0-9.*+!_-]+(\s*,\s*(===|==|!=|~=|<=|>=|<|>)\s*[A-Za-z0-9.*+!_-]+)*\s*\)?)?$`)

var (
	pythonComment     = regexp.MustCompile(`(^|\s)#.*$`)
	pythonCommentLine = regexp.MustCompile(`^\s*#`)
)

// checkRequirements accepts only registry requirements, --hash options and
// -r/-c references to other base requirements*.txt files. base is nil for
// the uv export, which may not reference other files.
func checkRequirements(name string, data []byte, base map[string][]byte) error {
	lines := strings.Split(string(bytes.ReplaceAll(data, []byte("\r\n"), []byte("\n"))), "\n")
	for i := 0; i < len(lines); i++ {
		// Join backslash continuations the way pip does: a comment line
		// ends the logical line even when it ends in a backslash.
		number, line := i+1, ""
		for ; i < len(lines); i++ {
			if pythonCommentLine.MatchString(lines[i]) || !strings.HasSuffix(lines[i], `\`) {
				line += " " + lines[i]
				break
			}
			line += " " + strings.TrimSuffix(lines[i], `\`)
		}
		line = strings.TrimSpace(pythonComment.ReplaceAllString(line, ""))
		if line == "" {
			continue
		}
		reject := func(why string) error {
			return fmt.Errorf("%s:%d: %q %s", name, number, line, why)
		}
		if strings.Contains(line, "${") {
			return reject("expands an environment variable")
		}
		fields := strings.Fields(line)
		if strings.HasPrefix(fields[0], "-") {
			opt, value := fields[0], strings.Join(fields[1:], " ")
			if len(fields) == 1 && strings.HasPrefix(opt, "--") {
				opt, value, _ = strings.Cut(opt, "=")
			} else if len(fields) == 1 && len(opt) > 2 {
				opt, value = opt[:2], opt[2:]
			}
			switch opt {
			case "-r", "--requirement", "-c", "--constraint":
				ok, _ := path.Match("requirements*.txt", value)
				if _, found := base[value]; !ok || !found {
					return reject("does not name another requirements*.txt file in the approved base")
				}
				continue
			}
			return reject("is not a registry requirement; it would change what pip fetches or builds")
		}
		spec, options := line, []string(nil)
		if i := slices.IndexFunc(fields, func(f string) bool { return strings.HasPrefix(f, "-") }); i >= 0 {
			spec, options = strings.Join(fields[:i], " "), fields[i:]
		}
		for j := 0; j < len(options); j++ {
			switch {
			case strings.HasPrefix(options[j], "--hash="):
			case options[j] == "--hash" && j+1 < len(options):
				j++
			default:
				return reject("has an option other than --hash")
			}
		}
		spec, _, _ = strings.Cut(spec, ";")
		spec = strings.TrimSpace(spec)
		if strings.ContainsAny(spec, "@/\\:") || !pythonRequirement.MatchString(spec) || looksLikeArchive(spec) {
			return reject("is not a registry requirement; it would be built")
		}
	}
	return nil
}

func looksLikeArchive(spec string) bool {
	lower := strings.ToLower(spec)
	for _, ext := range []string{".whl", ".zip", ".tar", ".tar.gz", ".tgz", ".tar.bz2", ".tbz", ".tar.xz", ".txz", ".tar.lz", ".tlz"} {
		if strings.HasSuffix(lower, ext) {
			return true
		}
	}
	return false
}

// pythonPrefetchFailure names the dependencies pip or uv reported. A
// requirement with no compatible wheel is an sdist-only dependency, which
// the prefetch never builds.
func pythonPrefetchFailure(stdout, stderr []byte, runErr error) string {
	var details []string
	for _, line := range strings.Split(string(stderr), "\n") {
		line = strings.TrimSpace(line)
		if req, ok := strings.CutPrefix(line, "ERROR: No matching distribution found for "); ok {
			details = append(details, req+": no compatible wheel (sdist-only dependencies are not built)")
		} else if msg, ok := strings.CutPrefix(line, "ERROR: "); ok && !strings.HasPrefix(msg, "Could not find a version that satisfies") {
			details = append(details, msg)
		} else if msg, ok := strings.CutPrefix(line, "error: "); ok {
			details = append(details, msg)
		}
	}
	if len(details) == 0 {
		if s := strings.TrimSpace(string(stderr)); s != "" {
			details = append(details, s)
		} else if s := strings.TrimSpace(string(stdout)); s != "" {
			details = append(details, s)
		} else {
			details = append(details, runErr.Error())
		}
	}
	details = dedupe(details)
	if len(details) > 10 {
		details = append(details[:10], fmt.Sprintf("and %d more", len(details)-10))
	}
	return strings.Join(details, "; ")
}
