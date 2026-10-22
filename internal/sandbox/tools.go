package sandbox

import (
	"context"
	"encoding/json"
	"fmt"

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
	if len(raw) > 128<<10 {
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
		const program = `import json, pathlib, sys
a=json.loads(sys.argv[2]); p=pathlib.Path(a["path"])
if sys.argv[1]=="swarm_read":
 if p.stat().st_size>1048576: raise ValueError("file exceeds 1 MiB; use shell to select a range")
 print(p.read_text(),end="")
elif sys.argv[1]=="swarm_write":
 p.parent.mkdir(parents=True,exist_ok=True); p.write_text(a["content"])
else:
 if p.stat().st_size>1048576: raise ValueError("file exceeds 1 MiB")
 text=p.read_text(); old=a["old_text"]
 if not old or text.count(old)!=1: raise ValueError("old_text must match exactly once")
 p.write_text(text.replace(old,a["new_text"],1))`
		err = e.Run(ctx, `exec /usr/bin/python3 -c "$1" "$2" "$3"`, &output, &output, program, name, string(raw))
	}
	text := output.String()
	if err != nil {
		text += fmt.Sprintf("\nTool failed: %v", err)
	}
	return result(text, err != nil)
}
