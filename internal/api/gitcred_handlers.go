package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/Mnshahawy/daffa/internal/auth"
	"github.com/Mnshahawy/daffa/internal/httpx"
	"github.com/Mnshahawy/daffa/internal/stacks"
	"github.com/Mnshahawy/daffa/internal/store"
)

type gitCredView struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Kind     string `json:"kind"` // token | ssh
	Username string `json:"username,omitempty"`
	// SSHKeyName is the name of the key an SSH credential draws from the shared store, for display.
	SSHKeyName string `json:"ssh_key_name,omitempty"`
	// Pinned says whether the SSH host key is verified. Shown because an unpinned
	// credential is a real (accepted) weakening and the operator should be able to see
	// which ones are which.
	Pinned bool `json:"pinned"`
	InUse  int  `json:"in_use"`
}

func (s *Server) handleListGitCredentials(w http.ResponseWriter, r *http.Request) {
	creds, err := s.store.ListGitCredentials(r.Context())
	if err != nil {
		httpx.Error(w, r, err)
		return
	}

	// The token and the private key never leave the server, not even sealed.
	out := make([]gitCredView, 0, len(creds))
	for _, c := range creds {
		n, _ := s.store.GitCredentialInUse(r.Context(), c.ID)
		v := gitCredView{
			ID: c.ID, Name: c.Name, Kind: c.Kind, Username: c.Username,
			Pinned: c.HostKey != "", InUse: n,
		}
		if c.SSHKeyID != "" {
			if k, err := s.store.SSHKeyByID(r.Context(), c.SSHKeyID); err == nil {
				v.SSHKeyName = k.Name
			}
		}
		out = append(out, v)
	}
	httpx.JSON(w, http.StatusOK, out)
}

// hostKeysResponse pre-fills the credential form's pinned-keys field. verified is true
// only when the keys came from an authenticated endpoint (github.com), so the UI can say
// "verified" versus "trust on first use" honestly.
type hostKeysResponse struct {
	KnownHosts string `json:"known_hosts"`
	Verified   bool   `json:"verified"`
}

// handleDiscoverHostKeys fetches a host's SSH keys so the credential form can pre-pin them, which
// is the whole difference between "add a deploy key" and "add a deploy key, but first go and work
// out ssh-keyscan by hand". For github.com the keys are authenticated (see stacks.DiscoverHostKeys);
// for everything else they are trust-on-first-use, and the response says which so the UI can too.
func (s *Server) handleDiscoverHostKeys(w http.ResponseWriter, r *http.Request) {
	host := r.URL.Query().Get("host")

	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()

	lines, verified, err := stacks.DiscoverHostKeys(ctx, host)
	if err != nil {
		httpx.Fail(w, r, http.StatusBadRequest, "host_keys_unavailable", err.Error())
		return
	}
	httpx.JSON(w, http.StatusOK, hostKeysResponse{
		KnownHosts: strings.Join(lines, "\n"),
		Verified:   verified,
	})
}

type gitTestRequest struct {
	URL string `json:"url"` // a repository to test the credential against, e.g. https://git…/me/repo.git
}

// gitTestResponse is a diagnostic result, not an API outcome: it comes back 200 whether the
// credential worked or not, so the UI renders pass/fail inline. OK true means ls-remote listed
// refs; on failure Error carries the friendly reason (bad token, rejected key, unreachable…).
type gitTestResponse struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

// handleTestGitCredential proves a stored credential can reach a repository the operator names —
// an ls-remote with the credential (and Daffa's managed-CA trust for an internal https server),
// the git analog of the registry login test. It reaches out with the sealed secret, which is why
// it takes GitCredsEdit like create; the result is a payload, never an API error, so a failed test
// is not a failed request.
func (s *Server) handleTestGitCredential(w http.ResponseWriter, r *http.Request) {
	var req gitTestRequest
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.BadRequest(w, r, err.Error())
		return
	}
	req.URL = strings.TrimSpace(req.URL)
	if req.URL == "" {
		httpx.BadRequest(w, r, "A repository URL is required to test the credential against.")
		return
	}

	c, err := s.store.GitCredentialByID(r.Context(), r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		httpx.Fail(w, r, http.StatusNotFound, "not_found", "That git credential no longer exists.")
		return
	}
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	auth, err := s.openGitCred(r.Context(), c)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}

	src := stacks.Source{Kind: "git", URL: req.URL, Auth: auth, CABundle: s.managedCABundle(r.Context())}
	if err := stacks.CheckAccess(r.Context(), src); err != nil {
		httpx.JSON(w, http.StatusOK, gitTestResponse{OK: false, Error: err.Error()})
		return
	}
	httpx.JSON(w, http.StatusOK, gitTestResponse{OK: true})
}

type gitCredRequest struct {
	Name     string `json:"name"`
	Kind     string `json:"kind"`
	Username string `json:"username"`
	Token    string `json:"token"`
	// SSHKeyID picks a key from the shared SSH-key store — git credentials no longer carry their
	// own key material (its management moved to Settings → SSH keys).
	SSHKeyID string `json:"ssh_key_id"`
	HostKey  string `json:"host_key"`
}

func (s *Server) handleCreateGitCredential(w http.ResponseWriter, r *http.Request) {
	var req gitCredRequest
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.BadRequest(w, r, err.Error())
		return
	}

	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" {
		httpx.BadRequest(w, r, "A name is required.")
		return
	}

	cred := &store.GitCredential{
		Name: req.Name, Kind: req.Kind, Username: strings.TrimSpace(req.Username),
		HostKey: strings.TrimSpace(req.HostKey),
	}

	switch req.Kind {
	case store.GitToken:
		if req.Token == "" {
			httpx.BadRequest(w, r, "An access token is required.")
			return
		}
		sealed, err := s.sealer.Seal(req.Token)
		if err != nil {
			httpx.Error(w, r, err)
			return
		}
		cred.TokenEnc = sealed

	case store.GitSSH:
		// The key material lives in the shared SSH-key store now; a git credential only points at
		// one. Confirm it exists so a dangling reference cannot be saved.
		if req.SSHKeyID == "" {
			httpx.BadRequest(w, r, "Choose an SSH key. Generate or import one under Settings → SSH keys first.")
			return
		}
		if _, err := s.store.SSHKeyByID(r.Context(), req.SSHKeyID); errors.Is(err, store.ErrNotFound) {
			httpx.BadRequest(w, r, "That SSH key no longer exists.")
			return
		} else if err != nil {
			httpx.Error(w, r, err)
			return
		}
		cred.SSHKeyID = req.SSHKeyID

	default:
		httpx.BadRequest(w, r, "The credential must be a token or an ssh key.")
		return
	}

	if u, ok := auth.UserFrom(r.Context()); ok {
		cred.CreatedBy = u.ID
	}
	if err := s.store.CreateGitCredential(r.Context(), cred); err != nil {
		httpx.Fail(w, r, http.StatusConflict, "name_taken", "A git credential with that name already exists.")
		return
	}

	s.audit(r.Context(), store.AuditEntry{
		Action: "gitcred.create", Target: cred.Name, Outcome: "ok",
		Detail: store.AuditDetail(map[string]any{"kind": cred.Kind, "host_key_pinned": cred.HostKey != ""}),
	})
	httpx.JSON(w, http.StatusOK, map[string]string{"id": cred.ID})
}

func (s *Server) handleDeleteGitCredential(w http.ResponseWriter, r *http.Request) {
	cred, err := s.store.GitCredentialByID(r.Context(), r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		httpx.Fail(w, r, http.StatusNotFound, "no_such_credential", "No such git credential.")
		return
	}
	if err != nil {
		httpx.Error(w, r, err)
		return
	}

	// Refuse rather than orphan: a stack whose credential vanished fails at its next
	// deploy, which is a bad time to find out.
	n, err := s.store.GitCredentialInUse(r.Context(), cred.ID)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	if n > 0 {
		httpx.Fail(w, r, http.StatusConflict, "in_use",
			"That credential is used by stacks. Point them elsewhere first.")
		return
	}

	if err := s.store.DeleteGitCredential(r.Context(), cred.ID); err != nil {
		httpx.Error(w, r, err)
		return
	}
	s.audit(r.Context(), store.AuditEntry{
		Action: "gitcred.delete", Target: cred.Name, Outcome: "ok",
	})
	httpx.JSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// gitAuth resolves a stack's credential to plaintext, just in time. The token and the
// private key exist in memory only for the length of the clone.
func (s *Server) gitAuth(ctx context.Context, credID string) (*stacks.GitAuth, error) {
	if credID == "" {
		return nil, nil // a public repository
	}

	c, err := s.store.GitCredentialByID(ctx, credID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, errors.New("the git credential for this stack no longer exists")
	}
	if err != nil {
		return nil, err
	}
	return s.openGitCred(ctx, c)
}

// openGitCred resolves a credential to the plaintext auth material, just in time. A token comes
// from the credential itself; an SSH key comes from the shared key store the credential points at
// (its private half lives there, never on the credential). Everything exists in memory only for
// the length of the clone.
func (s *Server) openGitCred(ctx context.Context, c *store.GitCredential) (*stacks.GitAuth, error) {
	token, err := s.sealer.Open(c.TokenEnc)
	if err != nil {
		return nil, errors.New("could not decrypt the git token (was the master key replaced?)")
	}

	auth := &stacks.GitAuth{Kind: c.Kind, Username: c.Username, Token: token, HostKey: c.HostKey}

	if c.Kind == store.GitSSH {
		if c.SSHKeyID == "" {
			return nil, errors.New("this SSH git credential has no key — re-select one under Settings → SSH keys")
		}
		key, err := s.store.SSHKeyByID(ctx, c.SSHKeyID)
		if errors.Is(err, store.ErrNotFound) {
			return nil, errors.New("the SSH key this credential uses no longer exists")
		}
		if err != nil {
			return nil, err
		}
		priv, err := s.sealer.Open(key.PrivateKeyEnc)
		if err != nil {
			return nil, errors.New("could not decrypt the SSH key (was the master key replaced?)")
		}
		pass, err := s.sealer.Open(key.PassphraseEnc)
		if err != nil {
			return nil, errors.New("could not decrypt the SSH key passphrase (was the master key replaced?)")
		}
		auth.SSHKey, auth.Passphrase = priv, pass
	}

	return auth, nil
}
