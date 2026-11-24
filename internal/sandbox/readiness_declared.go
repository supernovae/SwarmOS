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
	if len(flags) == 0 && len(headers) == 0 && len(libraries) == 0 && minimum == nil {
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

// readinessJavaCommand inspects only literal project declarations and checks
// the corresponding files in the store visible to the selected worker.
func readinessJavaCommand(p ReadinessProject) (string, string, string) {
	const remedy = "provision the matching Java tools and offline store in the selected environment"
	switch p.Ecosystem {
	case "maven":
		return readinessMavenCommand(p, remedy)
	case "gradle":
		return readinessGradleCommand(p, remedy)
	default:
		return "", p.Source, remedy
	}
}

var (
	javaComponent      = regexp.MustCompile(`^[A-Za-z0-9_-]+(?:\.[A-Za-z0-9_-]+)*$`)
	gradleDistribution = regexp.MustCompile(`(?m)^distributionUrl=[^\r\n]*[/]gradle-([0-9]+(?:\.[0-9]+){1,2})-(?:bin|all)\.zip\r?$`)
)

func literalJavaCoordinate(parts ...string) bool {
	for _, part := range parts {
		if len(part) > 128 || !javaComponent.MatchString(part) || strings.HasSuffix(part, "-SNAPSHOT") || part == "LATEST" || part == "RELEASE" {
			return false
		}
	}
	return true
}

func readinessMavenCommand(p ReadinessProject, remedy string) (string, string, string) {
	source := path.Join(p.Directory, "pom.xml")
	var pom struct {
		XMLName      xml.Name   `xml:"project"`
		Parents      []struct{} `xml:"parent"`
		Profiles     []struct{} `xml:"profiles>profile"`
		Managed      []struct{} `xml:"dependencyManagement"`
		Modules      []struct{} `xml:"modules>module"`
		Dependencies []struct {
			Group      string `xml:"groupId"`
			Artifact   string `xml:"artifactId"`
			Version    string `xml:"version"`
			Type       string `xml:"type"`
			Classifier string `xml:"classifier"`
			SystemPath string `xml:"systemPath"`
		} `xml:"dependencies>dependency"`
	}
	if err := xml.Unmarshal(p.inputs[source], &pom); err != nil || len(pom.Parents)+len(pom.Profiles)+len(pom.Managed)+len(pom.Modules) != 0 || len(pom.Dependencies) > 256 {
		return "", source, "unsupported Maven POM structure; " + remedy
	}
	commands := []string{"repo=${SWARM_MAVEN_REPOSITORY:-\"$HOME/.m2/repository\"}"}
	for _, dependency := range pom.Dependencies {
		group, artifact, version := dependency.Group, dependency.Artifact, dependency.Version
		if !literalJavaCoordinate(group, artifact, version) || dependency.Classifier != "" || dependency.SystemPath != "" || (dependency.Type != "" && dependency.Type != "jar") {
			return "", source, "unsupported Maven dependency declaration; use literal JAR coordinates and " + remedy
		}
		prefix := strings.ReplaceAll(group, ".", "/") + "/" + artifact + "/" + version + "/" + artifact + "-" + version
		commands = append(commands, fmt.Sprintf("test -s \"$repo/%s.pom\" && test -s \"$repo/%s.jar\" || { printf 'missing Maven dependency %%s\\n' '%s:%s:%s' >&2; exit 1; }", prefix, prefix, group, artifact, version))
	}
	return strings.Join(commands, "; "), source, ""
}

func readinessGradleCommand(p ReadinessProject, remedy string) (string, string, string) {
	source := path.Join(p.Directory, "gradle.lockfile")
	data, ok := p.inputs[source]
	if !ok {
		return "", p.Source, "commit a Gradle dependency lock with literal coordinates and " + remedy
	}
	commands := []string{"repo=${SWARM_GRADLE_CACHES:-\"$HOME/.gradle/caches\"}/modules-2/files-2.1"}
	wrapper := path.Join(p.Directory, "gradle/wrapper/gradle-wrapper.properties")
	if data, ok := p.inputs[wrapper]; ok {
		versions := gradleDistribution.FindAllSubmatch(data, 2)
		if len(versions) != 1 {
			return "", wrapper, "unsupported Gradle wrapper distribution version; " + remedy
		}
		version := string(versions[0][1])
		commands = append(commands, "gradle --version | grep -Fx 'Gradle "+version+"' || { printf '%s\\n' 'provision Gradle "+version+" for the declared wrapper distribution' >&2; exit 1; }")
		source = wrapper + " and " + source
	}
	entries := make(map[string]string)
	empty := false
	for index, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "empty=") {
			empty = true
			continue
		}
		coordinate, configurations, ok := strings.Cut(line, "=")
		parts := strings.Split(coordinate, ":")
		if !ok || configurations == "" || len(parts) != 3 || !literalJavaCoordinate(parts...) || len(commands) > 256 {
			return "", source, fmt.Sprintf("unsupported Gradle lock entry on line %d; %s", index+1, remedy)
		}
		module := parts[0] + ":" + parts[1]
		if prior, seen := entries[module]; seen && prior != parts[2] {
			return "", source, "ambiguous locked versions for " + module + "; " + remedy
		}
		entries[module] = parts[2]
		prefix, jar := strings.Join(parts, "/"), parts[1]+"-"+parts[2]+".jar"
		commands = append(commands, fmt.Sprintf("found=; for artifact in \"$repo/%s\"/*/%s; do if test -s \"$artifact\"; then found=1; break; fi; done; test -n \"$found\" || { printf 'missing Gradle dependency %%s\\n' '%s' >&2; exit 1; }", prefix, jar, coordinate))
	}
	if len(entries) == 0 && !empty {
		return "", source, "Gradle lock contains no recognized entries; regenerate the dependency lock and " + remedy
	}
	return strings.Join(commands, "; "), source, ""
}
