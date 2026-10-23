package cli

import (
	"runtime/debug"
	"testing"
)

func TestResolveBuildVersion(t *testing.T) {
	const revision = "989eaf9361d5ab6ee60585a30b4c37b9f4ff365d"
	const commitTime = "2026-10-23T16:07:02Z"
	for _, tt := range []struct {
		name     string
		module   string
		modified bool
		in       [3]string
		want     [3]string
	}{
		{
			name: "module install", module: "v0.5.0",
			in:   [3]string{"dev", "none", "unknown"},
			want: [3]string{"0.5.0", revision, commitTime},
		},
		{
			name: "development checkout", module: "(devel)",
			in:   [3]string{"dev", "none", "unknown"},
			want: [3]string{"dev", revision, commitTime},
		},
		{
			name: "dirty checkout", module: "v0.5.0", modified: true,
			in:   [3]string{"dev", "none", "unknown"},
			want: [3]string{"0.5.0+dirty", revision, commitTime},
		},
		{
			name: "Go already marked dirty", module: "v0.5.1-0.20261023160702-989eaf9361d5+dirty", modified: true,
			in:   [3]string{"dev", "none", "unknown"},
			want: [3]string{"0.5.1-0.20261023160702-989eaf9361d5+dirty", revision, commitTime},
		},
		{
			name: "release linker flags win", module: "v0.4.1", modified: true,
			in:   [3]string{"0.5.0", "release-commit", "release-time"},
			want: [3]string{"0.5.0", "release-commit", "release-time"},
		},
		{
			name: "partial linker flags", module: "v0.4.1",
			in:   [3]string{"0.5.0", "none", "unknown"},
			want: [3]string{"0.5.0", revision, commitTime},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			info := &debug.BuildInfo{
				Main: debug.Module{Version: tt.module},
				Settings: []debug.BuildSetting{
					{Key: "vcs.revision", Value: revision},
					{Key: "vcs.time", Value: commitTime},
				},
			}
			if tt.modified {
				info.Settings = append(info.Settings, debug.BuildSetting{Key: "vcs.modified", Value: "true"})
			}
			version, commit, date := resolveBuildVersion(tt.in[0], tt.in[1], tt.in[2], info)
			if got := [3]string{version, commit, date}; got != tt.want {
				t.Fatalf("metadata = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestResolveBuildVersionWithoutVCS(t *testing.T) {
	for _, tt := range []struct {
		module string
		want   string
	}{
		{module: "v0.5.0", want: "0.5.0"},
		{module: "(devel)", want: "dev"},
		{module: "", want: "dev"},
	} {
		info := &debug.BuildInfo{Main: debug.Module{Version: tt.module}}
		version, commit, date := resolveBuildVersion("dev", "none", "unknown", info)
		if version != tt.want || commit != "none" || date != "unknown" {
			t.Fatalf("module %q: metadata = %q, %q, %q", tt.module, version, commit, date)
		}
	}
}
