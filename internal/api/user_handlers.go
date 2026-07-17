package api

import (
	"errors"
	"net/http"
	"strings"

	"github.com/Mnshahawy/daffa/internal/auth"
	"github.com/Mnshahawy/daffa/internal/httpx"
	"github.com/Mnshahawy/daffa/internal/store"
)

// MinPasswordLength matches what `daffa user add` enforces. A console that can restart
// production is not the place for a six-character password, and a rule that differs
// between the CLI and the UI is a rule nobody believes.
const MinPasswordLength = 12

type userView struct {
	ID          string           `json:"id"`
	Label       string           `json:"label"`
	Username    string           `json:"username"`
	Email       string           `json:"email"`
	Kind        string           `json:"kind"` // local | oidc
	Disabled    bool             `json:"disabled"`
	Roles       []membershipView `json:"roles"`
	CreatedAt   string           `json:"created_at"`
	LastLoginAt string           `json:"last_login_at,omitempty"`
}

type membershipView struct {
	RoleID  string `json:"role_id"`
	Name    string `json:"name"`
	IsAdmin bool   `json:"is_admin"`
	// Source is "local" or "oidc". The UI locks the oidc ones: they are re-synced from
	// the provider on every login, so removing one here would last until the person next
	// signed in, which is the kind of half-working control that erodes trust in the rest.
	Source string `json:"source"`
	// Where the grant applies. env_id empty ⇒ everywhere.
	EnvID   string `json:"env_id,omitempty"`
	EnvName string `json:"env_name,omitempty"`
}

func (s *Server) viewUser(r *http.Request, u *store.User) (userView, error) {
	ms, err := s.store.UserRoles(r.Context(), u.ID)
	if err != nil {
		return userView{}, err
	}
	roles := make([]membershipView, 0, len(ms))
	for _, m := range ms {
		roles = append(roles, membershipView{
			RoleID: m.RoleID, Name: m.Name, IsAdmin: m.IsAdmin, Source: m.Source,
			EnvID: m.Scope.ID, EnvName: m.EnvName,
		})
	}

	v := userView{
		ID: u.ID, Label: u.Label(), Username: u.Username, Email: u.Email,
		Kind: u.Kind, Disabled: u.Disabled, Roles: roles,
		CreatedAt: u.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
	}
	if !u.LastLoginAt.IsZero() {
		v.LastLoginAt = u.LastLoginAt.UTC().Format("2006-01-02T15:04:05Z")
	}
	return v, nil
}

func (s *Server) handleListUsers(w http.ResponseWriter, r *http.Request) {
	users, err := s.store.ListUsers(r.Context())
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	out := make([]userView, 0, len(users))
	for _, u := range users {
		v, err := s.viewUser(r, u)
		if err != nil {
			httpx.Error(w, r, err)
			return
		}
		out = append(out, v)
	}
	httpx.JSON(w, http.StatusOK, out)
}

type createUserRequest struct {
	Username string         `json:"username"`
	Email    string         `json:"email"`
	Password string         `json:"password"`
	Grants   []grantRequest `json:"grants"`
}

// handleCreateUser creates a LOCAL user. OIDC users are not created here — they appear on
// first successful sign-in, with the roles their claims map to.
func (s *Server) handleCreateUser(w http.ResponseWriter, r *http.Request) {
	var req createUserRequest
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.BadRequest(w, r, err.Error())
		return
	}
	req.Username = strings.TrimSpace(req.Username)

	if req.Username == "" {
		httpx.BadRequest(w, r, "A username is required.")
		return
	}
	if len(req.Password) < MinPasswordLength {
		httpx.BadRequest(w, r, "The password must be at least 12 characters.")
		return
	}
	if len(req.Grants) == 0 {
		httpx.BadRequest(w, r, "Give the user at least one role, or they will be able to sign in and see nothing.")
		return
	}

	hash, err := auth.HashPassword(req.Password)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}

	u := &store.User{Kind: "local", Username: req.Username, Email: req.Email, PasswordHash: hash}
	if err := s.store.CreateUser(r.Context(), u); err != nil {
		if store.IsDuplicate(err) {
			httpx.Fail(w, r, http.StatusConflict, "duplicate_username", "That username is already taken.")
			return
		}
		httpx.Error(w, r, err)
		return
	}

	for _, g := range req.Grants {
		sc := store.Global()
		if g.EnvID != "" {
			sc = store.OnEnv(g.EnvID)
		}
		if err := s.store.GrantRole(r.Context(), u.ID, g.RoleID, store.SourceLocal, sc); err != nil {
			if errors.Is(err, store.ErrCannotScope) {
				httpx.Fail(w, r, http.StatusConflict, "cannot_scope", err.Error())
				return
			}
			httpx.Error(w, r, err)
			return
		}
	}
	s.caps.Invalidate()
	s.auditUser(r, "user.create", u, nil)

	v, err := s.viewUser(r, u)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusCreated, v)
}

type updateUserRequest struct {
	Email    *string `json:"email"`
	Disabled *bool   `json:"disabled"`
}

func (s *Server) handleUpdateUser(w http.ResponseWriter, r *http.Request) {
	u, err := s.userOr404(w, r)
	if u == nil {
		return
	}
	_ = err

	var req updateUserRequest
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.BadRequest(w, r, err.Error())
		return
	}

	if req.Email != nil {
		if err := s.store.SetUserEmail(r.Context(), u.ID, *req.Email); err != nil {
			httpx.Error(w, r, err)
			return
		}
	}
	if req.Disabled != nil {
		if err := s.store.SetUserDisabledGuarded(r.Context(), u.ID, *req.Disabled); err != nil {
			if errors.Is(err, store.ErrLastAdmin) {
				httpx.Fail(w, r, http.StatusConflict, "last_admin",
					"This is the only administrator. Disabling them would lock everyone out — grant someone else an admin role first.")
				return
			}
			httpx.Error(w, r, err)
			return
		}
		s.auditUser(r, map[bool]string{true: "user.disable", false: "user.enable"}[*req.Disabled], u, nil)
	}

	u, err = s.store.UserByID(r.Context(), u.ID)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	v, err := s.viewUser(r, u)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, v)
}

func (s *Server) handleDeleteUser(w http.ResponseWriter, r *http.Request) {
	u, _ := s.userOr404(w, r)
	if u == nil {
		return
	}

	if err := s.store.DeleteUserGuarded(r.Context(), u.ID); err != nil {
		if errors.Is(err, store.ErrLastAdmin) {
			httpx.Fail(w, r, http.StatusConflict, "last_admin",
				"This is the only administrator. Deleting them would lock everyone out — grant someone else an admin role first.")
			return
		}
		httpx.Error(w, r, err)
		return
	}

	s.caps.Invalidate()
	s.auditUser(r, "user.delete", u, nil)
	httpx.JSON(w, http.StatusOK, okStatus)
}

type passwordRequest struct {
	Password string `json:"password"`
}

func (s *Server) handleSetUserPassword(w http.ResponseWriter, r *http.Request) {
	// A token that can change a password re-keys an account it will outlive — the same
	// persistence hole as a token minting tokens. People change passwords.
	if refuseTokenCaller(w, r) {
		return
	}
	u, _ := s.userOr404(w, r)
	if u == nil {
		return
	}
	if u.Kind != "local" {
		httpx.Fail(w, r, http.StatusConflict, "not_local",
			"This account signs in through an identity provider. Its password is set there, not here.")
		return
	}

	var req passwordRequest
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.BadRequest(w, r, err.Error())
		return
	}
	if len(req.Password) < MinPasswordLength {
		httpx.BadRequest(w, r, "The password must be at least 12 characters.")
		return
	}

	hash, err := auth.HashPassword(req.Password)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	if err := s.store.SetUserPassword(r.Context(), u.ID, hash); err != nil {
		httpx.Error(w, r, err)
		return
	}

	s.auditUser(r, "user.password_reset", u, nil)
	httpx.JSON(w, http.StatusOK, okStatus)
}

// grantRequest is a role plus where it applies. env_id empty ⇒ everywhere.
type grantRequest struct {
	RoleID string `json:"role_id"`
	EnvID  string `json:"env_id"`
}

type rolesRequest struct {
	Grants []grantRequest `json:"grants"`
}

// handleSetUserRoles replaces the user's LOCALLY granted roles with exactly these grants.
//
// A grant is a role AND a scope, so the same role may appear twice — Viewer everywhere,
// Operator on staging. The pair is the identity of the grant: revoking "Operator on
// staging" must not revoke "Operator on prod".
//
// Roles the identity provider granted are untouched — they are the provider's to give and
// take, re-synced on every login, and a UI that let you remove one would be offering a
// button whose effect expires at the person's next sign-in.
func (s *Server) handleSetUserRoles(w http.ResponseWriter, r *http.Request) {
	u, _ := s.userOr404(w, r)
	if u == nil {
		return
	}

	var req rolesRequest
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.BadRequest(w, r, err.Error())
		return
	}

	current, err := s.store.UserRoles(r.Context(), u.ID)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}

	// Keyed on role AND scope, so the two are not confused for one another.
	type key struct{ role, kind, env string }
	want := map[key]bool{}
	grants := make([]store.ScopedGrant, 0, len(req.Grants))
	for _, g := range req.Grants {
		sc := store.Global()
		if g.EnvID != "" {
			sc = store.OnEnv(g.EnvID)
		}
		want[key{g.RoleID, sc.Kind, sc.ID}] = true
		grants = append(grants, store.ScopedGrant{RoleID: g.RoleID, Scope: sc})
	}

	// Revoke the local grants that are no longer wanted. The store refuses the revocation
	// that would strip the last administrator, so the guard is not something this handler
	// has to remember.
	for _, m := range current {
		if m.Source != store.SourceLocal || want[key{m.RoleID, m.Scope.Kind, m.Scope.ID}] {
			continue
		}
		if err := s.store.RevokeRole(r.Context(), u.ID, m.RoleID, m.Scope); err != nil {
			if errors.Is(err, store.ErrLastAdmin) {
				httpx.Fail(w, r, http.StatusConflict, "last_admin",
					"This is the only administrator. Removing their admin role would lock everyone out.")
				return
			}
			httpx.Error(w, r, err)
			return
		}
	}

	for _, g := range grants {
		if err := s.store.GrantRole(r.Context(), u.ID, g.RoleID, store.SourceLocal, g.Scope); err != nil {
			if errors.Is(err, store.ErrCannotScope) {
				httpx.Fail(w, r, http.StatusConflict, "cannot_scope", err.Error())
				return
			}
			httpx.Error(w, r, err)
			return
		}
	}

	s.caps.Invalidate()

	v, err := s.viewUser(r, u)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	s.auditUser(r, "user.roles", u, roleNames(v.Roles))
	httpx.JSON(w, http.StatusOK, v)
}

func roleNames(ms []membershipView) []string {
	out := make([]string, 0, len(ms))
	for _, m := range ms {
		out = append(out, m.Name)
	}
	return out
}

func (s *Server) userOr404(w http.ResponseWriter, r *http.Request) (*store.User, error) {
	u, err := s.store.UserByID(r.Context(), r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		httpx.Fail(w, r, http.StatusNotFound, "not_found", "That user does not exist.")
		return nil, err
	}
	if err != nil {
		httpx.Error(w, r, err)
		return nil, err
	}
	return u, nil
}

func (s *Server) auditUser(r *http.Request, action string, target *store.User, roles []string) {
	actor, _ := auth.UserFrom(r.Context())
	detail := map[string]any{}
	if roles != nil {
		detail["roles"] = roles
	}
	e := store.AuditEntry{
		Action: action, Target: target.Label(), Outcome: "ok",
		Detail: store.AuditDetail(detail),
	}
	if actor != nil {
		e.UserID, e.UserLabel = actor.ID, actor.Label()
	}
	s.audit(r.Context(), e)
}
