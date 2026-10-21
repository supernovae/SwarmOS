package workflow

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/supernovae/swarmos/internal/agent"
	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/harness"
	"github.com/supernovae/swarmos/internal/tracker"
	"github.com/supernovae/swarmos/internal/workspace"
	"gopkg.in/yaml.v3"
)

type ProjectPlanOptions struct {
	RepoRoot     string
	FromFile     string // path to markdown/YAML project roadmap or plan
	ScanOpen     bool   // scan and sense-make open tracker issues
	Normalize    bool   // when ScanOpen && Apply, rewrite raw open issues into structured WorkUnit markdown and label swarm:draft
	AgentName    string // optional agent to draft/enrich plan; deterministic analyzer when empty or mock
	Apply        bool   // create/update draft WorkUnit issues, apply triage labels, or post sense-making report on tracker
	Ready        bool   // when Apply is true, also approve created/normalized WorkUnits (requires maintainer auth)
	PublishIssue int    // optional tracking issue number to post the sense-making & goal attestation report to
}

type IssueSenseItem struct {
	IssueNumber      int                `yaml:"issue_number" json:"issue_number"`
	Title            string             `yaml:"title" json:"title"`
	ValidWorkUnit    bool               `yaml:"valid_work_unit" json:"valid_work_unit"`
	Approved         bool               `yaml:"approved" json:"approved"`
	RecommendedLabel string             `yaml:"recommended_label" json:"recommended_label"` // "swarm:ready", "swarm:draft", "swarm:needs-scope", "swarm:blocked"
	Workflow         string             `yaml:"workflow" json:"workflow"`
	Gate             contract.GateMode  `yaml:"gate" json:"gate"`
	DependsOn        []int              `yaml:"depends_on,omitempty" json:"depends_on,omitempty"`
	Touchpoints      []string           `yaml:"touchpoints,omitempty" json:"touchpoints,omitempty"`
	OverlapsWith     []int              `yaml:"overlaps_with,omitempty" json:"overlaps_with,omitempty"`
	GoalAlignment    string             `yaml:"goal_alignment" json:"goal_alignment"` // "PASS", "NEEDS_SCOPE", "RISK_FLAGGED"
	SafetyNotes      []string           `yaml:"safety_notes,omitempty" json:"safety_notes,omitempty"`
	SuggestedUpdate  *contract.WorkUnit `yaml:"suggested_work_unit,omitempty" json:"suggested_work_unit,omitempty"`
}

type ProjectPlanReport struct {
	Mode            string              `yaml:"mode" json:"mode"` // "from-plan" or "backlog-scan"
	BaseSHA         string              `yaml:"base_sha" json:"base_sha"`
	PolicyDigest    string              `yaml:"policy_digest" json:"policy_digest"`
	GoalsDigest     string              `yaml:"goals_digest" json:"goals_digest"`
	EpicIssue       *tracker.Issue      `yaml:"epic_issue,omitempty" json:"epic_issue,omitempty"`
	EpicCreated     bool                `yaml:"epic_created" json:"epic_created"`
	ProposedUnits   []contract.WorkUnit `yaml:"proposed_units,omitempty" json:"proposed_units,omitempty"`
	CreatedIssues   []*tracker.Issue    `yaml:"created_issues,omitempty" json:"created_issues,omitempty"`
	ExistingIssues  []*tracker.Issue    `yaml:"existing_issues,omitempty" json:"existing_issues,omitempty"`
	LeftoverIssues  []*tracker.Issue    `yaml:"leftover_issues,omitempty" json:"leftover_issues,omitempty"`
	BacklogItems    []IssueSenseItem    `yaml:"backlog_items,omitempty" json:"backlog_items,omitempty"`
	ExecutionWaves  [][]int             `yaml:"execution_waves,omitempty" json:"execution_waves,omitempty"`
	SummaryMarkdown string              `yaml:"summary_markdown" json:"summary_markdown"`
}

func (e *Engine) RunProjectPlan(ctx context.Context, opts ProjectPlanOptions) (*ProjectPlanReport, error) {
	root := opts.RepoRoot
	if root == "" {
		root = e.repoRoot
	}
	if root == "" {
		root = "."
	}
	if opts.FromFile == "" && !opts.ScanOpen {
		return nil, fmt.Errorf("specify --from <file> to decompose a project plan or --scan to sense-make open issues")
	}

	baseSHA, _ := workspace.ResolveBase(root)
	cfg := e.Config
	if baseSHA != "" {
		if pinnedCfg, err := harness.LoadConfigAt(root, baseSHA); err == nil {
			cfg = pinnedCfg
		}
	}
	policyDigest := contract.DigestApprovedPolicy(baseSHA, cfg)
	goalsDigest, defaultConstraints := loadGoalsAndRules(root)
	valSpec, _ := harness.LoadValidationSpec(root)

	report := &ProjectPlanReport{
		BaseSHA:      baseSHA,
		PolicyDigest: policyDigest,
		GoalsDigest:  goalsDigest,
	}

	if opts.FromFile != "" {
		report.Mode = "from-plan"
		if opts.Apply {
			if err := e.Tracker.Authorize(ctx, "create-epic"); err != nil {
				return nil, err
			}
		}

		absRoot, absSource, relSourceSlash, inRepo, err := canonicalizePlanSource(root, opts.FromFile)
		if err != nil {
			return nil, err
		}
		if opts.Apply && !inRepo {
			return nil, fmt.Errorf("plan file %q must reside inside repository root %q when --apply is set", opts.FromFile, absRoot)
		}

		parsed, err := e.parseOrSynthesizeRoadmap(ctx, absRoot, absSource, opts.FromFile, opts.AgentName, defaultConstraints, cfg)
		if err != nil {
			return nil, err
		}
		units := parsed.units
		if len(units) == 0 {
			return nil, fmt.Errorf("no WorkUnit tasks found in %s", opts.FromFile)
		}
		if err := validateRoadmapUnits(opts.FromFile, units); err != nil {
			return nil, err
		}

		epicTitle := deriveRoadmapEpicTitle(absSource, parsed.rawSource, parsed.isYAML)
		epicBodyBeforeMarker := formatRoadmapEpicBody(parsed.rawSource, parsed.isYAML)

		if opts.Apply {
			if err := e.applyRoadmapEpicAndChildren(ctx, opts, relSourceSlash, epicTitle, epicBodyBeforeMarker, units, report); err != nil {
				return nil, err
			}
		}

		report.ProposedUnits = units
		var waveItems []IssueSenseItem
		for i, u := range units {
			num := u.IssueNumber
			if !opts.Apply {
				num = i + 1
			}
			waveItems = append(waveItems, IssueSenseItem{
				IssueNumber: num,
				Title:       u.Title,
				DependsOn:   u.DependsOn,
				Touchpoints: extractTouchpoints(absRoot, u.Title+"\n"+u.Objective+"\n"+strings.Join(u.Acceptance, "\n")),
			})
		}
		populateOverlaps(waveItems)
		report.ExecutionWaves = computeExecutionWaves(waveItems)
		report.SummaryMarkdown = renderFromPlanMarkdown(report, epicTitle, opts.Apply, opts.Ready)
		if opts.PublishIssue > 0 && opts.Apply {
			_, _ = e.Tracker.CreateComment(ctx, opts.PublishIssue, report.SummaryMarkdown)
		}
		return report, nil
	}

	// Backlog Sense-Making & Issue Import Mode (--scan)
	report.Mode = "backlog-scan"
	issues, err := e.Tracker.ListIssues(ctx, tracker.IssueFilter{State: "open"})
	if err != nil {
		return nil, err
	}

	readyLabel := "swarm:ready"
	if cfg != nil && cfg.Security.ReadyLabel != "" {
		readyLabel = cfg.Security.ReadyLabel
	}

	items := make([]IssueSenseItem, 0, len(issues))
	for _, iss := range issues {
		if iss.IsEpic() {
			continue
		}
		tps := extractTouchpoints(root, iss.Title+"\n"+iss.Body)
		wu, parseErr := contract.ParseWorkUnitFromIssueWithRoot(iss.Number, iss.Title, iss.Body, root)
		if parseErr == nil && wu != nil {
			tps = extractTouchpoints(root, wu.Title+"\n"+wu.Objective+"\n"+strings.Join(wu.Acceptance, "\n"))
		}
		item := IssueSenseItem{
			IssueNumber: iss.Number,
			Title:       iss.Title,
			Touchpoints: tps,
		}
		if parseErr != nil {
			suggested := synthesizeWorkUnitFromRawIssue(iss, defaultConstraints, cfg)
			item.ValidWorkUnit = false
			item.Workflow = suggested.Workflow
			item.Gate = suggested.Gate
			item.GoalAlignment = "NEEDS_SCOPE"
			item.RecommendedLabel = "swarm:needs-scope"
			item.SuggestedUpdate = suggested
			item.SafetyNotes = append(item.SafetyNotes, "Raw issue missing structured WorkUnit sections (Objective/Constraints/Acceptance Criteria)")
			if touchesProtectedPath(tps, valSpec) {
				item.GoalAlignment = "RISK_FLAGGED"
				item.RecommendedLabel = "swarm:blocked"
				item.SafetyNotes = append(item.SafetyNotes, "Touches protected harness or CI paths without allow_harness_changes")
			}
		} else {
			item.ValidWorkUnit = true
			item.Workflow = wu.Workflow
			item.Gate = wu.Gate
			item.DependsOn = wu.DependsOn
			st, stErr := e.LeaseManager.GetCurrentState(ctx, iss.Number)
			if stErr == nil && st != nil && st.TaskDigest == contract.DigestWorkUnit(wu) && st.PolicyDigest == policyDigest && slices.Contains(iss.Labels, readyLabel) {
				item.Approved = true
			}
			item.GoalAlignment = "PASS"
			if touchesProtectedPath(tps, valSpec) && !wu.AllowHarnessChanges {
				item.GoalAlignment = "RISK_FLAGGED"
				item.SafetyNotes = append(item.SafetyNotes, "Touches protected harness or CI paths without allow_harness_changes: true")
			}
			depsUnmet := e.LeaseManager.VerifyDependencies(ctx, wu) != nil
			if depsUnmet {
				item.SafetyNotes = append(item.SafetyNotes, "Waiting on open prerequisite depends_on issue(s)")
			}
			switch {
			case item.GoalAlignment == "RISK_FLAGGED" || depsUnmet:
				item.RecommendedLabel = "swarm:blocked"
			case item.Approved:
				item.RecommendedLabel = readyLabel
			default:
				item.RecommendedLabel = "swarm:draft"
			}
		}
		items = append(items, item)
	}

	populateOverlaps(items)
	for i := range items {
		if len(items[i].OverlapsWith) > 0 {
			var refs []string
			for _, ov := range items[i].OverlapsWith {
				refs = append(refs, fmt.Sprintf("#%d", ov))
			}
			items[i].SafetyNotes = append(items[i].SafetyNotes, fmt.Sprintf("Overlapping touchpoints with %s (schedule in sequential execution waves)", strings.Join(refs, ", ")))
		}
	}

	if opts.Apply {
		var owner string
		if opts.Ready {
			owner, err = e.Tracker.CurrentUser(ctx)
			if err != nil || strings.TrimSpace(owner) == "" {
				return nil, fmt.Errorf("determine approving actor: %w", err)
			}
		}
		for i := range items {
			item := &items[i]
			if opts.Normalize && !item.ValidWorkUnit && item.SuggestedUpdate != nil {
				body := contract.FormatIssueMarkdown(item.SuggestedUpdate)
				if _, err := e.Tracker.UpdateIssueBody(ctx, item.IssueNumber, body); err != nil {
					return nil, err
				}
				_ = e.Tracker.RemoveLabel(ctx, item.IssueNumber, "swarm:needs-scope")
				item.ValidWorkUnit = true
				if item.GoalAlignment != "RISK_FLAGGED" {
					item.GoalAlignment = "PASS"
					item.RecommendedLabel = "swarm:draft"
				}
			}
			if item.RecommendedLabel != "" {
				_ = e.Tracker.AddLabels(ctx, item.IssueNumber, []string{item.RecommendedLabel})
			}
			if opts.Ready && item.ValidWorkUnit && item.RecommendedLabel != "swarm:blocked" && !item.Approved {
				if _, err := e.LeaseManager.ApproveTask(ctx, item.IssueNumber, owner); err != nil {
					return nil, fmt.Errorf("approve issue #%d: %w", item.IssueNumber, err)
				}
				item.Approved = true
				item.RecommendedLabel = readyLabel
			}
		}
	}

	report.BacklogItems = items
	report.ExecutionWaves = computeExecutionWaves(items)
	report.SummaryMarkdown = renderBacklogScanMarkdown(report, opts.Apply, opts.Normalize)
	if opts.PublishIssue > 0 && opts.Apply {
		_, _ = e.Tracker.CreateComment(ctx, opts.PublishIssue, report.SummaryMarkdown)
	}
	return report, nil
}

func loadGoalsAndRules(root string) (string, []string) {
	h := sha256.New()
	var constraints []string
	hasContent := false

	goalsPath := filepath.Join(root, ".swarm", "goals.md")
	if data, err := os.ReadFile(goalsPath); err == nil && len(strings.TrimSpace(string(data))) > 0 {
		hasContent = true
		_, _ = h.Write(data)
		constraints = append(constraints, "Honor project mission and safety invariants in .swarm/goals.md")
	}

	rulesDir := filepath.Join(root, ".swarm", "rules")
	if entries, err := os.ReadDir(rulesDir); err == nil {
		var names []string
		for _, e := range entries {
			if !e.IsDir() && strings.HasSuffix(strings.ToLower(e.Name()), ".md") {
				names = append(names, e.Name())
			}
		}
		sort.Strings(names)
		for _, name := range names {
			if data, err := os.ReadFile(filepath.Join(rulesDir, name)); err == nil && len(strings.TrimSpace(string(data))) > 0 {
				hasContent = true
				_, _ = h.Write(data)
			}
		}
		if len(names) > 0 {
			constraints = append(constraints, "Comply with repository rules in .swarm/rules/")
		}
	}

	if len(constraints) == 0 {
		constraints = []string{
			"Follow repository conventions in .swarm/conventions.md",
			"Do not modify protected harness or CI files",
		}
	}
	if !hasContent {
		return "none", constraints
	}
	return hex.EncodeToString(h.Sum(nil)), constraints
}

type parsedRoadmap struct {
	rawSource string
	isYAML    bool
	units     []contract.WorkUnit
}

func canonicalizePlanSource(root, fromFile string) (absRoot, absSource, relSlash string, inRepo bool, err error) {
	absRoot, err = filepath.Abs(root)
	if err != nil {
		return "", "", "", false, fmt.Errorf("resolve repository root %q: %w", root, err)
	}
	if evalRoot, evalErr := filepath.EvalSymlinks(absRoot); evalErr == nil {
		absRoot = evalRoot
	}

	sourceCandidate := fromFile
	if !filepath.IsAbs(sourceCandidate) {
		if _, statErr := os.Stat(sourceCandidate); statErr != nil {
			inRootCandidate := filepath.Join(absRoot, sourceCandidate)
			if _, inRootErr := os.Stat(inRootCandidate); inRootErr == nil {
				sourceCandidate = inRootCandidate
			}
		}
	}
	absSource, err = filepath.Abs(sourceCandidate)
	if err != nil {
		return "", "", "", false, fmt.Errorf("resolve plan file %q: %w", fromFile, err)
	}
	if evalSource, evalErr := filepath.EvalSymlinks(absSource); evalErr == nil {
		absSource = evalSource
	}

	rel, relErr := filepath.Rel(absRoot, absSource)
	relSlash = filepath.ToSlash(rel)
	inRepo = relErr == nil && relSlash != "." && relSlash != ".." && !strings.HasPrefix(relSlash, "../") && !filepath.IsAbs(rel)
	return absRoot, absSource, relSlash, inRepo, nil
}

func (e *Engine) parseOrSynthesizeRoadmap(ctx context.Context, root, absSource, displayPath, agentName string, defaultConstraints []string, cfg *contract.SwarmConfig) (*parsedRoadmap, error) {
	data, err := os.ReadFile(absSource)
	if err != nil {
		return nil, fmt.Errorf("read plan file %q: %w", displayPath, err)
	}
	rawSource := string(data)
	raw := rawSource

	// If an explicit non-mock agent is requested to synthesize/enrich a plan, run it when available.
	if strings.TrimSpace(agentName) != "" && !strings.EqualFold(agentName, "mock") && cfg != nil {
		if runner, err := agent.NewConfiguredRunner(agentName, cfg, e.Tracker.BackendName() == "local"); err == nil {
			activeDir := filepath.Join(root, ".swarm", "active")
			_ = os.MkdirAll(activeDir, 0o755)
			outPath := filepath.Join(activeDir, "PROJECT_PLAN.yaml")
			_ = os.Remove(outPath)
			prompt := fmt.Sprintf("Read %s, .swarm/goals.md, and .swarm/rules/. Decompose the roadmap into a YAML list of SwarmOS WorkUnit objects at .swarm/active/PROJECT_PLAN.yaml.", displayPath)
			if _, runErr := runner.Run(ctx, agent.RunRequest{
				Agent:        agentName,
				Role:         "planner",
				Mode:         "headless",
				WorktreePath: root,
				TaskPrompt:   prompt,
			}); runErr == nil {
				if synthBytes, readErr := os.ReadFile(outPath); readErr == nil && len(synthBytes) > 0 {
					raw = string(synthBytes)
				}
			}
		}
	}

	defaultGate := contract.GateAgent
	if cfg != nil && cfg.DefaultGate != "" {
		defaultGate = cfg.DefaultGate
	}

	// 1. Try YAML list or wrapper struct
	ext := strings.ToLower(filepath.Ext(absSource))
	trimmedRaw := strings.TrimSpace(raw)
	looksLikeYAML := ext == ".yaml" || ext == ".yml" ||
		strings.HasPrefix(trimmedRaw, "- ") ||
		strings.HasPrefix(trimmedRaw, "work_units:") ||
		strings.HasPrefix(trimmedRaw, "tasks:") ||
		strings.HasPrefix(trimmedRaw, "subtasks:")
	if looksLikeYAML {
		var list []contract.WorkUnit
		listErr := yaml.Unmarshal([]byte(raw), &list)
		if listErr == nil && len(list) > 0 && (list[0].Title != "" || list[0].Objective != "" || list[0].ID != "") {
			return &parsedRoadmap{
				rawSource: rawSource,
				isYAML:    ext == ".yaml" || ext == ".yml" || !strings.Contains(rawSource, "\n## "),
				units:     normalizeProposedUnits(list, defaultConstraints, defaultGate, true),
			}, nil
		}
		var wrapper struct {
			WorkUnits []contract.WorkUnit `yaml:"work_units"`
			Tasks     []contract.WorkUnit `yaml:"tasks"`
			Subtasks  []contract.WorkUnit `yaml:"subtasks"`
		}
		wrapErr := yaml.Unmarshal([]byte(raw), &wrapper)
		if wrapErr == nil {
			combined := append(append(wrapper.WorkUnits, wrapper.Tasks...), wrapper.Subtasks...)
			if len(combined) > 0 {
				return &parsedRoadmap{
					rawSource: rawSource,
					isYAML:    ext == ".yaml" || ext == ".yml" || !strings.Contains(rawSource, "\n## "),
					units:     normalizeProposedUnits(combined, defaultConstraints, defaultGate, true),
				}, nil
			}
		}
		if ext == ".yaml" || ext == ".yml" {
			if listErr != nil {
				return nil, fmt.Errorf("parse YAML roadmap %s: %w", displayPath, listErr)
			}
			if wrapErr != nil {
				return nil, fmt.Errorf("parse YAML roadmap %s: %w", displayPath, wrapErr)
			}
			return nil, fmt.Errorf("no WorkUnit tasks found in YAML roadmap %s", displayPath)
		}
	}

	// 2. Parse Markdown roadmap sections (## <Task Title>)
	units, err := parseMarkdownRoadmap(raw, displayPath, defaultConstraints, defaultGate)
	if err != nil {
		return nil, err
	}
	return &parsedRoadmap{
		rawSource: rawSource,
		isYAML:    false,
		units:     units,
	}, nil
}

func normalizeProposedUnits(units []contract.WorkUnit, defaultConstraints []string, defaultGate contract.GateMode, isYAML bool) []contract.WorkUnit {
	out := make([]contract.WorkUnit, 0, len(units))
	for _, u := range units {
		u.Title = strings.TrimSpace(u.Title)
		u.Objective = strings.TrimSpace(u.Objective)
		u.ID = strings.TrimSpace(u.ID)
		if u.Title == "" && u.Objective != "" {
			u.Title = truncateStr(u.Objective, 60)
		}
		if u.Objective == "" {
			u.Objective = u.Title
		}
		if isYAML {
			if u.ID == "" && u.Title != "" {
				u.ID = workspace.Slugify(u.Title)
			}
		} else if u.Title != "" {
			u.ID = "PLAN-" + workspace.Slugify(u.Title)
		}
		if len(u.Constraints) == 0 {
			u.Constraints = append([]string(nil), defaultConstraints...)
		} else {
			var cleanConstraints []string
			for _, c := range u.Constraints {
				if tc := strings.TrimSpace(c); tc != "" {
					cleanConstraints = append(cleanConstraints, tc)
				}
			}
			u.Constraints = cleanConstraints
		}
		var cleanAcceptance []string
		for _, a := range u.Acceptance {
			if ta := strings.TrimSpace(a); ta != "" {
				cleanAcceptance = append(cleanAcceptance, ta)
			}
		}
		u.Acceptance = cleanAcceptance
		if u.Workflow == "" {
			u.Workflow = inferWorkflowFromText(u.Title + " " + u.Objective)
		}
		if u.Gate == "" {
			u.Gate = defaultGate
		}
		u.IssueNumber = 0
		u.ParentIssue = 0
		out = append(out, u)
	}
	return out
}

var positiveDepTokenRegex = regexp.MustCompile(`^#?([1-9]\d*)$`)

func parseRoadmapDepTokens(rawVal string, allowEmpty bool) ([]int, error) {
	cleaned := strings.TrimSpace(rawVal)
	for _, p := range []string{"- [ ] ", "- [x] ", "- [X] ", "* [ ] ", "* [x] ", "- ", "* "} {
		if strings.HasPrefix(cleaned, p) {
			cleaned = strings.TrimSpace(strings.TrimPrefix(cleaned, p))
			break
		}
	}
	if cleaned == "" {
		if allowEmpty {
			return nil, nil
		}
		return nil, fmt.Errorf("empty depends_on directive")
	}
	if strings.EqualFold(cleaned, "none") || strings.EqualFold(cleaned, "_no response_") {
		return nil, nil
	}
	normalized := strings.ReplaceAll(cleaned, ",", " ")
	fields := strings.Fields(normalized)
	if len(fields) == 0 {
		if allowEmpty {
			return nil, nil
		}
		return nil, fmt.Errorf("empty depends_on directive")
	}
	var deps []int
	for _, tok := range fields {
		if strings.EqualFold(tok, "step") || strings.EqualFold(tok, "issue") {
			continue
		}
		m := positiveDepTokenRegex.FindStringSubmatch(tok)
		if len(m) != 2 {
			return nil, fmt.Errorf("malformed depends_on reference %q", tok)
		}
		n, err := strconv.Atoi(m[1])
		if err != nil || n <= 0 {
			return nil, fmt.Errorf("malformed depends_on reference %q", tok)
		}
		deps = append(deps, n)
	}
	if len(deps) == 0 {
		return nil, fmt.Errorf("malformed depends_on reference %q", rawVal)
	}
	return deps, nil
}

func parseMarkdownRoadmap(raw, sourcePath string, defaultConstraints []string, defaultGate contract.GateMode) ([]contract.WorkUnit, error) {
	lines := strings.Split(strings.ReplaceAll(raw, "\r\n", "\n"), "\n")
	type section struct {
		title string
		lines []string
	}
	var sections []section
	var current *section

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "## ") && !strings.HasPrefix(trimmed, "### ") {
			title := strings.TrimSpace(strings.TrimPrefix(trimmed, "## "))
			if title == "" {
				return nil, fmt.Errorf("%s: empty '##' task heading", sourcePath)
			}
			sections = append(sections, section{title: title})
			current = &sections[len(sections)-1]
			continue
		}
		if trimmed == "##" {
			return nil, fmt.Errorf("%s: empty '##' task heading", sourcePath)
		}
		if current != nil {
			current.lines = append(current.lines, line)
		}
	}

	if len(sections) == 0 {
		return nil, fmt.Errorf("%s: roadmap must contain at least one '## <Task Title>' section", sourcePath)
	}

	var units []contract.WorkUnit
	for idx, sec := range sections {
		stepNum := idx + 1
		var strippedLines []string
		var preExtractedDeps []int
		inDepsSection := false

		for _, l := range sec.lines {
			t := strings.TrimSpace(l)
			lower := strings.ToLower(t)
			if strings.HasPrefix(lower, "### ") {
				hdr := strings.TrimSpace(strings.TrimPrefix(lower, "### "))
				inDepsSection = hdr == "depends on" || hdr == "dependencies"
				strippedLines = append(strippedLines, l)
				continue
			}
			if strings.HasPrefix(lower, "depends on:") || strings.HasPrefix(lower, "depends_on:") {
				val := strings.TrimSpace(t[len("depends on:"):])
				parsedDeps, err := parseRoadmapDepTokens(val, false)
				if err != nil {
					return nil, fmt.Errorf("%s step %d (%s): %w", sourcePath, stepNum, sec.title, err)
				}
				preExtractedDeps = append(preExtractedDeps, parsedDeps...)
				continue
			}
			if inDepsSection && t != "" {
				parsedDeps, err := parseRoadmapDepTokens(t, true)
				if err != nil {
					return nil, fmt.Errorf("%s step %d (%s): %w", sourcePath, stepNum, sec.title, err)
				}
				preExtractedDeps = append(preExtractedDeps, parsedDeps...)
				continue
			}
			strippedLines = append(strippedLines, l)
		}

		body := strings.TrimSpace(strings.Join(strippedLines, "\n"))
		if wu, err := contract.ParseWorkUnitFromIssue(stepNum, sec.title, body); err == nil && wu != nil {
			wu.IssueNumber = 0
			wu.ParentIssue = 0
			wu.Title = sec.title
			wu.ID = "PLAN-" + workspace.Slugify(sec.title)
			if len(preExtractedDeps) > 0 {
				wu.DependsOn = preExtractedDeps
			}
			units = append(units, *wu)
			continue
		}

		var objectiveParts []string
		var constraints []string
		var acceptance []string
		dependsOn := append([]int(nil), preExtractedDeps...)
		workflowName := ""
		gate := defaultGate
		mode := ""

		for _, l := range strippedLines {
			t := strings.TrimSpace(l)
			if t == "" {
				continue
			}
			lower := strings.ToLower(t)
			switch {
			case strings.HasPrefix(lower, "### objective") || strings.HasPrefix(lower, "**objective**"):
				mode = "objective"
				continue
			case strings.HasPrefix(lower, "### constraint") || strings.HasPrefix(lower, "**constraint"):
				mode = "constraints"
				continue
			case strings.HasPrefix(lower, "### acceptance") || strings.HasPrefix(lower, "**acceptance"):
				mode = "acceptance"
				continue
			case strings.HasPrefix(lower, "### depends on") || strings.HasPrefix(lower, "### dependencies"):
				mode = "depends_on"
				continue
			case strings.HasPrefix(lower, "### workflow"):
				mode = "workflow"
				continue
			case strings.HasPrefix(lower, "### plan gate") || strings.HasPrefix(lower, "### plan review gate") || strings.HasPrefix(lower, "### gate"):
				mode = "gate"
				continue
			case strings.HasPrefix(lower, "workflow:"):
				workflowName = strings.TrimSpace(t[len("workflow:"):])
				continue
			case strings.HasPrefix(lower, "gate:"):
				g := contract.GateMode(strings.ToLower(strings.TrimSpace(t[len("gate:"):])))
				if err := contract.ValidateGate(g); err != nil {
					return nil, fmt.Errorf("%s step %d (%s): %w", sourcePath, stepNum, sec.title, err)
				}
				if g != "" {
					gate = g
				}
				continue
			}

			isBullet := strings.HasPrefix(t, "- ") || strings.HasPrefix(t, "* ")
			isCheck := strings.HasPrefix(t, "- [ ] ") || strings.HasPrefix(t, "- [x] ") || strings.HasPrefix(t, "- [X] ") || strings.HasPrefix(t, "* [ ] ") || strings.HasPrefix(t, "* [x] ")
			cleanItem := t
			for _, p := range []string{"- [ ] ", "- [x] ", "- [X] ", "* [ ] ", "* [x] ", "- ", "* "} {
				if strings.HasPrefix(cleanItem, p) {
					cleanItem = strings.TrimSpace(strings.TrimPrefix(cleanItem, p))
					break
				}
			}

			switch mode {
			case "constraints":
				if cleanItem != "" {
					constraints = append(constraints, cleanItem)
				}
			case "acceptance":
				if cleanItem != "" {
					acceptance = append(acceptance, cleanItem)
				}
			case "objective":
				if cleanItem != "" {
					objectiveParts = append(objectiveParts, cleanItem)
				}
			case "workflow":
				if cleanItem != "" {
					workflowName = strings.ToLower(cleanItem)
				}
			case "gate":
				if cleanItem != "" && !strings.EqualFold(cleanItem, "_no response_") {
					g := contract.GateMode(strings.ToLower(cleanItem))
					if err := contract.ValidateGate(g); err != nil {
						return nil, fmt.Errorf("%s step %d (%s): %w", sourcePath, stepNum, sec.title, err)
					}
					gate = g
				}
			case "depends_on":
				// Handled in pre-scan above
			default:
				if isCheck || isBullet {
					if cleanItem != "" {
						acceptance = append(acceptance, cleanItem)
					}
				} else {
					objectiveParts = append(objectiveParts, t)
				}
			}
		}

		obj := strings.TrimSpace(strings.Join(objectiveParts, " "))
		if obj == "" && len(acceptance) > 0 {
			obj = fmt.Sprintf("%s: %s", sec.title, strings.Join(acceptance, "; "))
		}
		if obj == "" {
			obj = sec.title
		}
		units = append(units, contract.WorkUnit{
			ID:          "PLAN-" + workspace.Slugify(sec.title),
			Title:       sec.title,
			Objective:   obj,
			Constraints: constraints,
			Acceptance:  acceptance,
			Workflow:    workflowName,
			Gate:        gate,
			DependsOn:   dependsOn,
		})
	}

	return normalizeProposedUnits(units, defaultConstraints, defaultGate, false), nil
}

var hasAlphaNumRegex = regexp.MustCompile(`[a-zA-Z0-9]`)

func validateRoadmapUnits(sourcePath string, units []contract.WorkUnit) error {
	seenIDs := make(map[string]int, len(units))
	seenSlugs := make(map[string]int, len(units))
	seenTitles := make(map[string]int, len(units))

	for i := range units {
		stepNum := i + 1
		u := &units[i]
		u.IssueNumber = 0
		u.ParentIssue = 0

		if strings.TrimSpace(u.Title) == "" || !hasAlphaNumRegex.MatchString(u.Title) {
			return fmt.Errorf("%s step %d: task title %q must contain alphanumeric characters", sourcePath, stepNum, u.Title)
		}
		if len(u.Acceptance) == 0 {
			return fmt.Errorf("%s step %d (%s): at least one non-blank acceptance criterion is required", sourcePath, stepNum, u.Title)
		}
		for _, acc := range u.Acceptance {
			if strings.TrimSpace(acc) == "" {
				return fmt.Errorf("%s step %d (%s): acceptance criteria must not contain blank items", sourcePath, stepNum, u.Title)
			}
		}
		if err := contract.ValidateGate(u.Gate); err != nil {
			return fmt.Errorf("%s step %d (%s): %w", sourcePath, stepNum, u.Title, err)
		}
		if err := contract.ValidateWorkUnit(u); err != nil {
			return fmt.Errorf("%s step %d (%s): %w", sourcePath, stepNum, u.Title, err)
		}

		idKey := strings.ToLower(strings.TrimSpace(u.ID))
		if prev, exists := seenIDs[idKey]; exists {
			return fmt.Errorf("%s step %d (%s): duplicate task ID %q (collides with step %d)", sourcePath, stepNum, u.Title, u.ID, prev)
		}
		seenIDs[idKey] = stepNum

		titleKey := strings.ToLower(strings.TrimSpace(u.Title))
		if prev, exists := seenTitles[titleKey]; exists {
			return fmt.Errorf("%s step %d (%s): duplicate task title (collides with step %d)", sourcePath, stepNum, u.Title, prev)
		}
		seenTitles[titleKey] = stepNum

		slugKey := workspace.Slugify(u.Title)
		if prev, exists := seenSlugs[slugKey]; exists {
			return fmt.Errorf("%s step %d (%s): task title slug %q collides with step %d", sourcePath, stepNum, u.Title, slugKey, prev)
		}
		seenSlugs[slugKey] = stepNum

		seenDeps := make(map[int]bool, len(u.DependsOn))
		for _, dep := range u.DependsOn {
			if dep <= 0 {
				return fmt.Errorf("%s step %d (%s): invalid depends_on reference %d", sourcePath, stepNum, u.Title, dep)
			}
			if seenDeps[dep] {
				return fmt.Errorf("%s step %d (%s): duplicate depends_on reference %d", sourcePath, stepNum, u.Title, dep)
			}
			seenDeps[dep] = true
			if dep == stepNum {
				return fmt.Errorf("%s step %d (%s): self-referential depends_on step %d", sourcePath, stepNum, u.Title, dep)
			}
			if dep > stepNum && dep <= len(units) {
				return fmt.Errorf("%s step %d (%s): forward depends_on reference to step %d", sourcePath, stepNum, u.Title, dep)
			}
		}
	}
	return nil
}

func deriveRoadmapEpicTitle(absSource, rawSource string, isYAML bool) string {
	if !isYAML {
		for _, line := range strings.Split(strings.ReplaceAll(rawSource, "\r\n", "\n"), "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "# ") && !strings.HasPrefix(trimmed, "## ") {
				if title := strings.TrimSpace(strings.TrimPrefix(trimmed, "# ")); title != "" {
					return title
				}
			}
		}
	}
	base := filepath.Base(absSource)
	ext := filepath.Ext(base)
	name := strings.TrimSpace(strings.TrimSuffix(base, ext))
	if name == "" {
		return base
	}
	return name
}

func formatRoadmapEpicBody(rawSource string, isYAML bool) string {
	normalized := strings.ReplaceAll(rawSource, "\r\n", "\n")
	trimmedRight := strings.TrimRight(normalized, "\n")
	if isYAML {
		return "```yaml\n" + trimmedRight + "\n```"
	}
	return trimmedRight
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func computeChildInputHash(u contract.WorkUnit) (string, error) {
	cp := u
	cp.IssueNumber = 0
	cp.ParentIssue = 0
	raw, err := json.Marshal(cp)
	if err != nil {
		return "", err
	}
	return sha256Hex(string(raw)), nil
}

var (
	epicMarkerLineRegex  = regexp.MustCompile(`(?m)^<!-- swarm:project-plan-epic:v1 source=([0-9a-f]{64}) body=([0-9a-f]{64}) -->\s*$`)
	childMarkerLineRegex = regexp.MustCompile(`(?m)^<!-- swarm:project-plan:v1 source=([0-9a-f]{64}) key=([0-9a-f]{64}) input=([0-9a-f]{64}) body=([0-9a-f]{64}) -->\s*$`)
)

type epicMarkerInfo struct {
	sourceHash string
	bodyHash   string
	bodyBefore string
}

type childMarkerInfo struct {
	sourceHash string
	keyHash    string
	inputHash  string
	bodyHash   string
	bodyBefore string
}

func parseEpicMarker(body string) (*epicMarkerInfo, bool, error) {
	normalized := strings.ReplaceAll(body, "\r\n", "\n")
	if !strings.Contains(normalized, "<!-- swarm:project-plan-epic:v1") {
		return nil, false, nil
	}
	loc := epicMarkerLineRegex.FindStringSubmatchIndex(normalized)
	if loc == nil {
		return nil, true, fmt.Errorf("malformed swarm:project-plan-epic:v1 metadata comment")
	}
	if strings.TrimSpace(normalized[loc[1]:]) != "" {
		return nil, true, fmt.Errorf("unexpected content after swarm:project-plan-epic:v1 metadata comment")
	}
	bodyBefore := normalized[:loc[0]]
	bodyBefore = strings.TrimSuffix(bodyBefore, "\n")
	return &epicMarkerInfo{
		sourceHash: normalized[loc[2]:loc[3]],
		bodyHash:   normalized[loc[4]:loc[5]],
		bodyBefore: bodyBefore,
	}, true, nil
}

func parseChildMarker(body string) (*childMarkerInfo, bool, error) {
	normalized := strings.ReplaceAll(body, "\r\n", "\n")
	if !strings.Contains(normalized, "<!-- swarm:project-plan:v1") {
		return nil, false, nil
	}
	loc := childMarkerLineRegex.FindStringSubmatchIndex(normalized)
	if loc == nil {
		return nil, true, fmt.Errorf("malformed swarm:project-plan:v1 metadata comment")
	}
	if strings.TrimSpace(normalized[loc[1]:]) != "" {
		return nil, true, fmt.Errorf("unexpected content after swarm:project-plan:v1 metadata comment")
	}
	bodyBefore := normalized[:loc[0]]
	bodyBefore = strings.TrimSuffix(bodyBefore, "\n")
	return &childMarkerInfo{
		sourceHash: normalized[loc[2]:loc[3]],
		keyHash:    normalized[loc[4]:loc[5]],
		inputHash:  normalized[loc[6]:loc[7]],
		bodyHash:   normalized[loc[8]:loc[9]],
		bodyBefore: bodyBefore,
	}, true, nil
}

func (e *Engine) applyRoadmapEpicAndChildren(
	ctx context.Context,
	opts ProjectPlanOptions,
	relSourceSlash string,
	epicTitle string,
	epicBodyBeforeMarker string,
	units []contract.WorkUnit,
	report *ProjectPlanReport,
) error {
	if err := e.Tracker.CheckIssueType(ctx, "Epic"); err != nil {
		return err
	}
	var owner string
	if opts.Ready {
		var err error
		owner, err = e.Tracker.CurrentUser(ctx)
		if err != nil || strings.TrimSpace(owner) == "" {
			return fmt.Errorf("determine approving actor: %w", err)
		}
		if err := e.Tracker.Authorize(ctx, "approve-task"); err != nil {
			return err
		}
	}

	allIssues, err := e.Tracker.ListIssues(ctx, tracker.IssueFilter{State: "all"})
	if err != nil {
		return err
	}
	issuesByNum := make(map[int]*tracker.Issue, len(allIssues))
	for _, iss := range allIssues {
		issuesByNum[iss.Number] = iss
	}

	// Check external dependencies (> N) exist before any writes.
	for i, u := range units {
		stepNum := i + 1
		for _, dep := range u.DependsOn {
			if dep > len(units) {
				extIss, ok := issuesByNum[dep]
				if !ok {
					return fmt.Errorf("%s step %d (%s): external depends_on issue #%d does not exist", opts.FromFile, stepNum, u.Title, dep)
				}
				if extIss.IsEpic() {
					return fmt.Errorf("%s step %d (%s): external depends_on issue #%d is an Epic and cannot be a WorkUnit dependency", opts.FromFile, stepNum, u.Title, dep)
				}
			}
		}
	}

	sourceHash := sha256Hex(relSourceSlash)
	expectedEpicBodyHash := sha256Hex(epicBodyBeforeMarker)

	// Check Epic title does not collide with any proposed task title in the same plan.
	for i, u := range units {
		if strings.EqualFold(strings.TrimSpace(u.Title), strings.TrimSpace(epicTitle)) {
			return fmt.Errorf("%s step %d (%s): task title collides with Epic title %q", opts.FromFile, i+1, u.Title, epicTitle)
		}
	}

	type matchedChild struct {
		issue  *tracker.Issue
		marker *childMarkerInfo
		parent int
	}
	var existingEpic *tracker.Issue
	var existingEpicMarker *epicMarkerInfo
	existingChildrenByKey := make(map[string]*matchedChild)

	for _, iss := range allIssues {
		epicM, hasEpicM, epicErr := parseEpicMarker(iss.Body)
		if epicErr != nil {
			return fmt.Errorf("issue #%d: %w", iss.Number, epicErr)
		}
		childM, hasChildM, childErr := parseChildMarker(iss.Body)
		if childErr != nil {
			return fmt.Errorf("issue #%d: %w", iss.Number, childErr)
		}
		if hasEpicM && hasChildM {
			return fmt.Errorf("issue #%d contains both Epic and child project-plan markers", iss.Number)
		}
		if hasEpicM && epicM.sourceHash == sourceHash {
			if existingEpic != nil {
				return fmt.Errorf("duplicate roadmap Epic issues #%d and #%d for %s", existingEpic.Number, iss.Number, relSourceSlash)
			}
			if !iss.IsEpic() {
				return fmt.Errorf("existing roadmap Epic issue #%d has type %q, want \"Epic\"", iss.Number, iss.Type)
			}
			if strings.TrimSpace(iss.Title) != strings.TrimSpace(epicTitle) {
				return fmt.Errorf("existing roadmap Epic issue #%d title %q was edited or does not match %q", iss.Number, iss.Title, epicTitle)
			}
			if sha256Hex(epicM.bodyBefore) != epicM.bodyHash {
				return fmt.Errorf("existing roadmap Epic issue #%d body was manually modified (body hash mismatch)", iss.Number)
			}
			existingEpic = iss
			existingEpicMarker = epicM
		}
		if hasChildM && childM.sourceHash == sourceHash {
			if iss.IsEpic() {
				return fmt.Errorf("issue #%d has type \"Epic\" but carries a child WorkUnit marker", iss.Number)
			}
			if prev, dup := existingChildrenByKey[childM.keyHash]; dup {
				return fmt.Errorf("duplicate roadmap child issues #%d and #%d for same task key in %s", prev.issue.Number, iss.Number, relSourceSlash)
			}
			if sha256Hex(childM.bodyBefore) != childM.bodyHash {
				return fmt.Errorf("existing roadmap child issue #%d body was manually modified (body hash mismatch)", iss.Number)
			}
			parentNum, err := e.Tracker.ParentIssueNumber(ctx, iss.Number)
			if err != nil {
				return fmt.Errorf("check parent of issue #%d: %w", iss.Number, err)
			}
			existingChildrenByKey[childM.keyHash] = &matchedChild{
				issue:  iss,
				marker: childM,
				parent: parentNum,
			}
		}
	}

	if existingEpic == nil && len(existingChildrenByKey) > 0 {
		for _, ch := range existingChildrenByKey {
			return fmt.Errorf("existing child issue #%d references roadmap %s, but no matching Epic issue exists", ch.issue.Number, relSourceSlash)
		}
	}

	// Check native parent consistency for all existing same-source children (including leftovers).
	for _, ch := range existingChildrenByKey {
		if ch.parent != 0 && existingEpic != nil && ch.parent != existingEpic.Number {
			return fmt.Errorf("roadmap child issue #%d is linked to conflicting parent #%d (expected Epic #%d)", ch.issue.Number, ch.parent, existingEpic.Number)
		}
		if wu, err := contract.ParseWorkUnitFromIssue(ch.issue.Number, ch.issue.Title, ch.issue.Body); err == nil && wu != nil {
			if existingEpic != nil && wu.ParentIssue != 0 && wu.ParentIssue != existingEpic.Number {
				return fmt.Errorf("roadmap child issue #%d body specifies parent #%d (expected Epic #%d)", ch.issue.Number, wu.ParentIssue, existingEpic.Number)
			}
		}
	}

	// Check Epic title collision with any unmatching existing issue.
	for _, iss := range allIssues {
		if existingEpic != nil && iss.Number == existingEpic.Number {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(iss.Title), strings.TrimSpace(epicTitle)) {
			return fmt.Errorf("epic title %q collides with existing issue #%d (%s)", epicTitle, iss.Number, iss.Title)
		}
	}

	proposedKeys := make(map[string]bool, len(units))
	inputHashes := make([]string, len(units))
	keyHashes := make([]string, len(units))
	stepExistingNum := make(map[int]int, len(units))

	for i, u := range units {
		stepNum := i + 1
		keyHash := sha256Hex(u.ID)
		inputHash, err := computeChildInputHash(u)
		if err != nil {
			return fmt.Errorf("%s step %d (%s): %w", opts.FromFile, stepNum, u.Title, err)
		}
		keyHashes[i] = keyHash
		inputHashes[i] = inputHash
		proposedKeys[keyHash] = true

		if existing, ok := existingChildrenByKey[keyHash]; ok {
			if existing.marker.inputHash != inputHash {
				return fmt.Errorf("%s step %d (%s): conflicts with existing issue #%d (task definition changed)", opts.FromFile, stepNum, u.Title, existing.issue.Number)
			}
			if strings.TrimSpace(existing.issue.Title) != strings.TrimSpace(u.Title) {
				return fmt.Errorf("%s step %d (%s): title conflicts with existing issue #%d (%s)", opts.FromFile, stepNum, u.Title, existing.issue.Number, existing.issue.Title)
			}
			stepExistingNum[stepNum] = existing.issue.Number
			for _, other := range allIssues {
				if other.Number == existing.issue.Number {
					continue
				}
				if strings.EqualFold(strings.TrimSpace(other.Title), strings.TrimSpace(u.Title)) {
					return fmt.Errorf("%s step %d (%s): task title collides with existing issue #%d (%s)", opts.FromFile, stepNum, u.Title, other.Number, other.Title)
				}
			}
		} else {
			// Proposed new task: refuse case-folded trimmed title collision with any existing issue (including Epics).
			for _, other := range allIssues {
				if strings.EqualFold(strings.TrimSpace(other.Title), strings.TrimSpace(u.Title)) {
					return fmt.Errorf("%s step %d (%s): proposed task title collides with existing issue #%d (%s)", opts.FromFile, stepNum, u.Title, other.Number, other.Title)
				}
			}
		}
	}

	// Verify existing children's formatted bodies match resolved dependencies before any writes.
	if existingEpic != nil {
		for i, u := range units {
			stepNum := i + 1
			existing, ok := existingChildrenByKey[keyHashes[i]]
			if !ok {
				continue
			}
			expectedUnit := u
			expectedUnit.ParentIssue = existingEpic.Number
			var resolvedDeps []int
			for _, dep := range u.DependsOn {
				if dep <= len(units) {
					depIssueNum, depExists := stepExistingNum[dep]
					if !depExists {
						return fmt.Errorf("%s step %d (%s): existing issue #%d cannot depend on newly added uncreated step %d", opts.FromFile, stepNum, u.Title, existing.issue.Number, dep)
					}
					resolvedDeps = append(resolvedDeps, depIssueNum)
				} else {
					resolvedDeps = append(resolvedDeps, dep)
				}
			}
			expectedUnit.DependsOn = resolvedDeps
			expectedFormatted := strings.TrimSuffix(contract.FormatIssueMarkdown(&expectedUnit), "\n")
			if sha256Hex(expectedFormatted) != existing.marker.bodyHash {
				return fmt.Errorf("%s step %d (%s): existing issue #%d body hash does not match resolved parent/dependencies", opts.FromFile, stepNum, u.Title, existing.issue.Number)
			}
		}
	}

	// Collect leftover issues (same source, missing from current plan).
	var leftovers []*tracker.Issue
	for keyHash, ch := range existingChildrenByKey {
		if !proposedKeys[keyHash] {
			leftovers = append(leftovers, ch.issue)
		}
	}
	sort.Slice(leftovers, func(i, j int) bool {
		return leftovers[i].Number < leftovers[j].Number
	})
	report.LeftoverIssues = leftovers

	// Preflight succeeded! Now reconcile or create Epic.
	epicMarker := fmt.Sprintf("<!-- swarm:project-plan-epic:v1 source=%s body=%s -->", sourceHash, expectedEpicBodyHash)
	fullEpicBody := epicBodyBeforeMarker + "\n" + epicMarker
	if existingEpic != nil {
		report.EpicCreated = false
		if existingEpicMarker.bodyHash != expectedEpicBodyHash {
			updated, err := e.Tracker.UpdateIssueBody(ctx, existingEpic.Number, fullEpicBody)
			if err != nil {
				return fmt.Errorf("refresh Epic issue #%d body: %w", existingEpic.Number, err)
			}
			existingEpic = updated
		}
		report.EpicIssue = existingEpic
	} else {
		createdEpic, err := e.Tracker.CreateTypedIssue(ctx, epicTitle, fullEpicBody, nil, "Epic")
		if err != nil {
			return err
		}
		existingEpic = createdEpic
		report.EpicIssue = createdEpic
		report.EpicCreated = true
	}

	// Create or reuse child issues in file order and link to Epic.
	stepToIssueNum := make(map[int]int, len(units))
	var createdNums []int
	for i := range units {
		stepNum := i + 1
		u := &units[i]
		u.ParentIssue = existingEpic.Number
		var mappedDeps []int
		for _, dep := range u.DependsOn {
			if dep >= 1 && dep <= len(units) {
				mappedDeps = append(mappedDeps, stepToIssueNum[dep])
			} else {
				mappedDeps = append(mappedDeps, dep)
			}
		}
		u.DependsOn = mappedDeps

		keyHash := keyHashes[i]
		inputHash := inputHashes[i]
		if existing, ok := existingChildrenByKey[keyHash]; ok {
			childIss := existing.issue
			u.IssueNumber = childIss.Number
			stepToIssueNum[stepNum] = childIss.Number
			if err := e.Tracker.LinkSubIssue(ctx, existingEpic.Number, childIss.Number); err != nil {
				return fmt.Errorf("link existing child issue #%d to Epic #%d (created children: %v): %w", childIss.Number, existingEpic.Number, createdNums, err)
			}
			report.ExistingIssues = append(report.ExistingIssues, childIss)
			continue
		}

		formattedBody := strings.TrimSuffix(contract.FormatIssueMarkdown(u), "\n")
		bodyHash := sha256Hex(formattedBody)
		childMarker := fmt.Sprintf("<!-- swarm:project-plan:v1 source=%s key=%s input=%s body=%s -->", sourceHash, keyHash, inputHash, bodyHash)
		fullChildBody := formattedBody + "\n" + childMarker

		childIss, err := e.Tracker.CreateIssue(ctx, u.Title, fullChildBody, []string{"swarm:draft"})
		if err != nil {
			return fmt.Errorf("create child step %d (%s) under Epic #%d (created children: %v): %w", stepNum, u.Title, existingEpic.Number, createdNums, err)
		}
		createdNums = append(createdNums, childIss.Number)
		u.IssueNumber = childIss.Number
		stepToIssueNum[stepNum] = childIss.Number

		if err := e.Tracker.LinkSubIssue(ctx, existingEpic.Number, childIss.Number); err != nil {
			return fmt.Errorf("link created child issue #%d to Epic #%d (created children: %v): %w", childIss.Number, existingEpic.Number, createdNums, err)
		}
		if opts.Ready {
			if _, err := e.LeaseManager.ApproveTask(ctx, childIss.Number, owner); err != nil {
				return fmt.Errorf("approve created child issue #%d under Epic #%d (created children: %v): %w", childIss.Number, existingEpic.Number, createdNums, err)
			}
			if refreshed, refErr := e.Tracker.GetIssue(ctx, childIss.Number); refErr == nil && refreshed != nil {
				childIss = refreshed
			}
		}
		report.CreatedIssues = append(report.CreatedIssues, childIss)
	}
	return nil
}

func synthesizeWorkUnitFromRawIssue(iss *tracker.Issue, defaultConstraints []string, cfg *contract.SwarmConfig) *contract.WorkUnit {
	cleanBody := strings.TrimSpace(contract.StripHTMLComments(iss.Body))
	var bullets []string
	var paragraphs []string
	for _, line := range strings.Split(strings.ReplaceAll(cleanBody, "\r\n", "\n"), "\n") {
		t := strings.TrimSpace(line)
		if t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		if strings.HasPrefix(t, "- ") || strings.HasPrefix(t, "* ") {
			item := t
			for _, p := range []string{"- [ ] ", "- [x] ", "- [X] ", "* [ ] ", "* [x] ", "- ", "* "} {
				if strings.HasPrefix(item, p) {
					item = strings.TrimSpace(strings.TrimPrefix(item, p))
					break
				}
			}
			if item != "" {
				bullets = append(bullets, item)
			}
		} else {
			paragraphs = append(paragraphs, t)
		}
	}
	objective := strings.TrimSpace(strings.Join(paragraphs, " "))
	if objective == "" {
		objective = iss.Title
	}
	acceptance := bullets
	if len(acceptance) == 0 {
		acceptance = []string{
			fmt.Sprintf("Resolve #%d (%s) with automated test coverage", iss.Number, iss.Title),
			"`swarm validate` passes",
		}
	}
	gate := contract.GateAgent
	if cfg != nil && cfg.DefaultGate != "" {
		gate = cfg.DefaultGate
	}
	return &contract.WorkUnit{
		ID:          fmt.Sprintf("ISSUE-%d", iss.Number),
		IssueNumber: iss.Number,
		Title:       iss.Title,
		Objective:   objective,
		Constraints: append([]string(nil), defaultConstraints...),
		Acceptance:  acceptance,
		Workflow:    inferWorkflowFromText(iss.Title + " " + objective),
		Gate:        gate,
	}
}

func inferWorkflowFromText(text string) string {
	lower := strings.ToLower(text)
	switch {
	case strings.Contains(lower, "bug") || strings.Contains(lower, "fix") || strings.Contains(lower, "crash") || strings.Contains(lower, "regression"):
		return "bugfix"
	case strings.Contains(lower, "refactor") || strings.Contains(lower, "cleanup") || strings.Contains(lower, "extract"):
		return "refactor"
	default:
		return "feature"
	}
}

var pathTokenRegex = regexp.MustCompile(`\.?[A-Za-z0-9_./-]+\.[A-Za-z0-9]+|\.?[A-Za-z0-9_-]+/[A-Za-z0-9_./-]+`)

func extractTouchpoints(repoRoot, text string) []string {
	seen := make(map[string]bool)
	var out []string
	matches := pathTokenRegex.FindAllString(text, -1)
	for _, tok := range matches {
		tok = strings.TrimLeft(tok, ",:;()[]\"'`")
		tok = strings.TrimRight(tok, ".,:;()[]\"'`")
		if tok == "" || strings.Contains(tok, "://") || strings.HasPrefix(tok, "http") {
			continue
		}
		clean := filepath.ToSlash(filepath.Clean(tok))
		if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") || strings.HasPrefix(clean, "/") {
			continue
		}
		if strings.HasPrefix(clean, ".swarm/active") || strings.HasPrefix(clean, ".swarm/worktrees") {
			continue
		}
		if isLikelyRepoTouchpoint(repoRoot, clean) && !seen[clean] {
			seen[clean] = true
			out = append(out, clean)
		}
	}
	sort.Strings(out)
	return out
}

func isLikelyRepoTouchpoint(repoRoot, rel string) bool {
	if _, err := os.Stat(filepath.Join(repoRoot, rel)); err == nil {
		return true
	}
	for _, prefix := range []string{"cmd/", "internal/", "pkg/", "src/", "docs/", ".github/", ".swarm/"} {
		if strings.HasPrefix(rel, prefix) {
			return true
		}
	}
	ext := strings.ToLower(filepath.Ext(rel))
	switch ext {
	case ".go", ".rs", ".py", ".ts", ".js", ".tsx", ".jsx", ".yaml", ".yml", ".json", ".md", ".sh", ".toml":
		if strings.Contains(rel, "/") || fileExistsInRepo(repoRoot, rel) {
			return true
		}
	}
	return false
}

func fileExistsInRepo(repoRoot, rel string) bool {
	_, err := os.Stat(filepath.Join(repoRoot, rel))
	return err == nil
}

func touchesProtectedPath(touchpoints []string, spec *contract.ValidationSpec) bool {
	var patterns []string
	if spec != nil && len(spec.ProtectedPaths) > 0 {
		patterns = spec.ProtectedPaths
	} else {
		patterns = []string{
			".github/workflows/**",
			".swarm/validation/**",
			".swarm/contracts/**",
			".swarm/config.yaml",
			".swarm/prompts/**",
			".swarm/workflows/**",
			".swarm/goals.md",
			".swarm/SKILLS.md",
			".swarm/rules/**",
			".swarm/skills/**",
		}
	}
	for _, tp := range touchpoints {
		for _, p := range patterns {
			prefix := strings.TrimSuffix(p, "/**")
			prefix = strings.TrimSuffix(prefix, "/*")
			if tp == prefix || strings.HasPrefix(tp, prefix+"/") {
				return true
			}
		}
	}
	return false
}

func populateOverlaps(items []IssueSenseItem) {
	for i := range items {
		var overlaps []int
		for j := range items {
			if i == j {
				continue
			}
			if sharesTouchpoint(items[i].Touchpoints, items[j].Touchpoints) {
				overlaps = append(overlaps, items[j].IssueNumber)
			}
		}
		sort.Ints(overlaps)
		items[i].OverlapsWith = overlaps
	}
}

func sharesTouchpoint(a, b []string) bool {
	for _, x := range a {
		for _, y := range b {
			if x == y || strings.HasPrefix(x, y+"/") || strings.HasPrefix(y, x+"/") {
				return true
			}
		}
	}
	return false
}

func computeExecutionWaves(items []IssueSenseItem) [][]int {
	if len(items) == 0 {
		return nil
	}
	sorted := append([]IssueSenseItem(nil), items...)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].IssueNumber < sorted[j].IssueNumber
	})

	byNum := make(map[int]IssueSenseItem, len(sorted))
	for _, it := range sorted {
		byNum[it.IssueNumber] = it
	}

	waveOf := make(map[int]int, len(sorted))
	for pass := 0; pass < len(sorted); pass++ {
		changed := false
		for _, it := range sorted {
			minWave := 0
			for _, dep := range it.DependsOn {
				if w, ok := waveOf[dep]; ok && w+1 > minWave {
					minWave = w + 1
				}
			}
			for _, ov := range it.OverlapsWith {
				if ov < it.IssueNumber {
					if w, ok := waveOf[ov]; ok && w+1 > minWave {
						minWave = w + 1
					}
				}
			}
			if cur, ok := waveOf[it.IssueNumber]; !ok || minWave > cur {
				waveOf[it.IssueNumber] = minWave
				changed = true
			}
		}
		if !changed {
			break
		}
	}

	maxWave := 0
	for _, w := range waveOf {
		if w > maxWave {
			maxWave = w
		}
	}
	waves := make([][]int, maxWave+1)
	for _, it := range sorted {
		w := waveOf[it.IssueNumber]
		waves[w] = append(waves[w], it.IssueNumber)
	}
	var compact [][]int
	for _, w := range waves {
		if len(w) > 0 {
			compact = append(compact, w)
		}
	}
	return compact
}

func renderFromPlanMarkdown(report *ProjectPlanReport, epicTitle string, applied, ready bool) string {
	var sb strings.Builder
	sb.WriteString("### 🗺️ SwarmOS Project Plan & Decomposition Report\n\n")
	sb.WriteString(fmt.Sprintf("- **Base SHA**: `%s`\n", shortHash(report.BaseSHA)))
	sb.WriteString(fmt.Sprintf("- **Policy Digest**: `%s`\n", shortHash(report.PolicyDigest)))
	sb.WriteString(fmt.Sprintf("- **Goals Digest**: `%s`\n", shortHash(report.GoalsDigest)))
	if applied && report.EpicIssue != nil {
		epicAction := "reused"
		if report.EpicCreated {
			epicAction = "created"
		}
		sb.WriteString(fmt.Sprintf("- **Epic Issue**: `#%d` %s (`type: %s`, state: `%s`, %s, %s)\n",
			report.EpicIssue.Number, report.EpicIssue.Title, report.EpicIssue.Type, report.EpicIssue.State, epicAction, report.EpicIssue.HTMLURL))
		sb.WriteString(fmt.Sprintf("- **Child WorkUnits**: `%d` total (`%d` created, `%d` existing, `%d` leftover)\n\n",
			len(report.ProposedUnits), len(report.CreatedIssues), len(report.ExistingIssues), len(report.LeftoverIssues)))
	} else {
		sb.WriteString(fmt.Sprintf("- **Epic Preview**: %s (`type: Epic`, preview only — not yet published)\n", epicTitle))
		sb.WriteString(fmt.Sprintf("- **Proposed WorkUnits**: `%d`\n\n", len(report.ProposedUnits)))
	}

	createdByNum := make(map[int]*tracker.Issue, len(report.CreatedIssues))
	for _, iss := range report.CreatedIssues {
		createdByNum[iss.Number] = iss
	}
	existingByNum := make(map[int]*tracker.Issue, len(report.ExistingIssues))
	for _, iss := range report.ExistingIssues {
		existingByNum[iss.Number] = iss
	}

	sb.WriteString("| Issue / Step | Title | Workflow | Gate | Depends On | Status |\n")
	sb.WriteString("| :--- | :--- | :--- | :--- | :--- | :--- |\n")
	for idx, u := range report.ProposedUnits {
		deps := "-"
		if len(u.DependsOn) > 0 {
			var parts []string
			for _, d := range u.DependsOn {
				if !applied && d <= len(report.ProposedUnits) {
					parts = append(parts, fmt.Sprintf("Step %d", d))
				} else {
					parts = append(parts, fmt.Sprintf("#%d", d))
				}
			}
			deps = strings.Join(parts, ", ")
		}
		stepLabel := fmt.Sprintf("`Step %d`", idx+1)
		status := "preview"
		if applied {
			stepLabel = fmt.Sprintf("`#%d`", u.IssueNumber)
			if iss, ok := createdByNum[u.IssueNumber]; ok {
				label := "`swarm:draft`"
				if ready {
					label = "`swarm:ready`"
				}
				status = fmt.Sprintf("created (%s, `%s`, %s)", label, iss.State, iss.HTMLURL)
			} else if iss, ok := existingByNum[u.IssueNumber]; ok {
				status = fmt.Sprintf("reused (`%s`, %s)", iss.State, iss.HTMLURL)
			}
		}
		sb.WriteString(fmt.Sprintf("| %s | %s | `%s` | `%s` | %s | %s |\n",
			stepLabel, u.Title, u.Workflow, u.Gate, deps, status))
	}

	if len(report.LeftoverIssues) > 0 {
		sb.WriteString("\n#### 📌 Leftover Roadmap Issues (Not in Current Plan)\n\n")
		for _, iss := range report.LeftoverIssues {
			sb.WriteString(fmt.Sprintf("- `#%d` %s (`%s`, %s)\n", iss.Number, iss.Title, iss.State, iss.HTMLURL))
		}
	}

	renderWavesSectionWithPrefix(&sb, report.ExecutionWaves, !applied)
	return sb.String()
}

func renderBacklogScanMarkdown(report *ProjectPlanReport, applied, normalized bool) string {
	var readyCount, draftCount, needsScopeCount, blockedCount int
	for _, it := range report.BacklogItems {
		switch it.RecommendedLabel {
		case "swarm:ready":
			readyCount++
		case "swarm:draft":
			draftCount++
		case "swarm:needs-scope":
			needsScopeCount++
		case "swarm:blocked":
			blockedCount++
		}
	}

	var sb strings.Builder
	sb.WriteString("### 🧭 SwarmOS Backlog Sense-Making & Goal Attestation\n\n")
	sb.WriteString(fmt.Sprintf("- **Base SHA**: `%s` | **Policy Digest**: `%s` | **Goals Digest**: `%s`\n",
		shortHash(report.BaseSHA), shortHash(report.PolicyDigest), shortHash(report.GoalsDigest)))
	sb.WriteString(fmt.Sprintf("- **Open Issues**: `%d` (`%d` ready, `%d` draft, `%d` needs-scope, `%d` blocked)\n",
		len(report.BacklogItems), readyCount, draftCount, needsScopeCount, blockedCount))
	if applied {
		sb.WriteString(fmt.Sprintf("- **Tracker Actions Applied**: labels synced (`normalize=%t`)\n", normalized))
	}
	sb.WriteString("\n| Issue | Recommended Label | Goal Alignment | Touchpoints | Overlaps | Depends On | Next Action |\n")
	sb.WriteString("| :--- | :--- | :--- | :--- | :--- | :--- | :--- |\n")

	for _, it := range report.BacklogItems {
		tps := "-"
		if len(it.Touchpoints) > 0 {
			tps = "`" + strings.Join(it.Touchpoints, "`, `") + "`"
		}
		ovs := "-"
		if len(it.OverlapsWith) > 0 {
			var parts []string
			for _, o := range it.OverlapsWith {
				parts = append(parts, fmt.Sprintf("#%d", o))
			}
			ovs = strings.Join(parts, ", ")
		}
		deps := "-"
		if len(it.DependsOn) > 0 {
			var parts []string
			for _, d := range it.DependsOn {
				parts = append(parts, fmt.Sprintf("#%d", d))
			}
			deps = strings.Join(parts, ", ")
		}
		var nextAction string
		switch it.RecommendedLabel {
		case "swarm:ready":
			nextAction = fmt.Sprintf("`swarm work %d`", it.IssueNumber)
		case "swarm:draft":
			nextAction = fmt.Sprintf("`swarm issue approve %d`", it.IssueNumber)
		case "swarm:needs-scope":
			nextAction = "`swarm plan --scan --apply --normalize`"
		case "swarm:blocked":
			nextAction = "Resolve dependencies / review protected paths"
		}
		sb.WriteString(fmt.Sprintf("| `#%d` %s | `%s` | `%s` | %s | %s | %s | %s |\n",
			it.IssueNumber, it.Title, it.RecommendedLabel, it.GoalAlignment, tps, ovs, deps, nextAction))
	}

	renderWavesSection(&sb, report.ExecutionWaves)
	return sb.String()
}

func renderWavesSection(sb *strings.Builder, waves [][]int) {
	renderWavesSectionWithPrefix(sb, waves, false)
}

func renderWavesSectionWithPrefix(sb *strings.Builder, waves [][]int, useStepPrefix bool) {
	if len(waves) == 0 {
		return
	}
	sb.WriteString("\n#### 🌊 Recommended Execution Waves\n\n")
	for idx, wave := range waves {
		var parts []string
		for _, num := range wave {
			if useStepPrefix {
				parts = append(parts, fmt.Sprintf("`Step %d`", num))
			} else {
				parts = append(parts, fmt.Sprintf("`#%d`", num))
			}
		}
		mode := "Parallel"
		if len(wave) == 1 {
			mode = "Single"
		}
		sb.WriteString(fmt.Sprintf("- **Wave %d (%s)**: %s\n", idx+1, mode, strings.Join(parts, ", ")))
	}
}

func shortHash(s string) string {
	if s == "" {
		return "-"
	}
	if len(s) > 12 {
		return s[:12]
	}
	return s
}
