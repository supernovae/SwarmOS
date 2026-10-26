package agent

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/supernovae/swarmos/internal/filelock"
	"github.com/supernovae/swarmos/internal/gitcredential"
	"github.com/supernovae/swarmos/internal/process"
	"github.com/supernovae/swarmos/internal/safefs"
)

// Failed-run transcripts are kept only in a coordinator-private state
// directory. Bounds are fixed so a failing harness cannot fill the disk.
const (
	diagnosticsMaxBytes  = 64 << 10
	diagnosticsMaxAge    = 7 * 24 * time.Hour
	diagnosticsMaxCount  = 20
	diagnosticsScanLimit = 4096
	diagnosticsTruncated = "[swarm: earlier output truncated]\n"
)

var diagnosticsFileName = regexp.MustCompile(`^failure-[0-9a-f]{32}\.log$`)

// Test seams; production code never reassigns them.
var (
	diagnosticsStateDir  = userStateDir
	diagnosticsNow       = time.Now
	diagnosticsNewName   = randomDiagnosticsName
	diagnosticsRedaction = gitcredential.NewRedaction
)

var errNoDiagnostics = errors.New("no harness output")

// diagnosticsUnavailable is the fixed reason reported when no private log can
// be written. Underlying errors may carry paths or provider text, so only these
// literals ever reach the operator.
type diagnosticsUnavailable string

const (
	unavailableStateDir  diagnosticsUnavailable = "state directory unavailable"
	unavailableUnsafe    diagnosticsUnavailable = "unsafe state directory"
	unavailablePrivate   diagnosticsUnavailable = "private directory unavailable"
	unavailableRedaction diagnosticsUnavailable = "credential redaction unavailable"
	unavailableWrite     diagnosticsUnavailable = "log write failed"
)

func (r diagnosticsUnavailable) Error() string { return string(r) }

func newDiagnosticsTail() *process.Tail { return process.NewTail(diagnosticsMaxBytes) }

// failureDiagnosticsNote saves the transcript and returns coordinator-rendered
// text naming only the saved file or a fixed unavailable reason. Storage errors
// are never interpolated.
func failureDiagnosticsNote(worktree string, transcript *process.Tail) string {
	path, size, err := saveDiagnostics(worktree, transcript)
	var reason diagnosticsUnavailable
	switch {
	case errors.Is(err, errNoDiagnostics):
		return "no harness output to save"
	case errors.As(err, &reason):
		return "diagnostics unavailable: " + string(reason)
	case err != nil:
		return "diagnostics unavailable: " + string(unavailableWrite)
	}
	return fmt.Sprintf("diagnostics saved to %s (%d bytes)", path, size)
}

func saveDiagnostics(worktree string, transcript *process.Tail) (string, int, error) {
	raw, truncated := transcript.Bytes()
	if len(raw) == 0 {
		return "", 0, errNoDiagnostics
	}
	redaction, err := diagnosticsRedaction()
	if err != nil {
		return "", 0, unavailableRedaction
	}
	data := renderDiagnostics(raw, truncated, redaction)
	dirPath, dir, err := openDiagnosticsDir(worktree)
	if err != nil {
		return "", 0, err
	}
	defer dir.Close()
	lock, err := dir.OpenLock(".lock")
	if err != nil {
		return "", 0, unavailableWrite
	}
	defer lock.Close()
	if err := filelock.LockExclusive(lock); err != nil {
		return "", 0, unavailableWrite
	}
	defer filelock.Unlock(lock)
	name := diagnosticsNewName()
	if !diagnosticsFileName.MatchString(name) {
		return "", 0, unavailableWrite
	}
	if err := dir.WriteFile(name, data, 0o600); err != nil {
		return "", 0, unavailableWrite
	}
	pruneDiagnostics(dir, name, diagnosticsNow())
	return filepath.Join(dirPath, name), len(data), nil
}

// renderDiagnostics redacts before cutting so a credential split by the tail
// boundary cannot survive, then escapes terminal controls into printable text.
func renderDiagnostics(raw []byte, truncated bool, redaction *gitcredential.Redaction) []byte {
	text := redaction.Redact(string(raw))
	if truncated {
		// Any credential fragment at the cut point is shorter than MaxLen.
		text = text[min(len(text), max(redaction.MaxLen()-1, 0)):]
		if newline := strings.IndexByte(text, '\n'); newline >= 0 {
			text = text[newline+1:]
		}
	}
	encoded := escapeDiagnostics(text)
	if limit := diagnosticsMaxBytes - len(diagnosticsTruncated); truncated || len(encoded) > diagnosticsMaxBytes {
		if len(encoded) > limit {
			encoded = encoded[len(encoded)-limit:]
			for len(encoded) > 0 && !utf8.RuneStart(encoded[0]) {
				encoded = encoded[1:]
			}
		}
		encoded = diagnosticsTruncated + encoded
	}
	return []byte(encoded)
}

// escapeDiagnostics keeps line breaks and tabs readable and renders every other
// control, format, bidi, or invalid byte as an inert escape sequence.
func escapeDiagnostics(text string) string {
	var b strings.Builder
	for i := 0; i < len(text); {
		r, size := utf8.DecodeRuneInString(text[i:])
		switch {
		case r == utf8.RuneError && size == 1:
			fmt.Fprintf(&b, `\x%02x`, text[i])
		case r == '\n' || r == '\t':
			b.WriteRune(r)
		case r == '\\':
			b.WriteString(`\\`)
		case r < 0x80 && !unicode.IsPrint(r):
			fmt.Fprintf(&b, `\x%02x`, r)
		case !unicode.IsPrint(r):
			fmt.Fprintf(&b, `\u%04x`, r)
		default:
			b.WriteRune(r)
		}
		i += size
	}
	return b.String()
}

func userStateDir() (string, error) {
	if dir := os.Getenv("XDG_STATE_HOME"); filepath.IsAbs(dir) {
		return dir, nil
	}
	switch runtime.GOOS {
	case "windows", "darwin", "ios", "plan9":
		return os.UserConfigDir()
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "state"), nil
}

// openDiagnosticsDir pins <state>/swarm/diagnostics, creating missing
// directories at 0700 without following symlinks, and refuses a location that
// is shared with other users or inside the agent's worktree.
func openDiagnosticsDir(worktree string) (string, *safefs.Root, error) {
	state, err := diagnosticsStateDir()
	if err != nil {
		return "", nil, unavailableStateDir
	}
	if !filepath.IsAbs(state) || !printablePath(state) {
		return "", nil, unavailableUnsafe
	}
	state = filepath.Clean(state)
	existing, missing := state, []string{}
	for {
		info, err := os.Stat(existing)
		if err == nil {
			if !info.IsDir() {
				return "", nil, unavailableUnsafe
			}
			break
		}
		parent := filepath.Dir(existing)
		if !errors.Is(err, os.ErrNotExist) || parent == existing {
			return "", nil, unavailableUnsafe
		}
		missing = append([]string{filepath.Base(existing)}, missing...)
		existing = parent
	}
	rel := filepath.Join(append(missing, "swarm", "diagnostics")...)
	dirPath := filepath.Join(existing, rel)
	if resolved, err := filepath.EvalSymlinks(existing); err != nil || insideWorktree(worktree, dirPath) || insideWorktree(worktree, filepath.Join(resolved, rel)) {
		return "", nil, unavailableUnsafe
	}
	base, err := safefs.Open(existing)
	if err != nil {
		return "", nil, unavailablePrivate
	}
	defer base.Close()
	dir, err := base.Sub(rel, true, 0o700)
	if err != nil {
		return "", nil, unavailablePrivate
	}
	if err := privateDirectory(dir); err != nil {
		dir.Close()
		return "", nil, unavailablePrivate
	}
	return dirPath, dir, nil
}

func privateDirectory(dir *safefs.Root) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	f, err := dir.OpenDirectory(".")
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("diagnostics directory is accessible to other users")
	}
	return nil
}

func insideWorktree(worktree, path string) bool {
	if worktree == "" {
		return false
	}
	candidates := []string{worktree}
	if resolved, err := filepath.EvalSymlinks(worktree); err == nil {
		candidates = append(candidates, resolved)
	}
	for _, root := range candidates {
		if rel, err := filepath.Rel(root, path); err == nil && (rel == "." || filepath.IsLocal(rel)) {
			return true
		}
	}
	return false
}

// printablePath keeps an environment-derived path from injecting terminal
// controls into the coordinator's error text.
func printablePath(path string) bool {
	if !utf8.ValidString(path) {
		return false
	}
	for _, r := range path {
		if !unicode.IsPrint(r) {
			return false
		}
	}
	return true
}

// pruneDiagnostics removes logs older than diagnosticsMaxAge and the oldest
// beyond diagnosticsMaxCount. It only removes matching regular files and is
// best effort: a pruning failure never hides the saved log or the run error.
func pruneDiagnostics(dir *safefs.Root, keep string, now time.Time) {
	f, err := dir.OpenDirectory(".")
	if err != nil {
		return
	}
	defer f.Close()
	var kept []os.FileInfo
	for scanned := 0; scanned < diagnosticsScanLimit; {
		entries, err := f.ReadDir(256)
		scanned += len(entries)
		for _, entry := range entries {
			name := entry.Name()
			if name == keep || !diagnosticsFileName.MatchString(name) || !entry.Type().IsRegular() {
				continue
			}
			info, err := entry.Info()
			if err != nil || !info.Mode().IsRegular() {
				continue
			}
			if now.Sub(info.ModTime()) > diagnosticsMaxAge {
				_ = dir.Remove(name)
				continue
			}
			kept = append(kept, info)
		}
		if err != nil {
			if !errors.Is(err, io.EOF) {
				return
			}
			break
		}
	}
	sort.Slice(kept, func(i, j int) bool {
		if !kept[i].ModTime().Equal(kept[j].ModTime()) {
			return kept[i].ModTime().After(kept[j].ModTime())
		}
		return kept[i].Name() > kept[j].Name()
	})
	// The just-saved log counts toward the limit and is always retained.
	for _, info := range kept[min(len(kept), diagnosticsMaxCount-1):] {
		_ = dir.Remove(info.Name())
	}
}

func randomDiagnosticsName() string {
	var random [16]byte
	_, _ = rand.Read(random[:])
	return "failure-" + hex.EncodeToString(random[:]) + ".log"
}
