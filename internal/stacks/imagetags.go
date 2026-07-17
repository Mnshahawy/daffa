package stacks

import (
	"fmt"

	yaml "go.yaml.in/yaml/v3"
)

// RewriteImageTags returns the compose YAML with each `image:` value that exactly equals a key in
// changes replaced by its mapped value. It edits the YAML tree rather than re-marshalling a parsed
// project — the same reasoning as InjectLogging: compose-go interpolates ${VAR} while loading, so
// re-rendering a parsed project would bake secrets and expansions into the file. Working on the
// node tree preserves comments, quoting and layout, and only touches the scalars asked for.
//
// The match is exact-value: `postgres:16` is replaced, `postgres:16.2` is not — no substring trap.
// Every service sharing an image is updated, because every matching scalar in the tree is.
func RewriteImageTags(yamlText string, changes map[string]string) (string, error) {
	if len(changes) == 0 {
		return yamlText, nil
	}

	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(yamlText), &doc); err != nil {
		return "", fmt.Errorf("stacks: %w", composeError(err))
	}
	replaceImageScalars(&doc, changes)

	out, err := yaml.Marshal(&doc)
	if err != nil {
		return "", fmt.Errorf("stacks: rendering the compose file with new image tags: %w", err)
	}
	return string(out), nil
}

// replaceImageScalars walks the tree and rewrites the scalar value of any `image:` key found in
// changes. It recurses through every mapping and sequence, so it does not care where images sit,
// and it leaves everything else — including aliases, which it does not descend into — untouched.
func replaceImageScalars(n *yaml.Node, changes map[string]string) {
	if n.Kind == yaml.MappingNode {
		for i := 0; i+1 < len(n.Content); i += 2 {
			k, v := n.Content[i], n.Content[i+1]
			if k.Value == "image" && v.Kind == yaml.ScalarNode {
				if repl, ok := changes[v.Value]; ok {
					v.Value = repl // Tag/Style left as-is, so quoting and form survive
				}
			}
			replaceImageScalars(v, changes)
		}
		return
	}
	for _, c := range n.Content {
		replaceImageScalars(c, changes)
	}
}
