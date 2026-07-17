package stacks

import (
	"context"
	"strings"
	"testing"

	"github.com/compose-spec/compose-go/v2/loader"
	"github.com/compose-spec/compose-go/v2/types"
)

// reloadServices parses an emitted compose file the way the engine would and hands back
// the services — assertions go through compose-go rather than string-matching YAML, so a
// cosmetic re-marshalling difference can never fail a test.
func reloadServices(t *testing.T, yamlText, project string, env []EnvVar) types.Services {
	t.Helper()
	envMap := map[string]string{}
	for _, v := range env {
		envMap[v.Key] = v.Value
	}
	p, err := loader.LoadWithContext(context.Background(), types.ConfigDetails{
		ConfigFiles: []types.ConfigFile{{Filename: composePath, Content: []byte(yamlText)}},
		Environment: envMap,
	}, func(o *loader.Options) {
		o.SetProjectName(project, true)
		o.SkipResolveEnvironment = true
	})
	if err != nil {
		t.Fatalf("re-parsing the injected file: %v", err)
	}
	return p.Services
}

var rotate = &LogConfig{Driver: "json-file", Opts: map[string]string{"max-size": "10m", "max-file": "3"}}

func TestInjectLoggingFillsBareServices(t *testing.T) {
	yamlText := `
services:
  app:
    image: acme/app:${TAG}
  db:
    image: postgres:17
    logging:
      driver: local
      options:
        max-size: 50m
`
	out, err := InjectLogging(context.Background(), yamlText, "shop", hookEnv, rotate)
	if err != nil {
		t.Fatalf("InjectLogging: %v", err)
	}
	svcs := reloadServices(t, out, "shop", hookEnv)

	app := svcs["app"].Logging
	if app == nil || app.Driver != "json-file" {
		t.Fatalf("app got no injected logging: %+v", app)
	}
	if app.Options["max-size"] != "10m" || app.Options["max-file"] != "3" {
		t.Errorf("app's options = %v; want the host defaults, as strings", app.Options)
	}

	// A service's own logging wins — this is a default, not an enforcement.
	db := svcs["db"].Logging
	if db == nil || db.Driver != "local" || db.Options["max-size"] != "50m" {
		t.Errorf("db's explicit logging was not preserved: %+v", db)
	}
}

func TestInjectLoggingRespectsAnchorsAndAliases(t *testing.T) {
	yamlText := `
x-defaults: &defaults
  logging:
    driver: syslog

services:
  inherits:
    image: acme/app
    <<: *defaults
  aliased: &tmpl
    image: acme/worker
  twin: *tmpl
  bare:
    image: acme/api
`
	out, err := InjectLogging(context.Background(), yamlText, "shop", nil, rotate)
	if err != nil {
		t.Fatalf("InjectLogging: %v", err)
	}
	svcs := reloadServices(t, out, "shop", nil)

	// Inherited via <<: counts as declared — appending an explicit key would OVERRIDE the
	// merge, the wrong direction.
	if l := svcs["inherits"].Logging; l == nil || l.Driver != "syslog" {
		t.Errorf("the anchor-inherited logging was overridden: %+v", l)
	}
	// An alias shares its anchor's node; whatever the pair ends up with, it must be the
	// SAME thing for both, and the file must still parse.
	al, tw := svcs["aliased"].Logging, svcs["twin"].Logging
	if (al == nil) != (tw == nil) {
		t.Errorf("alias and anchor diverged: aliased=%+v twin=%+v", al, tw)
	}
	if l := svcs["bare"].Logging; l == nil || l.Driver != "json-file" {
		t.Errorf("the bare service got no injected logging: %+v", l)
	}
}

func TestInjectLoggingKeepsVariablesUnresolved(t *testing.T) {
	yamlText := `
services:
  app:
    image: acme/app:${TAG}
    environment:
      DB_PASSWORD: ${DB_PASSWORD}
`
	out, err := InjectLogging(context.Background(), yamlText, "shop", hookEnv, rotate)
	if err != nil {
		t.Fatalf("InjectLogging: %v", err)
	}
	if !strings.Contains(out, "${TAG}") || !strings.Contains(out, "${DB_PASSWORD}") {
		t.Fatalf("a ${VAR} was resolved into the emitted file:\n%s", out)
	}
	if strings.Contains(out, "hunter2") {
		t.Fatal("a secret value was baked into the emitted file")
	}
}

func TestInjectLoggingNoConfigIsANoOp(t *testing.T) {
	yamlText := "services:\n  app:\n    image: acme/app\n"
	for _, cfg := range []*LogConfig{nil, {Driver: ""}} {
		out, err := InjectLogging(context.Background(), yamlText, "shop", nil, cfg)
		if err != nil {
			t.Fatalf("InjectLogging(%+v): %v", cfg, err)
		}
		if out != yamlText {
			t.Errorf("InjectLogging(%+v) changed the file", cfg)
		}
	}
}

func TestInjectLoggingWithoutOptions(t *testing.T) {
	yamlText := "services:\n  app:\n    image: acme/app\n"
	out, err := InjectLogging(context.Background(), yamlText, "shop", nil, &LogConfig{Driver: "journald"})
	if err != nil {
		t.Fatalf("InjectLogging: %v", err)
	}
	if l := reloadServices(t, out, "shop", nil)["app"].Logging; l == nil || l.Driver != "journald" || len(l.Options) != 0 {
		t.Errorf("opts-less injection = %+v; want just the driver, no empty options map", l)
	}
	if strings.Contains(out, "options") {
		t.Errorf("an empty options: key was emitted:\n%s", out)
	}
}

func TestInjectLoggingComposesWithTheHookSplit(t *testing.T) {
	plan, err := PlanHooks(context.Background(), hookedYAML, "shop", hookEnv, false, false)
	if err != nil {
		t.Fatalf("PlanHooks: %v", err)
	}
	hooksBefore := plan.HooksYAML

	out, err := InjectLogging(context.Background(), plan.DeployYAML, "shop", hookEnv, rotate)
	if err != nil {
		t.Fatalf("InjectLogging on the split deploy file: %v", err)
	}
	svcs := reloadServices(t, out, "shop", hookEnv)

	// The hook services left with the split; injection must not resurrect them.
	for _, hook := range []string{"migrate", "smoke"} {
		if _, ok := svcs[hook]; ok {
			t.Errorf("hook service %q reappeared in the deploy file", hook)
		}
	}
	for _, name := range []string{"app", "db"} {
		if l := svcs[name].Logging; l == nil || l.Driver != "json-file" {
			t.Errorf("%s got no injected logging after the hook split: %+v", name, l)
		}
	}
	// hooks.yml is one-shot `compose run --rm` containers — no retention problem, no edit.
	if plan.HooksYAML != hooksBefore {
		t.Error("injection touched the hooks file")
	}
}
