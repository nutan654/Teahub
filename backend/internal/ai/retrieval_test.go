package ai

import "testing"

func TestRetrieve(t *testing.T) {
	files := map[string]string{
		"auth/jwt.go":       "func VerifyToken(t string) bool { /* jwt verification */ return true }",
		"handlers/user.go":  "func GetUser(id string) User { return User{} }",
		"README.md":         "# Example project\nThis is just a readme.",
		"handlers/order.go": "func CreateOrder() {}",
	}

	got := Retrieve(files, "how is JWT authentication verified?", 2)
	if len(got) == 0 {
		t.Fatal("expected at least one result")
	}
	if got[0] != "auth/jwt.go" {
		t.Errorf("expected auth/jwt.go to rank first, got %v", got)
	}
}

func TestRetrieveNoMatch(t *testing.T) {
	files := map[string]string{"a.go": "package main"}
	got := Retrieve(files, "the a", 5) // only stopwords
	if got != nil {
		t.Errorf("expected nil for stopword-only query, got %v", got)
	}
}

func TestBuildContextRespectsBudget(t *testing.T) {
	files := map[string]string{"big.go": string(make([]byte, maxContextChars+1000))}
	ctx := BuildContext("tree", files, []string{"big.go"})
	if len(ctx) > maxContextChars+2000 { // tree header + markers add a little overhead
		t.Errorf("context length %d exceeds expected budget", len(ctx))
	}
}
