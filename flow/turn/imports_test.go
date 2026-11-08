package turn_test

import (
	"encoding/json"
	"go/parser"
	"go/token"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

const (
	module = "github.com/urmzd/saige/flow"
	saige  = "github.com/urmzd/saige"
)

// typeSeam is the part of saige's agent side that every flow package may
// import: the message and RAG types, the text extractor, and the test
// models. The agent loop and the providers are not in it.
var typeSeam = map[string]bool{
	saige + "/agent/types":            true,
	saige + "/agent/selector/rank":    true, // a dependency of agent/types
	saige + "/agent/agenttest":        true,
	saige + "/rag/types":              true,
	saige + "/rag/extractor":          true,
	saige + "/rag/internal/textclean": true, // a dependency of rag/extractor
}

// mayImportSaige reports whether a package may import saige: the native
// runtime and its alias packages, and pkg/definition,
// whose agents are saige definitions.
func mayImportSaige(pkg string) bool {
	for _, root := range []string{module + "/turn/native", module + "/definition"} {
		if pkg == root || strings.HasPrefix(pkg, root+"/") {
			return true
		}
	}
	return false
}

// testsMayImportSaige lists the test files that still import saige directly.
// These runtime tests build saige scripts and tools directly. A new test file
// in the same package is checked like any other.
var testsMayImportSaige = map[string]map[string]bool{
	module + "/runtime": {
		"actor_test.go":             true,
		"approving_test.go":         true,
		"catalog_test.go":           true,
		"compact_test.go":           true,
		"delegator_test.go":         true,
		"files_test.go":             true,
		"hostcall_approval_test.go": true,
		"park_test.go":              true,
		"stop_durable_test.go":      true,
		"stop_test.go":              true,
		"thread_test.go":            true,
		"toolsfor_test.go":          true,
		"unavailable_test.go":       true,
		"usage_test.go":             true,
	},
}

// isSaige reports whether an import path is saige or one of its packages.
// isSaige reports whether imp is saige's agent side: anything in the module
// outside flow and the engine and sandbox flow runs on.
func isSaige(imp string) bool {
	if imp != saige && !strings.HasPrefix(imp, saige+"/") {
		return false
	}
	for _, own := range []string{"/flow", "/durable", "/sandbox"} {
		if p := saige + own; imp == p || strings.HasPrefix(imp, p+"/") {
			return false
		}
	}
	return true
}

// isAgentLoop reports whether imp is saige's agent side beyond the type seam.
func isAgentLoop(imp string) bool { return isSaige(imp) && !typeSeam[imp] }

// testFileImports returns the saige imports of each test file of a package
// that is not on its allowlist.
func testFileImports(t *testing.T, dir string, files []string, allowed map[string]bool) []string {
	t.Helper()
	var bad []string
	fset := token.NewFileSet()
	for _, name := range files {
		f, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, spec := range f.Imports {
			imp, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				t.Fatalf("parse %s: %v", name, err)
			}
			if isAgentLoop(imp) && !allowed[name] {
				bad = append(bad, name+": "+imp)
			}
		}
	}
	return bad
}

// Every flow package and its tests reach saige's agent loop and providers
// only under flow/turn/native and flow/definition; the rest import saige's
// types (typeSeam) and nothing else of its agent side.
func TestOnlyNativeImportsSaige(t *testing.T) {
	t.Parallel()
	out, err := exec.Command("go", "list", "-json=ImportPath,Dir,Imports,TestImports,XTestImports,TestGoFiles,XTestGoFiles", module+"/...").Output()
	if err != nil {
		t.Fatalf("go list: %v", err)
	}
	dec := json.NewDecoder(strings.NewReader(string(out)))
	seen := 0
	for dec.More() {
		var p struct {
			ImportPath, Dir                    string
			Imports, TestImports, XTestImports []string
			TestGoFiles, XTestGoFiles          []string
		}
		if err := dec.Decode(&p); err != nil {
			t.Fatalf("decode go list: %v", err)
		}
		seen++
		if mayImportSaige(p.ImportPath) {
			continue
		}
		imports := p.Imports
		allowed, ok := testsMayImportSaige[p.ImportPath]
		if ok {
			files := append(append([]string{}, p.TestGoFiles...), p.XTestGoFiles...)
			for _, bad := range testFileImports(t, p.Dir, files, allowed) {
				t.Errorf("%s: %s", p.ImportPath, bad)
			}
		} else {
			imports = append(append(imports, p.TestImports...), p.XTestImports...)
		}
		for _, imp := range imports {
			if isAgentLoop(imp) {
				t.Errorf("%s imports %s", p.ImportPath, imp)
			}
		}
	}
	if seen == 0 {
		t.Fatal("go list returned no packages")
	}
}

// The turn API, its conformance suite, the sandbox decorator and the event
// envelope compile without saige's agent side or the native runtime,
// directly or through a dependency.
func TestTurnDoesNotDependOnNative(t *testing.T) {
	t.Parallel()
	for _, pkg := range []string{
		module + "/turn",
		module + "/turn/runtimetest",
		module + "/turn/sandbox",
		module + "/event",
	} {
		out, err := exec.Command("go", "list", "-deps", pkg).CombinedOutput()
		if err != nil {
			t.Fatalf("go list %s: %v\n%s", pkg, err, out)
		}
		for _, dep := range strings.Fields(string(out)) {
			if isSaige(dep) {
				t.Errorf("%s depends on %s", pkg, dep)
			}
			if mayImportSaige(dep) {
				t.Errorf("%s depends on %s", pkg, dep)
			}
		}
	}
}
