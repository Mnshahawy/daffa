package stacks

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/compose-spec/compose-go/v2/loader"
	"github.com/compose-spec/compose-go/v2/types"
)

// Service is what a compose file declares, reduced to what Daffa shows and compares.
type Service struct {
	Name  string `json:"name"`
	Image string `json:"image"`
	// Hook marks a service named in x-daffa.hooks: declared in the file, run AROUND
	// deploys, never deployed. Without the mark, the status page reports every hook as a
	// "missing" service and the stack as partially running — the healthy state, read as
	// an outage.
	Hook bool `json:"hook,omitempty"`
}

// ComposeSecret is one top-level `secrets:` declaration, reduced to what a deploy needs to
// check: the secret's name and the file it is sourced from. Daffa fills the files it owns
// (those under daffa-secrets/) from the sealed store; see docs/secrets.md.
type ComposeSecret struct {
	Name string
	File string // the file: source, verbatim; "" for an environment/external source
}

// Parse validates a compose file and returns its services.
//
// Validating BEFORE shipping a bundle to a runner means a typo produces an error in the
// browser, immediately, rather than a failed container on a remote host thirty seconds
// later whose logs someone has to go and read.
func Parse(ctx context.Context, yaml, projectName string, env []EnvVar) ([]Service, error) {
	project, err := loadProject(ctx, yaml, projectName, env)
	if err != nil {
		return nil, err
	}

	hooks := hookServiceNames(project)
	out := make([]Service, 0, len(project.Services))
	for name, svc := range project.Services {
		out = append(out, Service{Name: name, Image: svc.Image, Hook: hooks[name]})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// hookServiceNames reads which services x-daffa.hooks claims, for marking. Malformed
// hook declarations yield nothing here rather than an error — Parse's callers want the
// service list, and the deploy path (PlanHooks) is where a broken x-daffa block is
// refused with its real message.
func hookServiceNames(project *types.Project) map[string]bool {
	ext, ok := project.Extensions["x-daffa"]
	if !ok {
		return nil
	}
	hooks, err := parseHooksExt(ext)
	if err != nil || hooks == nil {
		return nil
	}
	return hooks.serviceSet()
}

// SecretsFromCompose returns the compose file's top-level `secrets:` declarations, so a
// deploy can cross-check that every daffa-secrets/ file the stack references has sealed
// material behind it. It shares Parse's loader options.
func SecretsFromCompose(ctx context.Context, yaml, projectName string, env []EnvVar) ([]ComposeSecret, error) {
	project, err := loadProject(ctx, yaml, projectName, env)
	if err != nil {
		return nil, err
	}

	out := make([]ComposeSecret, 0, len(project.Secrets))
	for name, sec := range project.Secrets {
		out = append(out, ComposeSecret{Name: name, File: sec.File})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func loadProject(ctx context.Context, yaml, projectName string, env []EnvVar) (*types.Project, error) {
	envMap := map[string]string{}
	for _, v := range env {
		envMap[v.Key] = v.Value
	}

	project, err := loader.LoadWithContext(ctx, types.ConfigDetails{
		ConfigFiles: []types.ConfigFile{{Filename: composePath, Content: []byte(yaml)}},
		Environment: envMap,
	}, func(o *loader.Options) {
		o.SetProjectName(projectName, true)
		o.SkipConsistencyCheck = false
		// Do NOT resolve paths or interpolate against the SERVER's environment: the
		// compose file is going to run on another machine, and inheriting Daffa's own
		// env would be both wrong and a leak.
		o.SkipResolveEnvironment = true
	})
	if err != nil {
		return nil, fmt.Errorf("stacks: %w", composeError(err))
	}
	return project, nil
}

// composeError strips the noise compose-go wraps around a validation failure, which is
// otherwise several lines of internal path before the one sentence that matters.
func composeError(err error) error {
	msg := err.Error()
	if i := strings.Index(msg, ": "); i > 0 && strings.HasPrefix(msg, "validating ") {
		msg = msg[i+2:]
	}
	return fmt.Errorf("%s", msg)
}
