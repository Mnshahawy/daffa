package web

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// Every CSS variable the frontend USES must be one it DEFINES.
//
// This is not tidiness. An undefined custom property makes the whole declaration invalid at
// computed-value time, and an invalid property falls back to its INITIAL value — which for
// `stroke` is `none`.
//
// So `stroke: var(--accent)`, against a token nobody ever defined, draws nothing. No error, no
// warning, no console message: the line is simply absent and the chart looks like it has no
// data. That shipped — the resource-monitor charts were empty for a week while the numbers
// beside them were right there in the header — and the gridlines rendered fine the whole time,
// because they used `var(--border)`, which happened to exist.
//
// The app's accent is --color-accent-500, and there was never an --accent.
func TestEveryCSSVariableIsDefined(t *testing.T) {
	root := filepath.Join("..", "..", "web", "src")

	var (
		used    = map[string][]string{} // name -> files that reference it
		defined = map[string]bool{}
		useRe   = regexp.MustCompile(`var\(\s*(--[a-zA-Z0-9_-]+)`)
		defRe   = regexp.MustCompile(`(?m)^\s*(--[a-zA-Z0-9_-]+)\s*:`)
	)

	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		switch filepath.Ext(path) {
		case ".css", ".vue", ".ts":
		default:
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		src := string(b)

		for _, m := range defRe.FindAllStringSubmatch(src, -1) {
			defined[m[1]] = true
		}
		rel, _ := filepath.Rel(root, path)
		for _, m := range useRe.FindAllStringSubmatch(src, -1) {
			used[m[1]] = append(used[m[1]], rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	// The colour system lives in the shared brand/tokens.css (imported by web/src/style.css and
	// by the docs site), OUTSIDE this root. Ingest its definitions too, or every --surface,
	// --text and --accent would read as undefined here.
	brandTokens := filepath.Join("..", "..", "brand", "tokens.css")
	tb, err := os.ReadFile(brandTokens)
	if err != nil {
		t.Fatalf("reading shared tokens %s: %v", brandTokens, err)
	}
	for _, m := range defRe.FindAllStringSubmatch(string(tb), -1) {
		defined[m[1]] = true
	}

	if len(used) == 0 {
		t.Fatal("found no CSS variables at all — the walk is not finding the sources")
	}

	// Tailwind generates its palette and spacing tokens itself; they are not ours to define.
	// Everything else is a token this codebase invented and therefore has to declare.
	generated := []string{"--color-", "--font-", "--spacing", "--radius", "--tw-"}

	var missing []string
	for name, files := range used {
		if defined[name] {
			continue
		}
		if slicesHasPrefix(generated, name) {
			continue
		}
		missing = append(missing, name+"  (used in "+strings.Join(dedupe(files), ", ")+")")
	}
	sort.Strings(missing)

	if len(missing) > 0 {
		t.Errorf("these CSS variables are used but never defined:\n\n  %s\n\n"+
			"An undefined custom property makes the declaration INVALID, and an invalid\n"+
			"property falls back to its initial value. For `stroke` that initial value is\n"+
			"`none` — so the line is not drawn, nothing errors, and the chart just looks\n"+
			"empty. Define the token in style.css, or use one that exists.",
			strings.Join(missing, "\n  "))
	}
}

func slicesHasPrefix(prefixes []string, s string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}
