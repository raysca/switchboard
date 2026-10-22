package core

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"strings"
	"testing"
)

// The library's API says handoffs and steps: no exported name carries the old route/node vocabulary (Router, the mechanism, stays).
func TestNoRouteOrNodeVocabularyInTheAPI(t *testing.T) {
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi fs.FileInfo) bool { return !strings.HasSuffix(fi.Name(), "_test.go") }, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, pkg := range pkgs {
		for name, file := range pkg.Files {
			for _, decl := range file.Decls {
				ast.Inspect(decl, func(n ast.Node) bool {
					id, ok := n.(*ast.Ident)
					if ok && id.IsExported() && (strings.Contains(strings.ReplaceAll(id.Name, "Router", ""), "Route") || strings.Contains(id.Name, "NodeStages") || strings.Contains(id.Name, "BuildNodeFunc")) {
						t.Errorf("%s: %s still uses the old vocabulary", name, id.Name)
					}
					return true
				})
			}
		}
	}
}
