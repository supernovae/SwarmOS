package sandbox

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/supernovae/swarmos/internal/process"
)

type ToolDefinition struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

func executorTools() []ToolDefinition {
	tool := func(name, description string, fields ...string) ToolDefinition {
		properties := map[string]any{}
		for _, field := range fields {
			properties[field] = map[string]string{"type": "string"}
		}
		return ToolDefinition{Name: name, Description: description, InputSchema: map[string]any{"type": "object", "properties": properties, "required": fields, "additionalProperties": false}}
	}
	return []ToolDefinition{
		tool("swarm_read", "Read a UTF-8 file in the task workspace. Paths are relative to /workspace.", "path"),
		tool("swarm_write", "Create or replace a UTF-8 workspace file, creating parent directories.", "path", "content"),
		tool("swarm_edit", "Replace exactly one occurrence of old_text in a workspace file.", "path", "old_text", "new_text"),
		tool("swarm_shell", "Run a shell command in /workspace to search, build, test or analyze. No host credentials or network. Background processes end with the command. Maximum duration: two minutes.", "command"),
	}
}

type ToolResult struct {
	Content []ToolText `json:"content"`
	IsError bool       `json:"isError,omitempty"`
}
type ToolText struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

func (e ToolExecutor) Call(ctx context.Context, name string, raw json.RawMessage) ToolResult {
	result := func(text string, err bool) ToolResult {
		return ToolResult{Content: []ToolText{{Type: "text", Text: text}}, IsError: err}
	}
	if len(raw) > toolArgumentBytes {
		return result("tool arguments exceed 128 KiB", true)
	}
	var args map[string]string
	if err := json.Unmarshal(raw, &args); err != nil {
		return result("tool arguments must be strings", true)
	}
	var definition *ToolDefinition
	for _, tool := range executorTools() {
		if tool.Name == name {
			definition = &tool
			break
		}
	}
	if definition == nil {
		return result("tool is not authorized", true)
	}
	fields := definition.InputSchema["required"].([]string)
	if len(args) != len(fields) {
		return result("unexpected tool arguments", true)
	}
	for _, field := range fields {
		if _, ok := args[field]; !ok {
			return result("missing "+field, true)
		}
	}
	var output process.Output
	var err error
	if name == "swarm_shell" {
		err = e.Run(ctx, args["command"], &output, &output)
	} else {
		// The file helpers themselves run inside the OS boundary, so symlinks,
		// absolute paths, races and interpreter subprocesses cannot reach the host.
		program := fmt.Sprintf(`import json, pathlib, sys
a=json.loads(sys.argv[2]); p=pathlib.Path(a["path"])
if sys.argv[1]=="swarm_read":
 if p.stat().st_size>%d: raise ValueError("file exceeds 1 MiB; use shell to select a range")
 print(p.read_text(),end="")
elif sys.argv[1]=="swarm_write":
 p.parent.mkdir(parents=True,exist_ok=True); p.write_text(a["content"])
else:
 if p.stat().st_size>%d: raise ValueError("file exceeds 1 MiB")
 text=p.read_text(); old=a["old_text"]
 if not old or text.count(old)!=1: raise ValueError("old_text must match exactly once")
 p.write_text(text.replace(old,a["new_text"],1))`, toolReadEditBytes, toolReadEditBytes)
		err = e.Run(ctx, `exec /usr/bin/python3 -c "$1" "$2" "$3"`, &output, &output, program, name, string(raw))
	}
	text := output.String()
	if err != nil {
		text += fmt.Sprintf("\nTool failed: %v", err)
	}
	return result(text, err != nil)
}

// ToolCapabilityReport describes observations, never grants. Scheduling labels
// remain node declarations and are independent of these runtime observations.
type ToolCapabilityReport struct {
	Version       int              `json:"version"`
	PolicyContext string           `json:"policy_context"`
	Harness       string           `json:"harness"`
	Role          string           `json:"role"`
	Entries       []ToolCapability `json:"entries"`
}

type ToolCapability struct {
	ID           string               `json:"id"`
	Tools        []string             `json:"tools"`
	Transport    string               `json:"transport"`
	Boundary     string               `json:"boundary"`
	Operations   []string             `json:"operations"`
	ReadOnly     bool                 `json:"read_only"`
	Restrictions []string             `json:"restrictions"`
	Limits       ToolCapabilityLimits `json:"limits"`
	Artifacts    []string             `json:"artifacts"`
	Declared     bool                 `json:"declared"`
	Authorized   bool                 `json:"authorized"`
	Runtime      string               `json:"runtime"`
	Usable       bool                 `json:"usable"`
	Reason       string               `json:"reason"`
	Action       string               `json:"action"`
}

type ToolCapabilityLimits struct {
	ArgumentBytes  int `json:"argument_bytes,omitempty"`
	ReadEditBytes  int `json:"read_edit_bytes,omitempty"`
	CommandSeconds int `json:"command_seconds,omitempty"`
}

const toolArgumentBytes = 128 << 10
const toolCommandSeconds = 120
const toolReadEditBytes = 1 << 20

// CapabilityReason returns only coordinator-owned prose, never probe output.
func CapabilityReason(reason string) string {
	switch reason {
	case "verified":
		return "Applicable runtime checks passed; launch rechecks the boundary."
	case "policy_denied":
		return "Select an approved isolated execution policy."
	case "role_denied":
		return "Select an agent permitted for this role."
	case "unsupported_boundary":
		return "Use a qualified native Codex or Bubblewrap tool boundary."
	case "unavailable":
		return "Restore the selected runtime prerequisites and rerun inspection."
	case "report_unavailable":
		return "Rerun inspection; optional report assembly did not complete."
	case "not_enabled":
		return "Enable the browser profile in approved policy when browser checks are needed."
	case "not_prepared":
		return "Coordinator dependency preparation runs before execution; it grants no agent network access."
	default:
		return "Rerun inspection to verify this entry."
	}
}

// ToolCapabilities accepts only resolved policy facts and successful existing
// probe results. Unknown strings and failed assembly cannot enter the report.
func ToolCapabilities(driver, role, policy string, delegated, authorized, verified, browser bool) ToolCapabilityReport {
	switch driver {
	case "codex", "claude", "opencode", "omp":
	default:
		driver = "unknown"
	}
	switch role {
	case "planner", "developer", "tester", "reviewer":
	default:
		role = "unknown"
	}
	switch policy {
	case "repository_inspection", "approved_task":
	default:
		policy = "not_probed"
	}
	ro := role == "planner" || role == "reviewer"
	r := ToolCapabilityReport{Version: 1, PolicyContext: policy, Harness: driver, Role: role}
	e := ToolCapability{ID: "workspace", Tools: []string{"native shell", "native file tools"}, Transport: "native", Boundary: "native_codex",
		Operations: []string{"read", "search", "build", "test"}, ReadOnly: ro,
		Restrictions: []string{"no host credentials", "no external network"}, Artifacts: []string{"workspace-relative task outputs", ".swarm/active role artifacts"},
		Declared: true, Authorized: authorized, Runtime: "not_probed", Reason: "not_probed"}
	if !ro {
		e.Operations = append(e.Operations, "write", "edit")
	} else {
		e.Restrictions = append(e.Restrictions, "workspace read-only; role artifacts remain writable")
	}
	if delegated {
		e.Tools = []string{"swarm_read", "swarm_write", "swarm_edit", "swarm_shell"}
		e.Transport = "owned_mcp"
		if driver == "codex" {
			for i := range e.Tools {
				e.Tools[i] = "mcp__swarm." + e.Tools[i]
			}
		}
		if driver == "claude" {
			for i := range e.Tools {
				e.Tools[i] = "mcp__swarm__" + e.Tools[i]
			}
		}
		if driver == "opencode" {
			for i := range e.Tools {
				e.Tools[i] = "swarm_" + e.Tools[i]
			}
		}
		if driver == "omp" {
			e.Transport = "host_rpc"
		}
		e.Boundary = "bubblewrap"
		e.Limits = ToolCapabilityLimits{ArgumentBytes: toolArgumentBytes, ReadEditBytes: toolReadEditBytes, CommandSeconds: toolCommandSeconds}
		e.Restrictions = append(e.Restrictions, "processes end after each command", "file helper paths relative to /workspace")
	}
	if driver == "unknown" {
		e.Authorized = false
		e.Reason = "unsupported_boundary"
	}
	if !authorized {
		e.Reason = "policy_denied"
	}
	if verified && e.Authorized {
		e.Runtime = "available"
		e.Reason = "verified"
		e.Usable = true
	}
	e.Action = CapabilityReason(e.Reason)
	r.Entries = append(r.Entries, e)
	b := ToolCapability{ID: "browser", Tools: []string{"swarm_shell"}, Transport: "owned_mcp", Boundary: "bubblewrap",
		Operations: []string{"private loopback preview", "headless Chromium"}, ReadOnly: ro,
		Restrictions: []string{"server and Chromium in one command", "native shell networking remains disabled"},
		Limits:       ToolCapabilityLimits{ArgumentBytes: toolArgumentBytes, CommandSeconds: toolCommandSeconds}, Artifacts: []string{"workspace-relative DOM, PNG and logs"},
		Declared: browser, Authorized: e.Authorized && browser && !ro, Runtime: "unavailable", Reason: "not_enabled"}
	if delegated {
		b.Transport = e.Transport
	}
	if browser {
		b.Runtime = "not_probed"
		b.Reason = "not_probed"
	}
	if browser && ro {
		b.Authorized = false
		b.Reason = "role_denied"
	}
	if b.Authorized && verified {
		b.Runtime = "available"
		b.Reason = "verified"
		b.Usable = true
	}
	if driver == "codex" {
		b.Tools = []string{"mcp__swarm.swarm_shell"}
	}
	if driver == "claude" {
		b.Tools = []string{"mcp__swarm__swarm_shell"}
	}
	if driver == "opencode" {
		b.Tools = []string{"swarm_swarm_shell"}
	}
	b.Action = CapabilityReason(b.Reason)
	r.Entries = append(r.Entries, b, ToolCapability{ID: "dependencies", Tools: []string{}, Transport: "coordinator_preparation", Boundary: "read_only_mounts",
		Operations: []string{"prepared dependency caches", "resolved toolchain mounts"}, ReadOnly: true, Restrictions: []string{"no agent network grant"},
		Artifacts: []string{"sandbox-relative cache mounts"}, Runtime: "not_probed", Reason: "not_prepared", Action: CapabilityReason("not_prepared")})
	return r
}

// InspectCapabilityEntry contains an optional assembler's failure to one entry.
// Neither its error text nor a panic can become a diagnostic or a blocker.
func InspectCapabilityEntry(e ToolCapability, inspect func() error) ToolCapability {
	if inspect == nil {
		return e
	}
	// Enrichment is optional: contain errors, panics and stalled inspection.
	done := make(chan bool, 1)
	go func() {
		success := false
		defer func() { _ = recover(); done <- success }()
		success = inspect() == nil
	}()
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	select {
	case success := <-done:
		if success {
			return e
		}
	case <-timer.C:
	}
	e.Runtime = "not_probed"
	e.Usable = false
	e.Reason = "report_unavailable"
	e.Action = CapabilityReason(e.Reason)
	return e
}

func FormatToolCapabilities(r ToolCapabilityReport) string {
	var out strings.Builder
	fmt.Fprintf(&out, "Tool capabilities v%d (%s; %s/%s). Node scheduling capabilities are separate declarations.\n", r.Version, r.PolicyContext, r.Harness, r.Role)
	for _, e := range r.Entries {
		fmt.Fprintf(&out, "- %s: tools=%s; transport=%s; boundary=%s; operations=%s; read_only=%t; declared=%t; authorized=%t; runtime=%s; usable=%t; reason=%s. %s Limits: arguments=%d bytes, read/edit=%d bytes, command=%d seconds (zero means unspecified). Artifacts: %s. Restrictions: %s.\n",
			e.ID, strings.Join(e.Tools, ", "), e.Transport, e.Boundary, strings.Join(e.Operations, ", "), e.ReadOnly, e.Declared, e.Authorized, e.Runtime, e.Usable, e.Reason, e.Action, e.Limits.ArgumentBytes, e.Limits.ReadEditBytes, e.Limits.CommandSeconds, strings.Join(e.Artifacts, ", "), strings.Join(e.Restrictions, ", "))
	}
	return out.String()
}

// PreparedCapabilities records only coordinator preparation results. It never
// discovers binaries or treats host lookup as proof of sandbox availability.
func PreparedCapabilities(r ToolCapabilityReport, deps Dependencies) ToolCapabilityReport {
	for i := range r.Entries {
		if r.Entries[i].ID != "dependencies" || r.Entries[i].Reason == "report_unavailable" {
			continue
		}
		e := &r.Entries[i]
		for _, eco := range []string{"go", "rust", "python", "node"} {
			for _, cache := range deps.Caches {
				if cache.Ecosystem == eco {
					e.Operations = append(e.Operations, eco+" dependency cache prepared")
					break
				}
			}
		}
		// This is preparation status, not authorization for an agent tool.
		if len(e.Operations) > 2 {
			e.Runtime = "available"
			e.Reason = "verified"
			e.Action = CapabilityReason("verified")
		}
	}
	return r
}

// LaunchToolCapabilities describes the actual role boundary after its existing
// launch checks, including callers that select a stricter read-only mode.
func LaunchToolCapabilities(driver, role string, delegated, browser, readOnly bool, deps Dependencies) ToolCapabilityReport {
	r := ToolCapabilities(driver, role, "approved_task", delegated, true, true, browser)
	if readOnly {
		for i := range r.Entries {
			e := &r.Entries[i]
			e.ReadOnly = true
			var ops []string
			for _, op := range e.Operations {
				if op != "write" && op != "edit" {
					ops = append(ops, op)
				}
			}
			e.Operations = ops
			if e.ID == "workspace" {
				e.Restrictions = append(e.Restrictions, "workspace read-only; role artifacts remain writable")
			}
			if e.ID == "browser" {
				e.Authorized = false
				e.Usable = false
				e.Runtime = "unavailable"
				e.Reason = "role_denied"
				e.Action = CapabilityReason(e.Reason)
			}
		}
	}
	return PreparedCapabilities(r, deps)
}
