package meta

import "testing"

func TestVisibilityHidesEntriesAndEverythingBelowThem(t *testing.T) {
	v := Visibility{Hidden: map[string]bool{".github": true, ".gitignore": false, "src/.env": false}}
	for path, hidden := range map[string]bool{
		".github":                  true,
		".github/workflows/ci.yml": true,
		".gitignore":               true,
		"src/.env":                 true,
		"src/main.go":              false,
		".githubx/file":            false, // a prefix of a name is not its parent
		"docs/.github/x":           false, // only the recorded path itself counts
	} {
		if got := v.Hides(path); got != hidden {
			t.Errorf("Hides(%q) = %v, want %v", path, got, hidden)
		}
	}
	if (Visibility{}).Hides(".github") {
		t.Error("an empty visibility hides nothing")
	}
}
