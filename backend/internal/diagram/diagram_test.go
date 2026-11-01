package diagram

import (
	"strings"
	"testing"

	"teahub/backend/internal/store"
)

func TestBuildSummary(t *testing.T) {
	repo := &store.Repo{
		Tree: &store.FileNode{
			Children: []*store.FileNode{
				{Path: "backend", IsDir: true},
				{Path: "frontend", IsDir: true},
				{Path: "README.md"},
			},
		},
		Files: map[string]string{
			"go.mod":                "module example",
			"backend/cmd/main.go":   "package main",
			"frontend/package.json": "{}",
		},
	}

	s := BuildSummary(repo)

	if len(s.TopLevelDirs) != 2 || s.TopLevelDirs[0] != "backend" || s.TopLevelDirs[1] != "frontend" {
		t.Errorf("unexpected top-level dirs: %v", s.TopLevelDirs)
	}
	if !contains(s.Ecosystems, "Go module") || !contains(s.Ecosystems, "Node.js/JavaScript") {
		t.Errorf("expected Go and Node ecosystems, got %v", s.Ecosystems)
	}
	if !contains(s.Entrypoints, "backend/cmd/main.go") {
		t.Errorf("expected main.go detected as entrypoint, got %v", s.Entrypoints)
	}
}

func TestBuildSummaryEmptyRepo(t *testing.T) {
	repo := &store.Repo{Tree: &store.FileNode{}, Files: map[string]string{}}
	s := BuildSummary(repo)
	str := s.String()
	if !strings.Contains(str, "(none)") && !strings.Contains(str, "(none detected)") {
		t.Errorf("expected placeholder text for empty summary, got: %s", str)
	}
}

func contains(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

func TestExtractMermaidWithCodeFence(t *testing.T) {
	raw := "Here's the diagram:\n\n```mermaid\nflowchart TD\n  A --> B\n```\n\nThis shows a simple flow."
	got, err := ExtractMermaid(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.HasPrefix(got, "flowchart TD") {
		t.Errorf("unexpected extracted diagram: %q", got)
	}
}

func TestExtractMermaidGenericFence(t *testing.T) {
	raw := "```\ngraph LR\n  A --> B\n```"
	got, err := ExtractMermaid(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.HasPrefix(got, "graph LR") {
		t.Errorf("unexpected extracted diagram: %q", got)
	}
}

func TestExtractMermaidRejectsNonMermaid(t *testing.T) {
	raw := "```\nThis is just some prose, not a diagram at all.\n```"
	_, err := ExtractMermaid(raw)
	if err != ErrInvalidDiagram {
		t.Errorf("expected ErrInvalidDiagram, got %v", err)
	}
}

func TestExtractMermaidRejectsPlainProse(t *testing.T) {
	_, err := ExtractMermaid("I cannot generate a diagram for this repository.")
	if err != ErrInvalidDiagram {
		t.Errorf("expected ErrInvalidDiagram, got %v", err)
	}
}

func TestExtractMermaidSequenceDiagram(t *testing.T) {
	raw := "```mermaid\nsequenceDiagram\n  A->>B: request\n```"
	got, err := ExtractMermaid(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.HasPrefix(got, "sequenceDiagram") {
		t.Errorf("unexpected extracted diagram: %q", got)
	}
}
<<<<<<< HEAD
=======

func TestSanitizeQuotesBareLabelsWithParens(t *testing.T) {
	in := "flowchart TD\n  A[API (PHP) / REST] -->|reads (SQL)| B[(Database)]\n  B --> C[Cache: redis]"
	got := SanitizeMermaid(in)
	for _, want := range []string{`A["API (PHP) / REST"]`, `|"reads (SQL)"|`, `B[("Database")]`, `C["Cache: redis"]`} {
		if !strings.Contains(got, want) {
			t.Errorf("sanitized output missing %q:\n%s", want, got)
		}
	}
}

func TestSanitizeDoesNotCorruptTwoEdgeLabelsOnOneLine(t *testing.T) {
	in := "flowchart TD\n  A -->|\"one\"| B -->|\"two\"| C"
	if got := SanitizeMermaid(in); got != in {
		t.Errorf("already-valid line was altered:\n%s", got)
	}
}

func TestSanitizeQuotesSubgraphTitlesAndStripsInit(t *testing.T) {
	in := "%%{init: {'theme':'forest'}}%%\nflowchart TD\n  subgraph Data Layer (SQL)\n    A --> B\n  end"
	got := SanitizeMermaid(in)
	if strings.Contains(got, "%%{") {
		t.Errorf("init directive not stripped:\n%s", got)
	}
	if !strings.Contains(got, `subgraph SG1["Data Layer (SQL)"]`) {
		t.Errorf("subgraph title not quoted:\n%s", got)
	}
}

func TestExtractMermaidSalvagesUnfencedDiagram(t *testing.T) {
	got, err := ExtractMermaid("Sure! Here you go:\nflowchart LR\n  A --> B")
	if err != nil || !strings.HasPrefix(got, "flowchart LR") {
		t.Fatalf("got %q, err %v", got, err)
	}
}

func TestValidateStructure(t *testing.T) {
	cases := map[string]bool{
		"flowchart TD\n  A --> B":                             true,
		"flowchart TD\n  subgraph S\n    A --> B":             false,
		"flowchart TD\n  A[\"x]\n  A --> B":                   false,
		"flowchart TD\n  A":                                   false,
		"flowchart TD\n  subgraph S[\"s\"]\n  A --> B\n  end": true,
	}
	for src, ok := range cases {
		if err := ValidateStructure(src); (err == nil) != ok {
			t.Errorf("ValidateStructure(%q) = %v, want ok=%v", src, err, ok)
		}
	}
}

func flatPHPRepo() *store.Repo {
	return &store.Repo{
		Tree: &store.FileNode{Children: []*store.FileNode{
			{Path: "migrations", IsDir: true, Children: []*store.FileNode{
				{Path: "migrations/000_base_schema.sql"}, {Path: "migrations/001_finance_tables.sql"},
			}},
			{Path: "Dockerfile.php"}, {Path: "docker-compose.yml"}, {Path: "admin.html"},
			{Path: "cart.html"}, {Path: "cart.js"}, {Path: "cart.css"}, {Path: "api.php"},
			{Path: "add_invoice_api.php"}, {Path: "add_vendor_api.php"}, {Path: "README.md"},
		}},
		Files: map[string]string{
			"migrations/000_base_schema.sql": "CREATE TABLE IF NOT EXISTS invoices (id INT);\nCREATE TABLE vendors (id INT);",
			"docker-compose.yml":             "services:\n  web:\n    build: .\n    ports:\n      - \"8080:80\"\n  db:\n    image: mysql:8\nvolumes:\n  data:\n",
			"README.md":                      "# Visonflow\n\nPharmacy billing.",
		},
	}
}

func TestBuildSummaryFlatRepoSeesRootFilesNotJustDirs(t *testing.T) {
	s := BuildSummary(flatPHPRepo())
	str := s.String()
	for _, want := range []string{"PHP (3 files)", "cart", "invoice", "vendor", "invoices", "vendors", "web (build: .", "db (image: mysql:8", "Pharmacy billing"} {
		if !strings.Contains(str, want) {
			t.Errorf("summary missing %q:\n%s", want, str)
		}
	}
	if len(s.Services) != 2 {
		t.Errorf("expected 2 compose services, got %v", s.Services)
	}
}

func TestFallbackMermaidIsValidAndLayered(t *testing.T) {
	m := FallbackMermaid(flatPHPRepo())
	if err := ValidateStructure(m); err != nil {
		t.Fatalf("fallback diagram failed validation: %v\n%s", err, m)
	}
	for _, want := range []string{"Presentation layer", "Application layer", "Data layer", "Runtime and delivery", "DATA_DB[(", "invoice"} {
		if !strings.Contains(m, want) {
			t.Errorf("fallback missing %q:\n%s", want, m)
		}
	}
	if SanitizeMermaid(m) != m {
		t.Errorf("sanitizer should leave the fallback diagram unchanged")
	}
}

func TestFallbackMermaidEmptyRepo(t *testing.T) {
	m := FallbackMermaid(&store.Repo{Tree: &store.FileNode{}, Files: map[string]string{}})
	if !strings.HasPrefix(m, "flowchart TD") || !strings.Contains(m, "REPO[") {
		t.Errorf("unexpected empty-repo fallback:\n%s", m)
	}
}
>>>>>>> 3edc2f6 (Update TeaHub)
