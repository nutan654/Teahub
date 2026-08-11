package gitclone

import "testing"

func TestNormalize(t *testing.T) {
	cases := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"https://github.com/golang/go", "https://github.com/golang/go", false},
		{"https://github.com/golang/go/", "https://github.com/golang/go", false},
		{"https://github.com/golang/go.git", "https://github.com/golang/go", false},
		{"golang/go", "https://github.com/golang/go", false},
		{"", "", true},
		{"https://gitlab.com/foo/bar", "", true},
		{"not a url", "", true},
	}
	for _, c := range cases {
		got, err := Normalize(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("Normalize(%q): expected error, got nil (result %q)", c.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("Normalize(%q): unexpected error: %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("Normalize(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
