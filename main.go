package main

import (
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"log"
	"path/filepath"
	"strings"
)

func main() {
	log.SetFlags(0)

	var ctxt context

	flag.BoolVar(&ctxt.Pedantic, "pedantic", false,
		`makes several diagnostics more pedantic and comprehensive`)
	flag.Parse()

	filenames := targetsToFilenames(flag.Args())

	ctxt.SetupOpsTable()
	if err := visitFiles(&ctxt, filenames, ctxt.InferConventions); err != nil {
		log.Fatalf("infer conventions: %v", err)
	}
	ctxt.SetupSuggestions()
	if err := visitFiles(&ctxt, filenames, ctxt.CaptureInconsistencies); err != nil {
		log.Fatalf("report inconsistent: %v", err)
	}

	for _, warn := range ctxt.Warnings {
		log.Printf("%s: %s", warn.pos, warn.text)
	}
}

type context struct {
	ops  []*operation
	fset *token.FileSet

	Pedantic bool

	Warnings []warning
}

type warning struct {
	pos  token.Position
	text string
}

type operation struct {
	scope    opScope
	name     string
	suggest  *opVariant
	variants []*opVariant
}

type opScope int

const (
	scopeAny opScope = iota
	scopeLocal
	scopeGlobal
)

type opVariant struct {
	name    string
	count   int
	matcher opMatcher
}

type opMatcher interface {
	Skip(ast.Node) bool
	Match(ast.Node) bool
}

func (ctxt *context) SetupOpsTable() {
	ctxt.ops = []*operation{
		{
			scope: scopeAny,
			name:  "zero value pointer allocation",
			variants: []*opVariant{
				{name: "new", matcher: newMatcher{}},
				{name: "address-of-lit", matcher: addressOfLitMatcher{}},
			},
		},

		{
			scope: scopeAny,
			name:  "empty slice",
			variants: []*opVariant{
				{name: "empty-slice-make", matcher: emptySliceMakeMatcher{}},
				{name: "empty-slice-lit", matcher: emptySliceLitMatcher{}},
			},
		},

		{
			scope: scopeLocal,
			// TODO(quasilyte): rename to "nil slice decl"?
			name: "nil slice",
			variants: []*opVariant{
				{name: "nil-slice-var", matcher: nilSliceVarMatcher{}},
				{name: "nil-slice-lit", matcher: nilSliceLitMatcher{}},
			},
		},

		{
			scope: scopeAny,
			name:  "empty map",
			variants: []*opVariant{
				{name: "empty-map-make", matcher: emptyMapMakeMatcher{}},
				{name: "empty-map-lit", matcher: emptyMapLitMatcher{}},
			},
		},

		// TODO(quasilyte): nil map
	}
}

func (ctxt *context) SetupSuggestions() {
	for _, op := range ctxt.ops {
		op.suggest = op.variants[0]
		// Find the most frequently used variant.
		for _, v := range op.variants[1:] {
			if v.count > op.suggest.count {
				op.suggest = v
			}
		}
		// Diagnostic: check if there were multiple candidates.
		if op.suggest.count == 0 {
			continue
		}
		for _, v := range op.variants {
			if v != op.suggest && v.count == op.suggest.count {
				log.Printf("warning: %s: can't decide between %s and %s",
					op.name, v.name, op.suggest.name)
			}
		}
	}
}

type opVisitFunc func(*operation, *opVariant, ast.Node) bool

func (ctxt *context) visitOps(f *ast.File, visit opVisitFunc) {
	for _, op := range ctxt.ops {
		switch op.scope {
		case scopeAny:
			for _, v := range op.variants {
				ast.Inspect(f, func(n ast.Node) bool {
					return visit(op, v, n)
				})
			}

		case scopeLocal:
			for _, v := range op.variants {
				for _, decl := range f.Decls {
					decl, ok := decl.(*ast.FuncDecl)
					if !ok {
						continue
					}
					ast.Inspect(decl.Body, func(n ast.Node) bool {
						return visit(op, v, n)
					})
				}
			}

		case scopeGlobal:
			// TODO(quasilyte): remove later if never used.
			panic("unimplemented and unused")

		default:
			panic(fmt.Sprintf("unexpected scope: %d", op.scope))
		}
	}
}

func (ctxt *context) InferConventions(f *ast.File) {
	ctxt.visitOps(f, func(op *operation, v *opVariant, n ast.Node) bool {
		if n == nil {
			return false
		}
		if v.matcher.Skip(n) {
			return false
		}
		if v.matcher.Match(n) {
			v.count++
		}
		return true
	})
}

func (ctxt *context) CaptureInconsistencies(f *ast.File) {
	ctxt.visitOps(f, func(op *operation, v *opVariant, n ast.Node) bool {
		if n == nil {
			return false
		}
		if v.matcher.Skip(n) {
			return false
		}
		if v.matcher.Match(n) && v != op.suggest {
			ctxt.pushWarning(n, op, v)
		}
		return true
	})
}

func (ctxt *context) pushWarning(cause ast.Node, op *operation, bad *opVariant) {
	pos := ctxt.fset.Position(cause.Pos())
	text := fmt.Sprintf("%s: use %s instead of %s", op.name, op.suggest.name, bad.name)
	ctxt.Warnings = append(ctxt.Warnings, warning{pos: pos, text: text})
}

func visitFiles(ctxt *context, filenames []string, visit func(*ast.File)) error {
	fset := token.NewFileSet()
	ctxt.fset = fset
	for _, filename := range filenames {
		f, err := parser.ParseFile(fset, filename, nil, 0)
		if err != nil {
			return err
		}
		visit(f)
	}
	return nil
}

func targetsToFilenames(targets []string) []string {
	var filenames []string

	for _, target := range targets {
		if !strings.HasSuffix(target, ".go") {
			// TODO(quasilyte): add package targets support.
			log.Printf("skip target %q: not a Go file", target)
			continue
		}
		abs, err := filepath.Abs(target)
		if err != nil {
			log.Printf("skip target %q: %v", target, err)
			continue
		}
		filenames = append(filenames, abs)
	}

	return filenames
}

func valueOf(x ast.Expr) string {
	switch x := x.(type) {
	case *ast.BasicLit:
		return x.Value
	case *ast.Ident:
		return x.Name
	default:
		return ""
	}
}
