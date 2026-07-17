package cli

import (
	"bufio"
	"crypto/tls"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"syscall"

	"golang.org/x/term"
)

func transport(insecure bool) http.RoundTripper {
	if !insecure {
		return http.DefaultTransport
	}
	// For a Daffa behind an internal CA. Unlike the agent, the CLI is driven by a human
	// who can see what they are connecting to, and it holds no long-lived credential to
	// leak — so trust-on-first-use pinning would be ceremony without much benefit here.
	return &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}
}

func urlEscape(s string) string { return url.QueryEscape(s) }

// promptPassword reads without echo from a terminal, or a line from a pipe — so this
// works both for a person and for a script.
func promptPassword(prompt string) (string, error) {
	if !term.IsTerminal(int(syscall.Stdin)) {
		line, err := bufio.NewReader(os.Stdin).ReadString('\n')
		if err != nil && line == "" {
			return "", fmt.Errorf("reading the password: %w", err)
		}
		return strings.TrimRight(line, "\r\n"), nil
	}

	fmt.Fprint(os.Stderr, prompt)
	b, err := term.ReadPassword(int(syscall.Stdin))
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return "", fmt.Errorf("reading the password: %w", err)
	}
	return string(b), nil
}
