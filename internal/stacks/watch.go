package stacks

import (
	"path"
	"regexp"
	"strings"
)

// Watch decides whether a push is one this stack cares about.
//
// The default matters more than the matcher. A stack with nothing configured watches its
// own compose file and nothing else — so enabling auto-deploy without thinking about it
// gives you the behaviour you would have guessed, rather than a redeploy on every README
// typo.

// WatchPatterns returns the globs a stack watches: what it was configured with, or its
// compose file if it was configured with nothing.
func WatchPatterns(watchPaths, composePath string) []string {
	var out []string
	for _, line := range strings.FieldsFunc(watchPaths, func(r rune) bool {
		return r == '\n' || r == '\r' || r == ','
	}) {
		if p := strings.TrimSpace(line); p != "" {
			out = append(out, p)
		}
	}
	if len(out) > 0 {
		return out
	}

	if composePath == "" {
		composePath = "docker-compose.yml"
	}
	return []string{composePath}
}

// Matches reports whether any changed file matches any pattern.
func Matches(changed, patterns []string) bool {
	for _, f := range changed {
		f = strings.TrimPrefix(path.Clean(f), "/")
		for _, p := range patterns {
			if matchGlob(p, f) {
				return true
			}
		}
	}
	return false
}

// matchGlob supports the two things people actually write:
//
//	compose/*.yml   — one path segment
//	config/**       — everything underneath
//
// path.Match alone will not do: its * does not cross separators (right) and it has no **
// at all (wrong, for this). So patterns are compiled to a regexp, where ** means "any
// characters" and * means "any characters except /".
func matchGlob(pattern, name string) bool {
	pattern = strings.TrimPrefix(strings.TrimSpace(pattern), "/")
	if pattern == "" {
		return false
	}

	// A bare directory ("config/") means everything inside it.
	if strings.HasSuffix(pattern, "/") {
		pattern += "**"
	}

	re, err := globRegexp(pattern)
	if err != nil {
		return false
	}
	return re.MatchString(name)
}

func globRegexp(pattern string) (*regexp.Regexp, error) {
	var b strings.Builder
	b.WriteString("^")

	for i := 0; i < len(pattern); i++ {
		c := pattern[i]
		switch c {
		case '*':
			if i+1 < len(pattern) && pattern[i+1] == '*' {
				b.WriteString(".*")
				i++
				// `**/` should also match zero directories, so that config/** matches
				// config/x AND foo/**/x matches foo/x.
				if i+1 < len(pattern) && pattern[i+1] == '/' {
					b.WriteString("/?")
					i++
				}
			} else {
				b.WriteString("[^/]*")
			}
		case '?':
			b.WriteString("[^/]")
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}

	b.WriteString("$")
	return regexp.Compile(b.String())
}
