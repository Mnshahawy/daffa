package api

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/Mnshahawy/daffa/internal/auth"
	"github.com/Mnshahawy/daffa/internal/caps"
	"github.com/Mnshahawy/daffa/internal/httpx"
	"github.com/Mnshahawy/daffa/internal/store"
)

// API tokens: automation without a browser. A token is a way to BE a user without a
// session — same capability mask, resolved live, one kill switch (disable the user).
// See docs/tokens.md.
//
// A person mints their own tokens and nobody else's. A BOT — an account that cannot sign
// in — cannot mint its own, so a users.edit holder mints for it; that is the only case of
// one account minting for another, because minting for a person would be impersonating them.

type tokenView struct {
	ID         string     `json:"id"`
	UserID     string     `json:"user_id"`
	UserLabel  string     `json:"user_label,omitempty"` // oversight list and mint response
	UserKind   string     `json:"user_kind,omitempty"`  // oversight list and mint response
	Name       string     `json:"name"`
	Prefix     string     `json:"prefix"`
	ExpiresAt  *time.Time `json:"expires_at,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
	Expired    bool       `json:"expired,omitempty"`
}

func viewToken(t *store.APIToken) tokenView {
	v := tokenView{
		ID: t.ID, UserID: t.UserID, Name: t.Name, Prefix: t.Prefix,
		CreatedAt: t.CreatedAt, Expired: t.Expired(),
	}
	if !t.ExpiresAt.IsZero() {
		e := t.ExpiresAt
		v.ExpiresAt = &e
	}
	if !t.LastUsedAt.IsZero() {
		l := t.LastUsedAt
		v.LastUsedAt = &l
	}
	return v
}

// refuseTokenCaller is the one place a token is less than its user: a leaked token that
// can mint tokens survives its own revocation, and one that can change passwords re-keys
// the account it dies with. Reported whether the caller was refused.
func refuseTokenCaller(w http.ResponseWriter, r *http.Request) bool {
	if _, ok := auth.TokenFrom(r.Context()); !ok {
		return false
	}
	httpx.Fail(w, r, http.StatusForbidden, "token_cannot",
		"API tokens cannot manage credentials. Sign in as a person to do this.")
	return true
}

func (s *Server) handleListMyTokens(w http.ResponseWriter, r *http.Request) {
	u, _ := auth.UserFrom(r.Context())
	list, err := s.store.ListAPITokens(r.Context(), u.ID)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	out := make([]tokenView, 0, len(list))
	for _, t := range list {
		out = append(out, viewToken(t))
	}
	httpx.JSON(w, http.StatusOK, out)
}

// handleListAllTokens is the users.edit oversight list: every token, with owners.
// Oversight without impersonation — the secret is unrecoverable even here.
func (s *Server) handleListAllTokens(w http.ResponseWriter, r *http.Request) {
	list, err := s.store.AllAPITokens(r.Context())
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	out := make([]tokenView, 0, len(list))
	for _, t := range list {
		v := viewToken(t)
		if owner, err := s.store.UserByID(r.Context(), t.UserID); err == nil {
			v.UserLabel, v.UserKind = owner.Label(), owner.Kind
		}
		out = append(out, v)
	}
	httpx.JSON(w, http.StatusOK, out)
}

type createTokenRequest struct {
	Name        string `json:"name"`
	ExpiresDays int    `json:"expires_days"` // 0 = does not expire, stated on the form
}

// createdTokenResponse is the one response that ever carries the secret. There is no
// second chance: only the hash is stored, which is the entire point.
type createdTokenResponse struct {
	tokenView
	Token string `json:"token"`
}

func (s *Server) handleCreateToken(w http.ResponseWriter, r *http.Request) {
	if refuseTokenCaller(w, r) {
		return
	}
	u, _ := auth.UserFrom(r.Context())
	s.mintToken(w, r, u)
}

// handleCreateBotToken mints a token for a bot. users.edit at the route; the handler adds
// the two refusals that make it safe: no token-authenticated caller (a bot's token must
// not mint its own successor), and no minting for a person.
func (s *Server) handleCreateBotToken(w http.ResponseWriter, r *http.Request) {
	if refuseTokenCaller(w, r) {
		return
	}
	u, _ := s.userOr404(w, r)
	if u == nil {
		return
	}
	if u.Kind != "bot" {
		httpx.Fail(w, r, http.StatusConflict, "not_a_bot",
			"Only a bot's tokens are minted by someone else. A person mints their own under API tokens — minting one for them would let you act as them.")
		return
	}
	s.mintToken(w, r, u)
}

// mintToken creates a token owned by owner and answers with the secret, once.
func (s *Server) mintToken(w http.ResponseWriter, r *http.Request, owner *store.User) {
	var req createTokenRequest
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.BadRequest(w, r, err.Error())
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if !certName.MatchString(req.Name) {
		badName(w, r)
		return
	}
	if req.ExpiresDays < 0 {
		httpx.BadRequest(w, r, "Expiry cannot be negative. Use 0 for a token that does not expire.")
		return
	}

	secret, err := auth.NewAPIToken()
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	tok := &store.APIToken{
		UserID: owner.ID, Name: req.Name,
		Prefix: secret[:len(auth.TokenPrefix)+6],
		Hash:   auth.HashToken(secret),
	}
	if req.ExpiresDays > 0 {
		tok.ExpiresAt = time.Now().Add(time.Duration(req.ExpiresDays) * 24 * time.Hour)
	}
	if err := s.store.CreateAPIToken(r.Context(), tok); err != nil {
		if store.IsDuplicate(err) {
			msg := "You already have a token with that name."
			if actor, _ := auth.UserFrom(r.Context()); actor == nil || actor.ID != owner.ID {
				msg = owner.Label() + " already has a token with that name."
			}
			httpx.Fail(w, r, http.StatusBadRequest, "name_taken", msg)
			return
		}
		httpx.Error(w, r, err)
		return
	}

	s.audit(r.Context(), store.AuditEntry{
		Action: "token.create", Target: req.Name, Outcome: "ok",
		Detail: store.AuditDetail(map[string]any{"expires_days": req.ExpiresDays, "owner": owner.Label()}),
	})

	// The one response that ever carries the secret.
	v := viewToken(tok)
	v.UserLabel, v.UserKind = owner.Label(), owner.Kind
	httpx.JSON(w, http.StatusOK, createdTokenResponse{v, secret})
}

func (s *Server) handleDeleteToken(w http.ResponseWriter, r *http.Request) {
	if refuseTokenCaller(w, r) {
		return
	}
	u, _ := auth.UserFrom(r.Context())

	tok, err := s.store.APITokenByID(r.Context(), r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		httpx.Fail(w, r, http.StatusNotFound, "no_such_token", "No such token.")
		return
	}
	if err != nil {
		httpx.Error(w, r, err)
		return
	}

	// Yours, or you manage who gets in. The refusal is a 404, like every entity route:
	// whether somebody else's token id exists is not the caller's business either.
	if tok.UserID != u.ID && !u.Caps.Has(caps.UsersEdit, "") {
		httpx.Fail(w, r, http.StatusNotFound, "no_such_token", "No such token.")
		return
	}

	if err := s.store.DeleteAPIToken(r.Context(), tok.ID); err != nil {
		httpx.Error(w, r, err)
		return
	}
	s.audit(r.Context(), store.AuditEntry{
		Action: "token.revoke", Target: tok.Name, Outcome: "ok",
		Detail: store.AuditDetail(map[string]any{"owner": tok.UserID}),
	})
	httpx.JSON(w, http.StatusOK, okStatus)
}
