package engine_test

// namespace_sync_test.go — CI guard that sdk.Namespaces and pkg/uri.Namespaces
// never drift from each other (issue #173).
//
// The sdk module cannot import pkg/uri directly (separate Go modules; importing
// would create an import cycle via the main module). Both namespace maps are
// therefore maintained as manual copies with a comment documenting the
// single-source-of-truth relationship:
//
//	pkg/uri/namespace.go   — canonical definition
//	sdk/uri.go             — copy (sdk.Namespaces, lines tagged "SINGLE SOURCE")
//
// This test parses both source files with go/ast and extracts the map literal
// keys, then asserts they are identical sets. It runs as part of `go test
// ./internal/engine/` and will fail CI if a key is added to one map but not
// the other.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// extractNamespaceKeys parses a Go source file and returns the set of string
// keys declared in the first map[string]bool composite literal whose
// enclosing var is named "Namespaces".
func extractNamespaceKeys(t *testing.T, srcPath string) map[string]struct{} {
	t.Helper()

	src, err := os.ReadFile(srcPath)
	if err != nil {
		t.Fatalf("read %s: %v", srcPath, err)
	}

	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, srcPath, src, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", srcPath, err)
	}

	keys := make(map[string]struct{})

	ast.Inspect(f, func(n ast.Node) bool {
		vs, ok := n.(*ast.ValueSpec)
		if !ok {
			return true
		}
		// Look for: var Namespaces = map[string]bool{ ... }
		for i, name := range vs.Names {
			if name.Name != "Namespaces" {
				continue
			}
			if i >= len(vs.Values) {
				continue
			}
			compLit, ok := vs.Values[i].(*ast.CompositeLit)
			if !ok {
				continue
			}
			for _, elt := range compLit.Elts {
				kv, ok := elt.(*ast.KeyValueExpr)
				if !ok {
					continue
				}
				lit, ok := kv.Key.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					continue
				}
				// Strip surrounding quotes.
				key := lit.Value
				if len(key) >= 2 && key[0] == '"' && key[len(key)-1] == '"' {
					key = key[1 : len(key)-1]
				}
				keys[key] = struct{}{}
			}
		}
		return true
	})

	if len(keys) == 0 {
		t.Fatalf("no Namespaces keys found in %s — check that the var is still named 'Namespaces'", srcPath)
	}
	return keys
}

// repoRoot returns the absolute path to the repository root by walking up from
// this test file's location.
func repoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	// file is .../internal/engine/namespace_sync_test.go
	// walk up two directories
	root := filepath.Dir(filepath.Dir(filepath.Dir(file)))
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("cannot locate repo root from %s: go.mod not found at %s", file, root)
	}
	return root
}

func TestNamespaces_SDKAndPkgURI_NeverDrift(t *testing.T) {
	root := repoRoot(t)

	pkgURIFile := filepath.Join(root, "pkg", "uri", "namespace.go")
	sdkFile := filepath.Join(root, "sdk", "uri.go")

	pkgKeys := extractNamespaceKeys(t, pkgURIFile)
	sdkKeys := extractNamespaceKeys(t, sdkFile)

	// Check: every pkg/uri key must be in sdk.
	for k := range pkgKeys {
		if _, ok := sdkKeys[k]; !ok {
			t.Errorf("namespace %q is in pkg/uri.Namespaces but missing from sdk.Namespaces\n"+
				"  add it to sdk/uri.go Namespaces map", k)
		}
	}

	// Check: every sdk key must be in pkg/uri.
	for k := range sdkKeys {
		if _, ok := pkgKeys[k]; !ok {
			t.Errorf("namespace %q is in sdk.Namespaces but missing from pkg/uri.Namespaces\n"+
				"  add it to pkg/uri/namespace.go Namespaces map", k)
		}
	}

	if t.Failed() {
		t.Logf("pkg/uri keys (%d): %v", len(pkgKeys), sortedKeys(pkgKeys))
		t.Logf("sdk keys    (%d): %v", len(sdkKeys), sortedKeys(sdkKeys))
	}
}

func sortedKeys(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	// simple insertion sort (small map, test helper)
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}
