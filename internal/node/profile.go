package node

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/supernovae/swarmos/internal/contract"
	"gopkg.in/yaml.v3"
)

func DefaultNodeConfigPath() string {
	if env := os.Getenv("SWARM_NODE_CONFIG"); env != "" {
		return env
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".", ".swarm", "node.yaml")
	}
	return filepath.Join(home, ".config", "swarm", "node.yaml")
}

func DetectHostProfile(nodeID string) *contract.NodeProfile {
	if nodeID == "" {
		host, err := os.Hostname()
		if err != nil || host == "" {
			host = "swarm-node"
		}
		nodeID = fmt.Sprintf("%s-%s-%s", strings.Split(host, ".")[0], runtime.GOOS, runtime.GOARCH)
	}

	var models []string
	for _, m := range []string{"claude", "codex", "opencode", "omp", "grok", "dot"} {
		if _, err := exec.LookPath(m); err == nil {
			models = append(models, m)
		}
	}

	return &contract.NodeProfile{
		NodeID:       nodeID,
		Capabilities: []string{"coding", "testing", "review", "docs"},
		Models:       models,
		Resources: contract.NodeResources{
			OS:   runtime.GOOS,
			Arch: runtime.GOARCH,
			CPUs: runtime.NumCPU(),
			RAM:  detectHostRAM(),
		},
	}
}

func LoadOrDetectProfile(customPath string) (*contract.NodeProfile, error) {
	if customPath == "" {
		customPath = DefaultNodeConfigPath()
	}
	data, err := os.ReadFile(customPath)
	if err != nil {
		return DetectHostProfile(""), nil
	}
	var prof contract.NodeProfile
	if err := yaml.Unmarshal(data, &prof); err != nil {
		return nil, fmt.Errorf("parse node profile %s: %w", customPath, err)
	}
	if prof.NodeID == "" {
		detected := DetectHostProfile("")
		prof.NodeID = detected.NodeID
	}
	return &prof, nil
}

func SaveProfile(customPath string, prof *contract.NodeProfile) error {
	if customPath == "" {
		customPath = DefaultNodeConfigPath()
	}
	if err := os.MkdirAll(filepath.Dir(customPath), 0o755); err != nil {
		return err
	}
	out, err := yaml.Marshal(prof)
	if err != nil {
		return err
	}
	return os.WriteFile(customPath, out, 0o644)
}

func MatchesCapabilities(prof *contract.NodeProfile, required []string) bool {
	if len(required) == 0 || prof == nil {
		return true
	}
	for _, req := range required {
		found := false
		for _, capName := range prof.Capabilities {
			if strings.EqualFold(strings.TrimSpace(req), strings.TrimSpace(capName)) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func SupportsPhaseCapability(prof *contract.NodeProfile, phase string) bool {
	if prof == nil || len(prof.Capabilities) == 0 {
		return true
	}
	switch strings.ToLower(phase) {
	case "review":
		return MatchesCapabilities(prof, []string{"review"})
	case "validate", "test":
		return MatchesCapabilities(prof, []string{"testing"})
	default:
		return MatchesCapabilities(prof, []string{"coding"})
	}
}

func detectHostRAM() string {
	if runtime.GOOS == "darwin" {
		if out, err := exec.Command("sysctl", "-n", "hw.memsize").Output(); err == nil {
			if bytesVal, err := strconv.ParseUint(strings.TrimSpace(string(out)), 10, 64); err == nil && bytesVal > 0 {
				gb := bytesVal / (1024 * 1024 * 1024)
				if gb > 0 {
					return fmt.Sprintf("%dGB", gb)
				}
			}
		}
	}
	if runtime.GOOS == "linux" {
		if data, err := os.ReadFile("/proc/meminfo"); err == nil {
			for _, line := range strings.Split(string(data), "\n") {
				if strings.HasPrefix(line, "MemTotal:") {
					fields := strings.Fields(line)
					if len(fields) >= 2 {
						if kb, err := strconv.ParseUint(fields[1], 10, 64); err == nil && kb > 0 {
							gb := (kb + 524288) / (1024 * 1024)
							if gb > 0 {
								return fmt.Sprintf("%dGB", gb)
							}
						}
					}
				}
			}
		}
	}
	return "16GB"
}

// LoadUpstreamRemote returns the profile's upstream_remote, or "" when the
// profile is missing or does not set it. A malformed profile is an error so a
// broken file never silently changes which remote is the project.
func LoadUpstreamRemote(customPath string) (string, error) {
	if customPath == "" {
		customPath = DefaultNodeConfigPath()
	}
	data, err := os.ReadFile(customPath)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read node profile %s: %w", customPath, err)
	}
	var prof contract.NodeProfile
	if err := yaml.Unmarshal(data, &prof); err != nil {
		return "", fmt.Errorf("parse node profile %s: %w", customPath, err)
	}
	return strings.TrimSpace(prof.UpstreamRemote), nil
}
