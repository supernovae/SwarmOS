package contract

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"
)

const MaxArchiveManifestBytes = 4 << 20

type ArchiveRef struct {
	Source      string `json:"source"`
	Destination string `json:"destination"`
	SHA         string `json:"sha"`
}

// ArchiveManifest describes immutable copies of one issue's ledger objects.
type ArchiveManifest struct {
	Version     int          `json:"version"`
	Repository  string       `json:"repository"`
	Issue       int          `json:"issue"`
	Namespace   string       `json:"namespace"`
	Destination string       `json:"destination"`
	Refs        []ArchiveRef `json:"refs"`
	StateTip    string       `json:"state_tip"`
	PROutcome   string       `json:"pr_outcome"`
	Actor       string       `json:"actor"`
	Cutoff      *time.Time   `json:"cutoff,omitempty"`
	ArchivedAt  time.Time    `json:"archived_at"`
}

var archiveSHA = regexp.MustCompile(`^[0-9a-f]{40}$`)
var archiveRepository = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
var archiveSSHUser = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.-]*$`)

var archiveActor = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]*$`)

// ValidateArchiveDestination rejects credential-bearing and executable Git URLs.
func ValidateArchiveDestination(destination string) error {
	invalid := fmt.Errorf("invalid archive destination; use HTTPS, SSH, or file:// without credentials")
	if destination == "" || len(destination) > 4096 || strings.ContainsAny(destination, "\x00\r\n\t \\") || strings.HasPrefix(destination, "-") {
		return invalid
	}
	if !strings.Contains(destination, "://") {
		// Git's scp-like SSH syntax requires an explicit host and absolute/relative path.
		host, path, ok := strings.Cut(destination, ":")
		if host == "file" || host == "ssh" || host == "http" || host == "https" || host == "ext" {
			return invalid
		}
		if !ok || host == "" || path == "" || strings.HasPrefix(path, "-") || strings.Contains(path, ":") {
			return invalid
		}
		if at := strings.IndexByte(host, '@'); at >= 0 {
			if !archiveSSHUser.MatchString(host[:at]) {
				return invalid
			}
			host = host[at+1:]
		}
		if host == "" || strings.ContainsAny(host, "/@?#%") || strings.HasPrefix(host, "-") {
			return invalid
		}
		return nil
	}
	u, err := url.Parse(destination)
	if err != nil || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" || u.Path == "" || strings.ContainsAny(u.Path, "\x00\r\n\t") {
		return invalid
	}
	switch u.Scheme {
	case "https":
		if u.Host == "" || u.User != nil {
			return invalid
		}
	case "ssh":
		if u.Host == "" {
			return invalid
		}
		if u.User != nil {
			if _, password := u.User.Password(); password || !archiveSSHUser.MatchString(u.User.Username()) {
				return invalid
			}
		}
	case "file":
		if u.Host != "" || u.User != nil || !strings.HasPrefix(u.Path, "/") {
			return invalid
		}
	default:
		return invalid
	}
	return nil
}

func ValidateArchiveManifest(m *ArchiveManifest) error {
	if m == nil {
		return fmt.Errorf("archive manifest is nil")
	}
	if m.Cutoff != nil && m.Cutoff.IsZero() {
		return fmt.Errorf("invalid archive cutoff")
	}
	if err := validateArchiveSchema(m); err != nil {
		return err
	}
	parts := strings.Split(m.Repository, "/")
	if !archiveRepository.MatchString(m.Repository) || len(parts) != 2 || parts[0] == "." || parts[0] == ".." || parts[1] == "." || parts[1] == ".." || !archiveActor.MatchString(m.Actor) || m.ArchivedAt.IsZero() {
		return fmt.Errorf("invalid archive identity or timestamp")
	}
	if err := ValidateArchiveDestination(m.Destination); err != nil {
		return err
	}
	prefix := "refs/heads/swarm-"
	state := fmt.Sprintf("%sstate/issue-%d", prefix, m.Issue)
	checkpoints := fmt.Sprintf("%scheckpoints/issue-%d/", prefix, m.Issue)
	if m.Namespace == "hidden" {
		state = fmt.Sprintf("refs/swarm/state/issue-%d", m.Issue)
		checkpoints = fmt.Sprintf("refs/swarm/checkpoints/issue-%d/", m.Issue)
	}
	dest := fmt.Sprintf("refs/archive/issue-%d/", m.Issue)
	seen := map[string]bool{}
	stateFound := false
	for _, ref := range m.Refs {
		if !archiveSHA.MatchString(ref.SHA) || ref.SHA == strings.Repeat("0", 40) || seen[ref.Source] || seen[ref.Destination] {
			return fmt.Errorf("invalid or duplicate archive ref")
		}
		seen[ref.Source], seen[ref.Destination] = true, true
		switch {
		case ref.Source == state:
			if ref.Destination != dest+"state" || ref.SHA != m.StateTip {
				return fmt.Errorf("archive state tip mismatch")
			}
			stateFound = true
		case strings.HasPrefix(ref.Source, checkpoints):
			head := strings.TrimPrefix(ref.Source, checkpoints)
			if head != ref.SHA || ref.Destination != dest+"checkpoints/"+head {
				return fmt.Errorf("invalid checkpoint mapping")
			}
		default:
			return fmt.Errorf("archive ref belongs to another issue or namespace")
		}
	}
	if !stateFound {
		return fmt.Errorf("archive state ref missing")
	}
	return nil
}

// MarshalArchiveManifest uses a stable ref order; its digest covers exact bytes.
func MarshalArchiveManifest(m *ArchiveManifest) ([]byte, string, error) {
	if err := ValidateArchiveManifest(m); err != nil {
		return nil, "", err
	}
	copy := *m
	copy.Refs = append([]ArchiveRef(nil), m.Refs...)
	sort.Slice(copy.Refs, func(i, j int) bool { return copy.Refs[i].Source < copy.Refs[j].Source })
	copy.ArchivedAt = copy.ArchivedAt.UTC()
	if copy.Cutoff != nil {
		t := copy.Cutoff.UTC()
		copy.Cutoff = &t
	}
	data, err := json.Marshal(copy)
	if err != nil {
		return nil, "", err
	}
	if len(data) > MaxArchiveManifestBytes {
		return nil, "", fmt.Errorf("archive manifest too large")
	}
	h := sha256.Sum256(data)
	return data, hex.EncodeToString(h[:]), nil
}

func ParseArchiveManifest(data []byte) (*ArchiveManifest, error) {
	if len(data) > MaxArchiveManifestBytes {
		return nil, fmt.Errorf("archive manifest too large")
	}
	if err := ValidateArchiveJSON(data); err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var m ArchiveManifest
	if err := dec.Decode(&m); err != nil {
		return nil, err
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		return nil, fmt.Errorf("trailing archive manifest data")
	}
	if err := ValidateArchiveManifest(&m); err != nil {
		return nil, err
	}
	return &m, nil
}

// ValidateArchiveJSON rejects duplicate object members, which otherwise let
// different readers assign different authority to the same archive document.
func ValidateArchiveJSON(data []byte) error {
	d := json.NewDecoder(bytes.NewReader(data))
	var value func(int) error
	value = func(depth int) error {
		if depth > 32 {
			return fmt.Errorf("archive JSON nesting limit exceeded")
		}
		token, err := d.Token()
		if err != nil {
			return err
		}
		delim, ok := token.(json.Delim)
		if !ok {
			return nil
		}
		switch delim {
		case '{':
			seen := map[string]bool{}
			for d.More() {
				key, err := d.Token()
				if err != nil {
					return err
				}
				name, ok := key.(string)
				if !ok || seen[name] {
					return fmt.Errorf("duplicate or invalid archive object member")
				}
				seen[name] = true
				if err = value(depth + 1); err != nil {
					return err
				}
			}
		case '[':
			for d.More() {
				if err = value(depth + 1); err != nil {
					return err
				}
			}
		default:
			return fmt.Errorf("invalid archive JSON delimiter")
		}
		_, err = d.Token()
		return err
	}
	if err := value(0); err != nil {
		return err
	}
	if _, err := d.Token(); err != io.EOF {
		return fmt.Errorf("trailing archive JSON data")
	}
	return nil
}

// ArchiveConfig is opt-in retention policy from the upstream default branch.
type ArchiveConfig struct {
	Strategy  string `yaml:"strategy,omitempty" json:"strategy,omitempty"`
	AfterDays *int   `yaml:"after_days,omitempty" json:"after_days,omitempty"`
	Repo      string `yaml:"repo,omitempty" json:"repo,omitempty"`
}

func (c *ArchiveConfig) EffectiveStrategy() string {
	if c == nil || c.Strategy == "" {
		return "keep"
	}
	return c.Strategy
}
func ValidateArchiveConfig(c *ArchiveConfig) error {
	if c == nil {
		return nil
	}
	if c.EffectiveStrategy() != "keep" && c.EffectiveStrategy() != "repo" {
		return fmt.Errorf("archive.strategy must be keep or repo")
	}
	if c.AfterDays != nil && (*c.AfterDays <= 0 || int64(*c.AfterDays) > int64(1<<63-1)/int64(24*time.Hour)) {
		return fmt.Errorf("archive.after_days must be a positive supported number of days")
	}
	if c.Repo != "" {
		parts := strings.Split(c.Repo, "/")
		if !archiveRepository.MatchString(c.Repo) || len(parts) != 2 || parts[0] == "." || parts[0] == ".." || parts[1] == "." || parts[1] == ".." {
			return fmt.Errorf("archive.repo must be owner/repo")
		}
	}
	if c.EffectiveStrategy() == "repo" && (c.AfterDays == nil || c.Repo == "") {
		return fmt.Errorf("archive repo strategy requires after_days and repo")
	}
	return nil
}
