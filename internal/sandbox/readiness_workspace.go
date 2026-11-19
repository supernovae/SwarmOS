package sandbox

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"path"
	"slices"
	"strings"

	"github.com/BurntSushi/toml"
	"golang.org/x/mod/modfile"
	"gopkg.in/yaml.v3"
)

type readinessWorkspace struct {
	dir, ecosystem   string
	members, exclude []string
}

func readinessWorkspaces(inputs map[string][]byte) []readinessWorkspace {
	var workspaces []readinessWorkspace
	for name, data := range inputs {
		root := readinessWorkspace{dir: path.Dir(name)}
		switch path.Base(name) {
		case "go.work":
			root.ecosystem = "go"
			if work, err := modfile.ParseWork(name, data, nil); err == nil {
				for _, member := range work.Use {
					if !path.IsAbs(member.Path) && !strings.Contains(member.Path, "\\") {
						root.members = append(root.members, path.Clean(member.Path))
					}
				}
			}
		case "Cargo.toml":
			root.ecosystem = "rust"
			var manifest struct {
				Workspace struct {
					Members []string
					Exclude []string
				}
			}
			if _, err := toml.Decode(string(data), &manifest); err == nil {
				root.members, root.exclude = manifest.Workspace.Members, manifest.Workspace.Exclude
			}
		case "package.json":
			root.ecosystem = "node"
			var manifest struct{ Workspaces json.RawMessage }
			if json.Unmarshal(data, &manifest) == nil && len(manifest.Workspaces) > 0 {
				if json.Unmarshal(manifest.Workspaces, &root.members) != nil {
					var workspace struct{ Packages []string }
					if json.Unmarshal(manifest.Workspaces, &workspace) == nil {
						root.members = workspace.Packages
					}
				}
			}
		case "pnpm-workspace.yaml":
			root.ecosystem = "node"
			var manifest struct{ Packages []string }
			if yaml.Unmarshal(data, &manifest) == nil {
				root.members = manifest.Packages
			}
		}
		if len(root.members) != 0 {
			workspaces = append(workspaces, root)
		}
	}
	return workspaces
}

func workspaceMember(workspace readinessWorkspace, directory string) bool {
	rel := directory
	if workspace.dir != "." {
		var ok bool
		rel, ok = strings.CutPrefix(directory, workspace.dir+"/")
		if !ok {
			return false
		}
	}
	for _, pattern := range workspace.exclude {
		if matches, _ := path.Match(pattern, rel); matches {
			return false
		}
	}
	matched := false
	for _, pattern := range workspace.members {
		include := !strings.HasPrefix(pattern, "!")
		if matches, _ := path.Match(path.Clean(strings.TrimPrefix(pattern, "!")), rel); matches {
			matched = include
		}
	}
	return matched
}

func deduplicateReadinessProjects(projects []ReadinessProject, inputs map[string][]byte) []ReadinessProject {
	for _, workspace := range readinessWorkspaces(inputs) {
		projects = slices.DeleteFunc(projects, func(p ReadinessProject) bool {
			if p.Ecosystem != workspace.ecosystem || p.Directory == workspace.dir || !workspaceMember(workspace, p.Directory) {
				return false
			}
			var independent []string
			switch p.Ecosystem {
			case "go":
				independent = []string{"go.work"}
			case "rust":
				if strings.Contains(string(inputs[path.Join(p.Directory, "Cargo.toml")]), "rust-version") {
					return false
				}
				independent = []string{"Cargo.lock", "rust-toolchain", "rust-toolchain.toml"}
			case "node":
				var declared struct {
					Engines        json.RawMessage
					PackageManager string
				}
				if json.Unmarshal(inputs[path.Join(p.Directory, "package.json")], &declared) == nil && (len(declared.Engines) > 0 || declared.PackageManager != "") {
					return false
				}
				independent = []string{"package-lock.json", "npm-shrinkwrap.json", "pnpm-lock.yaml", "yarn.lock", "pnpm-workspace.yaml"}
			}
			for _, name := range independent {
				if _, ok := inputs[path.Join(p.Directory, name)]; ok {
					return false
				}
			}
			return true
		})
	}
	return projects
}

func validateReadinessGoInput(p ReadinessProject, name string, data []byte) error {
	validModule := func(target string) bool {
		if path.IsAbs(target) || strings.Contains(target, "\\") {
			return false
		}
		joined := path.Join(path.Dir(name), target, "go.mod")
		if !fs.ValidPath(joined) {
			return false
		}
		_, ok := p.inputs[joined]
		return ok
	}
	var replacements []*modfile.Replace
	switch path.Base(name) {
	case "go.mod":
		file, err := modfile.Parse(name, data, nil)
		if err != nil {
			return err
		}
		replacements = file.Replace
	case "go.work":
		file, err := modfile.ParseWork(name, data, nil)
		if err != nil {
			return err
		}
		for _, use := range file.Use {
			if !validModule(use.Path) {
				return fmt.Errorf("%s uses module %q outside the staged project inputs", name, use.Path)
			}
		}
		replacements = file.Replace
	}
	for _, replace := range replacements {
		if replace.New.Version == "" && !validModule(replace.New.Path) {
			return fmt.Errorf("%s replaces %s with an unavailable local module %q", name, replace.Old.Path, replace.New.Path)
		}
	}
	return nil
}
