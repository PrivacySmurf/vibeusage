package testenv_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"testing"
)

func TestProviderCredentialTests_IsolateVibeusageEnv(t *testing.T) {
	cases := []struct {
		relPath string
		funcs   []string
	}{
		{
			relPath: filepath.Join("..", "provider", "kimicode", "kimicode_test.go"),
			funcs: []string{
				"TestAPIKeyStrategy_IsAvailable_EnvVar",
				"TestAPIKeyStrategy_IsAvailable_NoEnvVar",
				"TestAPIKeyStrategy_Fetch_NoKey",
				"TestDeviceFlowStrategy_Fetch_NoCredentials",
			},
		},
		{
			relPath: filepath.Join("..", "provider", "minimax", "minimax_test.go"),
			funcs: []string{
				"TestAPIKeyStrategy_IsAvailable_EnvVar",
				"TestAPIKeyStrategy_IsAvailable_NoEnvVar",
				"TestAPIKeyStrategy_Fetch_NoToken",
			},
		},
		{
			relPath: filepath.Join("..", "provider", "zai", "zai_test.go"),
			funcs: []string{
				"TestAPIKeyStrategy_IsAvailable_EnvVar",
				"TestAPIKeyStrategy_IsAvailable_NoEnvVar",
				"TestAPIKeyStrategy_Fetch_NoToken",
			},
		},
		{
			relPath: filepath.Join("..", "provider", "gemini", "apikey_test.go"),
			funcs: []string{
				"TestAPIKeyStrategyFetch_SendsAPIKeyInHeader",
			},
		},
	}

	for _, tc := range cases {
		fset := token.NewFileSet()
		fileAst, err := parser.ParseFile(fset, tc.relPath, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", tc.relPath, err)
		}

		funcsByName := make(map[string]*ast.FuncDecl)
		for _, decl := range fileAst.Decls {
			if fn, ok := decl.(*ast.FuncDecl); ok && fn.Recv == nil && fn.Name != nil {
				funcsByName[fn.Name.Name] = fn
			}
		}

		for _, fnName := range tc.funcs {
			fn, ok := funcsByName[fnName]
			if !ok || fn.Body == nil {
				t.Fatalf("%s: missing test function %s", tc.relPath, fnName)
			}

			var hasApplyVibeusage bool
			var hasSkip bool
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || sel.Sel == nil {
					return true
				}
				ident, ok := sel.X.(*ast.Ident)
				if !ok {
					return true
				}
				if ident.Name == "testenv" && (sel.Sel.Name == "ApplyVibeusage" || sel.Sel.Name == "ApplySameDir") {
					hasApplyVibeusage = true
				}
				if ident.Name == "t" && (sel.Sel.Name == "Skip" || sel.Sel.Name == "Skipf" || sel.Sel.Name == "SkipNow") {
					hasSkip = true
				}
				return true
			})

			if !hasApplyVibeusage {
				t.Errorf("%s:%s must call testenv.ApplyVibeusage before accessing credential sources", tc.relPath, fnName)
			}
			if hasSkip {
				t.Errorf("%s:%s must not call t.Skip when isolated with testenv.ApplyVibeusage", tc.relPath, fnName)
			}
		}
	}
}
