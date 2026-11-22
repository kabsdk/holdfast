package server

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// UI = API = CLI (decisions 014 and 028): every /api/v1 route has a CLI command and a UI call, except the
// ones spec §3.3 lists. The CLI's calls are read from its Go code, the UI's from api.js.

// uiExceptions are routes the UI does not call, by spec §3.3.
var uiExceptions = map[string]string{
	"POST /api/v1/escrow/export": "escrow export exists only on the server's root socket",
}

// norm makes a path comparable: every path parameter becomes {}, and the query string goes.
func norm(p string) string {
	if i := strings.IndexByte(p, '?'); i >= 0 {
		p = p[:i]
	}
	return regexp.MustCompile(`\{[^}]*\}`).ReplaceAllString(p, "{}")
}

// cliCalls finds every call("METHOD", path, ...) in the CLI, with the path's literals joined by {}.
func cliCalls(t *testing.T) map[string]bool {
	files, _ := filepath.Glob("../../cmd/holdfast/*.go")
	out := map[string]bool{}
	fset := token.NewFileSet()
	var flat func(e ast.Expr) string
	flat = func(e ast.Expr) string {
		switch x := e.(type) {
		case *ast.BasicLit:
			s, _ := strconv.Unquote(x.Value)
			return s
		case *ast.BinaryExpr:
			return flat(x.X) + flat(x.Y)
		}
		return "{}"
	}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		af, err := parser.ParseFile(fset, f, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		// Each function on its own, so a path built in a variable is looked up where it was built: every
		// string assigned to that variable in the same function is a candidate.
		var walk func(fn ast.Node)
		walk = func(fn ast.Node) {
			assigned := map[string][]string{}
			var calls []*ast.CallExpr
			ast.Inspect(fn, func(n ast.Node) bool {
				if lit, ok := n.(*ast.FuncLit); ok && n != fn {
					walk(lit)
					return false
				}
				switch x := n.(type) {
				case *ast.AssignStmt:
					if len(x.Lhs) == len(x.Rhs) {
						for i, l := range x.Lhs {
							if id, ok := l.(*ast.Ident); ok {
								if p := flat(x.Rhs[i]); strings.HasPrefix(p, "/api/") {
									assigned[id.Name] = append(assigned[id.Name], p)
								}
							}
						}
					}
				case *ast.CallExpr:
					if id, ok := x.Fun.(*ast.Ident); ok && id.Name == "call" && len(x.Args) >= 2 {
						calls = append(calls, x)
					}
				}
				return true
			})
			for _, c := range calls {
				m, ok := c.Args[0].(*ast.BasicLit)
				if !ok {
					continue
				}
				method, _ := strconv.Unquote(m.Value)
				if id, ok := c.Args[1].(*ast.Ident); ok {
					for _, p := range assigned[id.Name] {
						out[method+" "+norm(p)] = true
					}
					continue
				}
				out[method+" "+norm(flat(c.Args[1]))] = true
			}
		}
		for _, d := range af.Decls {
			if fd, ok := d.(*ast.FuncDecl); ok {
				walk(fd)
			}
		}
	}
	return out
}

// uiCalls reads api.js: each call is req('METHOD', path).
func uiCalls(t *testing.T) map[string]bool {
	b, err := os.ReadFile("../webui/static/api.js")
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]bool{}
	re := regexp.MustCompile("req\\('(GET|POST|PUT|DELETE)', [`']([^`']+)[`']")
	for _, m := range re.FindAllStringSubmatch(string(b), -1) {
		p := regexp.MustCompile(`\$\{q\([^}]*\}\)\}`).ReplaceAllString(m[2], "") // the query string
		p = regexp.MustCompile(`\$\{[^}]*\}`).ReplaceAllString(p, "{}")
		out[m[1]+" "+p] = true
	}
	return out
}

func TestUIAPICLIParity(t *testing.T) {
	cli, ui := cliCalls(t), uiCalls(t)
	if len(cli) < 20 || len(ui) < 20 {
		t.Fatalf("the parity test found too few calls (CLI %d, UI %d): has the code changed shape?", len(cli), len(ui))
	}
	routes := map[string]bool{}
	var missing []string
	for _, rt := range (&Server{}).Routes() {
		key := rt.Method + " " + norm(rt.Path)
		routes[key] = true
		if !cli[key] {
			missing = append(missing, key+": no CLI command")
		}
		if _, ok := uiExceptions[rt.Method+" "+rt.Path]; !ok && !ui[key] {
			missing = append(missing, key+": no UI call")
		}
	}
	for k := range cli {
		if !routes[k] {
			missing = append(missing, k+": the CLI calls a route the server does not have")
		}
	}
	for k := range ui {
		if !routes[k] && !strings.HasPrefix(k, "POST /auth/") {
			missing = append(missing, k+": the UI calls a route the server does not have")
		}
	}
	sort.Strings(missing)
	if len(slices.Compact(missing)) > 0 {
		t.Fatalf("UI = API = CLI is broken (spec §3.3 lists the only exceptions):\n%s", strings.Join(missing, "\n"))
	}
}
