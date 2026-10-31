package analysis

import "testing"

const jsFixture = `import express from 'express';
import { readFile } from 'fs/promises';
const path = require('path');

export const MAX_RETRIES = 3;

export function createServer(port) {
  return express();
}

const handleRequest = async (req, res) => {
  return res.send('ok');
};

export class UserController {
  constructor(db) {
    this.db = db;
  }

  async getUser(id) {
    return this.db.find(id);
  }

  static create(db) {
    return new UserController(db);
  }
}

function notExported() {
  return null;
}

// TODO: add rate limiting
`

func TestAnalyzeJS_MarksPartial(t *testing.T) {
	fa, err := analyzeJS("server.js", jsFixture)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !fa.Partial {
		t.Error("expected JS analysis to always be marked Partial (heuristic, not a real parser)")
	}
	if fa.Language != "javascript" {
		t.Errorf("expected language 'javascript', got %q", fa.Language)
	}
}

func TestAnalyzeJS_TypeScriptExtension(t *testing.T) {
	fa, _ := analyzeJS("server.ts", jsFixture)
	if fa.Language != "typescript" {
		t.Errorf("expected language 'typescript' for .ts file, got %q", fa.Language)
	}
}

func TestAnalyzeJS_Imports(t *testing.T) {
	fa, _ := analyzeJS("server.js", jsFixture)
	found := map[string]bool{}
	for _, imp := range fa.Imports {
		found[imp] = true
	}
	for _, want := range []string{"express", "fs/promises", "path"} {
		if !found[want] {
			t.Errorf("expected import %q, got %v", want, fa.Imports)
		}
	}
}

func TestAnalyzeJS_Functions(t *testing.T) {
	fa, _ := analyzeJS("server.js", jsFixture)

	createServer := symbolByName(fa.Symbols, "createServer")
	if createServer == nil || createServer.Kind != KindFunction || !createServer.Exported {
		t.Fatalf("expected exported function createServer, got %+v", createServer)
	}

	handleRequest := symbolByName(fa.Symbols, "handleRequest")
	if handleRequest == nil || handleRequest.Kind != KindFunction {
		t.Fatalf("expected arrow function handleRequest to be detected, got %+v", handleRequest)
	}

	notExported := symbolByName(fa.Symbols, "notExported")
	if notExported == nil || notExported.Exported {
		t.Fatalf("expected unexported function notExported, got %+v", notExported)
	}
}

func TestAnalyzeJS_ClassAndMethods(t *testing.T) {
	fa, _ := analyzeJS("server.js", jsFixture)

	class := symbolByName(fa.Symbols, "UserController")
	if class == nil || class.Kind != KindStruct || !class.Exported {
		t.Fatalf("expected exported class UserController, got %+v", class)
	}

	getUser := symbolByName(fa.Symbols, "getUser")
	if getUser == nil || getUser.Kind != KindMethod || getUser.Receiver != "UserController" {
		t.Fatalf("expected method getUser with receiver UserController, got %+v", getUser)
	}

	create := symbolByName(fa.Symbols, "create")
	if create == nil || create.Receiver != "UserController" {
		t.Fatalf("expected static method create attributed to UserController, got %+v", create)
	}
}

func TestAnalyzeJS_Constants(t *testing.T) {
	fa, _ := analyzeJS("server.js", jsFixture)
	maxRetries := symbolByName(fa.Symbols, "MAX_RETRIES")
	if maxRetries == nil || maxRetries.Kind != KindConstant || !maxRetries.Exported {
		t.Fatalf("expected exported constant MAX_RETRIES, got %+v", maxRetries)
	}
}

func TestAnalyzeJS_MethodDoesNotFalsePositiveOnControlFlow(t *testing.T) {
	src := `class Thing {
  run(x) {
    if (x) {
      return 1;
    }
    for (let i = 0; i < 10; i++) {}
    return 0;
  }
}
`
	fa, _ := analyzeJS("thing.js", src)
	if sym := symbolByName(fa.Symbols, "if"); sym != nil {
		t.Errorf("expected 'if' control flow to not be extracted as a method, got %+v", sym)
	}
	if sym := symbolByName(fa.Symbols, "for"); sym != nil {
		t.Errorf("expected 'for' control flow to not be extracted as a method, got %+v", sym)
	}
	if sym := symbolByName(fa.Symbols, "run"); sym == nil {
		t.Error("expected the real method 'run' to still be extracted")
	}
}
