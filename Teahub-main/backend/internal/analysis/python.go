package analysis

import (
	"bytes"
	"context"
	"encoding/json"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// pythonASTScript runs inside python3 itself and uses Python's own standard
// library `ast` module to parse the file passed on stdin, then prints a
// JSON summary to stdout.
//
// Why shell out instead of a pure-Go Python parser: there is no Python
// grammar parser in the Go standard library, and pulling in a third-party
// one would be exactly the "unnecessary dependency" this project is meant
// to avoid — plus this module's network sandbox doesn't reach the Go
// module proxy for arbitrary packages anyway. python3 itself is already a
// near-universal dev-machine and CI dependency, and using its *actual*
// ast module is more correct than any regex-based Go approximation could
// be. See analyzePython below for the fallback when python3 isn't present.
const pythonASTScript = `
import ast, json, sys

def get_doc(node):
    try:
        return ast.get_docstring(node) or ""
    except Exception:
        return ""

def func_sig(node):
    a = node.args
    parts = [arg.arg for arg in a.posonlyargs] + [arg.arg for arg in a.args]
    if a.vararg:
        parts.append("*" + a.vararg.arg)
    parts += [arg.arg for arg in a.kwonlyargs]
    if a.kwarg:
        parts.append("**" + a.kwarg.arg)
    prefix = "async def " if isinstance(node, ast.AsyncFunctionDef) else "def "
    return prefix + node.name + "(" + ", ".join(parts) + ")"

src = sys.stdin.read()
try:
    tree = ast.parse(src)
except SyntaxError as e:
    print(json.dumps({"error": str(e)}))
    sys.exit(0)

result = {"docstring": get_doc(tree), "imports": [], "symbols": []}

for node in ast.walk(tree):
    if isinstance(node, ast.Import):
        for alias in node.names:
            result["imports"].append(alias.name)
    elif isinstance(node, ast.ImportFrom):
        mod = node.module or ("." * node.level)
        for alias in node.names:
            result["imports"].append((mod + "." + alias.name) if mod else alias.name)

for node in tree.body:
    if isinstance(node, (ast.FunctionDef, ast.AsyncFunctionDef)):
        result["symbols"].append({
            "kind": "function", "name": node.name, "line": node.lineno,
            "signature": func_sig(node), "doc": get_doc(node),
            "exported": not node.name.startswith("_"),
        })
    elif isinstance(node, ast.ClassDef):
        result["symbols"].append({
            "kind": "struct", "name": node.name, "line": node.lineno,
            "signature": "class " + node.name, "doc": get_doc(node),
            "exported": not node.name.startswith("_"),
        })
        for item in node.body:
            if isinstance(item, (ast.FunctionDef, ast.AsyncFunctionDef)):
                result["symbols"].append({
                    "kind": "method", "name": item.name, "line": item.lineno,
                    "signature": func_sig(item), "doc": get_doc(item),
                    "receiver": node.name, "exported": not item.name.startswith("_"),
                })
    elif isinstance(node, ast.Assign):
        for target in node.targets:
            if isinstance(target, ast.Name) and target.id.isupper():
                result["symbols"].append({
                    "kind": "constant", "name": target.id, "line": node.lineno,
                    "exported": not target.id.startswith("_"),
                })

print(json.dumps(result))
`

type pythonSymbol struct {
	Kind      string `json:"kind"`
	Name      string `json:"name"`
	Line      int    `json:"line"`
	Signature string `json:"signature"`
	Doc       string `json:"doc"`
	Receiver  string `json:"receiver"`
	Exported  bool   `json:"exported"`
}

type pythonASTResult struct {
	Error     string         `json:"error"`
	Docstring string         `json:"docstring"`
	Imports   []string       `json:"imports"`
	Symbols   []pythonSymbol `json:"symbols"`
}

// pythonAvailable is checked once (not per file) since exec.LookPath does a
// filesystem/PATH scan — no reason to pay that cost per file in a repo with
// thousands of them.
var (
	pythonAvailableOnce = &sync.Once{}
	pythonAvailable     bool
)

func hasPython3() bool {
	pythonAvailableOnce.Do(func() {
		_, err := exec.LookPath("python3")
		pythonAvailable = err == nil
	})
	return pythonAvailable
}

const pythonAnalysisTimeout = 5 * time.Second

func analyzePython(ctx context.Context, path, content string) (*FileAnalysis, error) {
	fa := &FileAnalysis{Path: path, Language: "python", Package: strings.TrimSuffix(filepath.Base(path), ".py")}

	if !hasPython3() {
		fa.Partial = true
		fa.Warning = "python3 not found on PATH; structural Python analysis skipped (TODO/FIXME comments are still extracted)"
		return fa, nil
	}

	timeoutCtx, cancel := context.WithTimeout(ctx, pythonAnalysisTimeout)
	defer cancel()

	cmd := exec.CommandContext(timeoutCtx, "python3", "-c", pythonASTScript)
	cmd.Stdin = strings.NewReader(content)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err() // real cancellation propagates, doesn't get swallowed as "partial"
		}
		fa.Partial = true
		fa.Warning = "python3 ast invocation failed: " + err.Error() + " " + strings.TrimSpace(stderr.String())
		return fa, nil
	}

	var pr pythonASTResult
	if err := json.Unmarshal(stdout.Bytes(), &pr); err != nil {
		fa.Partial = true
		fa.Warning = "failed to parse python3 ast output: " + err.Error()
		return fa, nil
	}
	if pr.Error != "" {
		fa.Partial = true
		fa.Warning = "python syntax error: " + pr.Error
		return fa, nil
	}

	fa.Imports = pr.Imports
	for _, s := range pr.Symbols {
		fa.Symbols = append(fa.Symbols, Symbol{
			Kind: SymbolKind(s.Kind), Name: s.Name, Path: path, Line: s.Line,
			Signature: s.Signature, DocComment: s.Doc, Receiver: s.Receiver, Exported: s.Exported,
		})
	}
	return fa, nil
}
