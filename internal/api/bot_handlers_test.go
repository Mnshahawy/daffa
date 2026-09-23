package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Mnshahawy/daffa/internal/auth"
	"github.com/Mnshahawy/daffa/internal/caps"
	"github.com/Mnshahawy/daffa/internal/store"
)

// A bot end to end, through the real authentication middleware: created without a
// password, given a role, minted a token by a person, and that token then acts with the
// bot's role — and only that role. The refusals are the point as much as the happy path:
// no password for a bot, no minting for a person, no token minting a token, and disabling
// the bot kills its tokens.
func TestBotLifecycle(t *testing.T) {
	s, ctx := certServer(t)
	s.caps = caps.NewCache(s.store)
	s.sessions = auth.NewManager(s.store, s.caps, time.Hour, false)

	root := &store.User{Kind: "local", Username: "root"}
	if err := s.store.CreateUser(ctx, root); err != nil {
		t.Fatal(err)
	}
	if err := s.store.GrantRole(ctx, root.ID, "role_admin", store.SourceLocal, store.Global()); err != nil {
		t.Fatal(err)
	}
	issued := httptest.NewRecorder()
	if err := s.sessions.Issue(ctx, issued, root.ID, false); err != nil {
		t.Fatal(err)
	}
	session := issued.Result().Cookies()[0]

	// do runs a handler behind the real Require middleware, as the session or a bearer.
	do := func(h http.HandlerFunc, method, path string, pv map[string]string, body, bearer string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		} else {
			req.AddCookie(session)
		}
		for k, v := range pv {
			req.SetPathValue(k, v)
		}
		rec := httptest.NewRecorder()
		s.sessions.Require(h).ServeHTTP(rec, req)
		return rec
	}
	expect := func(rec *httptest.ResponseRecorder, code int, what string) {
		t.Helper()
		if rec.Code != code {
			t.Fatalf("%s: got %d, want %d: %s", what, rec.Code, code, rec.Body)
		}
	}

	// A password on a bot is refused, not ignored — whoever typed it thinks it signs in.
	expect(do(s.handleCreateUser, "POST", "/api/users", nil,
		`{"kind":"bot","username":"ci","password":"a-long-placeholder-passphrase","grants":[{"role_id":"role_viewer"}]}`, ""),
		http.StatusBadRequest, "bot with a password")
	expect(do(s.handleCreateUser, "POST", "/api/users", nil, `{"kind":"bot","username":"ci","grants":[]}`, ""),
		http.StatusBadRequest, "bot with no role")
	expect(do(s.handleCreateUser, "POST", "/api/users", nil, `{"kind":"robot","username":"ci","grants":[{"role_id":"role_viewer"}]}`, ""),
		http.StatusBadRequest, "unknown kind")

	rec := do(s.handleCreateUser, "POST", "/api/users", nil,
		`{"kind":"bot","username":"ci","grants":[{"role_id":"role_viewer"}]}`, "")
	expect(rec, http.StatusCreated, "create bot")
	var bot userView
	if err := json.Unmarshal(rec.Body.Bytes(), &bot); err != nil {
		t.Fatal(err)
	}
	if bot.Kind != "bot" {
		t.Fatalf("created kind %q, want bot", bot.Kind)
	}
	if u, _ := s.store.UserByID(ctx, bot.ID); u.PasswordHash != "" {
		t.Error("a bot was stored with a password hash")
	}
	botPV := map[string]string{"id": bot.ID}

	expect(do(s.handleSetUserPassword, "PUT", "/api/users/"+bot.ID+"/password", botPV,
		`{"password":"a-long-placeholder-passphrase"}`, ""), http.StatusConflict, "password for a bot")

	// Minting for a person would be impersonating them.
	expect(do(s.handleCreateBotToken, "POST", "/api/users/"+root.ID+"/tokens", map[string]string{"id": root.ID},
		`{"name":"sneaky"}`, ""), http.StatusConflict, "minting for a person")

	rec = do(s.handleCreateBotToken, "POST", "/api/users/"+bot.ID+"/tokens", botPV, `{"name":"forgejo-ci"}`, "")
	expect(rec, http.StatusOK, "mint bot token")
	var minted createdTokenResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &minted); err != nil {
		t.Fatal(err)
	}
	if minted.UserID != bot.ID || minted.UserKind != "bot" || !strings.HasPrefix(minted.Token, auth.TokenPrefix) {
		t.Fatalf("minted %+v; want a daffa_ token owned by the bot", minted.tokenView)
	}
	expect(do(s.handleCreateBotToken, "POST", "/api/users/"+bot.ID+"/tokens", botPV, `{"name":"forgejo-ci"}`, ""),
		http.StatusBadRequest, "duplicate bot token name")

	// The token is the bot: its role's powers, nothing more.
	req := httptest.NewRequest("GET", "/api/me", nil)
	req.Header.Set("Authorization", "Bearer "+minted.Token)
	u, tok, err := s.sessions.Resolve(req)
	if err != nil {
		t.Fatalf("resolving the bot's token: %v", err)
	}
	if u.ID != bot.ID || u.Kind != "bot" || tok == nil {
		t.Fatalf("token resolved to %s (%s); want the bot", u.Label(), u.Kind)
	}
	if !u.Caps.Has(caps.StacksView, "") || u.Caps.Has(caps.UsersEdit, "") {
		t.Errorf("bot token caps = %v; want Viewer's", u.Caps.Global.Names())
	}

	// A token never mints a token — not its own successor, not any other.
	expect(do(s.handleCreateBotToken, "POST", "/api/users/"+bot.ID+"/tokens", botPV, `{"name":"next"}`, minted.Token),
		http.StatusForbidden, "bot token minting for itself")
	expect(do(s.handleCreateToken, "POST", "/api/tokens", nil, `{"name":"next"}`, minted.Token),
		http.StatusForbidden, "bot token minting a personal token")

	// The oversight list names the owner as a bot.
	rec = do(s.handleListAllTokens, "GET", "/api/tokens/all", nil, "", "")
	expect(rec, http.StatusOK, "list all tokens")
	var all []tokenView
	if err := json.Unmarshal(rec.Body.Bytes(), &all); err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 || all[0].UserKind != "bot" || all[0].UserLabel != "ci" {
		t.Errorf("oversight list = %+v; want the bot's one token, labelled", all)
	}

	// Disabling the bot is the kill switch for every token it holds.
	expect(do(s.handleUpdateUser, "PATCH", "/api/users/"+bot.ID, botPV, `{"disabled":true}`, ""),
		http.StatusOK, "disable bot")
	if _, _, err := s.sessions.Resolve(req); !errors.Is(err, auth.ErrNoSession) {
		t.Errorf("a disabled bot's token resolved: err = %v", err)
	}
}
