package analysis

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"strings"
)

// analyzeGo parses Go source with the standard library's own parser — the
// most reliable of the three language analyzers here, since it's not a
// heuristic at all, and requires no runtime dependency beyond the Go
// toolchain TeaHub is already built with.
//
// A parse error never fails the whole build: it's recorded as
// Partial+Warning so the rest of the repo still indexes.
func analyzeGo(path, content string) (*FileAnalysis, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, content, parser.ParseComments)
	if err != nil {
		return &FileAnalysis{
			Path: path, Language: "go",
			Partial: true, Warning: "parse error: " + err.Error(),
		}, nil
	}

	fa := &FileAnalysis{Path: path, Language: "go", Package: f.Name.Name}

	for _, imp := range f.Imports {
		fa.Imports = append(fa.Imports, strings.Trim(imp.Path.Value, `"`))
	}

	for _, decl := range f.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			fa.Symbols = append(fa.Symbols, goFuncSymbol(fset, path, d))
		case *ast.GenDecl:
			fa.Symbols = append(fa.Symbols, goGenDeclSymbols(fset, path, d)...)
		}
	}

	return fa, nil
}

func goFuncSymbol(fset *token.FileSet, path string, d *ast.FuncDecl) Symbol {
	sym := Symbol{
		Name:       d.Name.Name,
		Path:       path,
		Line:       fset.Position(d.Pos()).Line,
		Exported:   d.Name.IsExported(),
		DocComment: strings.TrimSpace(d.Doc.Text()),
		Signature:  goFuncSignature(fset, d),
	}
	if d.Recv != nil && len(d.Recv.List) > 0 {
		sym.Kind = KindMethod
		sym.Receiver = goReceiverType(d.Recv.List[0].Type)
	} else {
		sym.Kind = KindFunction
	}
	return sym
}

func goGenDeclSymbols(fset *token.FileSet, path string, d *ast.GenDecl) []Symbol {
	var out []Symbol
	for _, spec := range d.Specs {
		switch s := spec.(type) {
		case *ast.TypeSpec:
			kind := KindStruct
			if _, ok := s.Type.(*ast.InterfaceType); ok {
				kind = KindInterface
			}
			doc := d.Doc
			if s.Doc != nil {
				doc = s.Doc // a doc comment directly above the type wins over the GenDecl's
			}
			out = append(out, Symbol{
				Kind: kind, Name: s.Name.Name, Path: path,
				Line:       fset.Position(s.Pos()).Line,
				Exported:   s.Name.IsExported(),
				DocComment: strings.TrimSpace(doc.Text()),
			})
		case *ast.ValueSpec:
			if d.Tok != token.CONST {
				continue
			}
			for _, name := range s.Names {
				if name.Name == "_" {
					continue
				}
				out = append(out, Symbol{
					Kind: KindConstant, Name: name.Name, Path: path,
					Line:     fset.Position(name.Pos()).Line,
					Exported: name.IsExported(),
				})
			}
		}
	}
	return out
}

// goFuncSignature reprints a function's receiver + name + type using the
// standard library's own printer, so the signature text is always exactly
// valid Go syntax rather than a hand-assembled approximation.
func goFuncSignature(fset *token.FileSet, d *ast.FuncDecl) string {
	var buf bytes.Buffer
	buf.WriteString("func ")
	if d.Recv != nil && len(d.Recv.List) > 0 {
		buf.WriteString("(")
		printer.Fprint(&buf, fset, d.Recv.List[0])
		buf.WriteString(") ")
	}
	buf.WriteString(d.Name.Name)
	printer.Fprint(&buf, fset, d.Type)
	return buf.String()
}

func goReceiverType(expr ast.Expr) string {
	switch t := expr.(type) {
	case *ast.StarExpr:
		return "*" + goReceiverType(t.X)
	case *ast.Ident:
		return t.Name
	case *ast.IndexExpr: // generic receiver, e.g. Stack[T]
		return goReceiverType(t.X)
	default:
		return ""
	}
}
