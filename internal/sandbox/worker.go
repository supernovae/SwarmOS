package sandbox

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/supernovae/swarmos/internal/claimpause"
	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/safefs"
)

// WorkerOptions describes a disposable Linux OCI worker. OutputDir is a trusted
// coordinator-owned destination used by the review snapshot's output channel.
type WorkerOptions struct {
	CheckImport func(context.Context, []string) error
	// PreserveRejected receives the patch of a run whose import was refused
	// with a RecoverableImport error; patch is nil when none could be built.
	PreserveRejected                func(ctx context.Context, importErr error, patch []byte)
	GuidanceDelivered               func(string) // coordinator callback for forwarded interactive guidance
	Lifecycle                       *Lifecycle   // optional coordinator timing and scratch-log observer
	Limits                          contract.AgentLimits
	Warning                         io.Writer
	RecoveryRoot                    string
	ProjectPlan                     bool
	Runtime, Image, Role, OutputDir string
	ReadOnly                        bool
	DiscardChanges                  bool
	Timeout                         time.Duration
	// Effort is the approved reasoning effort that native and harness-tool
	// launchers map to their harness flag. Empty keeps the harness default.
	Effort string
	// Dependencies are the coordinator's prepared dependency caches, mounted
	// read-only for native and harness-tool runs.
	Dependencies Dependencies
}

const workerMaxBytes int64 = 512 << 20
const workerFileBytes int64 = 64 << 20
const workerMaxEntries = 100000

var pinnedWorkerImage = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/-]*@sha256:[a-f0-9]{64}$`)
var workerContainerID = regexp.MustCompile(`^[a-f0-9]{64}$`)

func workerCommand(ctx context.Context, runtime string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, runtime, args...)
	cmd.Env = []string{}
	for _, key := range []string{"PATH", "SYSTEMROOT", "WINDIR", "TMPDIR", "TEMP", "TMP"} {
		if value, ok := os.LookupEnv(key); ok {
			cmd.Env = append(cmd.Env, key+"="+value)
		}
	}
	cmd.WaitDelay = time.Second
	return cmd
}

func workerRun(ctx context.Context, runtime string, args ...string) ([]byte, error) {
	var out, diagnostics limitedWorkerBuffer
	cmd := workerCommand(ctx, runtime, args...)
	cmd.Stdout, cmd.Stderr = &out, &diagnostics
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("OCI %s: %w: %s", args[0], err, diagnostics.String())
	}
	return out.Bytes(), nil
}

type limitedWorkerBuffer struct{ bytes.Buffer }

func (b *limitedWorkerBuffer) Write(p []byte) (int, error) {
	if b.Len()+len(p) > 1<<20 {
		return 0, fmt.Errorf("OCI diagnostic output exceeds limit")
	}
	return b.Buffer.Write(p)
}

func workerRuntime(runtime, image string) (string, error) {
	if runtime == "" {
		runtime = "docker"
	}
	if filepath.Base(runtime) != "docker" && filepath.Base(runtime) != "podman" {
		return "", fmt.Errorf("isolated workers require docker or podman")
	}
	if !pinnedWorkerImage.MatchString(image) {
		return "", fmt.Errorf("isolated worker image must be pinned by sha256 digest")
	}
	binary, err := exec.LookPath(runtime)
	if err != nil {
		return "", fmt.Errorf("isolated worker runtime unavailable: %w", err)
	}
	return binary, nil
}

func inspectWorkerImage(ctx context.Context, runtime, image string) error {
	raw, err := workerRun(ctx, runtime, "image", "inspect", image)
	if err != nil {
		return fmt.Errorf("inspect locally available worker image: %w", err)
	}
	var images []struct {
		OS     string `json:"Os"`
		Config struct {
			Volumes map[string]json.RawMessage `json:"Volumes"`
		} `json:"Config"`
	}
	if err := json.Unmarshal(raw, &images); err != nil || len(images) != 1 {
		return fmt.Errorf("worker image inspection returned invalid metadata")
	}
	if images[0].OS != "linux" {
		return fmt.Errorf("isolated workers require a Linux OCI image")
	}
	if len(images[0].Config.Volumes) != 0 {
		return fmt.Errorf("worker image declares volumes; use an image without implicit persistent mounts")
	}
	return nil
}

func inspectWorkerContainer(ctx context.Context, runtime, id string, readOnly bool) error {
	raw, err := workerRun(ctx, runtime, "inspect", id)
	if err != nil {
		return err
	}
	var containers []struct {
		Host struct {
			Privileged                                  bool
			NetworkMode, PidMode, IpcMode, CgroupnsMode string
			ReadonlyRootfs                              bool
			CapAdd, CapDrop, Binds                      []string
			SecurityOpt                                 []string
			Memory, MemorySwap, NanoCpus                int64
			PidsLimit                                   int64
			Tmpfs                                       map[string]string
		} `json:"HostConfig"`
		Mounts []struct{ Type, Destination string }
	}
	if err := json.Unmarshal(raw, &containers); err != nil || len(containers) != 1 {
		return fmt.Errorf("invalid worker container inspection")
	}
	config := containers[0].Host
	has := func(items []string, want string) bool {
		for _, item := range items {
			if strings.EqualFold(item, want) {
				return true
			}
		}
		return false
	}
	noNewPrivileges := has(config.SecurityOpt, "no-new-privileges") || has(config.SecurityOpt, "no-new-privileges=true")
	if config.Privileged || config.NetworkMode != "none" || config.PidMode != "private" || config.IpcMode != "private" || config.CgroupnsMode != "private" || !config.ReadonlyRootfs || len(config.CapAdd) != 0 || !has(config.CapDrop, "ALL") || len(config.Binds) != 0 || !noNewPrivileges || config.Memory != 2<<30 || config.MemorySwap != 2<<30 || config.NanoCpus != 2000000000 || config.PidsLimit != 128 {
		return fmt.Errorf("OCI runtime did not enforce required worker configuration")
	}
	allowed := map[string]bool{"/workspace": true, "/output": true, "/home/worker": true, "/tmp": true}
	if len(config.Tmpfs) != len(allowed) {
		return fmt.Errorf("unexpected worker temporary mount configuration")
	}
	for destination, options := range config.Tmpfs {
		if !allowed[destination] {
			return fmt.Errorf("unexpected worker mount %q", destination)
		}
		owner, mode, size := uint64(65534), uint64(0700), uint64(64<<20)
		if destination == "/workspace" {
			mode, size = 0755, 512<<20
			if readOnly {
				owner = 0
			}
		}
		if destination == "/tmp" {
			owner, mode = 0, 01777
		}
		if err := verifyWorkerTmpfs(options, owner, mode, size); err != nil {
			return fmt.Errorf("unsafe worker mount %q: %w", destination, err)
		}
	}
	seenMounts := map[string]bool{}
	for _, mount := range containers[0].Mounts {
		if mount.Type != "tmpfs" || !allowed[mount.Destination] || seenMounts[mount.Destination] {
			return fmt.Errorf("unexpected or duplicate worker mount")
		}
		seenMounts[mount.Destination] = true
	}
	return nil
}

// verifyWorkerTmpfs accepts only the requested options, allowing equivalent
// numeric formatting. Missing, duplicate, contradictory and extra options fail.
func verifyWorkerTmpfs(options string, owner, mode, size uint64) error {
	expected := map[string]uint64{"rw": 1, "nosuid": 1, "nodev": 1, "uid": owner, "gid": owner, "mode": mode, "size": size}
	seen := map[string]bool{}
	for _, option := range strings.Split(options, ",") {
		key, value, hasValue := strings.Cut(option, "=")
		want, allowed := expected[key]
		if !allowed || seen[key] {
			return fmt.Errorf("unexpected or duplicate option %q", key)
		}
		seen[key] = true
		if key == "rw" || key == "nosuid" || key == "nodev" {
			if hasValue {
				return fmt.Errorf("mount flag %q cannot have a value", key)
			}
			continue
		}
		if !hasValue || value == "" {
			return fmt.Errorf("mount option %q requires a value", key)
		}
		base, multiplier := 10, uint64(1)
		if key == "mode" {
			base = 8
		}
		if key == "size" {
			switch value[len(value)-1] {
			case 'k', 'K':
				multiplier = 1 << 10
				value = value[:len(value)-1]
			case 'm', 'M':
				multiplier = 1 << 20
				value = value[:len(value)-1]
			case 'g', 'G':
				multiplier = 1 << 30
				value = value[:len(value)-1]
			}
		}
		actual, err := strconv.ParseUint(value, base, 64)
		if err != nil || actual > ^uint64(0)/multiplier || actual*multiplier != want {
			return fmt.Errorf("unexpected value for mount option %q", key)
		}
	}
	if len(seen) != len(expected) {
		return fmt.Errorf("required mount options are missing")
	}
	return nil
}

func createWorker(ctx context.Context, runtime, image string, readOnly bool) (string, error) {
	if err := inspectWorkerImage(ctx, runtime, image); err != nil {
		return "", err
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return "", err
	}
	name := "swarm-worker-" + hex.EncodeToString(nonce[:])
	workspace := "--tmpfs=/workspace:rw,nosuid,nodev,size=536870912,mode=0755,uid=65534,gid=65534"
	if readOnly {
		workspace = "--tmpfs=/workspace:rw,nosuid,nodev,size=536870912,mode=0755,uid=0,gid=0"
	}
	args := []string{"create", "--name=" + name, "--label=swarmos.worker=true", "--pull=never", "--network=none", "--pid=private", "--ipc=private", "--cgroupns=private", "--read-only", "--cap-drop=ALL", "--security-opt=no-new-privileges", "--cpus=2", "--memory=2g", "--memory-swap=2g", "--pids-limit=128", "--user=0:0", "--workdir=/workspace",
		workspace,
		"--tmpfs=/output:rw,nosuid,nodev,size=67108864,mode=0700,uid=65534,gid=65534",
		"--tmpfs=/home/worker:rw,nosuid,nodev,size=67108864,mode=0700,uid=65534,gid=65534",
		"--tmpfs=/tmp:rw,nosuid,nodev,size=67108864,mode=1777,uid=0,gid=0",
		"--env=HOME=/home/worker", "--env=XDG_CONFIG_HOME=/home/worker/.config", "--env=XDG_CACHE_HOME=/home/worker/.cache",
		"--entrypoint=/bin/sh", image, "-c", "while :; do sleep 3600; done"}
	out, err := workerRun(ctx, runtime, args...)
	if err != nil {
		if cleanupErr := removeWorker(runtime, name); cleanupErr != nil {
			return "", errors.Join(err, cleanupErr)
		}
		return "", err
	}
	id := strings.TrimSpace(string(out))
	if !workerContainerID.MatchString(id) {
		err := fmt.Errorf("OCI returned invalid container identity")
		if cleanupErr := removeWorker(runtime, name); cleanupErr != nil {
			err = errors.Join(err, cleanupErr)
		}
		return "", err
	}
	if err := inspectWorkerContainer(ctx, runtime, id, readOnly); err != nil {
		if cleanupErr := removeWorker(runtime, id); cleanupErr != nil {
			return "", errors.Join(err, cleanupErr)
		}
		return "", err
	}
	return id, nil
}

func verifyWorkerLimits(ctx context.Context, runtime, id string) error {
	// Check the kernel's effective cgroup v2 limits, not merely accepted flags.
	// A rootless runtime without delegation must fail closed here.
	script := `test "$(cat /sys/fs/cgroup/memory.max)" = 2147483648
 test "$(cat /sys/fs/cgroup/memory.swap.max)" = 0
 test "$(cat /sys/fs/cgroup/pids.max)" = 128
 set -- $(cat /sys/fs/cgroup/cpu.max)
 test "$1" != max
 test "$1" -gt 0
 test "$2" -gt 0
 test "$1" -le "$((2 * $2))"`
	if _, err := workerRun(ctx, runtime, "exec", "--user=0:0", id, "/bin/sh", "-ec", script); err != nil {
		return fmt.Errorf("isolated worker requires enforced cgroup v2 resource limits: %w", err)
	}
	return nil
}

func removeWorker(runtime, id string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, err := workerRun(ctx, runtime, "rm", "--force", id)
	if err != nil {
		return fmt.Errorf("worker %s cleanup failed; verify and remove this container with %s: %w", id, runtime, err)
	}
	return err
}

// CheckWorkerRuntime checks the daemon, local image, resource configuration and
// pause capability using an empty disposable worker; it never executes a host
// agent binary. No image is downloaded implicitly.
func CheckWorkerRuntime(ctx context.Context, runtime, image string) (err error) {
	runtime, err = workerRuntime(runtime, image)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	id, err := createWorker(ctx, runtime, image, true)
	if err != nil {
		return err
	}
	defer func() {
		if cleanupErr := removeWorker(runtime, id); cleanupErr != nil {
			err = errors.Join(err, cleanupErr)
		}
	}()
	if _, err := workerRun(ctx, runtime, "start", id); err != nil {
		return err
	}
	if err := verifyWorkerLimits(ctx, runtime, id); err != nil {
		return err
	}
	if _, err := workerRun(ctx, runtime, "pause", id); err != nil {
		return fmt.Errorf("worker runtime cannot freeze descendants: %w", err)
	}
	if _, err := workerRun(ctx, runtime, "unpause", id); err != nil {
		return err
	}
	return nil
}

// RunWorker stages bounded regular files into an isolated container with no
// host mounts or network. After execution it freezes the entire container,
// exports to private scratch, and destroys the container before importing data.
func RunWorker(ctx context.Context, dir, bin string, args []string, opts WorkerOptions, stdin io.Reader, stdout, stderr io.Writer) (err error) {
	if opts.ProjectPlan && (opts.Role != "planner" || !opts.ReadOnly) {
		return fmt.Errorf("project-plan output requires a read-only planner")
	}
	runtime, err := workerRuntime(opts.Runtime, opts.Image)
	if err != nil {
		return err
	}
	ctx, budget := startAgentBudget(ctx, opts)
	defer budget.close()
	defer func() {
		if budget.timedOut() {
			err = &AgentTimeoutError{cause: budget.timeoutError(), diagnostic: "OCI timeout: no patch available"}
		}
	}()
	scratch, err := os.MkdirTemp("", "swarm-worker-*")
	if err != nil {
		return err
	}
	defer func() {
		if e := os.RemoveAll(scratch); err == nil && e != nil {
			err = fmt.Errorf("clean worker scratch: %w", e)
		}
	}()
	original, err := safefs.Open(dir)
	if err != nil {
		return err
	}
	defer original.Close()
	outputs := workerOutputs(opts)
	input := filepath.Join(scratch, "input.tar")
	initial, err := packWorker(ctx, dir, input, outputs, opts.ReadOnly)
	if err != nil {
		return err
	}
	id, err := createWorker(ctx, runtime, opts.Image, opts.ReadOnly)
	if err != nil {
		return err
	}
	removed := false
	defer func() {
		if !removed {
			if e := removeWorker(runtime, id); e != nil {
				err = errors.Join(err, e)
			}
		}
	}()
	if _, err := workerRun(ctx, runtime, "start", id); err != nil {
		return err
	}
	if err := verifyWorkerLimits(ctx, runtime, id); err != nil {
		return err
	}
	archive, err := os.Open(input)
	if err != nil {
		return err
	}
	copyCmd := workerCommand(ctx, runtime, "cp", "--archive", "-", id+":/workspace")
	copyCmd.Stdin = archive
	var diagnostics limitedWorkerBuffer
	copyCmd.Stderr = &diagnostics
	copyErr := copyCmd.Run()
	archive.Close()
	if copyErr != nil {
		return fmt.Errorf("stage isolated worker input: %w: %s", copyErr, diagnostics.String())
	}
	// This fixed setup runs as container root before any worker is started.
	// The image must provide standard /bin/sh, chmod, mkdir, rm and ln tools.
	script := ":"
	if opts.ReadOnly {
		script = "mkdir -p /workspace/.swarm/active"
		for _, name := range outputs {
			script += "; rm -f /workspace/.swarm/active/" + name + "; ln -s /output/" + name + " /workspace/.swarm/active/" + name
		}
		script += "; chmod -R a-w /workspace"
	}
	if _, err := workerRun(ctx, runtime, "exec", "--user=0:0", id, "/bin/sh", "-ec", script); err != nil {
		return err
	}
	runArgs := []string{"exec"}
	if stdin != nil {
		runArgs = append(runArgs, "-i")
	}
	runArgs = append(runArgs, "--user=65534:65534", "--workdir=/workspace", id, bin)
	runArgs = append(runArgs, args...)
	cmd := workerCommand(ctx, runtime, runArgs...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, stdout, stderr
	runErr := cmd.Run()
	paused := false
	interrupted := ctx.Err() != nil && (errors.Is(runErr, context.Canceled) || (cmd.ProcessState != nil && !cmd.ProcessState.Exited()))
	if cleanup, cancel, ok := claimpause.Cleanup(ctx, opts.Role); ok && interrupted && !opts.ReadOnly && !opts.DiscardChanges {
		defer cancel()
		ctx = cleanup
		paused = true
	} else if runErr != nil {
		return fmt.Errorf("isolated worker failed: %w", runErr)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if opts.DiscardChanges {
		if err := removeWorker(runtime, id); err != nil {
			return err
		}
		removed = true
		return ctx.Err()
	}
	if _, err := workerRun(ctx, runtime, "pause", id); err != nil {
		return fmt.Errorf("freeze worker before artifact export: %w", err)
	}
	exported := filepath.Join(scratch, "export.tar")
	out, err := os.OpenFile(exported, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	remote := "/workspace/."
	if opts.ReadOnly {
		remote = "/output/."
	}
	exportCmd := workerCommand(ctx, runtime, "cp", id+":"+remote, "-")
	exportCmd.Stdout = &workerBudgetWriter{writer: out, remaining: workerMaxBytes + (64 << 20)}
	var exportDiagnostics limitedWorkerBuffer
	exportCmd.Stderr = &exportDiagnostics
	exportErr := exportCmd.Run()
	closeErr := out.Close()
	if exportErr != nil {
		return fmt.Errorf("export frozen worker: %w: %s", exportErr, exportDiagnostics.String())
	}
	if closeErr != nil {
		return closeErr
	}
	// Force removal kills all processes, waits for removal, and unmounts tmpfs.
	// Nothing reaches the worktree until this succeeds.
	if err := removeWorker(runtime, id); err != nil {
		return fmt.Errorf("terminate worker before import: %w", err)
	}
	removed = true
	budget.finish()
	if err := ctx.Err(); err != nil {
		return err
	}
	extracted := filepath.Join(scratch, "result")
	if err := os.Mkdir(extracted, 0700); err != nil {
		return err
	}
	manifest, err := unpackWorker(ctx, exported, extracted)
	if err != nil {
		return err
	}
	if opts.ReadOnly {
		destination := original
		if opts.OutputDir != "" {
			destination, err = safefs.Open(opts.OutputDir)
			if err != nil {
				return err
			}
			defer destination.Close()
		}
		for name := range manifest {
			permitted := false
			for _, allowed := range outputs {
				if name == allowed {
					permitted = true
				}
			}
			if !permitted {
				return fmt.Errorf("unexpected isolated role artifact %q", name)
			}
		}
		for _, name := range outputs {
			data, err := safefs.ReadFile(extracted, name, 1<<20)
			if err != nil {
				return fmt.Errorf("isolated role did not produce %s: %w", name, err)
			}
			dest := filepath.Join(".swarm", "active", name)
			if opts.OutputDir != "" {
				dest = name
			}
			if err := destination.WriteFile(dest, data, 0644); err != nil {
				return err
			}
		}
		return nil
	}
	if err := checkImport(ctx, opts, initial, manifest); err != nil {
		return preserveRejected(ctx, opts, input, extracted, err)
	}
	if err := importWorker(ctx, original, extracted, initial, manifest); err != nil {
		return err
	}
	if paused {
		return claimpause.ErrBudget
	}
	return nil
}

// AgentRunLimit is the default agent budget for callers without resolved policy.
// Required-check execution bypasses agent budgeting.
const AgentRunLimit = 60 * time.Minute

func workerOutputs(opts WorkerOptions) []string {
	if !opts.ReadOnly {
		return nil
	}
	if opts.Role == "planner" {
		if opts.ProjectPlan {
			return []string{"PROJECT_PLAN.yaml"}
		}
		return []string{"PLAN.md", "TEST_PLAN.md"}
	}
	return []string{"REVIEW.md"}
}

type workerFile struct {
	digest string
	mode   os.FileMode
}
type workerManifest map[string]workerFile

func workerPath(name string) (string, error) {
	name = strings.TrimPrefix(name, "./")
	if name == "." || name == "" {
		return ".", nil
	}
	if strings.Contains(name, "\\") || path.IsAbs(name) || path.Clean(name) != strings.TrimSuffix(name, "/") {
		return "", fmt.Errorf("unsafe worker archive path %q", name)
	}
	name = strings.TrimSuffix(name, "/")
	if !filepath.IsLocal(filepath.FromSlash(name)) {
		return "", fmt.Errorf("unsafe worker path %q", name)
	}
	parts := strings.Split(name, "/")
	if len(parts) > 64 {
		return "", fmt.Errorf("worker directory depth exceeded")
	}
	for _, part := range parts {
		canonical := strings.ToLower(strings.TrimRight(part, " ."))
		if canonical == ".git" {
			return "", fmt.Errorf("worker cannot import Git metadata")
		}
		if canonical != strings.ToLower(part) || strings.ContainsAny(part, ":<>\"|?*~") {
			return "", fmt.Errorf("worker rejects Windows path aliases")
		}
		for _, character := range part {
			if character < 32 || unicode.Is(unicode.Cf, character) {
				// HFS+ ignores several format characters when comparing names;
				// accepting them could alias protected .git or coordinator paths.
				return "", fmt.Errorf("worker rejects control or format characters in paths")
			}
		}
		stem := strings.SplitN(canonical, ".", 2)[0]
		if stem == "con" || stem == "prn" || stem == "aux" || stem == "nul" || (len(stem) == 4 && (strings.HasPrefix(stem, "com") || strings.HasPrefix(stem, "lpt")) && stem[3] >= '1' && stem[3] <= '9') {
			return "", fmt.Errorf("worker rejects reserved device path")
		}
	}
	for _, prefix := range []string{".swarm/coordination", ".swarm/local-tracker", ".swarm/worktrees", ".swarm/active-output"} {
		if strings.EqualFold(name, prefix) || strings.HasPrefix(strings.ToLower(name), prefix+"/") {
			return "", fmt.Errorf("worker cannot access coordinator state")
		}
	}
	return name, nil
}

// IsEphemeralBuildDir reports whether a directory name is a language build,
// test, or local virtual-environment cache directory that should not be
// archived into or imported out of detached worker snapshots.
func IsEphemeralBuildDir(name string) bool {
	switch name {
	case "node_modules", ".venv", "venv", "__pycache__", ".pytest_cache", ".mypy_cache", ".ruff_cache", ".tox", ".nox", ".gradle", ".next", ".nuxt", ".turbo", ".parcel-cache":
		return true
	default:
		return false
	}
}

func packWorker(ctx context.Context, source, archive string, outputs []string, readOnly bool, excludedDirectories ...string) (workerManifest, error) {
	root, err := safefs.Open(source)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	out, err := os.OpenFile(archive, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return nil, err
	}
	defer out.Close()
	tw := tar.NewWriter(out)
	owner := 65534
	if readOnly {
		owner = 0
	}
	manifest := workerManifest{}
	seen := map[string]bool{}
	entries := 0
	var total int64
	var walk func(string) error
	walk = func(dir string) error {
		in, err := root.OpenDirectory(filepath.FromSlash(dir))
		if err != nil {
			return err
		}
		defer in.Close()
		for {
			children, readErr := in.ReadDir(128)
			if readErr != nil && readErr != io.EOF {
				return readErr
			}
			for _, entry := range children {
				excluded := false
				for _, name := range excludedDirectories {
					if entry.Name() == name {
						excluded = true
					}
				}
				if excluded {
					continue
				}
				if err := ctx.Err(); err != nil {
					return err
				}
				name := path.Join(dir, entry.Name())
				if entry.Name() == ".git" || name == ".swarm/coordination" || name == ".swarm/local-tracker" || name == ".swarm/worktrees" || name == ".swarm/active-output" || (entry.IsDir() && IsEphemeralBuildDir(entry.Name())) {
					continue
				}
				if _, err := workerPath(name); err != nil {
					return err
				}
				canonical := strings.ToLower(name)
				if seen[canonical] {
					return fmt.Errorf("case-colliding worker input path %q", name)
				}
				seen[canonical] = true
				entries++
				if entries > workerMaxEntries {
					return fmt.Errorf("worker input entry limit exceeded")
				}
				if entry.IsDir() {
					if err := tw.WriteHeader(&tar.Header{Name: name + "/", Typeflag: tar.TypeDir, Mode: 0755, Uid: owner, Gid: owner}); err != nil {
						return err
					}
					if err := walk(name); err != nil {
						return err
					}
					continue
				}
				skip := false
				if entry.Type()&os.ModeSymlink != 0 {
					// RunReview supplies this sole, coordinator-created output channel.
					if name == ".swarm/active/REVIEW.md" {
						for _, artifact := range outputs {
							if artifact == "REVIEW.md" {
								link, err := os.Readlink(filepath.Join(source, filepath.FromSlash(name)))
								skip = err == nil && filepath.Clean(link) == filepath.Join(source, ".swarm", "active-output", "REVIEW.md")
							}
						}
					}
					if skip {
						continue
					}
					return fmt.Errorf("isolated worker input rejects link %q", name)
				}
				f, err := root.OpenRegular(filepath.FromSlash(name))
				if err != nil {
					return err
				}
				info, err := f.Stat()
				if err != nil {
					f.Close()
					return err
				}
				if info.Size() > workerFileBytes || total+info.Size() > workerMaxBytes {
					f.Close()
					return fmt.Errorf("worker input byte limit exceeded")
				}
				mode := info.Mode().Perm() & 0755
				if readOnly {
					mode = 0644
					if info.Mode().Perm()&0111 != 0 {
						mode = 0755
					}
				}
				if err := tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: int64(mode), Size: info.Size(), Uid: owner, Gid: owner}); err != nil {
					f.Close()
					return err
				}
				hash := sha256.New()
				n, copyErr := io.CopyN(io.MultiWriter(tw, hash), &workerContextReader{ctx: ctx, reader: f}, info.Size())
				after, statErr := f.Stat()
				closeErr := f.Close()
				if statErr != nil {
					return statErr
				}
				if !os.SameFile(info, after) || info.Size() != after.Size() || !info.ModTime().Equal(after.ModTime()) {
					return fmt.Errorf("worker input changed while copying: %s", name)
				}
				if copyErr != nil {
					return copyErr
				}
				if closeErr != nil {
					return closeErr
				}
				total += n
				manifest[name] = workerFile{digest: hex.EncodeToString(hash.Sum(nil)), mode: info.Mode().Perm() & 0755}
			}
			if readErr == io.EOF {
				return nil
			}
		}
	}
	if err := walk("."); err != nil {
		return nil, err
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	return manifest, out.Close()
}

func unpackWorker(ctx context.Context, archive, target string) (workerManifest, error) {
	in, err := os.Open(archive)
	if err != nil {
		return nil, err
	}
	defer in.Close()
	tr := tar.NewReader(&workerContextReader{ctx: ctx, reader: in})
	root, err := safefs.Open(target)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	manifest := workerManifest{}
	seen := map[string]bool{}
	entries := 0
	var total int64
	for {
		header, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return manifest, nil
		}
		if err != nil {
			return nil, err
		}
		entries++
		if entries > workerMaxEntries {
			return nil, fmt.Errorf("worker output entry limit exceeded")
		}
		name, err := workerPath(header.Name)
		if err != nil {
			return nil, err
		}
		if name == "." && header.Typeflag == tar.TypeDir {
			continue
		}
		if seen[strings.ToLower(name)] {
			return nil, fmt.Errorf("duplicate worker output path %q", name)
		}
		seen[strings.ToLower(name)] = true
		switch header.Typeflag {
		case tar.TypeDir:
			if err := root.MkdirAll(filepath.FromSlash(name), 0700); err != nil {
				return nil, err
			}
		case tar.TypeReg:
			if header.Size < 0 || header.Size > workerFileBytes || total+header.Size > workerMaxBytes {
				return nil, fmt.Errorf("worker output byte limit exceeded")
			}
			data, err := io.ReadAll(io.LimitReader(tr, header.Size+1))
			if err != nil {
				return nil, err
			}
			if int64(len(data)) != header.Size {
				return nil, fmt.Errorf("worker output length mismatch")
			}
			mode := os.FileMode(header.Mode) & 0755
			if err := root.WriteFile(filepath.FromSlash(name), data, mode); err != nil {
				return nil, err
			}
			digest := sha256.Sum256(data)
			manifest[name] = workerFile{digest: hex.EncodeToString(digest[:]), mode: mode}
			total += header.Size
		default:
			return nil, fmt.Errorf("worker output rejects links and special files %q", name)
		}
	}
}

func importWorker(ctx context.Context, dest *safefs.Root, source string, initial, final workerManifest) error {
	// Check all original files and new destinations before changing any worktree
	// data. Root operations remain confined if an external process races import.
	for name, before := range initial {
		if err := ctx.Err(); err != nil {
			return err
		}
		data, err := dest.ReadFile(filepath.FromSlash(name), workerFileBytes)
		if err != nil {
			return fmt.Errorf("worktree changed during worker execution: %w", err)
		}
		digest := sha256.Sum256(data)
		if hex.EncodeToString(digest[:]) != before.digest {
			return fmt.Errorf("worktree changed during worker execution: %s", name)
		}
	}
	for name := range final {
		if _, existed := initial[name]; existed {
			continue
		}
		file, err := dest.OpenRegular(filepath.FromSlash(name))
		if err == nil {
			file.Close()
			return fmt.Errorf("new worker file conflicts with worktree: %s", name)
		}
		if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	for name, after := range final {
		if err := ctx.Err(); err != nil {
			return err
		}
		if before, existed := initial[name]; existed && before == after {
			continue
		}
		data, err := safefs.ReadFile(source, filepath.FromSlash(name), workerFileBytes)
		if err != nil {
			return err
		}
		if err := dest.WriteFile(filepath.FromSlash(name), data, after.mode); err != nil {
			return err
		}
	}
	for name := range initial {
		if _, exists := final[name]; exists {
			continue
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := dest.Remove(filepath.FromSlash(name)); err != nil {
			return err
		}
	}
	return nil
}

type workerBudgetWriter struct {
	writer    io.Writer
	remaining int64
}

func (w *workerBudgetWriter) Write(p []byte) (int, error) {
	if int64(len(p)) > w.remaining {
		return 0, fmt.Errorf("worker export archive exceeds limit")
	}
	n, err := w.writer.Write(p)
	w.remaining -= int64(n)
	return n, err
}

type workerContextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r *workerContextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}
