package caps

import "sort"

// ScopedMask is what a user may do, and where.
//
// Global is what they hold everywhere. Env is what they hold on one particular host, on top of
// that. A capability held globally is held on every host; the reverse is not true, which is the
// entire point.
type ScopedMask struct {
	Global Set
	Env    map[string]Set // env id → the extra capabilities held on that host
}

// Has reports whether the capability is held on that environment.
//
// Pass "" for a fleet-wide question. Note what that does NOT do: it does not mean "held
// anywhere". A global-only capability lives in Global and answers correctly; an env-scopable
// one asked without an environment answers only if it is held globally — which is the strict
// reading, and the safe one to get wrong.
func (s ScopedMask) Has(c Cap, env string) bool {
	if s.Global.Has(c) {
		return true
	}
	if env == "" {
		return false
	}
	return s.Env[env].Has(c)
}

// HasAnywhere reports whether the capability is held globally or on ANY host.
//
// This is for the fleet-wide read lists — git credentials, registries, storage targets — which
// have no environment of their own but which an env-scoped operator still needs to see in order
// to pick one when creating a stack. It is a deliberate widening, and the only routes that use
// it are those whose responses carry no secrets.
func (s ScopedMask) HasAnywhere(c Cap) bool {
	if s.Global.Has(c) {
		return true
	}
	for _, m := range s.Env {
		if m.Has(c) {
			return true
		}
	}
	return false
}

// Envs lists the environments the user holds anything on, sorted. Combined with Global it is
// what the environment list filters on: a host you hold nothing on does not exist as far as you
// are concerned.
func (s ScopedMask) Envs() []string {
	out := make([]string, 0, len(s.Env))
	for id, m := range s.Env {
		if !m.IsZero() {
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}
