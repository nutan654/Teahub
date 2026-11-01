package diagram

import (
	"fmt"
	"regexp"
	"strings"
)

// ErrInvalidDiagram is returned when the model's response doesn't look like
// a real Mermaid diagram, so callers never forward garbage to the frontend.
var ErrInvalidDiagram = fmt.Errorf("model response is not a valid mermaid diagram")

var diagramHeaderRe = regexp.MustCompile(
	`^(?:(?:flowchart|graph)(?:\s+(?:TD|TB|BT|RL|LR))?|sequenceDiagram|classDiagram|erDiagram|stateDiagram(?:-v2)?)\s*;?$`)

func isDiagramHeader(line string) bool {
	return diagramHeaderRe.MatchString(strings.TrimSpace(line))
}

// ExtractMermaid pulls the mermaid code block out of a model response (which
// may include prose around it), sanitizes common model syntax slips, and
// validates it starts with a recognized diagram type keyword. This is
// deliberately strict: a diagram feature that silently renders broken output
// is worse than one that fails loudly.
func ExtractMermaid(modelOutput string) (string, error) {
	text := modelOutput
	if idx := strings.Index(text, "```mermaid"); idx != -1 {
		rest := text[idx+len("```mermaid"):]
		if end := strings.Index(rest, "```"); end != -1 {
			text = rest[:end]
		} else {
			text = rest
		}
	} else if idx := strings.Index(text, "```"); idx != -1 {
		rest := text[idx+3:]
		if end := strings.Index(rest, "```"); end != -1 {
			text = rest[:end]
		}
	} else {
		// No fence at all: salvage from the first line that is a diagram header.
		lines := strings.Split(text, "\n")
		for i, l := range lines {
			if isDiagramHeader(l) {
				text = strings.Join(lines[i:], "\n")
				break
			}
		}
	}
	text = strings.TrimSpace(strings.ReplaceAll(text, "\r\n", "\n"))

	// Models sometimes put a language tag or stray prose line before the header.
	lines := strings.Split(text, "\n")
	for i, l := range lines {
		if strings.TrimSpace(l) == "" {
			continue
		}
		if isDiagramHeader(l) {
			text = strings.Join(lines[i:], "\n")
			return SanitizeMermaid(text), nil
		}
		break
	}
	return "", ErrInvalidDiagram
}

var (
	bareNodeLabelRe = regexp.MustCompile(`(\b[A-Za-z_][A-Za-z0-9_]*)\[([^\[\]"(/\\\n][^\[\]"\n]*)\]`)
	bareCylinderRe  = regexp.MustCompile(`(\b[A-Za-z_][A-Za-z0-9_]*)\[\(([^"\n]*?)\)\]`)
	bareEdgeLabelRe = regexp.MustCompile(`(-+>|-\.+->|=+>|-{3,})\|([^|"\n]+)\|`)
	subgraphRe      = regexp.MustCompile(`^(\s*)subgraph\s+(.+?)\s*$`)
	initDirectiveRe = regexp.MustCompile(`(?m)^\s*%%\{.*\}%%\s*$`)
	plainWordsRe    = regexp.MustCompile(`^[A-Za-z0-9_]+$`)
)

// SanitizeMermaid fixes the syntax slips LLMs make most often. The dominant
// cause of "Syntax error in text" in the browser is an unquoted label that
// contains parentheses, slashes, colons or similar, so every bare label is
// quoted. It is conservative: anything already quoted or using a special
// shape ([( )], [[ ]], {{ }}, (( ))) is left untouched.
func SanitizeMermaid(src string) string {
	r := strings.NewReplacer("“", `"`, "”", `"`, "‘", "'", "’", "'", "\t", "  ")
	src = r.Replace(src)
	src = initDirectiveRe.ReplaceAllString(src, "")

	lines := strings.Split(src, "\n")
	out := make([]string, 0, len(lines))
	sg := 0
	for _, line := range lines {
		trim := strings.TrimSpace(line)
		// click handlers and raw HTML are never wanted (and are blocked by
		// the frontend's strict security level anyway).
		if strings.HasPrefix(trim, "click ") {
			continue
		}
		if m := subgraphRe.FindStringSubmatch(line); m != nil {
			title := m[2]
			if !strings.Contains(title, "[") && !strings.HasPrefix(title, `"`) && !plainWordsRe.MatchString(title) {
				sg++
				line = fmt.Sprintf(`%ssubgraph SG%d["%s"]`, m[1], sg, strings.ReplaceAll(title, `"`, ""))
			}
			out = append(out, line)
			continue
		}
		if strings.HasPrefix(trim, "classDef ") || strings.HasPrefix(trim, "class ") ||
			strings.HasPrefix(trim, "style ") || strings.HasPrefix(trim, "linkStyle ") {
			out = append(out, line)
			continue
		}
		line = bareCylinderRe.ReplaceAllString(line, `$1[("$2")]`)
		line = bareNodeLabelRe.ReplaceAllString(line, `$1["$2"]`)
		line = bareEdgeLabelRe.ReplaceAllString(line, `$1|"$2"|`)
		out = append(out, line)
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}

// ValidateStructure runs cheap structural checks that catch output Mermaid
// would reject or render as an empty canvas.
func ValidateStructure(src string) error {
	lines := strings.Split(src, "\n")
	if len(lines) < 2 {
		return fmt.Errorf("%w: diagram has no body", ErrInvalidDiagram)
	}
	open, edges := 0, 0
	for _, l := range lines[1:] {
		t := strings.TrimSpace(l)
		switch {
		case strings.HasPrefix(t, "subgraph"):
			open++
		case t == "end":
			open--
			if open < 0 {
				return fmt.Errorf("%w: unbalanced 'end'", ErrInvalidDiagram)
			}
		}
		if strings.Contains(t, "-->") || strings.Contains(t, "---") || strings.Contains(t, "-.->") ||
			strings.Contains(t, "==>") || strings.Contains(t, "->>") {
			edges++
		}
	}
	if open != 0 {
		return fmt.Errorf("%w: unclosed subgraph", ErrInvalidDiagram)
	}
	if edges == 0 && !strings.HasPrefix(src, "classDiagram") && !strings.HasPrefix(src, "erDiagram") {
		return fmt.Errorf("%w: diagram has no connections", ErrInvalidDiagram)
	}
	if strings.Count(src, `"`)%2 != 0 {
		return fmt.Errorf("%w: unbalanced quotes", ErrInvalidDiagram)
	}
	return nil
}
