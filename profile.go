package main

import (
	"embed"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

//go:embed profiles/*.md
var profileFS embed.FS

// loadProfile returns the text of profile NAME: profiles/NAME.md in the qh
// home of the instance, then in ~/.qh, then the one built into qh.
func loadProfile(name string) (string, error) {
	if !validName.MatchString(name) {
		return "", fmt.Errorf("bad profile name %q", name)
	}
	var dirs []string
	if d, err := qhDir(); err == nil {
		dirs = append(dirs, d)
	}
	if h, err := os.UserHomeDir(); err == nil {
		dirs = append(dirs, filepath.Join(h, ".qh"))
	}
	for _, d := range dirs {
		if b, err := os.ReadFile(filepath.Join(d, "profiles", name+".md")); err == nil {
			return strings.TrimSpace(string(b)), nil
		}
	}
	if b, err := profileFS.ReadFile("profiles/" + name + ".md"); err == nil {
		return strings.TrimSpace(string(b)), nil
	}
	return "", fmt.Errorf("no profile %q (built in: %s)", name, strings.Join(profileNames(), ", "))
}

func profileNames() []string {
	var out []string
	ents, _ := profileFS.ReadDir("profiles")
	for _, e := range ents {
		out = append(out, strings.TrimSuffix(e.Name(), ".md"))
	}
	return out
}
