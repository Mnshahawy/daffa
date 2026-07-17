package stacks

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/compose-spec/compose-go/v2/loader"
	"github.com/compose-spec/compose-go/v2/types"
	yaml "go.yaml.in/yaml/v3"
)

// Lifecycle hooks: one-shot containers that run BEFORE a deploy touches anything
// (migrations) or AFTER the engine has applied it (smoke tests, cache warms). See
// docs/hooks.md for the design, and for why this is Daffa's job rather than the
// orchestrator's — the short version is that `docker stack deploy` has no job concept and
// ignores depends_on entirely, and the deployer is the only thing positioned to sequence
// "migrate, then ship, then verify" for BOTH engines identically.
//
// A hook is an ordinary service in the compose file, marked as a hook in the top-level
// x-daffa extension:
//
//	services:
//	  app:     {image: acme/app:latest}
//	  migrate: {image: acme/app:latest, command: ["./migrate", "up"], networks: [backend]}
//	x-daffa:
//	  hooks:
//	    pre_deploy: [migrate]
//	    post_deploy: [smoke]
//	    rollback_on_failure: true
//	    timeout: 10m
//
// The file stays the single source of truth — hooks version with the code they migrate,
// and a rollback naturally carries the OLD deployment's hooks, because they are in the
// stored compose file like everything else.

// Hook is one entry in a hook list. In the file it is either a bare service name or a
// mapping with options:
//
//	pre_deploy:
//	  - migrate                    # shorthand: priority 0, on_failure fail
//	  - service: warm-cache
//	    priority: 10               # runs after migrate — ascending, like a run-order number
//	    on_failure: continue       # best-effort: its failure does not stop the pipeline
type Hook struct {
	Service string
	// Priority is the sort key WITHIN a list: ascending, ties keep declared order. It
	// exists for the file whose hooks accumulate over time — an explicit number survives
	// a reordering diff in a way that "the list happened to be in the right order" does
	// not. Bare-string entries are priority 0.
	Priority int
	// OnFailure is what a non-zero exit means: "fail" (default) stops the pipeline —
	// blocking the deploy from a pre hook, failing it (and possibly rolling back) from a
	// post hook. "continue" logs the failure and proceeds: for the cache warm whose
	// failure is worth knowing about and not worth blocking a release over.
	OnFailure string
}

const (
	OnFailureFail     = "fail"
	OnFailureContinue = "continue"
)

// Blocking reports whether this hook's failure stops the pipeline.
func (h Hook) Blocking() bool { return h.OnFailure != OnFailureContinue }

// Hooks is what x-daffa.hooks declares. Each list runs in priority order (ascending,
// ties in declared order).
type Hooks struct {
	PreDeploy  []Hook
	PostDeploy []Hook
	// RollbackOnFailure redeploys the last successful deployment when a BLOCKING
	// post_deploy hook fails. Pre-deploy failures never roll back: nothing was touched
	// yet. Non-blocking (on_failure: continue) failures never roll back either — a hook
	// whose failure was declared ignorable cannot also be a reason to undo a release.
	RollbackOnFailure bool
	// Timeout bounds EACH hook. A migration that has run for ten minutes is not late, it
	// is wedged — and a wedged hook would otherwise hold the stack's deploy claim for the
	// full deploy timeout.
	Timeout time.Duration
}

// HookPlan is the bundle-time derivation: what the engine applies, and what the hooks run
// from. The two are split because neither engine may ever see a hook service — compose
// `up` would start it alongside the app, and `stack deploy` would run it in a restart
// loop forever, which for a database migration is a disaster with a schedule.
type HookPlan struct {
	Hooks *Hooks // nil when the file declares none

	// DeployYAML is the compose file minus the hook services. Identical to the source
	// when there are no hooks, so stacks without x-daffa ship byte-identical bundles.
	DeployYAML string

	// HooksYAML is a self-contained compose file holding ONLY the hook services, with
	// every project resource they touch (networks, named volumes) redeclared as external
	// under its deployed name. `docker compose run` against this file therefore attaches
	// hook containers to the RUNNING stack's networks — including a swarm stack's overlay
	// networks — instead of creating a parallel world of its own.
	//
	// On a FIRST deploy those resources do not exist yet, and the two engines need
	// opposite treatments (both verified against live daemons):
	//
	//   * compose: the hook services keep the ORIGINAL network/volume declarations
	//     instead of external stubs. The hook's `compose run` — the same pinned compose
	//     binary the engine runs — creates them with authentic compose labels (including
	//     the config-hash Daffa could never forge), and the later `compose up` for the
	//     same project adopts them silently.
	//   * swarm: `stack deploy` does NOT adopt a compose-created network — it collides
	//     ("network … already exists"). It DOES adopt one carrying its own
	//     com.docker.stack.namespace label, and `stack rm` removes it as its own. So the
	//     hooks file keeps its external stubs and Provision below lists what Daffa must
	//     create through the API before the first hook runs.
	HooksYAML string

	// Provision is what Daffa must create on the daemon before a FIRST deploy's hooks
	// can run. Only ever set for swarm first deploys; nil otherwise.
	Provision *HookProvision
}

// HookProvision is the swarm first-deploy resource list: the networks and named volumes
// the hook services reference, translated from the compose file, each labelled with
// com.docker.stack.namespace so the engine adopts rather than collides. External
// resources are never in here — those are the operator's to have created.
type HookProvision struct {
	Networks []HookNetwork
	Volumes  []HookVolume
}

// HookNetwork carries enough of the compose network definition to create it exactly as
// `docker stack deploy` would have. Anything not translated here (a network config the
// engine would honour and this create call would not) silently becoming load-bearing is
// why the translation stays small and the fields explicit.
type HookNetwork struct {
	Name       string // the deployed name: <project>_<key>, or the explicit `name:`
	Driver     string // "" means overlay, stack deploy's default
	Attachable bool
	Internal   bool
	Options    map[string]string
	Labels     map[string]string // includes com.docker.stack.namespace
	// IPAM, translated so a declared subnet is not silently dropped — a network created
	// without it would be adopted by the engine and carry the wrong addressing forever.
	IpamDriver string
	Subnets    []HookSubnet
}

type HookSubnet struct {
	Subnet  string
	Gateway string
	IPRange string
}

// HookVolume mirrors HookNetwork for named volumes. Volumes are the easy half: nothing
// label-checks a volume at mount time, and swarm creates task volumes lazily anyway.
type HookVolume struct {
	Name    string
	Driver  string
	Options map[string]string
	Labels  map[string]string
}

const (
	hooksPath = "hooks.yml"

	// DefaultHookTimeout bounds a hook that declares none.
	DefaultHookTimeout = 10 * time.Minute
)

// PlanHooks reads x-daffa.hooks and derives the split. It is called at bundle time, on
// every deploy, so a rollback re-derives the plan from the OLD file it is restoring.
//
// swarm tightens validation: hook containers are plain containers even on a swarm, and a
// plain container can only join an overlay network that is `attachable: true` — which
// `docker stack deploy` does not make them by default. That failure would otherwise
// surface as a compose error deep in a runner log; here it is a browser error with the
// exact fix.
//
// firstDeploy switches the hooks file (and, on swarm, Provision) into the mode where the
// stack's networks and volumes do not exist yet — see the HooksYAML comment. It never
// changes DeployYAML, so the bundle hash (computed over the original file) and drift
// detection are untouched by it.
func PlanHooks(ctx context.Context, yamlText, projectName string, env []EnvVar, swarm, firstDeploy bool) (*HookPlan, error) {
	envMap := map[string]string{}
	for _, v := range env {
		envMap[v.Key] = v.Value
	}
	project, err := loader.LoadWithContext(ctx, types.ConfigDetails{
		ConfigFiles: []types.ConfigFile{{Filename: composePath, Content: []byte(yamlText)}},
		Environment: envMap,
	}, func(o *loader.Options) {
		o.SetProjectName(projectName, true)
		o.SkipResolveEnvironment = true
	})
	if err != nil {
		return nil, fmt.Errorf("stacks: %w", composeError(err))
	}

	ext, ok := project.Extensions["x-daffa"]
	if !ok {
		return &HookPlan{DeployYAML: yamlText}, nil
	}
	hooks, err := parseHooksExt(ext)
	if err != nil {
		return nil, err
	}
	if hooks == nil || (len(hooks.PreDeploy) == 0 && len(hooks.PostDeploy) == 0) {
		return &HookPlan{DeployYAML: yamlText}, nil
	}

	if err := validateHooks(project, hooks, swarm); err != nil {
		return nil, err
	}

	// Compose first deploys inline the original resource definitions into the hooks
	// file; every other combination uses external stubs.
	inlineDefs := firstDeploy && !swarm
	deployYAML, hooksYAML, err := splitHooks(yamlText, project, hooks, inlineDefs)
	if err != nil {
		return nil, err
	}
	plan := &HookPlan{Hooks: hooks, DeployYAML: deployYAML, HooksYAML: hooksYAML}
	if firstDeploy && swarm {
		plan.Provision = provisionList(project, hooks)
	}
	return plan, nil
}

// provisionList translates the hook-referenced, stack-owned networks and volumes into
// create calls for a swarm first deploy. The namespace label is the whole trick: with it,
// `docker stack deploy` treats the resource as its own and `stack rm` cleans it up;
// without it, the engine collides and the first deploy fails.
func provisionList(project *types.Project, hooks *Hooks) *HookProvision {
	hookSet := hooks.serviceSet()
	prov := &HookProvision{}
	nsLabel := map[string]string{"com.docker.stack.namespace": project.Name}

	for _, key := range hookNetworkKeys(project, hookSet) {
		net := project.Networks[key]
		if bool(net.External) {
			continue // the operator's; presumed to exist
		}
		n := HookNetwork{
			Name:       deployedName(project.Name, key, net.Name),
			Driver:     net.Driver, // "" = overlay, the engine's own default
			Attachable: net.Attachable,
			Internal:   net.Internal,
			Options:    net.DriverOpts,
			Labels:     mergeLabels(net.Labels, nsLabel),
			IpamDriver: net.Ipam.Driver,
		}
		for _, pool := range net.Ipam.Config {
			if pool == nil {
				continue
			}
			n.Subnets = append(n.Subnets, HookSubnet{
				Subnet: pool.Subnet, Gateway: pool.Gateway, IPRange: pool.IPRange,
			})
		}
		prov.Networks = append(prov.Networks, n)
	}

	for _, key := range hookVolumeKeys(project, hookSet) {
		vol := project.Volumes[key]
		if bool(vol.External) {
			continue
		}
		prov.Volumes = append(prov.Volumes, HookVolume{
			Name:    deployedName(project.Name, key, vol.Name),
			Driver:  vol.Driver,
			Options: vol.DriverOpts,
			Labels:  mergeLabels(vol.Labels, nsLabel),
		})
	}
	return prov
}

// deployedName is the name a project resource has once deployed: the explicit `name:`
// when the file sets one, `<project>_<key>` otherwise.
func deployedName(projectName, key, explicit string) string {
	if explicit != "" {
		return explicit
	}
	return projectName + "_" + key
}

func mergeLabels(from map[string]string, add map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range from {
		out[k] = v
	}
	for k, v := range add {
		out[k] = v
	}
	return out
}

// services named as hooks, deduplicated across both lists.
func (h *Hooks) serviceSet() map[string]bool {
	set := map[string]bool{}
	for _, hk := range append(append([]Hook{}, h.PreDeploy...), h.PostDeploy...) {
		set[hk.Service] = true
	}
	return set
}

// parseHooksExt decodes the x-daffa block. Unknown keys are ERRORS, not shrugs: a typo
// like `post_deplyo` that was silently ignored would mean a smoke test that never runs
// and a deploy pipeline that looks green because it is incomplete.
func parseHooksExt(ext any) (*Hooks, error) {
	root, ok := ext.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("stacks: x-daffa must be a mapping")
	}
	for k := range root {
		if k != "hooks" {
			return nil, fmt.Errorf("stacks: unknown x-daffa key %q (this Daffa understands: hooks)", k)
		}
	}
	raw, ok := root["hooks"]
	if !ok {
		return nil, nil
	}
	m, ok := raw.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("stacks: x-daffa.hooks must be a mapping")
	}

	h := &Hooks{Timeout: DefaultHookTimeout}
	for k, v := range m {
		switch k {
		case "pre_deploy":
			list, err := hookList(v, "x-daffa.hooks.pre_deploy")
			if err != nil {
				return nil, err
			}
			h.PreDeploy = list
		case "post_deploy":
			list, err := hookList(v, "x-daffa.hooks.post_deploy")
			if err != nil {
				return nil, err
			}
			h.PostDeploy = list
		case "rollback_on_failure":
			b, ok := v.(bool)
			if !ok {
				return nil, fmt.Errorf("stacks: x-daffa.hooks.rollback_on_failure must be true or false")
			}
			h.RollbackOnFailure = b
		case "timeout":
			s, ok := v.(string)
			if !ok {
				return nil, fmt.Errorf("stacks: x-daffa.hooks.timeout must be a duration string, e.g. \"10m\"")
			}
			d, err := time.ParseDuration(s)
			if err != nil || d <= 0 {
				return nil, fmt.Errorf("stacks: x-daffa.hooks.timeout: %q is not a duration (e.g. \"10m\", \"1h\")", s)
			}
			h.Timeout = d
		default:
			return nil, fmt.Errorf("stacks: unknown x-daffa.hooks key %q (this Daffa understands: pre_deploy, post_deploy, rollback_on_failure, timeout)", k)
		}
	}
	if h.RollbackOnFailure && !anyBlocking(h.PostDeploy) {
		return nil, fmt.Errorf("stacks: x-daffa.hooks.rollback_on_failure is set but no post_deploy hook can fail the deploy — every one is missing or marked on_failure: continue, so nothing could ever trigger the rollback")
	}
	return h, nil
}

func anyBlocking(hooks []Hook) bool {
	for _, h := range hooks {
		if h.Blocking() {
			return true
		}
	}
	return false
}

// hookList decodes one hook list: bare strings and option mappings, freely mixed, then
// sorted by priority — STABLY, so equal priorities keep the order the file declares.
func hookList(v any, where string) ([]Hook, error) {
	items, ok := v.([]any)
	if !ok {
		return nil, fmt.Errorf("stacks: %s must be a list of hooks (a service name, or a mapping with service/priority/on_failure)", where)
	}
	out := make([]Hook, 0, len(items))
	for _, it := range items {
		switch e := it.(type) {
		case string:
			if strings.TrimSpace(e) == "" {
				return nil, fmt.Errorf("stacks: %s contains an empty service name", where)
			}
			out = append(out, Hook{Service: e, OnFailure: OnFailureFail})

		case map[string]any:
			hk := Hook{OnFailure: OnFailureFail}
			for k, val := range e {
				switch k {
				case "service":
					s, ok := val.(string)
					if !ok || strings.TrimSpace(s) == "" {
						return nil, fmt.Errorf("stacks: %s: service must be a service name", where)
					}
					hk.Service = s
				case "priority":
					n, ok := val.(int)
					if !ok {
						return nil, fmt.Errorf("stacks: %s: priority must be an integer (lower runs first)", where)
					}
					hk.Priority = n
				case "on_failure":
					s, _ := val.(string)
					if s != OnFailureFail && s != OnFailureContinue {
						return nil, fmt.Errorf("stacks: %s: on_failure must be %q (stop the pipeline — the default) or %q (log it and proceed)", where, OnFailureFail, OnFailureContinue)
					}
					hk.OnFailure = s
				default:
					return nil, fmt.Errorf("stacks: unknown %s key %q (this Daffa understands: service, priority, on_failure)", where, k)
				}
			}
			if hk.Service == "" {
				return nil, fmt.Errorf("stacks: %s: a hook mapping needs a service", where)
			}
			out = append(out, hk)

		default:
			return nil, fmt.Errorf("stacks: %s must be a list of hooks (a service name, or a mapping with service/priority/on_failure)", where)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Priority < out[j].Priority })
	return out, nil
}

func validateHooks(project *types.Project, hooks *Hooks, swarm bool) error {
	hookSet := hooks.serviceSet()

	// Every hook names a service that exists.
	for name := range hookSet {
		if _, ok := project.Services[name]; !ok {
			return fmt.Errorf("stacks: hook %q is not a service in this compose file", name)
		}
	}
	// And not EVERY service: a stack that is all hooks deploys nothing.
	if len(hookSet) == len(project.Services) {
		return fmt.Errorf("stacks: every service is a hook — there is nothing left to deploy")
	}

	for name, svc := range project.Services {
		if hookSet[name] {
			// Hook containers are plain one-shots; a swarm config/secret cannot be
			// mounted into one. Saying so now beats a compose error in a runner log.
			if len(svc.Configs) > 0 || len(svc.Secrets) > 0 {
				return fmt.Errorf("stacks: hook %q mounts configs/secrets, which only exist inside the orchestrator — hooks are plain containers and cannot receive them. Pass what it needs as environment or a volume", name)
			}
			continue
		}
		// A deployed service must not depend_on a hook: the hook is stripped from the
		// file the engine sees, and compose would refuse the dangling reference.
		for dep := range svc.DependsOn {
			if hookSet[dep] {
				return fmt.Errorf("stacks: service %q depends_on %q, which is a hook — hooks are not deployed, they are run around the deploy. Sequence with the hook lists instead", name, dep)
			}
		}
	}

	if !swarm {
		return nil
	}
	// Swarm: every network a hook touches must be attachable, or the hook container
	// cannot join it. Checked per network, reported with the exact fix.
	for _, key := range hookNetworkKeys(project, hookSet) {
		net, ok := project.Networks[key]
		if !ok {
			continue
		}
		if bool(net.External) {
			continue // not ours to create; the operator presumably made it attachable
		}
		if !net.Attachable {
			return fmt.Errorf(
				"stacks: hook containers join the stack's networks as plain containers, and on Swarm "+
					"that requires the network to be attachable. Add to the compose file:\n\n"+
					"networks:\n  %s:\n    driver: overlay\n    attachable: true", key)
		}
	}
	return nil
}

// hookNetworkKeys is every network key the hook services reference — `default` when a
// service declares none, because that is what compose attaches it to.
func hookNetworkKeys(project *types.Project, hookSet map[string]bool) []string {
	keys := map[string]bool{}
	for name, svc := range project.Services {
		if !hookSet[name] {
			continue
		}
		if len(svc.Networks) == 0 {
			keys["default"] = true
			continue
		}
		for k := range svc.Networks {
			keys[k] = true
		}
	}
	out := make([]string, 0, len(keys))
	for k := range keys {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// splitHooks does the file surgery, on the yaml TREE rather than on the parsed project.
//
// That distinction is load-bearing. compose-go interpolates ${VAR} while loading, so
// re-marshalling the parsed project would bake every secret into the emitted YAML — and
// DeployYAML is exactly what a deployment row stores for rollback, where docs/stacks.md
// §2 forbids a resolved secret to ever land. Working on the tree keeps every ${VAR}
// spelled as the author wrote it.
func splitHooks(yamlText string, project *types.Project, hooks *Hooks, inlineDefs bool) (deployYAML, hooksYAML string, err error) {
	hookSet := hooks.serviceSet()

	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(yamlText), &doc); err != nil || len(doc.Content) == 0 {
		return "", "", fmt.Errorf("stacks: re-reading the compose file for hook splitting: %w", err)
	}
	root := doc.Content[0]

	servicesNode := mapValue(root, "services")
	if servicesNode == nil || servicesNode.Kind != yaml.MappingNode {
		return "", "", fmt.Errorf("stacks: the compose file has no services mapping")
	}

	// Partition the service entries. The nodes are shared, not copied — each service
	// definition ends up in exactly one of the two files.
	var keep, hooked []*yaml.Node // alternating key/value pairs
	for i := 0; i+1 < len(servicesNode.Content); i += 2 {
		k, v := servicesNode.Content[i], servicesNode.Content[i+1]
		if hookSet[k.Value] {
			hooked = append(hooked, k, pruneHookService(v))
		} else {
			keep = append(keep, k, v)
		}
	}

	// The original resource definition nodes, grabbed BEFORE the deploy doc is
	// re-marshalled: inlineDefs copies them into the hooks file verbatim, ${VAR}s intact.
	var origNets, origVols *yaml.Node
	if inlineDefs {
		origNets, origVols = mapValue(root, "networks"), mapValue(root, "volumes")
	}

	servicesNode.Content = keep
	// The x-daffa block goes too: the deploy file no longer contains the services it
	// names, and a derived file that describes hooks it does not carry would send the
	// next reader looking for services that are not there. The ORIGINAL file — stored on
	// the deployment, shown in the UI — keeps it, and is where the plan is re-derived
	// from on every deploy including rollbacks.
	dropMapKey(root, "x-daffa")
	deployBytes, err := yaml.Marshal(&doc)
	if err != nil {
		return "", "", fmt.Errorf("stacks: rendering the deploy file: %w", err)
	}

	hooksDoc := buildHooksDoc(project, hookSet, hooked, origNets, origVols)
	hooksBytes, err := yaml.Marshal(hooksDoc)
	if err != nil {
		return "", "", fmt.Errorf("stacks: rendering the hooks file: %w", err)
	}
	return string(deployBytes), string(hooksBytes), nil
}

// pruneHookService drops the keys that only make sense for a deployed service. depends_on
// would dangle (its targets live in the other file, and `compose run --no-deps` would not
// honour it anyway); profiles would make the hook invisible to `compose run`'s file;
// deploy/restart are the orchestrator's vocabulary and a one-shot has no orchestrator.
func pruneHookService(svc *yaml.Node) *yaml.Node {
	if svc.Kind != yaml.MappingNode {
		return svc
	}
	drop := map[string]bool{"depends_on": true, "profiles": true, "deploy": true, "restart": true}
	var kept []*yaml.Node
	for i := 0; i+1 < len(svc.Content); i += 2 {
		if drop[svc.Content[i].Value] {
			continue
		}
		kept = append(kept, svc.Content[i], svc.Content[i+1])
	}
	svc.Content = kept
	return svc
}

// buildHooksDoc assembles hooks.yml: the hook services plus declarations for every
// project resource they touch.
//
// The default declaration is `external: true` under the deployed name — which is how a
// hook container lands on the REAL stack's network instead of a freshly created
// lookalike. When origNets/origVols are non-nil (a compose first deploy), stack-owned
// resources get the ORIGINAL definition inlined instead: the resources do not exist yet,
// and the hook's own `compose run` creating them from the same definition, under the same
// project name, is exactly what lets the engine adopt them afterwards. Resources the
// operator declared `external: true` keep the external stub in both modes — they were
// never the stack's to create.
func buildHooksDoc(project *types.Project, hookSet map[string]bool, hooked []*yaml.Node, origNets, origVols *yaml.Node) *yaml.Node {
	services := &yaml.Node{Kind: yaml.MappingNode, Content: hooked}

	declare := func(key string, external bool, orig *yaml.Node, deployed string) *yaml.Node {
		if orig == nil || external {
			return externalRef(deployed)
		}
		if def := mapValue(orig, key); def != nil && def.Kind == yaml.MappingNode {
			return def
		}
		// Declared bare (`backend:`) or not declared at all (`default`): an empty
		// mapping lets compose create it with its defaults, same as the engine would.
		return &yaml.Node{Kind: yaml.MappingNode}
	}

	// Networks: everything the hook services reference (or `default`).
	networks := &yaml.Node{Kind: yaml.MappingNode}
	for _, key := range hookNetworkKeys(project, hookSet) {
		net := project.Networks[key]
		name := deployedName(project.Name, key, net.Name)
		networks.Content = append(networks.Content,
			scalar(key), declare(key, bool(net.External), origNets, name))
	}

	// A hook service with no networks key must still be told to join `default`, or
	// compose run would put it on a hooks-file default of its own.
	for i := 0; i+1 < len(hooked); i += 2 {
		name, svc := hooked[i].Value, hooked[i+1]
		if s, ok := project.Services[name]; ok && len(s.Networks) == 0 && svc.Kind == yaml.MappingNode {
			svc.Content = append(svc.Content, scalar("networks"),
				&yaml.Node{Kind: yaml.SequenceNode, Content: []*yaml.Node{scalar("default")}})
		}
	}

	// Named volumes the hook services mount.
	volumes := &yaml.Node{Kind: yaml.MappingNode}
	for _, key := range hookVolumeKeys(project, hookSet) {
		vol := project.Volumes[key]
		name := deployedName(project.Name, key, vol.Name)
		volumes.Content = append(volumes.Content,
			scalar(key), declare(key, bool(vol.External), origVols, name))
	}

	root := &yaml.Node{Kind: yaml.MappingNode}
	root.Content = append(root.Content, scalar("services"), services)
	if len(networks.Content) > 0 {
		root.Content = append(root.Content, scalar("networks"), networks)
	}
	if len(volumes.Content) > 0 {
		root.Content = append(root.Content, scalar("volumes"), volumes)
	}
	return &yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{root}}
}

// hookVolumeKeys is every named-volume key the hook services mount, sorted.
func hookVolumeKeys(project *types.Project, hookSet map[string]bool) []string {
	keys := map[string]bool{}
	for name, svc := range project.Services {
		if !hookSet[name] {
			continue
		}
		for _, v := range svc.Volumes {
			if v.Type == string(types.VolumeTypeVolume) && v.Source != "" {
				keys[v.Source] = true
			}
		}
	}
	out := make([]string, 0, len(keys))
	for k := range keys {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func scalar(s string) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Value: s}
}

// externalRef renders `{external: true, name: <name>}`.
func externalRef(name string) *yaml.Node {
	return &yaml.Node{Kind: yaml.MappingNode, Content: []*yaml.Node{
		scalar("external"), {Kind: yaml.ScalarNode, Value: "true", Tag: "!!bool"},
		scalar("name"), scalar(name),
	}}
}

func dropMapKey(mapping *yaml.Node, key string) {
	if mapping == nil || mapping.Kind != yaml.MappingNode {
		return
	}
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == key {
			mapping.Content = append(mapping.Content[:i], mapping.Content[i+2:]...)
			return
		}
	}
}

func mapValue(mapping *yaml.Node, key string) *yaml.Node {
	if mapping == nil || mapping.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == key {
			return mapping.Content[i+1]
		}
	}
	return nil
}

// HookCommand is the runner argv for one hook: `compose run` against hooks.yml, which
// resolves the service's image, env, entrypoint and mounts exactly as compose always
// does. --no-deps because sequencing is the hook lists' job; --rm because the RUNNER is
// what carries the exit code and the log, and the hook's own container has nothing to
// say once it has exited. Identical for both engines — that symmetry is the feature.
func HookCommand(project, service string) []string {
	return []string{
		"docker", "compose", "-p", project,
		"-f", "/stack/" + hooksPath, "--env-file", "/stack/" + envPath,
		"run", "--rm", "--no-deps", "--quiet-pull", service,
	}
}
