package api

import (
	"errors"
	"net/http"
	"strings"

	"github.com/Mnshahawy/daffa/internal/auth"
	"github.com/Mnshahawy/daffa/internal/caps"
	"github.com/Mnshahawy/daffa/internal/httpx"
	"github.com/Mnshahawy/daffa/internal/store"
)

// areaView is a functional area: one mask, one section of the role editor.
type areaView struct {
	NS          string `json:"ns"`
	Label       string `json:"label"`
	Description string `json:"description"`
}

// capView is one capability, as the server defines it.
type capView struct {
	Name        string `json:"name"`
	NS          string `json:"ns"`
	Bit         int    `json:"bit"`
	Object      string `json:"object"`
	Mode        string `json:"mode"` // view | edit | "" for standalone
	Description string `json:"description"`
}

// capabilitiesResponse carries the AREAS as well as the capabilities, because the areas are
// the matrix's section headers — and a UI that grouped by an area list of its own would put
// a new capability in the wrong section, or in no section at all, the first time Go added
// one.
type capabilitiesResponse struct {
	Areas        []areaView `json:"areas"`
	Capabilities []capView  `json:"capabilities"`
}

// handleListCapabilities is the registry, as data. The role editor renders its matrix from this
// rather than from a hardcoded copy, so a capability added in Go appears in the UI with no
// frontend change and the two cannot drift.
func (s *Server) handleListCapabilities(w http.ResponseWriter, r *http.Request) {
	areas := make([]areaView, 0, len(caps.Namespaces))
	for _, a := range caps.Namespaces {
		areas = append(areas, areaView{NS: string(a.NS), Label: a.Label, Description: a.Description})
	}

	list := make([]capView, 0, len(caps.All))
	for _, d := range caps.All {
		list = append(list, capView{
			Name:        d.Name,
			NS:          string(d.NS()),
			Bit:         d.Bit(),
			Object:      d.Object,
			Mode:        string(d.Mode),
			Description: d.Description,
		})
	}

	httpx.JSON(w, http.StatusOK, capabilitiesResponse{Areas: areas, Capabilities: list})
}

type roleView struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Caps        caps.Set `json:"caps"` // one mask per area
	CapNames    []string `json:"cap_names"`
	IsAdmin     bool     `json:"is_admin"`
	Builtin     bool     `json:"builtin"`
	Members     int      `json:"members"`

	// Scopable is false when the role carries a capability that has no meaning on a single
	// host. The UI then offers only "Everywhere" — and names the offending capabilities,
	// because "you cannot do that" without a reason is the kind of refusal people work around
	// rather than understand.
	Scopable   bool     `json:"scopable"`
	GlobalOnly []string `json:"global_only,omitempty"`
}

func viewRole(r *store.Role) roleView {
	eff := r.Effective()
	globalOnly := caps.GlobalOnly(eff)

	return roleView{
		ID: r.ID, Name: r.Name, Description: r.Description,
		// The EFFECTIVE set, so an admin role reports what it actually grants rather than the
		// nothing it stores.
		Caps: eff, CapNames: eff.Names(),
		IsAdmin: r.IsAdmin, Builtin: r.Builtin, Members: r.Members,
		Scopable: globalOnly.IsZero(), GlobalOnly: globalOnly.Names(),
	}
}

func (s *Server) handleListRoles(w http.ResponseWriter, r *http.Request) {
	roles, err := s.store.ListRoles(r.Context())
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	out := make([]roleView, 0, len(roles))
	for _, role := range roles {
		out = append(out, viewRole(role))
	}
	httpx.JSON(w, http.StatusOK, out)
}

type roleRequest struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	CapNames    []string `json:"cap_names"`
}

// Roles are addressed by capability NAME on the wire, never by a raw bitmask. A client
// that sent an integer could set a bit that does not exist yet, or one belonging to a
// capability it does not understand; names can only ever mean what the server says.
func (rq *roleRequest) capSet() (caps.Set, error) { return caps.SetFromNames(rq.CapNames) }

func (s *Server) handleCreateRole(w http.ResponseWriter, r *http.Request) {
	var req roleRequest
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.BadRequest(w, r, err.Error())
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" {
		httpx.BadRequest(w, r, "A role needs a name.")
		return
	}

	set, err := req.capSet()
	if err != nil {
		httpx.BadRequest(w, r, err.Error())
		return
	}

	role := &store.Role{Name: req.Name, Description: req.Description, Caps: set}
	if err := s.store.CreateRole(r.Context(), role); err != nil {
		if store.IsDuplicate(err) {
			httpx.Fail(w, r, http.StatusConflict, "duplicate_name", "A role with that name already exists.")
			return
		}
		httpx.Error(w, r, err)
		return
	}

	s.caps.Invalidate()
	s.auditRole(r, "role.create", role)
	httpx.JSON(w, http.StatusCreated, viewRole(role))
}

func (s *Server) handleUpdateRole(w http.ResponseWriter, r *http.Request) {
	role, err := s.store.RoleByID(r.Context(), r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		httpx.Fail(w, r, http.StatusNotFound, "not_found", "That role does not exist.")
		return
	}
	if err != nil {
		httpx.Error(w, r, err)
		return
	}

	var req roleRequest
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.BadRequest(w, r, err.Error())
		return
	}

	// An admin role holds everything by definition. Letting its capability list be edited
	// would present a checkbox grid that does nothing, which is worse than not showing one.
	if role.IsAdmin {
		httpx.Fail(w, r, http.StatusConflict, "admin_role",
			"The Admin role always holds every capability, including ones added in future versions. It cannot be narrowed — make a new role instead.")
		return
	}

	set, err := req.capSet()
	if err != nil {
		httpx.BadRequest(w, r, err.Error())
		return
	}

	role.Name = strings.TrimSpace(req.Name)
	role.Description = req.Description
	role.Caps = set
	if role.Name == "" {
		httpx.BadRequest(w, r, "A role needs a name.")
		return
	}

	if err := s.store.UpdateRole(r.Context(), role); err != nil {
		if store.IsDuplicate(err) {
			httpx.Fail(w, r, http.StatusConflict, "duplicate_name", "A role with that name already exists.")
			return
		}
		httpx.Error(w, r, err)
		return
	}

	// Everyone holding this role has a different mask now.
	s.caps.Invalidate()
	s.auditRole(r, "role.update", role)
	httpx.JSON(w, http.StatusOK, viewRole(role))
}

func (s *Server) handleDeleteRole(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	role, err := s.store.RoleByID(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		httpx.Fail(w, r, http.StatusNotFound, "not_found", "That role does not exist.")
		return
	}
	if err != nil {
		httpx.Error(w, r, err)
		return
	}

	if err := s.store.DeleteRole(r.Context(), id); err != nil {
		switch {
		case errors.Is(err, store.ErrBuiltinRole):
			httpx.Fail(w, r, http.StatusConflict, "builtin_role",
				"The Admin role is built in and cannot be deleted.")
		case errors.Is(err, store.ErrLastAdmin):
			httpx.Fail(w, r, http.StatusConflict, "last_admin",
				"This is the only role granting administrator access. Deleting it would lock everyone out.")
		default:
			httpx.Error(w, r, err)
		}
		return
	}

	s.caps.Invalidate()
	s.auditRole(r, "role.delete", role)
	httpx.JSON(w, http.StatusOK, okStatus)
}

func (s *Server) auditRole(r *http.Request, action string, role *store.Role) {
	u, _ := auth.UserFrom(r.Context())
	e := store.AuditEntry{Action: action, Target: role.Name, Outcome: "ok",
		Detail: store.AuditDetail(map[string]any{"caps": role.Effective().Names()})}
	if u != nil {
		e.UserID, e.UserLabel = u.ID, u.Label()
	}
	s.audit(r.Context(), e)
}
