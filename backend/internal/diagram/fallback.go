package diagram

import (
	"fmt"
	"sort"
	"strings"

	"teahub/backend/internal/store"
)

// cleanLabel makes arbitrary repo-derived text safe inside a quoted Mermaid label.
func cleanLabel(s string) string {
	r := strings.NewReplacer(`"`, "", "<", "", ">", "", "#", "", "`", "", "\n", " ", "[", "(", "]", ")")
	return strings.TrimSpace(r.Replace(s))
}

func joinCapped(items []string, n int, sep string) string {
	if len(items) > n {
		extra := len(items) - n
		items = append(append([]string{}, items[:n]...), fmt.Sprintf("+%d more", extra))
	}
	clean := make([]string, len(items))
	for i, it := range items {
		clean[i] = cleanLabel(it)
	}
	return strings.Join(clean, sep)
}

func extCount(s StructuralSummary, exts ...string) int {
	n := 0
	for _, e := range exts {
		n += s.ExtCounts[e]
	}
	return n
}

// FallbackMermaid builds a deterministic, always-valid architecture diagram
// straight from the structural summary. It is used when the AI output is
// unusable, and sent to the browser as a second chance if the browser's own
// Mermaid parser rejects the AI diagram.
func FallbackMermaid(repo *store.Repo) string {
	return buildFallback(BuildSummary(repo))
}

func buildFallback(s StructuralSummary) string {
	var b strings.Builder
	w := func(format string, a ...any) { fmt.Fprintf(&b, format+"\n", a...) }

	w("flowchart TD")
	w(`  classDef ui fill:#12301f,stroke:#5fb98c,stroke-width:1.5px,color:#e6edf3`)
	w(`  classDef app fill:#1b2740,stroke:#6ea8fe,stroke-width:1.5px,color:#e6edf3`)
	w(`  classDef data fill:#3a2a12,stroke:#e0a458,stroke-width:1.5px,color:#e6edf3`)
	w(`  classDef infra fill:#2d1f3d,stroke:#b48ead,stroke-width:1.5px,color:#e6edf3`)
	w(`  classDef repo fill:#161c25,stroke:#7b8794,stroke-width:1.5px,color:#e6edf3`)

	hasFE := s.RoleCounts[RoleFrontend] > 0
	hasBE := s.RoleCounts[RoleBackend] > 0
	hasDB := s.RoleCounts[RoleDatabase] > 0 || len(s.DBTables) > 0
	hasInfra := s.RoleCounts[RoleInfra] > 0 || len(s.Services) > 0

	if !hasFE && !hasBE && !hasDB && !hasInfra {
		w(`  REPO["Repository<br/>%d files, no recognizable application structure"]:::repo`, s.FileCount)
		return strings.TrimSpace(b.String())
	}

	if hasFE {
		w(`  subgraph SG_UI["Presentation layer"]`)
		w(`    direction TB`)
		if len(s.Pages) > 0 {
			w(`    UI_PAGES["Pages (%d)<br/>%s"]:::ui`, extCount(s, ".html"), joinCapped(s.Pages, 8, " · "))
		}
		var assets []string
		for _, e := range []struct{ ext, name string }{{".js", "JS"}, {".ts", "TS"}, {".jsx", "JSX"}, {".tsx", "TSX"}, {".vue", "Vue"}, {".css", "CSS"}, {".scss", "SCSS"}} {
			if n := s.ExtCounts[e.ext]; n > 0 {
				assets = append(assets, fmt.Sprintf("%d %s", n, e.name))
			}
		}
		if len(assets) > 0 {
			w(`    UI_ASSETS["Client scripts and styles<br/>%s"]:::ui`, joinCapped(assets, 6, " · "))
		}
		if len(s.Pages) > 0 && len(assets) > 0 {
			w(`    UI_PAGES -->|"load"| UI_ASSETS`)
		}
		w(`  end`)
	}

	if hasBE {
		w(`  subgraph SG_APP["Application layer"]`)
		var langs []string
		for _, l := range s.Languages {
			if strings.HasPrefix(l, "PHP") || strings.HasPrefix(l, "Python") || strings.HasPrefix(l, "Go ") ||
				strings.HasPrefix(l, "Ruby") || strings.HasPrefix(l, "Java ") || strings.HasPrefix(l, "Kotlin") ||
				strings.HasPrefix(l, "Rust") || strings.HasPrefix(l, "C#") {
				langs = append(langs, l)
			}
		}
		title := "Server-side code"
		if len(langs) > 0 {
			title += "<br/>" + joinCapped(langs, 3, " · ")
		}
		w(`    APP_CORE["%s"]:::app`, title)
		if len(s.Entrypoints) > 0 {
			w(`    APP_ENTRY["Entrypoints<br/>%s"]:::app`, joinCapped(s.Entrypoints, 4, " · "))
			w(`    APP_ENTRY --> APP_CORE`)
		}
		if len(s.Features) > 0 {
			w(`    APP_FEATURES["Feature areas<br/>%s"]:::app`, joinCapped(s.Features, 10, " · "))
			w(`    APP_CORE --> APP_FEATURES`)
		}
		w(`  end`)
	}

	if hasDB {
		w(`  subgraph SG_DATA["Data layer"]`)
		if s.RoleCounts[RoleDatabase] > 0 {
			w(`    DATA_MIG["SQL migrations and schema<br/>%d files"]:::data`, s.RoleCounts[RoleDatabase])
		}
		label := "Database"
		if len(s.DBTables) > 0 {
			label += "<br/>Tables: " + joinCapped(s.DBTables, 8, " · ")
		}
		w(`    DATA_DB[("%s")]:::data`, label)
		if s.RoleCounts[RoleDatabase] > 0 {
			w(`    DATA_MIG -->|"defines schema"| DATA_DB`)
		}
		w(`  end`)
	}

	if hasInfra {
		w(`  subgraph SG_INFRA["Runtime and delivery"]`)
		w(`    INFRA_BUILD["Container and CI config<br/>%d files"]:::infra`, s.RoleCounts[RoleInfra])
		svcs := append([]string{}, s.Services...)
		sort.Strings(svcs)
		for i, svc := range svcs {
			if i >= 6 {
				break
			}
			name := svc
			if k := strings.Index(name, " ("); k > 0 {
				name = name[:k]
			}
			w(`    INFRA_S%d["Service: %s"]:::infra`, i, cleanLabel(name))
			w(`    INFRA_BUILD -->|"runs"| INFRA_S%d`, i)
		}
		w(`  end`)
	}

	// Cross-layer wiring: only draw links both ends of which exist.
	switch {
	case hasFE && hasBE:
		anchor := "UI_ASSETS"
		if len(s.Pages) > 0 && extCount(s, ".js", ".ts", ".jsx", ".tsx", ".vue") == 0 {
			anchor = "UI_PAGES"
		}
		w(`  %s -->|"HTTP requests"| APP_CORE`, anchor)
	}
	if hasBE && hasDB {
		w(`  APP_CORE -->|"reads and writes"| DATA_DB`)
	}
	if hasInfra && hasBE {
		w(`  INFRA_BUILD -.->|"packages and deploys"| APP_CORE`)
	} else if hasInfra && hasFE {
		w(`  INFRA_BUILD -.->|"packages and deploys"| UI_PAGES`)
	}
	return strings.TrimSpace(b.String())
}

func fallbackExplanation(s StructuralSummary) string {
	var parts []string
	if len(s.Languages) > 0 {
		parts = append(parts, fmt.Sprintf("This repository has %d files, mainly %s.", s.FileCount, joinCapped(s.Languages, 3, ", ")))
	}
	var layers []string
	if s.RoleCounts[RoleFrontend] > 0 {
		layers = append(layers, fmt.Sprintf("a presentation layer (%d files)", s.RoleCounts[RoleFrontend]))
	}
	if s.RoleCounts[RoleBackend] > 0 {
		layers = append(layers, fmt.Sprintf("an application layer (%d files)", s.RoleCounts[RoleBackend]))
	}
	if s.RoleCounts[RoleDatabase] > 0 || len(s.DBTables) > 0 {
		layers = append(layers, "a database layer")
	}
	if s.RoleCounts[RoleInfra] > 0 {
		layers = append(layers, "container/CI configuration")
	}
	if len(layers) > 0 {
		parts = append(parts, "It contains "+strings.Join(layers, ", ")+".")
	}
	if len(s.Features) > 0 {
		parts = append(parts, "Feature areas visible in the file names: "+joinCapped(s.Features, 12, ", ")+".")
	}
	parts = append(parts, "This diagram was generated from the repository structure alone because the AI's diagram could not be used. Press Regenerate to try the AI version again.")
	return strings.Join(parts, " ")
}
