package api

import (
	"context"
	"errors"
	"fmt"

	"github.com/Mnshahawy/daffa/internal/stacks"
	"github.com/Mnshahawy/daffa/internal/store"
)

// EnvKV is one environment variable to record on an adopted stack. Secret marks it for
// sealing-with-a-hidden-value in the UI, exactly like a variable typed into the stack editor.
type EnvKV struct {
	Key    string
	Value  string
	Secret bool
}

// InlineVolumeSourceOptions parameterises EnsureInlineVolumeSource.
type InlineVolumeSourceOptions struct {
	EnvID          string
	Volume         string
	StackID        string // link to the stack whose deploys sync this first (may be empty)
	RestartTargets string
	Files          []store.VolSourceFile
}

// EnsureInlineVolumeSource creates (or re-adopts) an inline volume source for a volume, so
// its files are managed and editable in the console and re-delivered on the stack's deploys.
// Idempotent — used by the installer to put Daffa's own Traefik config (traefik.yml, and a
// place for middlewares) under management, and reusable for any inline stack. It delivers
// the files now, too, so the volume is populated immediately.
func (s *Server) EnsureInlineVolumeSource(ctx context.Context, opts InlineVolumeSourceOptions) error {
	if opts.EnvID == "" || opts.Volume == "" {
		return errors.New("inline volume source: env and volume are required")
	}
	if len(opts.Files) == 0 {
		return errors.New("inline volume source: at least one file is required")
	}

	v, err := s.store.VolumeSourceByVolume(ctx, opts.EnvID, opts.Volume)
	switch {
	case errors.Is(err, store.ErrNotFound):
		v = &store.VolumeSource{
			EnvID: opts.EnvID, Volume: opts.Volume, SourceKind: "inline",
			StackID: opts.StackID, RestartTargets: opts.RestartTargets,
		}
		if err := s.store.CreateVolumeSource(ctx, v); err != nil {
			return fmt.Errorf("inline volume source: creating: %w", err)
		}
	case err != nil:
		return err
	default:
		v.SourceKind = "inline"
		v.StackID = opts.StackID
		v.RestartTargets = opts.RestartTargets
		v.GitURL, v.GitRef, v.GitPath, v.GitCredentialID, v.AutoSync = "", "", "", "", false
		if err := s.store.UpdateVolumeSource(ctx, v); err != nil {
			return fmt.Errorf("inline volume source: updating: %w", err)
		}
	}

	if err := s.store.SetVolSourceFiles(ctx, v.ID, opts.Files); err != nil {
		return fmt.Errorf("inline volume source: files: %w", err)
	}
	// Deliver now (a stack deploy would also, before the runner starts).
	v.SyncedHash = ""
	if err := s.reportVolumeSourceSync(ctx, v); err != nil {
		return fmt.Errorf("inline volume source: sync: %w", err)
	}
	return nil
}

// AdoptStackOptions parameterises AdoptStack.
type AdoptStackOptions struct {
	Name        string // the compose project name — must match what is already running
	EnvID       string // the environment the stack runs on (the local host, normally)
	ComposeYAML string // the compose file, verbatim, becomes the inline source
	Env         []EnvKV
}

// AdoptStackResult reports what AdoptStack did.
type AdoptStackResult struct {
	StackID string
	Created bool // false when an existing stack of this name was re-adopted
	Hash    string
}

// AdoptStack records an already-running compose deployment as an inline Daffa stack, so the
// console's stack machinery — drift detection, redeploy, env editing — applies to a stack
// that was brought up outside Daffa (the installer's own `docker compose up`). It is how the
// deployment becomes editable: change the domain in the UI, redeploy, done.
//
// It deliberately does NOT deploy. The stack is already up, so DeployedHash is set to the
// exact hash the next deploy would compute (over the original YAML + sorted env — auth,
// hooks and logging never feed it, so this matches buildBundle), and drift reads as clean
// instead of "never deployed". The project name is the stack name, so a later redeploy lands
// on the same containers rather than a parallel copy.
//
// Idempotent: re-running re-adopts the current compose and env (an upgrade path).
func (s *Server) AdoptStack(ctx context.Context, opts AdoptStackOptions) (AdoptStackResult, error) {
	var res AdoptStackResult
	if opts.Name == "" || opts.EnvID == "" {
		return res, errors.New("adopt: name and environment are required")
	}
	if len(opts.ComposeYAML) == 0 {
		return res, errors.New("adopt: the compose file is empty")
	}

	// Seal for storage; keep a plaintext copy for the hash. Order does not matter — Build
	// sorts by key — but the two must carry the same key/value pairs.
	stored := make([]store.StackEnv, 0, len(opts.Env))
	plain := make([]stacks.EnvVar, 0, len(opts.Env))
	for _, e := range opts.Env {
		sealed, err := s.sealer.Seal(e.Value)
		if err != nil {
			return res, err
		}
		stored = append(stored, store.StackEnv{Key: e.Key, ValueEnc: sealed, IsSecret: e.Secret})
		plain = append(plain, stacks.EnvVar{Key: e.Key, Value: e.Value})
	}

	// The identity the next deploy compares against. Registry auth is not hashed, and this
	// stack carries no file-shaped secrets, so Build with nil for both is exactly right.
	bundle, err := stacks.Build(opts.ComposeYAML, plain, nil)
	if err != nil {
		return res, fmt.Errorf("adopt: %w", err)
	}
	res.Hash = bundle.Hash

	// Re-adopt an existing stack of this name on this env rather than creating a duplicate.
	existing, err := s.store.ListStacks(ctx, false, []string{opts.EnvID})
	if err != nil {
		return res, err
	}
	var st *store.Stack
	for _, cand := range existing {
		if cand.Name == opts.Name {
			st = cand
			break
		}
	}

	if st == nil {
		st = &store.Stack{
			EnvID: opts.EnvID, Name: opts.Name, Engine: stacks.ComposeEngine.Name(),
			SourceKind: "inline", InlineYAML: opts.ComposeYAML,
		}
		if err := s.store.CreateStack(ctx, st); err != nil {
			return res, fmt.Errorf("adopt: creating stack: %w", err)
		}
		res.Created = true
	} else {
		st.Engine = stacks.ComposeEngine.Name()
		st.SourceKind = "inline"
		st.InlineYAML = opts.ComposeYAML
		if err := s.store.UpdateStackSource(ctx, st); err != nil {
			return res, fmt.Errorf("adopt: updating stack: %w", err)
		}
	}
	res.StackID = st.ID

	if err := s.store.SetStackEnv(ctx, st.ID, stored); err != nil {
		return res, fmt.Errorf("adopt: storing env: %w", err)
	}
	// commit "" — this is an inline stack, there is no git commit to record.
	if err := s.store.MarkStackDeployed(ctx, st.ID, bundle.Hash, ""); err != nil {
		return res, fmt.Errorf("adopt: marking deployed: %w", err)
	}
	return res, nil
}
