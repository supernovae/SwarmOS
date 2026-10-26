package agent

import (
	"os"
	"path/filepath"
	"strings"
)

// restrictedEnv constructs a credential-free environment. It deliberately does
// not inherit HOME, XDG, SSH, Git, or token variables; model credentials are
// admitted only by their exact configured names.
func restrictedEnv(allowed []string, home string) []string {
	return restrictedEnvWithLookup(allowed, home, nil)
}

// restrictedEnvWithLookup is restrictedEnv reading the base and allowlisted
// names through lookup; nil reads the process environment.
func restrictedEnvWithLookup(allowed []string, home string, lookup func(string) (string, bool)) []string {
	if lookup == nil {
		lookup = os.LookupEnv
	}
	allow := make(map[string]struct{}, len(allowed))
	for _, name := range allowed {
		if name = strings.TrimSpace(name); name != "" {
			allow[name] = struct{}{}
		}
	}
	base := []string{"PATH", "SYSTEMROOT", "WINDIR", "COMSPEC"}
	out := make([]string, 0, len(base)+len(allow)+6)
	for _, name := range base {
		if value, ok := lookup(name); ok {
			out = append(out, name+"="+value)
		}
	}
	out = append(out, "HOME="+home, "XDG_CONFIG_HOME="+filepath.Join(home, ".config"), "XDG_CACHE_HOME="+filepath.Join(home, ".cache"), "XDG_DATA_HOME="+filepath.Join(home, ".local", "share"), "TMPDIR="+filepath.Join(home, "tmp"), "TMP="+filepath.Join(home, "tmp"), "TEMP="+filepath.Join(home, "tmp"))
	for name := range allow {
		if value, ok := lookup(name); ok {
			out = append(out, name+"="+value)
		}
	}
	return out
}

// getenvWith reads name through lookup, or the process environment when
// lookup is nil.
func getenvWith(lookup func(string) (string, bool), name string) string {
	if lookup == nil {
		return os.Getenv(name)
	}
	value, _ := lookup(name)
	return value
}
