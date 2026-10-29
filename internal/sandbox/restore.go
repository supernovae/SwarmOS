package sandbox

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/supernovae/swarmos/internal/process"
	"github.com/supernovae/swarmos/internal/safefs"
)

// RestoreRequest is a trusted coordinator input, not candidate configuration.
// Source is detached before launch. CacheRoot must belong to RepositoryRoot;
// neither directory is mounted. Only a fresh private result directory is writable.
// No production agent or validator calls this dormant prerequisite.
type RestoreRequest struct {
	Source, RepositoryRoot, CacheRoot string
	Registries, Argv                  []string
}

// RestoreBoundary requires an explicitly supplied trusted helper executable.
// The caller verifies its provenance; this package checks its filesystem shape.
type RestoreBoundary struct {
	Helper string
	// Unexported fixture seam; external callers always use verified public upstreams.
	configureBroker func(*registryBroker)
}

// Run returns a private cache only after every transport and payload check succeeds.
// The caller owns removal of a successful result. Failed results are removed.
func (boundary RestoreBoundary) Run(ctx context.Context, request RestoreRequest) (result string, retErr error) {
	return boundary.run(ctx, request, false)
}

// CheckRestoreBoundary runs the identical launcher, broker and helper preflight,
// without executing caller code or contacting a public registry.
func CheckRestoreBoundary(ctx context.Context, boundary RestoreBoundary, request RestoreRequest) error {
	_, err := boundary.run(ctx, request, true)
	return err
}

func (boundary RestoreBoundary) run(ctx context.Context, request RestoreRequest, checkOnly bool) (result string, retErr error) {
	var output string
	var discard func() error
	var outputHandle *os.Root
	defer func() {
		if retErr != nil {
			result = ""
			if discard != nil {
				retErr = errors.Join(retErr, discard())
			}
		}
		if outputHandle != nil {
			retErr = errors.Join(retErr, outputHandle.Close())
			if retErr != nil {
				result = ""
			}
		}
	}()
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	policy, err := newRegistryPolicy(request.Registries)
	if err != nil {
		return "", err
	}
	if !checkOnly && (len(request.Argv) == 0 || !filepath.IsAbs(request.Argv[0])) {
		return "", errors.New("restore payload requires absolute argv")
	}
	source, err := openRestoreDirectory(request.Source)
	if err != nil {
		return "", fmt.Errorf("restore source: %w", err)
	}
	defer source.Close()
	repo, err := openRestoreDirectory(request.RepositoryRoot)
	if err != nil {
		return "", fmt.Errorf("restore repository: %w", err)
	}
	defer repo.Close()
	rel, err := filepath.Rel(request.RepositoryRoot, request.CacheRoot)
	if err != nil || rel == "." || !filepath.IsLocal(rel) || restoreOverlap(request.Source, request.CacheRoot) {
		return "", errors.New("restore cache must be disjoint and repository-owned")
	}
	cache, err := repo.Sub(rel, false, 0)
	if err != nil {
		return "", fmt.Errorf("restore cache root: %w", err)
	}
	defer cache.Close()
	for _, root := range []string{request.RepositoryRoot, request.CacheRoot} {
		if err := validateRestoreRoot(root, os.Getenv("HOME")); err != nil {
			return "", err
		}
	}

	helper, err := openRestoreRegular(boundary.Helper)
	if err != nil {
		return "", fmt.Errorf("restore helper unavailable: %w", err)
	}
	defer helper.Close()
	info, err := helper.Stat()
	if err != nil || info.Mode().Perm()&0111 == 0 {
		return "", errors.New("restore helper is not executable")
	}
	temporary, err := openRestoreDirectory(os.TempDir())
	if err != nil {
		return "", fmt.Errorf("restore temporary root: %w", err)
	}
	defer temporary.Close()
	controlHandle, controlName, err := createRestoreOutput(temporary, os.TempDir())
	if err != nil {
		return "", err
	}
	defer func() { retErr = errors.Join(retErr, controlHandle.Close()) }()
	defer func() { retErr = errors.Join(retErr, controlHandle.RemoveAll(controlName)) }()
	control := filepath.Join(os.TempDir(), controlName)
	if restoreOverlap(control, request.Source) || restoreOverlap(control, request.CacheRoot) {
		return "", errors.New("restore control overlap")
	}
	pinned, err := openRestoreDirectory(control)
	if err != nil {
		return "", err
	}
	defer pinned.Close()
	for _, dir := range []string{"source", "trust/empty", "socket"} {
		if err = pinned.MkdirAll(dir, 0700); err != nil {
			return "", err
		}
	}
	snapshot, err := pinned.Sub("source", false, 0)
	if err != nil {
		return "", err
	}
	err = copyRestoreSource(source, snapshot, "", new(int64))
	closeErr := snapshot.Close()
	if err = errors.Join(err, closeErr); err != nil {
		return "", err
	}
	data, err := io.ReadAll(io.LimitReader(helper, 64<<20+1))
	after, statErr := helper.Stat()
	if statErr != nil || !os.SameFile(info, after) || info.Size() != after.Size() || !info.ModTime().Equal(after.ModTime()) {
		return "", errors.New("restore helper changed while reading")
	}
	if err != nil || len(data) > 64<<20 {
		return "", errors.New("restore helper size/read failure")
	}
	if err = pinned.WriteFile("helper", data, 0500); err != nil {
		return "", err
	}
	hosts := []string{restoreCanaryHost}
	for host := range policy {
		hosts = append(hosts, host)
	}
	trust, err := newRestoreTrust(hosts)
	if err != nil {
		return "", err
	}
	if err = pinned.WriteFile("trust/ca.pem", trust.pem, 0400); err != nil {
		return "", err
	}
	if err = pinned.WriteFile("trust/ca.jks", trust.jks, 0400); err != nil {
		return "", err
	}
	// Exclusive randomized subdirectory; only this directory crosses the boundary.
	var outputName string
	outputHandle, outputName, err = createRestoreOutput(cache, request.CacheRoot)
	output = filepath.Join(request.CacheRoot, outputName)
	if err != nil {
		return "", err
	}
	discard = func() error { return outputHandle.RemoveAll(outputName) }

	success := false
	defer func() {
		if !success || checkOnly {
			retErr = errors.Join(retErr, discard())
		}
	}()
	outputRoot, err := openRestoreDirectory(output)
	if err != nil {
		return "", err
	}
	for _, name := range []string{"home", "tmp"} {
		if err = outputRoot.MkdirAll(name, 0700); err != nil {
			outputRoot.Close()
			return "", err
		}
	}
	if err = outputRoot.Close(); err != nil {
		return "", err
	}
	socket := filepath.Join(control, "socket", "broker.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		return "", fmt.Errorf("restore broker unavailable: %w", err)
	}
	broker := newRegistryBroker(ctx, listener, policy, trust)
	if boundary.configureBroker != nil {
		boundary.configureBroker(broker)
	}
	broker.preflight.Store(true)
	broker.serve()
	defer func() {
		retErr = errors.Join(retErr, broker.close())
		if retErr != nil {
			result = ""
			retErr = errors.Join(retErr, discard())
		}
	}()
	// A reachable host-side positive control makes direct-network denial meaningful.
	positive, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return "", fmt.Errorf("restore network control unavailable: %w", err)
	}
	defer positive.Close()
	c, err := net.DialTimeout("tcp4", positive.Addr().String(), time.Second)
	if err != nil {
		return "", fmt.Errorf("restore positive network control failed: %w", err)
	}
	_ = c.Close()
	readyRead, readyWrite, err := os.Pipe()
	if err != nil {
		return "", err
	}
	defer readyRead.Close()
	defer readyWrite.Close()
	ackRead, ackWrite, err := os.Pipe()
	if err != nil {
		return "", err
	}
	defer ackRead.Close()
	defer ackWrite.Close()
	argv := request.Argv
	if checkOnly {
		argv = nil
	}
	cmd, err := restoreCommand(ctx, control, output, positive.Addr().String(), argv)
	if err != nil {
		return "", err
	}
	cmd.Stdin = ackRead
	cmd.Stdout = readyWrite
	// Payload output is deliberately not copied to coordinator diagnostics.
	var diagnostics process.Output
	cmd.Stderr = &diagnostics
	if err = cmd.Start(); err != nil {
		return "", fmt.Errorf("restore launcher unavailable: %w", err)
	}
	_ = readyWrite.Close()
	_ = ackRead.Close()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	defer func() { retErr = errors.Join(retErr, process.StopVerified(cmd)) }()
	ready := make(chan string, 1)
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		ready <- readRestoreReadyFrame(readyRead)
	}()
	defer func() {
		_ = readyRead.Close()
		<-readerDone
	}()
	timer := time.NewTimer(20 * time.Second)
	defer timer.Stop()
	select {
	case status := <-ready:
		if status != "READY\n" {
			_ = process.StopVerified(cmd)
			<-done
			return "", fmt.Errorf("restore capability unavailable: %s", safeRestoreStatus(status))
		}
	case <-done:
		_ = readyRead.Close()
		status := <-ready
		if status != "" && status != "READY\n" {
			return "", fmt.Errorf("restore capability unavailable: %s", safeRestoreStatus(status))
		}
		return "", errors.New(classifyRestoreLauncher(diagnostics.String()))
	case <-broker.ctx.Done():
		_ = process.StopVerified(cmd)
		<-done
		return "", errors.New("restore preflight broker failed")
	case <-ctx.Done():
		_ = process.StopVerified(cmd)
		<-done
		return "", ctx.Err()
	case <-timer.C:
		_ = process.StopVerified(cmd)
		<-done
		return "", errors.New("restore helper readiness timeout")
	}
	broker.preflight.Store(false)
	if _, err = ackWrite.Write([]byte("GO\n")); err != nil {
		_ = process.StopVerified(cmd)
		<-done
		return "", errors.New("restore helper acknowledgement failed")
	}
	_ = ackWrite.Close()
	select {
	case err := <-done:
		if err != nil {
			return "", errors.New("restore payload/helper failed")
		}
	case <-broker.ctx.Done():
		_ = process.StopVerified(cmd)
		<-done
		return "", errors.New("restore broker failed")
	case <-ctx.Done():
		_ = process.StopVerified(cmd)
		<-done
		return "", ctx.Err()
	}
	if err = broker.close(); err != nil {
		return "", err
	}
	success = true
	if checkOnly {
		return "", nil
	}
	return output, nil
}

// readRestoreReadyFrame reads one bounded control frame, without requiring EOF:
// the launcher may retain a writer while the helper waits for our acknowledgement.
// Incomplete and oversized frames cannot equal any accepted status.
func readRestoreReadyFrame(r io.Reader) string {
	var frame [256]byte
	for i := range frame {
		if _, err := io.ReadFull(r, frame[i:i+1]); err != nil {
			return ""
		}
		if frame[i] == '\n' {
			return string(frame[:i+1])
		}
	}
	return ""
}

func safeRestoreStatus(s string) string {
	switch s {
	case "NETWORK\n":
		return "direct-network canary"
	case "FILESYSTEM\n":
		return "filesystem canary"
	case "BROKER\n":
		return "CONNECT/TLS broker canary"
	case "LISTENER\n":
		return "loopback listener"
	case "TRUST\n":
		return "public trust artifacts"
	default:
		return "helper startup/control framing"
	}
}
func restoreOverlap(a, b string) bool { return a == b || isSubpath(a, b) || isSubpath(b, a) }
func openRestoreDirectory(path string) (*safefs.Root, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || path == "/" {
		return nil, errors.New("restore requires canonical absolute directory")
	}
	root, err := safefs.Open(string(filepath.Separator))
	if err != nil {
		return nil, err
	}
	defer root.Close()
	return root.Sub(strings.TrimPrefix(path, string(filepath.Separator)), false, 0)
}
func openRestoreRegular(path string) (*os.File, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, errors.New("restore requires canonical absolute file")
	}
	parent, err := openRestoreDirectory(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	defer parent.Close()
	return parent.OpenRegular(filepath.Base(path))
}
func copyRestoreSource(src, dst *safefs.Root, prefix string, total *int64) error {
	if strings.Count(prefix, string(filepath.Separator)) > 128 {
		return errors.New("restore source nesting limit")
	}
	name := prefix
	if name == "" {
		name = "."
	}
	dir, err := src.OpenDirectory(name)
	if err != nil {
		return err
	}
	defer dir.Close()
	for {
		entries, err := dir.ReadDir(128)
		if err != nil && err != io.EOF {
			return err
		}
		for _, entry := range entries {
			*total += 4096
			if *total > 256<<20 {
				return errors.New("restore source entry budget")
			}
			if strings.EqualFold(entry.Name(), ".git") {
				return errors.New("restore source contains Git metadata")
			}
			path := filepath.Join(prefix, entry.Name())
			info, err := entry.Info()
			if err != nil {
				return err
			}
			if info.IsDir() {
				if err = dst.MkdirAll(path, 0700); err != nil {
					return err
				}
				if err = copyRestoreSource(src, dst, path, total); err != nil {
					return err
				}
				continue
			}
			if !info.Mode().IsRegular() {
				return errors.New("restore source contains non-regular entry")
			}
			data, err := src.ReadFile(path, 64<<20)
			if err != nil {
				return err
			}
			*total += int64(len(data)) + 1
			if *total > 256<<20 {
				return errors.New("restore source exceeds snapshot budget")
			}
			mode := os.FileMode(0400)
			if info.Mode()&0111 != 0 {
				mode = 0500
			}
			if err = dst.WriteFile(path, data, mode); err != nil {
				return err
			}
		}
		if err == io.EOF {
			return nil
		}
	}
}

func classifyRestoreLauncher(s string) string {
	switch {
	case strings.Contains(s, "namespace"):
		return "restore capability unavailable: Bubblewrap namespaces"
	case strings.Contains(s, "mount") || strings.Contains(s, "bind"):
		return "restore capability unavailable: Bubblewrap mounts"
	default:
		return "restore capability unavailable: helper startup/control framing"
	}
}

// Create output through a pinned root, verifying it against the safefs handle
// before the first mutation. Removal uses the same handle, even after renames.
func createRestoreOutput(pinned *safefs.Root, path string) (*os.Root, string, error) {
	expected, err := pinned.OpenDirectory(".")
	if err != nil {
		return nil, "", err
	}
	defer expected.Close()
	root, err := os.OpenRoot(path)
	if err != nil {
		return nil, "", err
	}
	before, e1 := expected.Stat()
	after, e2 := root.Stat(".")
	if e1 != nil || e2 != nil || !os.SameFile(before, after) {
		root.Close()
		return nil, "", errors.New("restore cache root changed")
	}
	var random [16]byte
	if _, err = rand.Read(random[:]); err != nil {
		root.Close()
		return nil, "", err
	}
	name := "restore-" + hex.EncodeToString(random[:])
	if err = root.Mkdir(name, 0700); err != nil {
		root.Close()
		return nil, "", err
	}
	return root, name, nil
}

// validateRestoreRoot permits private repository children of home directories,
// while denying home roots themselves and system trees.
func validateRestoreRoot(root, home string) error {
	for _, system := range []string{"/usr", "/etc", "/proc", "/sys", "/dev", "/run", "/boot"} {
		if root == system || isSubpath(root, system) {
			return errors.New("restore system root denied")
		}
	}
	for _, denied := range []string{"/", "/tmp", "/var", "/home", "/root", home} {
		if denied != "" && root == denied {
			return errors.New("restore home/system root denied")
		}
	}
	return nil
}
