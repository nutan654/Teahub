package analysis

import (
	"context"
	"sync"
	"testing"
	"time"
)

const pyFixture = `"""Module docstring for widgets."""
import os
from collections import OrderedDict

MAX_WIDGETS = 100
_internal_limit = 10


def make_widget(name, size=1):
    """Create a widget with the given name."""
    return {"name": name, "size": size}


def _private_helper():
    return True


class Widget:
    """A single widget."""

    def __init__(self, name):
        self.name = name

    def describe(self):
        return f"widget: {self.name}"

    def _internal(self):
        pass


# TODO: add validation for widget names
def placeholder():
    pass
`

func skipIfNoPython3(t *testing.T) {
	t.Helper()
	if !hasPython3() {
		t.Skip("python3 not found on PATH; skipping Python analyzer test")
	}
}

func TestAnalyzePython_ImportsAndDocstring(t *testing.T) {
	skipIfNoPython3(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	fa, err := analyzePython(ctx, "widgets.py", pyFixture)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if fa.Partial {
		t.Fatalf("expected non-partial result for valid Python, got warning: %s", fa.Warning)
	}
	found := map[string]bool{}
	for _, imp := range fa.Imports {
		found[imp] = true
	}
	if !found["os"] {
		t.Errorf("expected 'os' import, got %v", fa.Imports)
	}
	hasCollections := false
	for _, imp := range fa.Imports {
		if imp == "collections.OrderedDict" {
			hasCollections = true
		}
	}
	if !hasCollections {
		t.Errorf("expected 'collections.OrderedDict' from the from-import, got %v", fa.Imports)
	}
}

func TestAnalyzePython_FunctionsAndClasses(t *testing.T) {
	skipIfNoPython3(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	fa, err := analyzePython(ctx, "widgets.py", pyFixture)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	makeWidget := symbolByName(fa.Symbols, "make_widget")
	if makeWidget == nil || makeWidget.Kind != KindFunction || !makeWidget.Exported {
		t.Fatalf("expected exported function make_widget, got %+v", makeWidget)
	}
	if makeWidget.DocComment != "Create a widget with the given name." {
		t.Errorf("unexpected docstring: %q", makeWidget.DocComment)
	}

	private := symbolByName(fa.Symbols, "_private_helper")
	if private == nil || private.Exported {
		t.Fatalf("expected unexported _private_helper, got %+v", private)
	}

	widget := symbolByName(fa.Symbols, "Widget")
	if widget == nil || widget.Kind != KindStruct {
		t.Fatalf("expected class Widget mapped to KindStruct, got %+v", widget)
	}

	describe := symbolByName(fa.Symbols, "describe")
	if describe == nil || describe.Kind != KindMethod || describe.Receiver != "Widget" {
		t.Fatalf("expected method describe with receiver Widget, got %+v", describe)
	}

	internal := symbolByName(fa.Symbols, "_internal")
	if internal == nil || internal.Exported {
		t.Fatalf("expected unexported method _internal, got %+v", internal)
	}
}

func TestAnalyzePython_Constants(t *testing.T) {
	skipIfNoPython3(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	fa, err := analyzePython(ctx, "widgets.py", pyFixture)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	maxWidgets := symbolByName(fa.Symbols, "MAX_WIDGETS")
	if maxWidgets == nil || maxWidgets.Kind != KindConstant || !maxWidgets.Exported {
		t.Fatalf("expected exported constant MAX_WIDGETS, got %+v", maxWidgets)
	}
}

func TestAnalyzePython_SyntaxErrorIsPartialNotFatal(t *testing.T) {
	skipIfNoPython3(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	fa, err := analyzePython(ctx, "broken.py", "def broken(:\n  pass")
	if err != nil {
		t.Fatalf("expected no error (partial result instead), got %v", err)
	}
	if !fa.Partial {
		t.Error("expected Partial=true for a Python syntax error")
	}
}

func TestAnalyzePython_GracefulWhenPython3Missing(t *testing.T) {
	// Directly exercises the fallback path regardless of whether python3 is
	// actually installed, by simulating its absence. Swaps the *sync.Once
	// pointer (not a value copy — sync.Once must never be copied) so the
	// "already resolved" state can be forced for this test only.
	origOnce := pythonAvailableOnce
	origAvailable := pythonAvailable
	t.Cleanup(func() {
		pythonAvailableOnce = origOnce
		pythonAvailable = origAvailable
	})
	pythonAvailableOnce = &sync.Once{}
	pythonAvailableOnce.Do(func() {}) // mark as already-resolved
	pythonAvailable = false

	fa, err := analyzePython(context.Background(), "widgets.py", pyFixture)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !fa.Partial {
		t.Error("expected Partial=true when python3 is unavailable")
	}
	if fa.Warning == "" {
		t.Error("expected a warning explaining python3 is unavailable")
	}
}

func TestAnalyzePython_ContextCancellationPropagatesAsError(t *testing.T) {
	skipIfNoPython3(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled

	_, err := analyzePython(ctx, "widgets.py", pyFixture)
	if err == nil {
		t.Error("expected an error when context is already cancelled")
	}
}
