package safefs

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// InstructionNames are the agent instruction entrypoints. They are the only
// names safefs lets resolve through a symlink, and only to a regular file that
// stays inside the root at every hop.
var InstructionNames = []string{"AGENTS.md", "CLAUDE.md"}

// maxInstructionHops bounds link chains, so cycles fail instead of looping.
const maxInstructionHops = 40

// Instruction is a resolved agent instruction entrypoint.
type Instruction struct {
	// Name is the entrypoint, AGENTS.md or CLAUDE.md.
	Name string
	// Target is the root-relative regular file that Name resolves to. It equals
	// Name unless Name is a symlink, and contains no symlink components.
	Target string
	// Link reports whether Name is a symlink.
	Link bool
	// Exists is false only when Name is absent; a dangling link is an error.
	Exists bool
}

// ResolveInstructions resolves every instruction entrypoint, so callers can
// refuse an unsafe link before changing any of them.
func (r *Root) ResolveInstructions() ([]Instruction, error) {
	resolved := make([]Instruction, 0, len(InstructionNames))
	for _, name := range InstructionNames {
		in, err := r.ResolveInstruction(name)
		if err != nil {
			return nil, err
		}
		resolved = append(resolved, in)
	}
	return resolved, nil
}

// ResolveInstruction follows name one component at a time. Each link is read
// relative to the directory holding it, and the chain is refused if it leaves
// the root (even briefly), uses an absolute path, enters .git or .swarm, is
// dangling or cyclic, or ends at anything but a regular file. Read and write
// the returned Target with the ordinary strict methods, which reject any link
// substituted after resolution.
func (r *Root) ResolveInstruction(name string) (Instruction, error) {
	known := false
	for _, n := range InstructionNames {
		known = known || n == name
	}
	if !known {
		return Instruction{}, fmt.Errorf("%q is not an agent instruction file", name)
	}
	in := Instruction{Name: name, Target: name}
	refuse := func(format string, args ...any) (Instruction, error) {
		return Instruction{}, fmt.Errorf("refuse instruction file %s: "+format+"; it must be a regular file or a relative link to a regular file inside the repository", append([]any{name}, args...)...)
	}
	var resolved []string
	pending := []string{name}
	hops := 0
	for len(pending) > 0 {
		part := pending[0]
		pending = pending[1:]
		switch part {
		case "", ".":
			continue
		case "..":
			if len(resolved) == 0 {
				return refuse("its link leaves the repository")
			}
			resolved = resolved[:len(resolved)-1]
			continue
		}
		parts := append(append([]string{}, resolved...), part)
		current := filepath.Join(parts...)
		if top := strings.ToLower(parts[0]); top == ".git" || top == ".swarm" {
			return refuse("its link points into %s", top)
		}
		info, err := r.root.Lstat(current)
		if errors.Is(err, os.ErrNotExist) && hops == 0 && len(pending) == 0 {
			return in, nil
		}
		if errors.Is(err, os.ErrNotExist) {
			return refuse("its link target %s does not exist", filepath.ToSlash(current))
		}
		if err != nil {
			return Instruction{}, err
		}
		mode := info.Mode()
		switch {
		case mode&os.ModeSymlink != 0:
			if hops++; hops > maxInstructionHops {
				return refuse("its links nest too deeply or form a cycle")
			}
			target, err := r.root.Readlink(current)
			if err != nil {
				return Instruction{}, err
			}
			if target == "" || filepath.IsAbs(target) || filepath.VolumeName(target) != "" || strings.HasPrefix(filepath.ToSlash(target), "/") {
				return refuse("its link target %q is not a relative path", target)
			}
			pending = append(strings.Split(filepath.ToSlash(target), "/"), pending...)
		case mode.IsDir():
			if len(pending) == 0 {
				return refuse("it resolves to the directory %s", filepath.ToSlash(current))
			}
			resolved = append(resolved, part)
		case mode.IsRegular():
			if len(pending) != 0 {
				return refuse("its link passes through the file %s", filepath.ToSlash(current))
			}
			resolved = append(resolved, part)
		default:
			return refuse("it resolves to the special file %s", filepath.ToSlash(current))
		}
	}
	if len(resolved) == 0 {
		return refuse("it resolves to the repository root")
	}
	in.Target = filepath.Join(resolved...)
	in.Link = hops > 0
	in.Exists = true
	return in, nil
}
