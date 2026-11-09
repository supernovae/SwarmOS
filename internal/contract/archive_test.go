package contract

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func archiveManifestFixture(namespace string) *ArchiveManifest {
	sha := strings.Repeat("a", 40)
	state, checkpoint := "refs/heads/swarm-state/issue-363", "refs/heads/swarm-checkpoints/issue-363/"+sha
	if namespace == "hidden" {
		state, checkpoint = "refs/swarm/state/issue-363", "refs/swarm/checkpoints/issue-363/"+sha
	}
	return &ArchiveManifest{Version: 1, Repository: "supernovae/swarmos", Issue: 363, Namespace: namespace, Destination: "file:///archive.git", Refs: []ArchiveRef{
		{Source: state, Destination: "refs/archive/issue-363/state", SHA: sha},
		{Source: checkpoint, Destination: "refs/archive/issue-363/checkpoints/" + sha, SHA: sha},
	}, StateTip: sha, PROutcome: "none", Actor: "maintainer", ArchivedAt: time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)}
}

func TestArchiveManifestValidation(t *testing.T) {
	cases := []struct {
		name   string
		change func(*ArchiveManifest)
	}{
		{"version", func(m *ArchiveManifest) { m.Version = 2 }},
		{"issue", func(m *ArchiveManifest) { m.Issue = 0 }},
		{"repository", func(m *ArchiveManifest) { m.Repository = "../repo" }},
		{"missing repository", func(m *ArchiveManifest) { m.Repository = "" }},
		{"namespace", func(m *ArchiveManifest) { m.Namespace = "other" }},
		{"destination", func(m *ArchiveManifest) { m.Destination = "ext::command" }},
		{"credentials", func(m *ArchiveManifest) { m.Destination = "https://secret@example.invalid/archive" }},
		{"actor", func(m *ArchiveManifest) { m.Actor = "" }},
		{"timestamp", func(m *ArchiveManifest) { m.ArchivedAt = time.Time{} }},
		{"outcome", func(m *ArchiveManifest) { m.PROutcome = "open" }},
		{"no state", func(m *ArchiveManifest) { m.Refs = m.Refs[1:] }},
		{"empty refs", func(m *ArchiveManifest) { m.Refs = nil }},
		{"zero SHA", func(m *ArchiveManifest) { m.Refs[0].SHA = strings.Repeat("0", 40); m.StateTip = m.Refs[0].SHA }},
		{"bad SHA", func(m *ArchiveManifest) { m.Refs[0].SHA = strings.Repeat("z", 40) }},
		{"uppercase SHA", func(m *ArchiveManifest) { m.Refs[0].SHA = strings.Repeat("A", 40) }},
		{"wrong state tip", func(m *ArchiveManifest) { m.StateTip = strings.Repeat("b", 40) }},
		{"duplicate source", func(m *ArchiveManifest) { m.Refs = append(m.Refs, m.Refs[0]) }},
		{"duplicate destination", func(m *ArchiveManifest) { m.Refs[1].Destination = m.Refs[0].Destination }},
		{"cross issue source", func(m *ArchiveManifest) { m.Refs[0].Source = strings.ReplaceAll(m.Refs[0].Source, "363", "364") }},
		{"cross issue destination", func(m *ArchiveManifest) {
			m.Refs[1].Destination = strings.ReplaceAll(m.Refs[1].Destination, "363", "364")
		}},
		{"checkpoint identity", func(m *ArchiveManifest) { m.Refs[1].SHA = strings.Repeat("b", 40) }},
		{"state suffix", func(m *ArchiveManifest) { m.Refs[0].Source += "/extra" }},
		{"checkpoint suffix", func(m *ArchiveManifest) { m.Refs[1].Source += "/extra" }},
	}
	for _, namespace := range []string{"branch", "hidden"} {
		t.Run(namespace, func(t *testing.T) {
			m := archiveManifestFixture(namespace)
			if err := ValidateArchiveManifest(m); err != nil {
				t.Fatal(err)
			}
			// A finished issue need not have any checkpoints or a linked PR.
			m.Refs = m.Refs[:1]
			for _, outcome := range []string{"none", "closed", "merged"} {
				m.PROutcome = outcome
				if err := ValidateArchiveManifest(m); err != nil {
					t.Fatal(err)
				}
			}
			for _, tc := range cases {
				t.Run(tc.name, func(t *testing.T) {
					m := archiveManifestFixture(namespace)
					tc.change(m)
					if err := ValidateArchiveManifest(m); err == nil {
						t.Fatal("invalid manifest accepted")
					}
				})
			}
		})
	}
	if err := ValidateArchiveManifest(nil); err == nil {
		t.Fatal("nil accepted")
	}
}

func TestArchiveManifestSerialization(t *testing.T) {
	m := archiveManifestFixture("branch")
	first, digest, err := MarshalArchiveManifest(m)
	if err != nil {
		t.Fatal(err)
	}
	m.Refs[0], m.Refs[1] = m.Refs[1], m.Refs[0]
	m.ArchivedAt = m.ArchivedAt.In(time.FixedZone("offset", 3600))
	second, secondDigest, err := MarshalArchiveManifest(m)
	if err != nil || !bytes.Equal(first, second) || digest != secondDigest || len(digest) != 64 {
		t.Fatalf("serialization differs: %v", err)
	}
	if m.Refs[0].Source == "refs/heads/swarm-state/issue-363" {
		t.Fatal("marshal mutated caller ref order")
	}
	parsed, err := ParseArchiveManifest(first)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Issue != 363 || parsed.StateTip != m.StateTip {
		t.Fatal("round trip identity changed")
	}
	m.Actor = "another-maintainer"
	_, changedDigest, err := MarshalArchiveManifest(m)
	if err != nil || changedDigest == digest {
		t.Fatal("altered content retained digest")
	}
	for _, data := range [][]byte{
		nil, []byte("{"), append(append([]byte(nil), first...), []byte(" {}")...),
		[]byte(strings.Replace(string(first), `"version":1`, `"version":2`, 1)),
		[]byte(strings.Replace(string(first), `"version":1`, `"unexpected":true,"version":1`, 1)),
		bytes.Repeat([]byte(" "), MaxArchiveManifestBytes+1),
	} {
		if _, err := ParseArchiveManifest(data); err == nil {
			t.Fatal("malformed manifest accepted")
		}
	}
	// All required schema fields are checked, independently of Go zero values.
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(first, &raw); err != nil {
		t.Fatal(err)
	}
	for key := range raw {
		copy := make(map[string]json.RawMessage, len(raw))
		for k, v := range raw {
			if k != key {
				copy[k] = v
			}
		}
		data, err := json.Marshal(copy)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := ParseArchiveManifest(data); err == nil {
			t.Errorf("missing %s accepted", key)
		}
	}
}

func TestArchiveDestinationValidation(t *testing.T) {
	for _, destination := range []string{"https://example.invalid/archive.git", "ssh://git@example.invalid/archive.git", "ssh://builder@example.invalid/archive.git", "builder@example.invalid:archive.git", "git@example.invalid:archive.git", "example.invalid:/archive.git", "file:///tmp/archive.git"} {
		if err := ValidateArchiveDestination(destination); err != nil {
			t.Errorf("valid destination %q: %v", destination, err)
		}
	}
	for _, destination := range []string{"", "-option", "/tmp/archive.git", "../archive.git", "ext::command", "http://example.invalid/repo", "https://secret@example.invalid/repo", "https://user:secret@example.invalid/repo", "ssh://git:secret@example.invalid/repo", "file://host/archive", "file:relative", "https://example.invalid/repo?token=secret", "https://example.invalid/repo#secret", "git@example.invalid:repo\ncommand", "git@-option:repo", "git@example.invalid:", "ssh://git@example.invalid/with%0anewline"} {
		err := ValidateArchiveDestination(destination)
		if err == nil {
			t.Errorf("unsafe destination accepted: %q", destination)
		} else if strings.Contains(err.Error(), "secret") {
			t.Fatal("error exposed credentials")
		}
	}
}

func TestArchiveDuplicateJSON(t *testing.T) {
	for _, data := range []string{`{"version":1,"version":2}`, `{"entries":{"1":{},"1":{}}}`, `[] []`, strings.Repeat("[", 40) + strings.Repeat("]", 40)} {
		if err := ValidateArchiveJSON([]byte(data)); err == nil {
			t.Fatal("ambiguous or excessive document accepted")
		}
	}
}

func TestArchivePolicyValidation(t *testing.T) {
	days := 30
	zero := 0
	negative := -1
	overflow := 106752
	cases := []struct {
		c     *ArchiveConfig
		valid bool
	}{
		{nil, true}, {&ArchiveConfig{}, true}, {&ArchiveConfig{Strategy: "keep"}, true},
		{&ArchiveConfig{Strategy: "repo", AfterDays: &days, Repo: "acme/private-archive"}, true},
		{&ArchiveConfig{Strategy: "purge"}, false}, {&ArchiveConfig{Strategy: "local"}, false},
		{&ArchiveConfig{Strategy: "unknown"}, false}, {&ArchiveConfig{AfterDays: &zero}, false},
		{&ArchiveConfig{AfterDays: &negative}, false}, {&ArchiveConfig{AfterDays: &overflow}, false},
		{&ArchiveConfig{Strategy: "repo", Repo: "acme/archive"}, false},
		{&ArchiveConfig{Strategy: "repo", AfterDays: &days}, false},
	}
	for _, repo := range []string{"../archive", "acme/..", "https://github.com/acme/archive", "acme/a b", "acme/a;rm", "acme/a/b", "a@b/c", "./repo"} {
		cases = append(cases, struct {
			c     *ArchiveConfig
			valid bool
		}{&ArchiveConfig{Repo: repo}, false})
	}
	for _, tc := range cases {
		if err := ValidateArchiveConfig(tc.c); (err == nil) != tc.valid {
			t.Fatalf("%+v: %v", tc.c, err)
		}
	}
	data, _ := json.Marshal(SwarmConfig{})
	if bytes.Contains(data, []byte(`"archive"`)) {
		t.Fatal("omitted policy serialized")
	}
}
func TestArchiveManifestCutoff(t *testing.T) {
	m := archiveManifestFixture("branch")
	legacy, oldDigest, e := MarshalArchiveManifest(m)
	if e != nil {
		t.Fatal(e)
	}
	if bytes.Contains(legacy, []byte("cutoff")) {
		t.Fatal("legacy bytes changed")
	}
	cutoff := time.Date(2026, 10, 1, 2, 0, 0, 0, time.FixedZone("offset", 7200))
	m.Cutoff = &cutoff
	data, digest, e := MarshalArchiveManifest(m)
	if e != nil {
		t.Fatal(e)
	}
	if digest == oldDigest {
		t.Fatal("cutoff not in digest")
	}
	parsed, e := ParseArchiveManifest(data)
	if e != nil || !parsed.Cutoff.Equal(cutoff) || parsed.Cutoff.Location() != time.UTC {
		t.Fatalf("%s %v", data, e)
	}
	zero := time.Time{}
	m.Cutoff = &zero
	if _, _, e = MarshalArchiveManifest(m); e == nil {
		t.Fatal("zero cutoff accepted")
	}
}

func TestArchiveConfigSchemaAgreement(t *testing.T) {
	for _, raw := range []string{`{}`, `{"strategy":""}`, `{"strategy":"keep"}`, `{"strategy":"repo","after_days":30,"repo":"acme/archive"}`, `{"strategy":"purge"}`, `{"after_days":0}`, `{"after_days":-1}`, `{"after_days":1.5}`, `{"after_days":106752}`, `{"strategy":"repo"}`, `{"repo":"../archive"}`, `{"repo":"a/.."}`, `{"repo":"https://github.com/a/b"}`} {
		var c ArchiveConfig
		err := json.Unmarshal([]byte(raw), &c)
		valid := err == nil && ValidateArchiveConfig(&c) == nil
		schemaErr := ValidateConfigJSON([]byte(`{"archive":` + raw + `}`))
		if (schemaErr == nil) != valid {
			t.Fatalf("%s: Go valid=%t schema=%v", raw, valid, schemaErr)
		}
	}
}
