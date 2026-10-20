package contract

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

// DigestWorkUnit returns the stable digest of the executable task contract.
// Presentation fields are included deliberately: changing any task text requires approval.
func DigestWorkUnit(wu *WorkUnit) string {
	if wu == nil {
		return ""
	}
	b, err := json.Marshal(wu)
	if err != nil {
		return ""
	}
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func ValidateGate(g GateMode) error {
	switch g {
	case "", GateNone, GateAgent, GateHuman:
		return nil
	}
	return fmt.Errorf("invalid plan gate %q", g)
}

func ResolveGate(task, workflow, policy GateMode) (GateMode, error) {
	for _, gate := range []GateMode{task, workflow, policy} {
		if err := ValidateGate(gate); err != nil {
			return "", err
		}
	}
	rank := func(g GateMode) int {
		switch g {
		case GateHuman:
			return 3
		case GateAgent:
			return 2
		case GateNone:
			return 1
		default:
			return 0
		}
	}
	result := GateNone
	for _, gate := range []GateMode{task, workflow, policy} {
		if rank(gate) > rank(result) {
			result = gate
		}
	}
	return result, nil
}

func ValidatePhase(phase string) error {
	switch phase {
	case "plan", "review", "implement", "all":
		return nil
	}
	return fmt.Errorf("invalid workflow phase %q", phase)
}

func DigestSecurityPolicy(sec SecurityConfig) string {
	b, err := json.Marshal(sec)
	if err != nil {
		return ""
	}
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func DigestConfig(cfg *SwarmConfig) string {
	if cfg == nil {
		return ""
	}
	b, err := json.Marshal(cfg)
	if err != nil {
		return ""
	}
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// DigestApprovedPolicy binds the complete immutable policy tree through its
// commit identity as well as the parsed configuration contract.
func DigestApprovedPolicy(baseSHA string, cfg *SwarmConfig) string {
	h := sha256.New()
	_, _ = h.Write([]byte(baseSHA))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(DigestConfig(cfg)))
	return hex.EncodeToString(h.Sum(nil))
}
