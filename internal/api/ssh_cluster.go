package api

import (
	"context"
	"errors"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/Mnshahawy/daffa/internal/dockerx"
	"github.com/Mnshahawy/daffa/internal/httpx"
	"github.com/Mnshahawy/daffa/internal/sshx"
	"github.com/Mnshahawy/daffa/internal/store"
)

// sshManager is the server-side mirror of the agents registry. An agent dials IN and the server
// holds its tunnel; an SSH cluster is the inverse — the server dials OUT and holds the client. So
// this owns two things per SSH node: the connect LOOP (a cancel func, mirroring the agent's
// client-side connectLoop) and the live *ssh.Client (so a swarm move can re-register it, the way
// the agents registry serves reconnectNode).
type sshManager struct {
	mu      sync.Mutex
	loops   map[string]context.CancelFunc // node id → its connect loop's cancel
	clients map[string]*ssh.Client        // node id → live client, while connected
}

func newSSHManager() *sshManager {
	return &sshManager{loops: map[string]context.CancelFunc{}, clients: map[string]*ssh.Client{}}
}

func (m *sshManager) setClient(nodeID string, c *ssh.Client) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.clients[nodeID] = c
}

func (m *sshManager) clearClient(nodeID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.clients, nodeID)
}

func (m *sshManager) client(nodeID string) (*ssh.Client, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.clients[nodeID]
	return c, ok
}

// watchSSHClusters is the boot worker: it starts a connect loop for every persisted SSH node and,
// on a slow ticker, re-checks that each still has one — a safety net for a loop that ended for any
// reason other than a deliberate stop. Adding a cluster starts its loop directly, so this is not
// the only path in.
func (s *Server) watchSSHClusters(ctx context.Context) {
	s.ensureAllSSHLoops(ctx)

	t := time.NewTicker(60 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.ensureAllSSHLoops(ctx)
		}
	}
}

func (s *Server) ensureAllSSHLoops(ctx context.Context) {
	nodes, err := s.store.SSHNodes(ctx)
	if err != nil {
		slog.Warn("listing ssh clusters", "err", err)
		return
	}
	for _, n := range nodes {
		s.ensureSSHLoop(n.ID)
	}
}

// ensureSSHLoop starts a connect loop for a node unless one is already running. It is idempotent,
// so both boot and the add handler can call it. The loop is parented on workerCtx, so Stop winds
// it down with the rest of the background work.
func (s *Server) ensureSSHLoop(nodeID string) {
	s.ssh.mu.Lock()
	if _, running := s.ssh.loops[nodeID]; running {
		s.ssh.mu.Unlock()
		return
	}
	parent := s.workerCtx
	if parent == nil {
		parent = context.Background() // Start not yet called (tests); the loop still self-cancels
	}
	ctx, cancel := context.WithCancel(parent)
	s.ssh.loops[nodeID] = cancel
	s.ssh.mu.Unlock()

	s.spawn(ctx, func(ctx context.Context) {
		s.sshConnectLoop(ctx, nodeID)
		s.ssh.mu.Lock()
		delete(s.ssh.loops, nodeID)
		s.ssh.mu.Unlock()
	})
}

// stopSSHLoop cancels a node's connect loop — used when its cluster is removed. Cancelling the
// context makes sshConnectLoop return and tear the client out of the pool.
func (s *Server) stopSSHLoop(nodeID string) {
	s.ssh.mu.Lock()
	cancel := s.ssh.loops[nodeID]
	delete(s.ssh.loops, nodeID)
	s.ssh.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// sshConnectLoop keeps one SSH cluster connected, retrying with jittered backoff — the exact shape
// of the agent's client-side connectLoop, inverted to run on the server (docs/clusters.md §4).
func (s *Server) sshConnectLoop(ctx context.Context, nodeID string) {
	backoff := time.Second
	for {
		if ctx.Err() != nil {
			return
		}
		connected, err := s.sshConnectOnce(ctx, nodeID)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			slog.Warn("ssh cluster connection ended", "node", nodeID, "err", err)
		}
		if connected {
			backoff = time.Second // a good, long-lived connection resets the penalty
		}
		// Jitter so a fleet of clusters coming back after a shared outage does not reconnect in
		// lockstep and hammer the box.
		jittered := backoff + time.Duration(rand.Int64N(int64(backoff/2+1)))
		select {
		case <-ctx.Done():
			return
		case <-time.After(jittered):
		}
		if backoff < 30*time.Second {
			backoff *= 2
		}
		if backoff > 30*time.Second {
			backoff = 30 * time.Second
		}
	}
}

// sshConnectOnce establishes one connection and blocks until it dies (or ctx is cancelled). It
// returns whether a live connection was established, so the loop can reset its backoff.
func (s *Server) sshConnectOnce(ctx context.Context, nodeID string) (bool, error) {
	node, err := s.store.NodeByID(ctx, nodeID)
	if errors.Is(err, store.ErrNotFound) {
		return false, nil // deleted; the loop's ctx will be cancelled by stopSSHLoop
	}
	if err != nil || node.Kind != "ssh" {
		return false, err
	}

	cfg, err := s.sshDialConfig(ctx, node)
	if err != nil {
		_ = s.store.SetNodeStatus(ctx, nodeID, "offline")
		return false, err
	}

	client, pinned, err := sshx.Connect(ctx, cfg)
	if err != nil {
		_ = s.store.SetNodeStatus(ctx, nodeID, "offline")
		return false, err
	}
	defer client.Close()

	// Trust on first use: record the host key we saw so a later change is caught (§7).
	if node.SSHHostKey == "" && pinned != "" {
		_ = s.store.SetNodeHostKey(ctx, nodeID, pinned)
	}

	env, err := s.store.EnvironmentByID(ctx, node.EnvID)
	if err != nil {
		return false, err
	}
	if err := s.pool.RegisterSSH(env, node, sshx.SocketDialer(client, node.SSHEndpoint)); err != nil {
		return false, err
	}
	s.ssh.setClient(nodeID, client)
	_ = s.store.SetNodeStatus(ctx, nodeID, "online")

	// Now that we can reach the daemon, discover what it is — the same reconcile the agent path
	// runs, so a remote Swarm we already know is joined to its environment here.
	s.reconcileNode(ctx, env.ID, mustNode(s.pool, env.ID, node.ID))

	// Block until the connection dies or we are cancelled.
	wait := make(chan error, 1)
	go func() { wait <- client.Wait() }()
	select {
	case <-ctx.Done():
	case <-wait:
	}

	// Tear down. Re-read the node in case reconcile moved it into a swarm environment, so we
	// deregister from wherever it actually is. context.WithoutCancel: cleanup must run even though
	// the loop's context is the thing that just ended.
	clean := context.WithoutCancel(ctx)
	s.ssh.clearClient(nodeID)
	envID := env.ID
	if cur, err := s.store.NodeByID(clean, nodeID); err == nil {
		envID = cur.EnvID
	}
	s.pool.Deregister(envID, nodeID)
	_ = s.store.SetNodeStatus(clean, nodeID, "offline")
	return true, nil
}

// sshDialConfig assembles a dial config for a node, unsealing its key just in time. The private
// key lives in memory only for the length of the connection.
func (s *Server) sshDialConfig(ctx context.Context, node *store.Node) (sshx.DialConfig, error) {
	key, err := s.store.SSHKeyByID(ctx, node.SSHKeyID)
	if errors.Is(err, store.ErrNotFound) {
		return sshx.DialConfig{}, errors.New("the SSH key for this cluster no longer exists")
	}
	if err != nil {
		return sshx.DialConfig{}, err
	}
	priv, err := s.sealer.Open(key.PrivateKeyEnc)
	if err != nil {
		return sshx.DialConfig{}, errors.New("could not decrypt the SSH key (was the master key replaced?)")
	}
	pass, err := s.sealer.Open(key.PassphraseEnc)
	if err != nil {
		return sshx.DialConfig{}, errors.New("could not decrypt the SSH key passphrase (was the master key replaced?)")
	}
	return sshx.DialConfig{
		Host:          node.SSHHost,
		Port:          node.SSHPort,
		User:          node.SSHUser,
		PrivateKeyPEM: priv,
		Passphrase:    pass,
		KnownHostKey:  node.SSHHostKey,
	}, nil
}

// ── handlers ──────────────────────────────────────────────────────────────────

type sshClusterRequest struct {
	Name     string `json:"name"`
	Host     string `json:"host"`
	Port     int    `json:"port"`      // 0 ⇒ 22
	User     string `json:"user"`      // the SSH user; needs Docker-socket access on the target
	KeyID    string `json:"key_id"`    // an ssh_keys id
	Endpoint string `json:"endpoint"`  // remote Docker endpoint; "" ⇒ unix:///var/run/docker.sock
	HostKey  string `json:"host_key"`  // optional pin; "" ⇒ trust on first use
}

// sshTestResponse is a diagnostic, not an API outcome: it comes back 200 whether the dial worked
// or not, so the wizard renders pass/fail inline. On success it echoes what the daemon reports, so
// the operator can confirm they reached the machine they meant to.
type sshTestResponse struct {
	OK            bool   `json:"ok"`
	Error         string `json:"error,omitempty"`
	ServerVersion string `json:"server_version,omitempty"`
	OS            string `json:"os,omitempty"`
	Arch          string `json:"arch,omitempty"`
	HostKey       string `json:"host_key,omitempty"` // the key pinned on first use, for display
}

func (r sshClusterRequest) toDialConfig(privPEM, pass string) sshx.DialConfig {
	return sshx.DialConfig{
		Host: strings.TrimSpace(r.Host), Port: r.Port, User: strings.TrimSpace(r.User),
		PrivateKeyPEM: privPEM, Passphrase: pass, KnownHostKey: strings.TrimSpace(r.HostKey),
	}
}

// dialForRequest resolves the key and dials, shared by test and create. It returns the live
// client and the pinned host-key line; the caller closes the client.
func (s *Server) dialForRequest(ctx context.Context, req sshClusterRequest) (*ssh.Client, string, error) {
	if strings.TrimSpace(req.Host) == "" || strings.TrimSpace(req.User) == "" || req.KeyID == "" {
		return nil, "", errors.New("a host, a user and an SSH key are required")
	}
	key, err := s.store.SSHKeyByID(ctx, req.KeyID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, "", errors.New("that SSH key no longer exists")
	}
	if err != nil {
		return nil, "", err
	}
	priv, err := s.sealer.Open(key.PrivateKeyEnc)
	if err != nil {
		return nil, "", errors.New("could not decrypt the SSH key")
	}
	pass, err := s.sealer.Open(key.PassphraseEnc)
	if err != nil {
		return nil, "", errors.New("could not decrypt the SSH key passphrase")
	}
	return sshx.Connect(ctx, req.toDialConfig(priv, pass))
}

// handleTestSSHConnection dials and reads the remote daemon's Info without persisting anything —
// the "Test" button in the Add-cluster wizard, and the git-credential test's analog.
func (s *Server) handleTestSSHConnection(w http.ResponseWriter, r *http.Request) {
	var req sshClusterRequest
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.BadRequest(w, r, err.Error())
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
	defer cancel()

	client, pinned, err := s.dialForRequest(ctx, req)
	if err != nil {
		httpx.JSON(w, http.StatusOK, sshTestResponse{OK: false, Error: friendlySSHError(err)})
		return
	}
	defer client.Close()

	info, err := dockerx.Probe(ctx, sshx.SocketDialer(client, req.Endpoint))
	if err != nil {
		httpx.JSON(w, http.StatusOK, sshTestResponse{
			OK: false, HostKey: pinned,
			Error: "Reached the machine over SSH, but its Docker daemon did not answer at " +
				endpointOrDefault(req.Endpoint) + ": " + err.Error(),
		})
		return
	}
	httpx.JSON(w, http.StatusOK, sshTestResponse{
		OK: true, ServerVersion: info.ServerVersion, OS: info.OS, Arch: info.Arch, HostKey: pinned,
	})
}

type sshClusterCreatedResponse struct {
	ID     string `json:"id"`      // the new environment id
	NodeID string `json:"node_id"` // the ssh node id
}

// handleCreateSSHCluster adds a remote cluster: dial (pinning the host key), confirm the daemon
// answers, persist the environment + its SSH node, and start the connect loop that keeps it up.
func (s *Server) handleCreateSSHCluster(w http.ResponseWriter, r *http.Request) {
	var req sshClusterRequest
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.BadRequest(w, r, err.Error())
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" || len(req.Name) > 40 {
		httpx.BadRequest(w, r, "A name is required (up to 40 characters).")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
	defer cancel()

	// Dial before persisting: an unreachable machine should fail here, not become a stored cluster
	// that never connects. This also pins the host key (§7).
	client, pinned, err := s.dialForRequest(ctx, req)
	if err != nil {
		httpx.Fail(w, r, http.StatusBadRequest, "ssh_unreachable", friendlySSHError(err))
		return
	}
	if _, err := dockerx.Probe(ctx, sshx.SocketDialer(client, req.Endpoint)); err != nil {
		client.Close()
		httpx.Fail(w, r, http.StatusBadRequest, "docker_unreachable",
			"Reached the machine, but its Docker daemon did not answer: "+err.Error())
		return
	}
	client.Close() // the connect loop opens the long-lived connection

	node := &store.Node{
		SSHHost: strings.TrimSpace(req.Host), SSHPort: req.Port, SSHUser: strings.TrimSpace(req.User),
		SSHKeyID: req.KeyID, SSHEndpoint: strings.TrimSpace(req.Endpoint), SSHHostKey: pinned,
	}
	env, node, err := s.store.CreateSSHNode(r.Context(), req.Name, node)
	if err != nil {
		httpx.Fail(w, r, http.StatusConflict, "name_taken", "A cluster with that name already exists.")
		return
	}

	s.ensureSSHLoop(node.ID)

	s.audit(r.Context(), store.AuditEntry{
		EnvID: env.ID, Action: "cluster.create", Target: req.Name, Outcome: "ok",
		Detail: store.AuditDetail(map[string]string{
			"transport": "ssh", "host": node.SSHHost, "user": node.SSHUser,
		}),
	})
	httpx.JSON(w, http.StatusOK, sshClusterCreatedResponse{ID: env.ID, NodeID: node.ID})
}

// handleDeleteCluster removes an SSH cluster: stop its connect loop, drop it from the pool, and
// delete the environment and its node. The local cluster cannot be removed — it is the box Daffa
// runs on. Agent-backed clusters are removed by revoking their agent, not here.
func (s *Server) handleDeleteCluster(w http.ResponseWriter, r *http.Request) {
	envID := r.PathValue("cluster")
	env, err := s.store.EnvironmentByID(r.Context(), envID)
	if errors.Is(err, store.ErrNotFound) {
		httpx.Fail(w, r, http.StatusNotFound, "no_such_cluster", "No such cluster.")
		return
	}
	if err != nil {
		httpx.Error(w, r, err)
		return
	}

	nodes, err := s.store.NodesByEnv(r.Context(), envID)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	for _, n := range nodes {
		if n.Kind != "ssh" {
			httpx.Fail(w, r, http.StatusConflict, "not_ssh_cluster",
				"Only clusters added over SSH are removed here. The local cluster stays, and an "+
					"agent-backed one is removed by revoking its agent.")
			return
		}
	}

	for _, n := range nodes {
		s.stopSSHLoop(n.ID)
		s.ssh.clearClient(n.ID)
		s.pool.Deregister(envID, n.ID)
	}
	if err := s.store.DeleteEnvironment(r.Context(), envID); err != nil {
		httpx.Error(w, r, err)
		return
	}
	s.audit(r.Context(), store.AuditEntry{
		EnvID: envID, Action: "cluster.delete", Target: env.Name, Outcome: "ok",
	})
	httpx.JSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func endpointOrDefault(e string) string {
	if strings.TrimSpace(e) == "" {
		return "unix:///var/run/docker.sock"
	}
	return e
}

// friendlySSHError is defined below; the routes for these handlers live in the apiRoutes table in
// server.go (POST /api/clusters, POST /api/clusters/test-connection, DELETE /api/clusters/{cluster}).

// friendlySSHError turns the common dial failures into something an operator can act on, and
// names the host-key-changed case specifically because it is a security event, not a typo.
func friendlySSHError(err error) string {
	switch {
	case errors.Is(err, sshx.ErrHostKeyChanged):
		return "The machine's SSH host key changed since it was last pinned. If this is expected " +
			"(the host was rebuilt), remove the cluster and add it again; otherwise treat it as a warning."
	case errors.Is(err, sshx.ErrPassphraseRequired):
		return "That SSH key is passphrase-protected but no passphrase is stored with it."
	default:
		return err.Error()
	}
}
