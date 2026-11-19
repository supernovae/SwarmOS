package sandbox

import (
	"encoding/json"
	"encoding/xml"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"

	"github.com/supernovae/swarmos/internal/safefs"
)

var (
	cmakeProject = regexp.MustCompile(`(?im)^\s*project\s*\(([^)]*)\)`)
	sdkFramework = regexp.MustCompile(`^net([0-9]+\.[0-9]+|standard[0-9]+\.[0-9]+|coreapp[0-9]+\.[0-9]+)$`)
	packageID    = regexp.MustCompile(`^[a-zA-Z0-9_.-]+$`)
	packageVer   = regexp.MustCompile(`^[0-9]+(\.[0-9]+){1,3}(-[a-zA-Z0-9.-]+)?$`)
	cStandard    = regexp.MustCompile(`(?im)^\s*set\(\s*CMAKE_(C|CXX)_STANDARD\s+([0-9]+)\s*\)`)
	cLibrary     = regexp.MustCompile(`(?im)^\s*find_library\(\s*[a-zA-Z0-9_]+\s+(?:NAMES\s+)?([a-zA-Z0-9_+-]+)\s+REQUIRED\s*\)`)
	cHeader      = regexp.MustCompile(`(?im)^\s*find_path\(\s*[a-zA-Z0-9_]+\s+(?:NAMES\s+)?([a-zA-Z0-9_/+.-]+\.h)\s+REQUIRED\s*\)`)
	cRequired    = regexp.MustCompile(`(?im)^\s*set\(\s*CMAKE_(C|CXX)_STANDARD_REQUIRED\s+ON\s*\)`)
)

func cmakeLanguages(data []byte) []string {
	project := cmakeProject.FindSubmatch(data)
	if project == nil {
		return nil
	}
	fields := strings.Fields(strings.ToUpper(string(project[1])))
	var languages []string
	for _, field := range fields[1:] {
		switch field {
		case "C":
			languages = append(languages, "c")
		case "CXX":
			languages = append(languages, "cpp")
		}
	}
	for _, field := range fields[1:] {
		if field == "LANGUAGES" || field == "NONE" {
			return languages
		}
	}
	if len(languages) == 0 {
		return []string{"c", "cpp"}
	}
	return languages
}

var (
	mesonProject = regexp.MustCompile(`(?im)^\s*project\s*\(([^)]*)\)`)
	mesonC       = regexp.MustCompile(`[, ]['"]c['"]`)
	mesonCPP     = regexp.MustCompile(`[, ]['"]cpp['"]`)
	makeC        = regexp.MustCompile(`(?m)^[ \t]*(?:override[ \t]+)?CC[ \t]*[:?+]?=`)
	makeCPP      = regexp.MustCompile(`(?m)^[ \t]*(?:override[ \t]+)?CXX[ \t]*[:?+]?=`)
)

func readinessNativeLanguages(name string, data []byte) []string {
	var c, cpp bool
	switch name {
	case "CMakeLists.txt":
		return cmakeLanguages(data)
	case "meson.build":
		if match := mesonProject.FindSubmatch(data); match != nil {
			c, cpp = mesonC.Match(match[1]), mesonCPP.Match(match[1])
		}
	case "Makefile":
		c, cpp = makeC.Match(data), makeCPP.Match(data)
	}
	var languages []string
	if c {
		languages = append(languages, "c")
	}
	if cpp {
		languages = append(languages, "cpp")
	}
	return languages
}

var cmakeMinimumVersion = regexp.MustCompile(`(?im)^\s*cmake_minimum_required\s*\(\s*VERSION\s+([0-9]+(?:\.[0-9]+){1,2})(?:\.\.\.[0-9.]+)?(?:\s+FATAL_ERROR)?\s*\)`)
var cmakeConditional = regexp.MustCompile(`(?im)^\s*(if|elseif|function|macro|foreach|while)\s*\(`)

func readinessCCommand(p ReadinessProject) (string, string) {
	source := path.Join(p.Directory, "CMakeLists.txt")
	cmake := p.inputs[source]
	if len(cmake) == 0 || cmakeConditional.Match(cmake) {
		return "", source
	}
	kind, lang, cc, code := "C", "c", "cc", "int main(void) { return 0; }"
	if p.Ecosystem == "cpp" {
		kind, lang, cc, code = "CXX", "c++", "c++", "int main() { return 0; }"
	}
	var flags []string
	for _, required := range cRequired.FindAllSubmatch(cmake, -1) {
		if string(required[1]) != kind {
			continue
		}
		for _, standard := range cStandard.FindAllSubmatch(cmake, -1) {
			if string(standard[1]) == kind {
				version := string(standard[2])
				flags = append(flags, "-std="+lang+version)
			}
		}
	}
	libraries := cLibrary.FindAllSubmatch(cmake, -1)
	headers := cHeader.FindAllSubmatch(cmake, -1)
	for _, header := range headers {
		name := string(header[1])
		if !strings.HasPrefix(name, "/") && !strings.Contains(name, "..") {
			code = "#include <" + name + ">\n" + code
		}
	}
	for _, library := range libraries {
		flags = append(flags, "-l"+string(library[1]))
	}
	minimum := cmakeMinimumVersion.FindSubmatch(cmake)
	if len(flags) == 0 && len(headers) == 0 && minimum == nil {
		return "", source
	}
	command := fmt.Sprintf("printf '%%s\\n' '%s' | %s -x %s - -x none %s -o ./readiness", code, cc, lang, strings.Join(flags, " "))
	if minimum != nil {
		command = "command -v cmake && cmake --version && " + command
	}
	return command, source
}

var unsupportedMSBuild = regexp.MustCompile(`(?i)<\s*(Import|ProjectReference|FrameworkReference|Choose|Target)(\s|/|>)`)

type readinessSDKProject struct {
	XMLName    xml.Name `xml:"Project"`
	SDK        string   `xml:"Sdk,attr"`
	Properties []struct {
		Condition  string `xml:"Condition,attr"`
		Framework  string `xml:"TargetFramework"`
		Frameworks string `xml:"TargetFrameworks"`
	} `xml:"PropertyGroup"`
	Items []struct {
		Condition string `xml:"Condition,attr"`
		Packages  []struct {
			Name           string `xml:"Include,attr"`
			Version        string `xml:"Version,attr"`
			VersionElement string `xml:"Version"`
			Condition      string `xml:"Condition,attr"`
		} `xml:"PackageReference"`
	} `xml:"ItemGroup"`
}

func readinessDotnetInputs(p ReadinessProject) (map[string][]byte, string) {
	files := map[string][]byte{}
	var projects []string
	for name := range p.inputs {
		if path.Dir(name) == p.Directory && strings.HasSuffix(path.Base(name), "proj") && readinessEcosystem(name) == "dotnet" {
			projects = append(projects, name)
		}
	}
	sort.Strings(projects)
	for _, name := range projects {
		var project readinessSDKProject
		if unsupportedMSBuild.Match(p.inputs[name]) || xml.Unmarshal(p.inputs[name], &project) != nil || project.SDK != "Microsoft.NET.Sdk" {
			return nil, name
		}
		var frameworks []string
		for _, property := range project.Properties {
			if property.Framework == "" && property.Frameworks == "" {
				continue
			}
			if property.Condition != "" {
				return nil, name
			}
			frameworks = append(frameworks, strings.Split(strings.TrimSpace(property.Framework+property.Frameworks), ";")...)
		}
		if len(frameworks) == 0 {
			return nil, name
		}
		for _, framework := range frameworks {
			if !sdkFramework.MatchString(framework) {
				return nil, name
			}
		}
		var packages strings.Builder
		for _, group := range project.Items {
			for _, pkg := range group.Packages {
				version := pkg.Version + pkg.VersionElement
				if group.Condition != "" || pkg.Condition != "" || !packageID.MatchString(pkg.Name) || !packageVer.MatchString(version) {
					return nil, name
				}
				fmt.Fprintf(&packages, `<PackageReference Include="%s" Version="%s" />`, pkg.Name, version)
			}
		}
		generated := fmt.Sprintf(`<Project Sdk="Microsoft.NET.Sdk"><PropertyGroup><TargetFrameworks>%s</TargetFrameworks><NuGetAudit>false</NuGetAudit></PropertyGroup><ItemGroup>%s</ItemGroup></Project>`, strings.Join(frameworks, ";"), packages.String())
		index := len(files)
		files[path.Join(p.Directory, fmt.Sprintf("readiness-%d.csproj", index))] = []byte(generated)
		if lock, ok := p.inputs[path.Join(p.Directory, "packages.lock.json")]; ok {
			if len(projects) != 1 || !json.Valid(lock) {
				return nil, name
			}
			files[path.Join(p.Directory, fmt.Sprintf("packages.readiness-%d.lock.json", index))] = lock
		}
	}
	for dir := p.Directory; ; dir = path.Dir(dir) {
		for _, props := range []string{"Directory.Build.props", "Directory.Build.targets", "Directory.Packages.props"} {
			if _, ok := p.inputs[path.Join(dir, props)]; ok {
				return nil, path.Join(dir, props)
			}
		}

		name := path.Join(dir, "global.json")
		if data, ok := p.inputs[name]; ok {
			var selector struct {
				SDK struct {
					Version         string   `json:"version"`
					RollForward     string   `json:"rollForward,omitempty"`
					AllowPrerelease *bool    `json:"allowPrerelease,omitempty"`
					Paths           []string `json:"paths,omitempty"`
				} `json:"sdk"`
			}
			if json.Unmarshal(data, &selector) != nil || !packageVer.MatchString(selector.SDK.Version) || len(selector.SDK.Paths) > 0 {
				return nil, name
			}
			clean, err := json.Marshal(selector)
			if err != nil {
				return nil, name
			}
			files[name] = clean
			if len(projects) == 0 && dir == p.Directory {
				return files, name
			}
			break
		}
		if dir == "." {
			break
		}
	}
	if len(projects) == 0 {
		return nil, ""
	}
	files[path.Join(p.Directory, "NuGet.Config")] = []byte(`<configuration><packageSources><clear/></packageSources></configuration>`)
	return files, projects[0]
}

func writeReadinessFiles(root *safefs.Root, files map[string][]byte) error {
	for name, data := range files {
		if err := root.MkdirAll(path.Dir(name), 0700); err != nil {
			return err
		}
		if err := root.WriteFile(name, data, 0600); err != nil {
			return err
		}
	}
	return nil
}
