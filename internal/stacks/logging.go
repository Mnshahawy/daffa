package stacks

import (
	"context"
	"fmt"
	"maps"
	"slices"

	"github.com/compose-spec/compose-go/v2/loader"
	"github.com/compose-spec/compose-go/v2/types"
	yaml "go.yaml.in/yaml/v3"
)

// A host's default container logging, injected into stack deploys. See docs/stacks.md —
// Daffa cannot reach a host's daemon.json, so the per-host default log driver has to
// travel inside the compose file, and this is where it boards.

// LogConfig is the effective host default a deploy injects. It is stacks' own type, not
// store's: this package deploys, it does not read databases.
type LogConfig struct {
	Driver string
	Opts   map[string]string
}

// InjectLogging returns deployYAML with cfg merged as a `logging:` block into every
// service that does not already declare one. A service with its own logging keeps it —
// the host config is a default, not an enforcement.
//
// Detection goes through compose-go, so a logging block a service inherits via an anchor
// or a `<<:` merge key counts as declared; a raw tree check would append an explicit key
// that OVERRIDES the merge, which is exactly the wrong direction. The edit itself happens
// on the yaml tree — the splitHooks rationale: compose-go interpolates ${VAR} while
// loading, and re-marshalling the parsed project would bake every secret into the file.
// Alias service nodes are left alone: an alias shares its anchor's node, and editing one
// would edit every other user of it.
//
// It runs on plan.DeployYAML, never the original file. The bundle hash and Bundle.YAML
// are computed over the original, so a changed host config never reads as source drift —
// it simply applies at the next deploy.
func InjectLogging(ctx context.Context, deployYAML, projectName string, env []EnvVar, cfg *LogConfig) (string, error) {
	if cfg == nil || cfg.Driver == "" {
		return deployYAML, nil
	}

	envMap := map[string]string{}
	for _, v := range env {
		envMap[v.Key] = v.Value
	}
	project, err := loader.LoadWithContext(ctx, types.ConfigDetails{
		ConfigFiles: []types.ConfigFile{{Filename: composePath, Content: []byte(deployYAML)}},
		Environment: envMap,
	}, func(o *loader.Options) {
		o.SetProjectName(projectName, true)
		o.SkipResolveEnvironment = true
	})
	if err != nil {
		return "", fmt.Errorf("stacks: %w", composeError(err))
	}
	declared := map[string]bool{}
	for name, svc := range project.Services {
		declared[name] = svc.Logging != nil
	}

	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(deployYAML), &doc); err != nil || len(doc.Content) == 0 {
		return "", fmt.Errorf("stacks: re-reading the compose file for logging injection: %w", err)
	}
	root := doc.Content[0]
	servicesNode := mapValue(root, "services")
	if servicesNode == nil || servicesNode.Kind != yaml.MappingNode {
		return "", fmt.Errorf("stacks: the compose file has no services mapping")
	}

	for i := 0; i+1 < len(servicesNode.Content); i += 2 {
		k, v := servicesNode.Content[i], servicesNode.Content[i+1]
		if declared[k.Value] || v.Kind != yaml.MappingNode {
			continue
		}
		v.Content = append(v.Content, scalar("logging"), loggingNode(cfg))
	}

	out, err := yaml.Marshal(&doc)
	if err != nil {
		return "", fmt.Errorf("stacks: rendering the compose file with logging defaults: %w", err)
	}
	return string(out), nil
}

func loggingNode(cfg *LogConfig) *yaml.Node {
	n := &yaml.Node{Kind: yaml.MappingNode,
		Content: []*yaml.Node{scalar("driver"), strScalar(cfg.Driver)}}
	if len(cfg.Opts) > 0 {
		opts := &yaml.Node{Kind: yaml.MappingNode}
		for _, k := range slices.Sorted(maps.Keys(cfg.Opts)) { // deterministic output
			opts.Content = append(opts.Content, strScalar(k), strScalar(cfg.Opts[k]))
		}
		n.Content = append(n.Content, scalar("options"), opts)
	}
	return n
}

// strScalar forces !!str. Docker requires option values to be strings — a bare
// `max-file: 3` int fails `docker stack deploy` schema validation — and a value that can
// only ever be a string scalar is also a value that cannot smuggle YAML structure in.
func strScalar(s string) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: s}
}
