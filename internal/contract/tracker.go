package contract

import "fmt"

// ResolveTracker returns the configured tracker, defaulting to GitHub.
func ResolveTracker(cfg *SwarmConfig) (string, error) {
	if cfg == nil || cfg.Tracker == "" {
		return "github", nil
	}
	switch cfg.Tracker {
	case "github", "local":
		return cfg.Tracker, nil
	}
	return "", fmt.Errorf("tracker in .swarm/config.yaml must be github or local (got %q)", cfg.Tracker)
}
