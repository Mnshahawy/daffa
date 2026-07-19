// Command daffa-agent is a SPIKE: an agent-only build of Daffa, built to measure the
// binary/image footprint of shipping the agent as its own artifact rather than as a
// mode of the full binary.
//
// It imports only internal/agent (→ internal/tunnel → websocket/yamux) and nothing of
// the server (internal/api, internal/store, internal/web, internal/dockerx) or the SPA
// embed — which is precisely the surface a dedicated agent image would shed. The flag
// set mirrors `daffa agent` in cmd/daffa/main.go so the two are functionally equivalent.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/Mnshahawy/daffa/internal/agent"
)

// version is stamped at build time (-ldflags "-X main.version=…"), same as cmd/daffa.
var version = "dev"

func main() {
	fs := flag.NewFlagSet("daffa-agent", flag.ExitOnError)
	server := fs.String("server", os.Getenv("DAFFA_SERVER"), "Daffa server base URL, e.g. https://ops.example.com")
	token := fs.String("token", os.Getenv("DAFFA_JOIN_TOKEN"), "one-time join token (first run only)")
	dockerHost := fs.String("docker-host", envOr("DAFFA_DOCKER_HOST", "unix:///var/run/docker.sock"), "local Docker endpoint to proxy")
	stateFile := fs.String("state", envOr("DAFFA_AGENT_STATE", "/var/lib/daffa/agent.json"), "where to persist this agent's identity")
	insecure := fs.Bool("insecure", os.Getenv("DAFFA_INSECURE") != "", "accept the server's certificate without a trusted CA (pinned on first use)")
	if err := fs.Parse(os.Args[1:]); err != nil {
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := agent.Run(ctx, agent.Config{
		Server:     *server,
		JoinToken:  *token,
		DockerHost: *dockerHost,
		StateFile:  *stateFile,
		Version:    version,
		Insecure:   *insecure,
	}); err != nil {
		fmt.Fprintln(os.Stderr, "daffa-agent:", err)
		os.Exit(1)
	}
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
