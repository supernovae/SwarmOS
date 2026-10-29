package agent

import (
	"context"
	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/progress"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestCLIAdapterActivityOutputHelper(t *testing.T) {
	if !strings.Contains(strings.Join(os.Args, " "), "--swarm-activity-helper") {
		return
	}
	os.Stdout.WriteString("private prompt ghp_secret\n")
	os.Exit(0)
}
func TestCLIAdapterOutputMarksActivity(t *testing.T) {
	a := progress.NewActivity()
	ctx := progress.WithActivity(t.Context(), a)
	runner := &CLIAdapter{agentName: "custom", driver: "mock", binary: os.Args[0], prefixArgs: []string{"-test.run=^TestCLIAdapterActivityOutputHelper$", "--", "--swarm-activity-helper"}}
	result, err := runner.Run(ctx, RunRequest{Role: "reviewer", WorktreePath: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Output, "private prompt ghp_secret") {
		t.Fatal("capture changed")
	}
	// The scope has closed, but its last timestamp proves that adapter output reached the writer.
	if reflect.ValueOf(a).Elem().FieldByName("last").IsZero() {
		t.Fatal("adapter output was not observed")
	}
	typ := reflect.TypeOf(a).Elem()
	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)
		if field.Type.Kind() == reflect.Slice || field.Type.Kind() == reflect.Map {
			t.Fatalf("unbounded byte-holding field: %s", field.Name)
		}
	}
	e := progress.Event{Status: "running"}
	a.Enrich(&e, time.Now())
	if e.Role != "" {
		t.Fatal(e)
	}

}
func TestCLIAdapterSingleRoleScope(t *testing.T) {
	for _, file := range []string{"adapters.go", "isolated.go"} {
		f, err := parser.ParseFile(token.NewFileSet(), file, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range f.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Recv == nil {
				continue
			}
			if fn.Name.Name != "Run" && fn.Name.Name != "runIsolated" {
				continue
			}
			n := 0
			ast.Inspect(fn.Body, func(nod ast.Node) bool {
				if call, ok := nod.(*ast.CallExpr); ok {
					if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Role" {
						n++
					}
				}
				return true
			})
			want := 1
			if fn.Name.Name == "runIsolated" {
				want = 0
			}
			if n != want {
				t.Fatalf("%s %s scopes=%d", file, fn.Name.Name, n)
			}
		}
	}
	a := progress.NewActivity()
	runner := &CLIAdapter{driver: "claude", executionMode: contract.ExecutionIsolated, authentication: "forbidden"}
	_, err := runner.Run(progress.WithActivity(context.Background(), a), RunRequest{Role: "reviewer"})
	if err == nil {
		t.Fatal("expected isolated precondition failure")
	}
	e := progress.Event{Status: "running"}
	a.Enrich(&e, time.Now())
	if e.Role != "" {
		t.Fatal(e)
	}
}

func TestMockRunnerReportsRoleWithoutActivity(t *testing.T) {
	a := progress.NewActivity()
	_, err := (&MockRunner{}).Run(progress.WithActivity(t.Context(), a), RunRequest{Role: "reviewer", WorktreePath: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.ValueOf(a).Elem().FieldByName("last").IsZero() {
		t.Fatal("mock fabricated observed activity")
	}
	e := progress.Event{Status: "running"}
	a.Enrich(&e, time.Now())
	if e.Role != "" {
		t.Fatal("mock scope leaked", e)
	}
	end := a.Role("reviewer", "mock", time.Now())
	defer end()
	a.Enrich(&e, time.Now())
	if e.Role != "reviewer" || e.Tool != "mock" || e.ActivityKnown {
		t.Fatal(e)
	}
}
