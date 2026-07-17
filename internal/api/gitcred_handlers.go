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
		out = append(out, gitCredView{
			ID: c.ID, Name: c.Name, Kind: c.Kind, Username: c.Username,
			Pinned: c.HostKey != "", InUse: n,
		})
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

type gitCredRequest struct {
	Name       string `json:"name"`
	Kind       string `json:"kind"`
	Username   string `json:"username"`
	Token      string `json:"token"`
	SSHKey     string `json:"ssh_key"`
	Passphrase string `json:"passphrase"`
	HostKey    string `json:"host_key"`
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
		key := strings.TrimSpace(req.SSHKey)
		if key == "" {
			httpx.BadRequest(w, r, "An SSH private key is required.")
			return
		}
		if strings.HasPrefix(key, "ssh-") || strings.HasPrefix(key, "ecdsa-") {
			httpx.BadRequest(w, r,
				"That is an SSH PUBLIC key. Paste the PRIVATE key (the file without .pub) — "+
					"the public half is what you add to the repository as a deploy key.")
			return
		}

		// Parse it now, with the passphrase, so a wrong passphrase or a mangled paste is
		// an error the person sees while they still have the key in their clipboard.
		if err := stacks.CheckSSHKey(key, req.Passphrase, cred.HostKey); err != nil {
			httpx.Fail(w, r, http.StatusBadRequest, "bad_key", err.Error())
			return
		}

		sealedKey, err := s.sealer.Seal(key + "\n")
		if err != nil {
			httpx.Error(w, r, err)
			return
		}
		sealedPass, err := s.sealer.Seal(req.Passphrase)
		if err != nil {
			httpx.Error(w, r, err)
			return
		}
		cred.SSHKeyEnc, cred.PassphraseEnc = sealedKey, sealedPass

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
	return s.openGitCred(c)
}

func (s *Server) openGitCred(c *store.GitCredential) (*stacks.GitAuth, error) {
	token, err := s.sealer.Open(c.TokenEnc)
	if err != nil {
		return nil, errors.New("could not decrypt the git token (was the master key replaced?)")
	}
	key, err := s.sealer.Open(c.SSHKeyEnc)
	if err != nil {
		return nil, errors.New("could not decrypt the SSH key (was the master key replaced?)")
	}
	pass, err := s.sealer.Open(c.PassphraseEnc)
	if err != nil {
		return nil, errors.New("could not decrypt the key passphrase (was the master key replaced?)")
	}

	return &stacks.GitAuth{
		Kind: c.Kind, Username: c.Username, Token: token,
		SSHKey: key, Passphrase: pass, HostKey: c.HostKey,
	}, nil
}
