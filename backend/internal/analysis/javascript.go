package analysis

import (
	"regexp"
	"strings"
)

// analyzeJS is deliberately NOT a real parser. There's no JS/TS AST in the
// Go standard library, and pulling in a full parser (e.g. via an external
// module or a Node.js subprocess) for one analyzer would be a heavier
// dependency than this project's "avoid unnecessary dependencies" goal
// allows — especially compared to Go's own stdlib parser and Python's
// subprocess-to-its-own-ast approach, neither of which added anything.
//
// So this is honest, bounded heuristic extraction: line-based regex
// matching for the common declaration shapes (named function, arrow
// function assigned to a const, class, class method, import/require,
// SCREAMING_CASE const). It will miss decorators, multi-line signatures,
// destructured exports, and other real-world syntax — every FileAnalysis
// this produces is marked Partial so callers never mistake it for a
// complete parse.
func analyzeJS(path, content string) (*FileAnalysis, error) {
	lang := "javascript"
	if strings.HasSuffix(path, ".ts") || strings.HasSuffix(path, ".tsx") {
		lang = "typescript"
	}
	fa := &FileAnalysis{
		Path: path, Language: lang,
		Partial: true,
		Warning: "JS/TS analysis is heuristic line-based regex matching, not a real parser — " +
			"complex, multi-line, or decorated declarations may be missed",
	}

	lines := strings.Split(content, "\n")
	currentClass := ""
	classIndent := -1

	for i, line := range lines {
		lineNo := i + 1

		if m := jsImportFromRe.FindStringSubmatch(line); m != nil {
			fa.Imports = append(fa.Imports, m[1])
		}
		if m := jsRequireRe.FindStringSubmatch(line); m != nil {
			fa.Imports = append(fa.Imports, m[1])
		}

		if m := jsClassRe.FindStringSubmatch(line); m != nil {
			currentClass = m[3]
			classIndent = indentWidth(line)
			fa.Symbols = append(fa.Symbols, Symbol{
				Kind: KindStruct, Name: currentClass, Path: path, Line: lineNo,
				Signature: "class " + currentClass, Exported: m[1] != "",
			})
			continue
		}
		// Heuristic dedent check: once we hit a non-blank line at or below
		// the class declaration's own indent, we've left its body.
		if currentClass != "" {
			trimmed := strings.TrimSpace(line)
			if trimmed != "" && trimmed != "{" && indentWidth(line) <= classIndent {
				currentClass = ""
			}
		}

		switch {
		case jsFuncRe.MatchString(line):
			m := jsFuncRe.FindStringSubmatch(line)
			fa.Symbols = append(fa.Symbols, Symbol{
				Kind: KindFunction, Name: m[4], Path: path, Line: lineNo,
				Signature: "function " + m[4] + "(" + m[5] + ")", Exported: m[1] != "",
			})
		case jsArrowConstRe.MatchString(line):
			m := jsArrowConstRe.FindStringSubmatch(line)
			fa.Symbols = append(fa.Symbols, Symbol{
				Kind: KindFunction, Name: m[3], Path: path, Line: lineNo,
				Signature: "const " + m[3] + " = (" + m[5] + ") =>", Exported: m[1] != "",
			})
		case currentClass != "" && jsMethodRe.MatchString(line):
			m := jsMethodRe.FindStringSubmatch(line)
			if !jsControlKeywords[m[1]] {
				fa.Symbols = append(fa.Symbols, Symbol{
					Kind: KindMethod, Name: m[1], Path: path, Line: lineNo,
					Signature: m[1] + "(" + m[2] + ")", Receiver: currentClass,
				})
			}
		}

		if m := jsConstRe.FindStringSubmatch(line); m != nil {
			fa.Symbols = append(fa.Symbols, Symbol{
				Kind: KindConstant, Name: m[2], Path: path, Line: lineNo, Exported: m[1] != "",
			})
		}
	}

	return fa, nil
}

func indentWidth(line string) int {
	n := 0
	for _, r := range line {
		switch r {
		case ' ':
			n++
		case '\t':
			n += 2
		default:
			return n
		}
	}
	return n
}

var (
	jsImportFromRe = regexp.MustCompile(`^\s*import\s+.*?from\s+['"]([^'"]+)['"]`)
	jsRequireRe    = regexp.MustCompile(`require\(\s*['"]([^'"]+)['"]\s*\)`)
	jsFuncRe       = regexp.MustCompile(`^\s*(export\s+)?(default\s+)?(async\s+)?function\s*\*?\s+([A-Za-z_$][\w$]*)\s*\(([^)]*)\)`)
	jsArrowConstRe = regexp.MustCompile(`^\s*(export\s+)?(default\s+)?const\s+([A-Za-z_$][\w$]*)\s*(?::\s*[^=]+)?=\s*(async\s+)?\(([^)]*)\)\s*=>`)
	jsClassRe      = regexp.MustCompile(`^\s*(export\s+)?(default\s+)?class\s+([A-Za-z_$][\w$]*)`)
	jsMethodRe     = regexp.MustCompile(`^\s{2,}(?:static\s+)?(?:async\s+)?(?:get\s+|set\s+)?([A-Za-z_$][\w$]*)\s*\(([^)]*)\)\s*\{`)
	jsConstRe      = regexp.MustCompile(`^\s*(export\s+)?const\s+([A-Z][A-Z0-9_]*)\s*=`)
)

// jsControlKeywords guards against jsMethodRe false-positiving on control
// flow that happens to look like "word(...) {" inside a class body, e.g.
// "if (x) {" or "for (...) {".
var jsControlKeywords = map[string]bool{
	"if": true, "for": true, "while": true, "switch": true, "catch": true, "function": true,
}
