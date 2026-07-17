package stacks

import (
	"context"
	"strings"
	"testing"
	"time"
)

const hookedYAML = `
services:
  app:
    image: acme/app:${TAG}
    environment:
      DB_PASSWORD: ${DB_PASSWORD}
    networks: [backend]
  db:
    image: postgres:17
    networks: [backend]
    volumes:
      - data:/var/lib/postgresql/data
  migrate:
    image: acme/app:${TAG}
    command: ["./migrate", "up"]
    environment:
      DB_PASSWORD: ${DB_PASSWORD}
    networks: [backend]
    depends_on: [db]
  smoke:
    image: curlimages/curl:8
    command: ["curl", "-fsS", "http://app:8080/health"]

networks:
  backend:
    driver: overlay
    attachable: true

volumes:
  data:

x-daffa:
  hooks:
    pre_deploy: [migrate]
    post_deploy: [smoke]
    rollback_on_failure: true
    timeout: 5m
`

var hookEnv = []EnvVar{{Key: "TAG", Value: "v2"}, {Key: "DB_PASSWORD", Value: "hunter2"}}

func TestPlanHooksSplitsTheFile(t *testing.T) {
	plan, err := PlanHooks(context.Background(), hookedYAML, "shop", hookEnv, false, false)
	if err != nil {
		t.Fatalf("PlanHooks: %v", err)
	}
	if plan.Hooks == nil {
		t.Fatal("no hooks parsed")
	}
	h := plan.Hooks
	if len(h.PreDeploy) != 1 || h.PreDeploy[0].Service != "migrate" ||
		len(h.PostDeploy) != 1 || h.PostDeploy[0].Service != "smoke" {
		t.Fatalf("hooks: %+v", h)
	}
	if !h.PreDeploy[0].Blocking() {
		t.Error("a bare-string hook must default to blocking")
	}
	if !h.RollbackOnFailure || h.Timeout != 5*time.Minute {
		t.Fatalf("options: %+v", h)
	}

	// The deploy file: hooks gone, app and db intact, ${VAR} NOT interpolated — the
	// deploy YAML is what a deployment row stores, and a baked secret there is forbidden.
	if strings.Contains(plan.DeployYAML, "migrate") || strings.Contains(plan.DeployYAML, "smoke") {
		t.Errorf("hook services leaked into the deploy file:\n%s", plan.DeployYAML)
	}
	if strings.Contains(plan.DeployYAML, "hunter2") || !strings.Contains(plan.DeployYAML, "${DB_PASSWORD}") {
		t.Errorf("splitting interpolated a secret into the deploy file:\n%s", plan.DeployYAML)
	}
	// And it must still be a valid compose file the engine can apply.
	svcs, err := Parse(context.Background(), plan.DeployYAML, "shop", hookEnv)
	if err != nil {
		t.Fatalf("the split deploy file does not load: %v", err)
	}
	if len(svcs) != 2 {
		t.Errorf("deploy file has %d services; want app and db", len(svcs))
	}

	// The hooks file: only hooks, secrets unresolved, depends_on pruned, and every
	// project resource externalized under its deployed name.
	if strings.Contains(plan.HooksYAML, "hunter2") {
		t.Errorf("splitting interpolated a secret into the hooks file:\n%s", plan.HooksYAML)
	}
	if strings.Contains(plan.HooksYAML, "depends_on") {
		t.Errorf("depends_on survived into the hooks file, where its target does not exist:\n%s", plan.HooksYAML)
	}
	for _, want := range []string{"shop_backend", "external: true", "shop_default"} {
		if !strings.Contains(plan.HooksYAML, want) {
			t.Errorf("hooks file lacks %q:\n%s", want, plan.HooksYAML)
		}
	}
	// It must load standalone: this is the file `compose run` is handed.
	if _, err := Parse(context.Background(), plan.HooksYAML, "shop", hookEnv); err != nil {
		t.Fatalf("the hooks file does not load on its own: %v", err)
	}
}

func TestHookPriorityOrdersTheList(t *testing.T) {
	yaml := `
services:
  app: {image: a}
  seed: {image: a}
  migrate: {image: a}
  index: {image: a}
  warm: {image: a}
x-daffa:
  hooks:
    pre_deploy:
      - service: seed
        priority: 20
      - service: migrate
        priority: -10
      - index                     # bare string: priority 0
      - service: warm
        priority: 0               # ties with index — declared order breaks the tie
        on_failure: continue
`
	plan, err := PlanHooks(context.Background(), yaml, "t", nil, false, false)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, h := range plan.Hooks.PreDeploy {
		got = append(got, h.Service)
	}
	// Ascending priority; index before warm because equal priorities keep file order.
	want := "migrate index warm seed"
	if strings.Join(got, " ") != want {
		t.Fatalf("run order %q, want %q", strings.Join(got, " "), want)
	}
	if plan.Hooks.PreDeploy[2].Blocking() {
		t.Error("on_failure: continue did not parse")
	}
}

func TestPlanHooksWithoutXDaffaIsByteIdentical(t *testing.T) {
	yaml := "services:\n  app:\n    image: nginx:1.27\n"
	plan, err := PlanHooks(context.Background(), yaml, "plain", nil, true, false)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Hooks != nil || plan.DeployYAML != yaml || plan.HooksYAML != "" {
		t.Fatalf("a hook-free stack must pass through untouched: %+v", plan)
	}
}

func TestPlanHooksValidation(t *testing.T) {
	cases := []struct {
		name  string
		yaml  string
		swarm bool
		want  string // substring of the error
	}{
		{
			name: "unknown hook key is a typo, not a shrug",
			yaml: `
services:
  app: {image: a}
  m: {image: a}
x-daffa:
  hooks:
    post_deplyo: [m]
`,
			want: "post_deplyo",
		},
		{
			name: "hook must be a service",
			yaml: `
services:
  app: {image: a}
x-daffa:
  hooks:
    pre_deploy: [migrate]
`,
			want: `"migrate" is not a service`,
		},
		{
			name: "a stack of only hooks deploys nothing",
			yaml: `
services:
  m: {image: a}
x-daffa:
  hooks:
    pre_deploy: [m]
`,
			want: "nothing left to deploy",
		},
		{
			name: "deployed service must not depend on a hook",
			yaml: `
services:
  app: {image: a, depends_on: [m]}
  m: {image: a}
x-daffa:
  hooks:
    pre_deploy: [m]
`,
			want: "depends_on",
		},
		{
			name: "rollback flag without post hooks can never fire",
			yaml: `
services:
  app: {image: a}
  m: {image: a}
x-daffa:
  hooks:
    pre_deploy: [m]
    rollback_on_failure: true
`,
			want: "rollback_on_failure",
		},
		{
			name: "rollback flag with only best-effort post hooks can never fire either",
			yaml: `
services:
  app: {image: a}
  m: {image: a}
x-daffa:
  hooks:
    post_deploy:
      - service: m
        on_failure: continue
    rollback_on_failure: true
`,
			want: "rollback_on_failure",
		},
		{
			name: "on_failure takes only fail or continue",
			yaml: `
services:
  app: {image: a}
  m: {image: a}
x-daffa:
  hooks:
    pre_deploy:
      - service: m
        on_failure: shrug
`,
			want: "on_failure",
		},
		{
			name: "unknown hook-entry key is refused",
			yaml: `
services:
  app: {image: a}
  m: {image: a}
x-daffa:
  hooks:
    pre_deploy:
      - service: m
        prioriti: 3
`,
			want: "prioriti",
		},
		{
			name:  "swarm demands attachable networks",
			swarm: true,
			yaml: `
services:
  app: {image: a, networks: [backend]}
  m: {image: a, networks: [backend]}
networks:
  backend:
    driver: overlay
x-daffa:
  hooks:
    pre_deploy: [m]
`,
			want: "attachable",
		},
		{
			name: "hooks cannot mount swarm secrets",
			yaml: `
services:
  app: {image: a}
  m:
    image: a
    secrets: [tls_key]
secrets:
  tls_key:
    external: true
x-daffa:
  hooks:
    pre_deploy: [m]
`,
			want: "secrets",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := PlanHooks(context.Background(), c.yaml, "t", nil, c.swarm, false)
			if err == nil {
				t.Fatalf("want an error containing %q, got nil", c.want)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("error %q does not mention %q", err, c.want)
			}
		})
	}
}

// The status page builds on Parse, so Parse must know which services are hooks — a hook
// reported as a "missing" service reads a healthy stack as partially running.
func TestParseMarksHookServices(t *testing.T) {
	svcs, err := Parse(context.Background(), hookedYAML, "shop", hookEnv)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"app": false, "db": false, "migrate": true, "smoke": true}
	for _, s := range svcs {
		if s.Hook != want[s.Name] {
			t.Errorf("service %q: hook=%v, want %v", s.Name, s.Hook, want[s.Name])
		}
	}
}

// A compose first deploy: the hooks file carries the ORIGINAL resource definitions, so
// the hook's own `compose run` creates them — with authentic compose labels the engine
// then adopts. Verified against a live daemon: stack deploy does NOT adopt a
// compose-created network, which is why this mode is compose-only.
func TestPlanHooksFirstDeployOnCompose(t *testing.T) {
	plan, err := PlanHooks(context.Background(), hookedYAML, "shop", hookEnv, false, true)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Provision != nil {
		t.Fatal("compose first deploys provision through the hooks file, never through the API")
	}
	if strings.Contains(plan.HooksYAML, "external") {
		t.Errorf("a compose first-deploy hooks file must not externalize anything:\n%s", plan.HooksYAML)
	}
	// The original definition came through verbatim, not a stub.
	for _, want := range []string{"driver: overlay", "attachable: true"} {
		if !strings.Contains(plan.HooksYAML, want) {
			t.Errorf("hooks file lost the original network definition (%q):\n%s", want, plan.HooksYAML)
		}
	}
	// Secrets stay unresolved even in this mode — the tree copy, not the parsed project.
	if strings.Contains(plan.HooksYAML, "hunter2") {
		t.Errorf("first-deploy mode interpolated a secret:\n%s", plan.HooksYAML)
	}
	if _, err := Parse(context.Background(), plan.HooksYAML, "shop", hookEnv); err != nil {
		t.Fatalf("the first-deploy hooks file does not load on its own: %v", err)
	}
	// The deploy file and hash are untouched by the mode — drift detection must not be
	// able to tell a first deploy from any other.
	later, err := PlanHooks(context.Background(), hookedYAML, "shop", hookEnv, false, false)
	if err != nil {
		t.Fatal(err)
	}
	if plan.DeployYAML != later.DeployYAML {
		t.Error("firstDeploy changed the deploy file")
	}
}

// A swarm first deploy: the hooks file keeps its external stubs (they will exist — Daffa
// creates them) and Provision lists what to create, namespace-labelled so `stack deploy`
// adopts instead of colliding. Both behaviours verified against a live daemon.
func TestPlanHooksFirstDeployOnSwarm(t *testing.T) {
	// A swarm-legal fixture: every hook network attachable, and the migrate hook mounts
	// a named volume so volume provisioning is exercised too.
	yaml := `
services:
  app:
    image: acme/app:v2
    networks: [backend]
  db:
    image: postgres:17
    networks: [backend]
    volumes: ["data:/var/lib/postgresql/data"]
  migrate:
    image: acme/app:v2
    command: ["./migrate", "up"]
    networks: [backend]
    volumes: ["cache:/cache"]
  smoke:
    image: curlimages/curl:8
    networks: [backend]

networks:
  backend:
    driver: overlay
    attachable: true

volumes:
  data:
  cache:

x-daffa:
  hooks:
    pre_deploy: [migrate]
    post_deploy: [smoke]
`
	plan, err := PlanHooks(context.Background(), yaml, "shop", nil, true, true)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(plan.HooksYAML, "external: true") {
		t.Errorf("a swarm hooks file must stay external in every mode:\n%s", plan.HooksYAML)
	}
	prov := plan.Provision
	if prov == nil {
		t.Fatal("a swarm first deploy must carry a provision list")
	}
	if len(prov.Networks) != 1 || prov.Networks[0].Name != "shop_backend" {
		t.Fatalf("provisioned networks %+v; want exactly shop_backend", prov.Networks)
	}
	n := prov.Networks[0]
	if n.Labels["com.docker.stack.namespace"] != "shop" {
		t.Error("network lacks the namespace label — stack deploy would collide instead of adopting")
	}
	if !n.Attachable {
		t.Error("backend lost attachable in translation — the hook could not join it")
	}
	// cache is a hook's mount and gets provisioned; data belongs to db, which is not a
	// hook, and must not be.
	if len(prov.Volumes) != 1 || prov.Volumes[0].Name != "shop_cache" {
		t.Fatalf("provisioned volumes %+v; want exactly shop_cache", prov.Volumes)
	}
	if prov.Volumes[0].Labels["com.docker.stack.namespace"] != "shop" {
		t.Error("volume lacks the namespace label")
	}

	// A later deploy provisions nothing.
	later, err := PlanHooks(context.Background(), yaml, "shop", nil, true, false)
	if err != nil {
		t.Fatal(err)
	}
	if later.Provision != nil {
		t.Error("a non-first deploy must not provision")
	}
}

func TestBundleHashIgnoresTheSplit(t *testing.T) {
	// The hash is the deployment's identity. It must be a function of the SOURCE, not of
	// Daffa's derivation — or the first deploy after a Daffa upgrade that changes the
	// split would tell every hooked stack "the source has changed" when it has not.
	plan, err := PlanHooks(context.Background(), hookedYAML, "shop", hookEnv, false, false)
	if err != nil {
		t.Fatal(err)
	}
	planned, err := BuildPlanned(hookedYAML, plan, hookEnv, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	plain, err := Build(hookedYAML, hookEnv, nil)
	if err != nil {
		t.Fatal(err)
	}
	if planned.Hash != plain.Hash {
		t.Errorf("the split changed the bundle hash: %s vs %s", planned.Hash, plain.Hash)
	}
	if planned.YAML != hookedYAML {
		t.Error("Bundle.YAML must stay the file the user wrote")
	}
}

func TestHookCommand(t *testing.T) {
	cmd := strings.Join(HookCommand("shop", "migrate"), " ")
	for _, want := range []string{"compose", "-p shop", "hooks.yml", "run", "--rm", "--no-deps", "migrate"} {
		if !strings.Contains(cmd, want) {
			t.Errorf("hook command %q lacks %q", cmd, want)
		}
	}
}
