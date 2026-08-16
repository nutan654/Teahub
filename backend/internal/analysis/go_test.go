package analysis

import (
	"context"
	"testing"
)

const goFixture = `// Package widgets does widget things.
package widgets

import (
	"fmt"
	"strings"
)

// MaxWidgets is the maximum number of widgets allowed.
const MaxWidgets = 100

const internalLimit = 10

// Widget represents a single widget.
type Widget struct {
	Name string
}

// Factory builds widgets.
type Factory interface {
	Build() Widget
}

// NewWidget creates a widget with the given name.
func NewWidget(name string) Widget {
	return Widget{Name: name}
}

// Describe returns a human-readable description of the widget.
func (w Widget) Describe() string {
	return fmt.Sprintf("widget: %s", w.Name)
}

func (w *Widget) rename(name string) {
	w.Name = name
}

func unexportedHelper() string {
	return strings.ToUpper("help")
}

// TODO: add validation for widget names
func placeholder() {}
`

func TestAnalyzeGo_Package(t *testing.T) {
	fa, err := analyzeGo("widgets.go", goFixture)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if fa.Package != "widgets" {
		t.Errorf("expected package 'widgets', got %q", fa.Package)
	}
	if fa.Partial {
		t.Errorf("expected non-partial result for valid Go, got warning: %s", fa.Warning)
	}
}

func TestAnalyzeGo_Imports(t *testing.T) {
	fa, _ := analyzeGo("widgets.go", goFixture)
	want := map[string]bool{"fmt": true, "strings": true}
	if len(fa.Imports) != 2 {
		t.Fatalf("expected 2 imports, got %v", fa.Imports)
	}
	for _, imp := range fa.Imports {
		if !want[imp] {
			t.Errorf("unexpected import: %s", imp)
		}
	}
}

func symbolByName(symbols []Symbol, name string) *Symbol {
	for i := range symbols {
		if symbols[i].Name == name {
			return &symbols[i]
		}
	}
	return nil
}

func TestAnalyzeGo_FunctionsAndMethods(t *testing.T) {
	fa, _ := analyzeGo("widgets.go", goFixture)

	newWidget := symbolByName(fa.Symbols, "NewWidget")
	if newWidget == nil || newWidget.Kind != KindFunction || !newWidget.Exported {
		t.Fatalf("expected exported function NewWidget, got %+v", newWidget)
	}
	if newWidget.DocComment != "NewWidget creates a widget with the given name." {
		t.Errorf("unexpected doc comment: %q", newWidget.DocComment)
	}

	describe := symbolByName(fa.Symbols, "Describe")
	if describe == nil || describe.Kind != KindMethod || describe.Receiver != "Widget" {
		t.Fatalf("expected method Describe with receiver Widget, got %+v", describe)
	}

	rename := symbolByName(fa.Symbols, "rename")
	if rename == nil || rename.Kind != KindMethod || rename.Receiver != "*Widget" || rename.Exported {
		t.Fatalf("expected unexported method rename with pointer receiver *Widget, got %+v", rename)
	}

	helper := symbolByName(fa.Symbols, "unexportedHelper")
	if helper == nil || helper.Exported {
		t.Fatalf("expected unexported function unexportedHelper, got %+v", helper)
	}
}

func TestAnalyzeGo_StructsAndInterfaces(t *testing.T) {
	fa, _ := analyzeGo("widgets.go", goFixture)

	widget := symbolByName(fa.Symbols, "Widget")
	if widget == nil || widget.Kind != KindStruct {
		t.Fatalf("expected struct Widget, got %+v", widget)
	}
	if widget.DocComment != "Widget represents a single widget." {
		t.Errorf("unexpected doc comment: %q", widget.DocComment)
	}

	factory := symbolByName(fa.Symbols, "Factory")
	if factory == nil || factory.Kind != KindInterface {
		t.Fatalf("expected interface Factory, got %+v", factory)
	}
}

func TestAnalyzeGo_Constants(t *testing.T) {
	fa, _ := analyzeGo("widgets.go", goFixture)

	maxWidgets := symbolByName(fa.Symbols, "MaxWidgets")
	if maxWidgets == nil || maxWidgets.Kind != KindConstant || !maxWidgets.Exported {
		t.Fatalf("expected exported constant MaxWidgets, got %+v", maxWidgets)
	}

	internal := symbolByName(fa.Symbols, "internalLimit")
	if internal == nil || internal.Exported {
		t.Fatalf("expected unexported constant internalLimit, got %+v", internal)
	}
}

func TestAnalyzeGo_SyntaxErrorIsPartialNotFatal(t *testing.T) {
	fa, err := analyzeGo("broken.go", "package widgets\nfunc broken( {{{")
	if err != nil {
		t.Fatalf("expected no error (partial result instead), got %v", err)
	}
	if !fa.Partial {
		t.Error("expected Partial=true for a syntax error")
	}
	if fa.Warning == "" {
		t.Error("expected a warning message explaining the parse failure")
	}
}

func TestAnalyzeFile_Go_IncludesTODOs(t *testing.T) {
	fa, err := AnalyzeFile(context.Background(), "widgets.go", goFixture)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	todo := symbolByName(fa.Symbols, "TODO")
	if todo == nil {
		t.Fatal("expected a TODO symbol to be extracted alongside Go symbols")
	}
	if todo.Signature != "add validation for widget names" {
		t.Errorf("unexpected TODO text: %q", todo.Signature)
	}
}
