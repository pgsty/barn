package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestCommandRunnersReturnOutcomesWithoutRenderingFailures(t *testing.T) {
	runners := map[string]bool{
		"runVersionCommand": true, "runNetwork": true, "runPrivateSSH": true, "runSSH": true,
		"runProvision": true, "runPrivateCommand": true, "runLifecycleCommand": true, "runPurgeCommand": true,
		"runSSHConfig": true, "runHosts": true, "runLogs": true, "runValidate": true,
		"runInit": true, "runImage": true, "runDoctor": true, "runSetupCommand": true,
	}
	streaming := map[string]bool{"runPrivateSSH": true, "runSSH": true, "runLogs": true}
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	seen := make(map[string]bool, len(runners))
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		parsed, parseErr := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if parseErr != nil {
			t.Fatal(parseErr)
		}
		for _, declaration := range parsed.Decls {
			function, ok := declaration.(*ast.FuncDecl)
			if !ok || !runners[function.Name.Name] {
				continue
			}
			seen[function.Name.Name] = true
			hasOutcome := false
			if function.Type.Results != nil {
				for _, field := range function.Type.Results.List {
					if name, ok := field.Type.(*ast.Ident); ok && name.Name == "commandOutcome" {
						hasOutcome = true
					}
				}
			}
			if !hasOutcome {
				t.Errorf("%s does not return commandOutcome", function.Name.Name)
			}
			for _, field := range function.Type.Params.List {
				for _, name := range field.Names {
					if name.Name == "stdout" && !streaming[function.Name.Name] {
						t.Errorf("%s receives stdout but is not a stream/interactive runner", function.Name.Name)
					}
				}
			}
			ast.Inspect(function.Body, func(node ast.Node) bool {
				call, ok := node.(*ast.CallExpr)
				if !ok {
					return true
				}
				if name, ok := call.Fun.(*ast.Ident); ok && (name.Name == "errorf" || name.Name == "encodeJSON") {
					t.Errorf("%s directly calls root failure renderer %s", function.Name.Name, name.Name)
				}
				return true
			})
		}
	}
	for name := range runners {
		if !seen[name] {
			t.Errorf("runner %s was not found", name)
		}
	}
}

func TestCommandPackageHasNoSecondaryFlagParser(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		parsed, parseErr := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if parseErr != nil {
			t.Fatalf("parse %s: %v", path, parseErr)
		}
		for _, imported := range parsed.Imports {
			name, unquoteErr := strconv.Unquote(imported.Path.Value)
			if unquoteErr != nil {
				t.Fatalf("decode import in %s: %v", path, unquoteErr)
			}
			if name == "flag" || name == "github.com/spf13/pflag" {
				t.Errorf("%s imports %s; Cobra must remain the only command parser", path, name)
			}
		}
	}
}
