package api

import (
	"errors"
	"net/http"
	"strings"

	"github.com/Mnshahawy/daffa/internal/auth"
	"github.com/Mnshahawy/daffa/internal/httpx"
	"github.com/Mnshahawy/daffa/internal/sshx"
	"github.com/Mnshahawy/daffa/internal/store"
)

// sshKeyView is an SSH key as the client sees it: enough to identify and pick a key, and to
// copy its PUBLIC half into a target's authorized_keys. The private key is never here — not
// even sealed — which is the whole reason has_secret-style views exist.
type sshKeyView struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Algo        string `json:"algo"` // ed25519 | rsa | ecdsa
	PublicKey   string `json:"public_key"`
	Fingerprint string `json:"fingerprint"`
	// HasPassphrase says the sealed private key is itself passphrase-protected (an imported
	// key can be). Shown so an operator knows a bare copy of the sealed blob is still useless
	// without the passphrase Daffa also holds.
	HasPassphrase bool `json:"has_passphrase"`
	InUse         int  `json:"in_use"`
}

func (s *Server) handleListSSHKeys(w http.ResponseWriter, r *http.Request) {
	keys, err := s.store.ListSSHKeys(r.Context())
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	out := make([]sshKeyView, 0, len(keys))
	for _, k := range keys {
		n, _ := s.store.SSHKeyInUse(r.Context(), k.ID)
		out = append(out, sshKeyView{
			ID: k.ID, Name: k.Name, Algo: k.Algo, PublicKey: k.PublicKey,
			Fingerprint: k.Fingerprint, HasPassphrase: k.PassphraseEnc != "", InUse: n,
		})
	}
	httpx.JSON(w, http.StatusOK, out)
}

type sshKeyRequest struct {
	Name string `json:"name"`
	Mode string `json:"mode"` // generate | import
	// generate: the algorithm. "" or "ed25519" (default), or "rsa" (4096-bit).
	Algo string `json:"algo"`
	// import: the OpenSSH/PEM private key, and its passphrase if it has one.
	PrivateKey string `json:"private_key"`
	Passphrase string `json:"passphrase"`
}

// sshKeyCreateResponse returns the PUBLIC key on creation so the UI can show and copy it
// immediately — the one moment the operator needs it in hand, to paste into the target.
type sshKeyCreateResponse struct {
	ID          string `json:"id"`
	PublicKey   string `json:"public_key"`
	Fingerprint string `json:"fingerprint"`
}

func (s *Server) handleCreateSSHKey(w http.ResponseWriter, r *http.Request) {
	var req sshKeyRequest
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.BadRequest(w, r, err.Error())
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" || len(req.Name) > 60 {
		httpx.BadRequest(w, r, "A name is required (up to 60 characters).")
		return
	}

	key := &store.SSHKey{Name: req.Name}

	switch req.Mode {
	case "generate":
		// The name rides along as the key comment, so a key pasted into authorized_keys says
		// where it came from.
		km, err := sshx.Generate(req.Algo, "daffa:"+req.Name)
		if err != nil {
			httpx.BadRequest(w, r, err.Error())
			return
		}
		sealed, err := s.sealer.Seal(km.PrivatePEM)
		if err != nil {
			httpx.Error(w, r, err)
			return
		}
		// A generated key has no passphrase; seal the empty string so the column is uniform
		// (Open of "" returns "", never a decrypt error).
		sealedPass, err := s.sealer.Seal("")
		if err != nil {
			httpx.Error(w, r, err)
			return
		}
		key.Algo, key.PublicKey, key.Fingerprint = km.Algo, km.AuthorizedKey, km.Fingerprint
		key.PrivateKeyEnc, key.PassphraseEnc = sealed, sealedPass

	case "import":
		pem := strings.TrimSpace(req.PrivateKey)
		if pem == "" {
			httpx.BadRequest(w, r, "A private key is required to import.")
			return
		}
		if strings.HasPrefix(pem, "ssh-") || strings.HasPrefix(pem, "ecdsa-") {
			httpx.BadRequest(w, r,
				"That is an SSH PUBLIC key. Paste the PRIVATE key (the file without .pub) — "+
					"Daffa derives the public half from it.")
			return
		}
		// Parse now, with the passphrase, so a wrong passphrase or a mangled paste is an error
		// the operator sees while the key is still in their clipboard — and so we store the
		// public half Daffa computed, not one the client asserted.
		km, err := sshx.PublicFromPrivate(pem, req.Passphrase, "daffa:"+req.Name)
		if errors.Is(err, sshx.ErrPassphraseRequired) {
			httpx.Fail(w, r, http.StatusBadRequest, "passphrase_required",
				"This private key is encrypted. Enter its passphrase to import it.")
			return
		}
		if err != nil {
			httpx.Fail(w, r, http.StatusBadRequest, "bad_key", err.Error())
			return
		}
		sealedKey, err := s.sealer.Seal(pem + "\n")
		if err != nil {
			httpx.Error(w, r, err)
			return
		}
		sealedPass, err := s.sealer.Seal(req.Passphrase)
		if err != nil {
			httpx.Error(w, r, err)
			return
		}
		key.Algo, key.PublicKey, key.Fingerprint = km.Algo, km.AuthorizedKey, km.Fingerprint
		key.PrivateKeyEnc, key.PassphraseEnc = sealedKey, sealedPass

	default:
		httpx.BadRequest(w, r, "The key must be generated or imported.")
		return
	}

	if u, ok := auth.UserFrom(r.Context()); ok {
		key.CreatedBy = u.ID
	}
	if err := s.store.CreateSSHKey(r.Context(), key); err != nil {
		httpx.Fail(w, r, http.StatusConflict, "name_taken", "An SSH key with that name already exists.")
		return
	}

	s.audit(r.Context(), store.AuditEntry{
		Action: "sshkey.create", Target: key.Name, Outcome: "ok",
		Detail: store.AuditDetail(map[string]any{
			"mode": req.Mode, "algo": key.Algo, "fingerprint": key.Fingerprint,
		}),
	})
	httpx.JSON(w, http.StatusOK, sshKeyCreateResponse{
		ID: key.ID, PublicKey: key.PublicKey, Fingerprint: key.Fingerprint,
	})
}

func (s *Server) handleDeleteSSHKey(w http.ResponseWriter, r *http.Request) {
	key, err := s.store.SSHKeyByID(r.Context(), r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		httpx.Fail(w, r, http.StatusNotFound, "no_such_key", "No such SSH key.")
		return
	}
	if err != nil {
		httpx.Error(w, r, err)
		return
	}

	// Refuse rather than orphan: a cluster or node that authenticates with this key would
	// fail to reconnect the moment it vanished.
	n, err := s.store.SSHKeyInUse(r.Context(), key.ID)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	if n > 0 {
		httpx.Fail(w, r, http.StatusConflict, "in_use",
			"That key is in use by a cluster or node. Point them at another key first.")
		return
	}

	if err := s.store.DeleteSSHKey(r.Context(), key.ID); err != nil {
		httpx.Error(w, r, err)
		return
	}
	s.audit(r.Context(), store.AuditEntry{
		Action: "sshkey.delete", Target: key.Name, Outcome: "ok",
	})
	httpx.JSON(w, http.StatusOK, map[string]string{"status": "ok"})
}
