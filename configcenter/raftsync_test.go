package configcenter

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"nexus/raft"
)

// newRaftServer builds a single-node Raft cluster (which is immediately its own
// leader) backed by an isolated store and wired into the HTTP server.
func newRaftServer(t *testing.T) (*Server, *Store, *httptest.Server, *raft.RaftNode) {
	t.Helper()
	store, err := OpenStore(DatabaseConfig{Driver: "sqlite3", DSN: filepath.Join(t.TempDir(), "test.db")})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	node, err := NewRaftNode(store, "127.0.0.1", nil)
	if err != nil {
		t.Fatalf("new raft node: %v", err)
	}
	if !node.IsLeader() {
		t.Fatal("single-node cluster should elect itself leader")
	}

	server := NewServer(store, adminToken)
	server.SetRaft(node)
	mux := http.NewServeMux()
	server.RegisterRoutes(mux)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return server, store, ts, node
}

func TestConfigWriteCommitsThroughRaft(t *testing.T) {
	_, store, ts, node := newRaftServer(t)

	body := `{"namespace":"ns","key":"k","format":"json","value":"{\"a\":1}"}`
	status, raw, _ := do(t, ts, http.MethodPost, "/api/v1/configs", adminToken, body)
	if status != http.StatusOK {
		t.Fatalf("status = %d, body = %s", status, raw)
	}

	// The command was applied to the state machine.
	cfg, err := store.Get("ns", "k")
	if err != nil {
		t.Fatalf("state machine did not apply the write: %v", err)
	}
	if cfg.Value != `{"a":1}` || cfg.Format != FormatJSON {
		t.Fatalf("unexpected config: %+v", cfg)
	}

	// It was recorded in the replicated log.
	entries, err := store.LoadLogEntries()
	if err != nil {
		t.Fatalf("load log: %v", err)
	}
	if len(entries) != 1 || entries[0].Comm.OpType != OpConfigPut {
		t.Fatalf("unexpected log: %+v", entries)
	}

	status2 := node.Status()
	if got := status2["last_applied"].(int); got != 0 {
		t.Fatalf("last_applied = %d, want 0", got)
	}
	if got := status2["commit_index"].(int); got != 0 {
		t.Fatalf("commit_index = %d, want 0", got)
	}
}

func TestConfigUpdateAndDeleteThroughRaft(t *testing.T) {
	_, store, ts, _ := newRaftServer(t)

	// Create then update: the second apply must bump the version.
	do(t, ts, http.MethodPost, "/api/v1/configs", adminToken,
		`{"namespace":"ns","key":"k","format":"text","value":"v1"}`)
	status, raw, _ := do(t, ts, http.MethodPost, "/api/v1/configs", adminToken,
		`{"namespace":"ns","key":"k","format":"text","value":"v2"}`)
	if status != http.StatusOK {
		t.Fatalf("update status = %d, body = %s", status, raw)
	}
	cfg, err := store.Get("ns", "k")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if cfg.Value != "v2" || cfg.Version != 2 {
		t.Fatalf("unexpected config after update: %+v", cfg)
	}

	// Delete is replicated too.
	status, raw, _ = do(t, ts, http.MethodDelete, "/api/v1/configs/ns/k", adminToken, "")
	if status != http.StatusNoContent {
		t.Fatalf("delete status = %d, body = %s", status, raw)
	}
	if _, err := store.Get("ns", "k"); err != ErrNotFound {
		t.Fatalf("config still present after delete: err = %v", err)
	}
	entries, _ := store.LoadLogEntries()
	if len(entries) != 3 || entries[2].Comm.OpType != OpConfigDelete {
		t.Fatalf("unexpected log: %+v", entries)
	}
}

func TestConfigInvalidValueRejectedBeforeReplication(t *testing.T) {
	_, store, ts, _ := newRaftServer(t)

	// Invalid JSON must be rejected by the leader before it enters the log.
	status, _, _ := do(t, ts, http.MethodPost, "/api/v1/configs", adminToken,
		`{"namespace":"ns","key":"bad","format":"json","value":"{oops}"}`)
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", status)
	}
	entries, _ := store.LoadLogEntries()
	if len(entries) != 0 {
		t.Fatalf("invalid command reached the log: %+v", entries)
	}
}

func TestConfigWriteOnFollowerRedirectsToLeader(t *testing.T) {
	store, err := OpenStore(DatabaseConfig{Driver: "sqlite3", DSN: filepath.Join(t.TempDir(), "test.db")})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	// A node with a peer is a follower, not a leader.
	node := raft.InitRaftNode("127.0.0.1", []string{"127.0.0.2:11451"})
	node.LeaderIP = "127.0.0.2" // learned from an earlier heartbeat

	server := NewServer(store, adminToken)
	server.SetRaft(node)
	mux := http.NewServeMux()
	server.RegisterRoutes(mux)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	// Use a client that does not follow the redirect, so the 307 itself is
	// observable.
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	req, err := http.NewRequest(http.MethodPost, ts.URL+"/api/v1/configs",
		strings.NewReader(`{"namespace":"ns","key":"k","format":"text","value":"v"}`))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Auth", adminToken)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusTemporaryRedirect {
		t.Fatalf("status = %d, want 307", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "http://127.0.0.2:11451/api/v1/configs" {
		t.Fatalf("Location = %q, want http://127.0.0.2:11451/api/v1/configs", loc)
	}
	var body map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&body)
	if body["leader"] != "127.0.0.2:11451" {
		t.Fatalf("leader = %v, want 127.0.0.2:11451", body["leader"])
	}
	if _, err := store.Get("ns", "k"); err != ErrNotFound {
		t.Fatalf("follower wrote locally: err = %v", err)
	}
}

func opTypes(t *testing.T, store *Store) []string {
	t.Helper()
	entries, err := store.LoadLogEntries()
	if err != nil {
		t.Fatalf("load log: %v", err)
	}
	types := make([]string, 0, len(entries))
	for _, e := range entries {
		types = append(types, e.Comm.OpType)
	}
	return types
}

func TestRoleLifecycleThroughRaft(t *testing.T) {
	_, store, ts, _ := newRaftServer(t)

	status, raw, body := do(t, ts, http.MethodPost, "/api/v1/roles", adminToken,
		`{"name":"ops","description":"operators","namespaces":["team-a"]}`)
	if status != http.StatusCreated {
		t.Fatalf("create status = %d, body = %s", status, raw)
	}
	roleID := uint(body["id"].(float64))
	role, err := store.RoleByID(roleID)
	if err != nil {
		t.Fatalf("role not applied: %v", err)
	}
	if role.Token == "" || role.Name != "ops" {
		t.Fatalf("unexpected role: %+v", role)
	}
	if names := role.NamespaceList(); len(names) != 1 || names[0] != "team-a" {
		t.Fatalf("namespaces = %v, want [team-a]", names)
	}
	// The granted namespace is created as a side effect of the role command.
	if ns, _ := store.ListNamespaces(); len(ns) != 1 || ns[0] != "team-a" {
		t.Fatalf("namespaces = %v, want [team-a]", ns)
	}

	// Update: rename and move to another namespace.
	status, raw, _ = do(t, ts, http.MethodPut, "/api/v1/roles/"+roleIDString(body), adminToken,
		`{"name":"ops2","description":"updated","namespaces":["team-b"]}`)
	if status != http.StatusOK {
		t.Fatalf("update status = %d, body = %s", status, raw)
	}
	role, _ = store.RoleByID(roleID)
	if role.Name != "ops2" || role.Description != "updated" {
		t.Fatalf("role not updated: %+v", role)
	}
	if names := role.NamespaceList(); len(names) != 1 || names[0] != "team-b" {
		t.Fatalf("namespaces = %v, want [team-b]", names)
	}

	// Rotate: the token must change and stay persisted.
	oldToken := role.Token
	status, raw, _ = do(t, ts, http.MethodPost, "/api/v1/roles/"+roleIDString(body)+"/rotate", adminToken, `{}`)
	if status != http.StatusOK {
		t.Fatalf("rotate status = %d, body = %s", status, raw)
	}
	role, _ = store.RoleByID(roleID)
	if role.Token == oldToken || role.Token == "" {
		t.Fatalf("token not rotated: %q -> %q", oldToken, role.Token)
	}

	// Delete.
	status, raw, _ = do(t, ts, http.MethodDelete, "/api/v1/roles/"+roleIDString(body), adminToken, "")
	if status != http.StatusNoContent {
		t.Fatalf("delete status = %d, body = %s", status, raw)
	}
	if _, err := store.RoleByID(roleID); err != ErrNotFound {
		t.Fatalf("role still present: err = %v", err)
	}

	want := []string{OpRoleCreate, OpRoleUpdate, OpRoleRotate, OpRoleDelete}
	got := opTypes(t, store)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("log op types = %v, want %v", got, want)
	}
}

// roleIDString renders the created role id from the decoded JSON body.
func roleIDString(body map[string]any) string {
	return strconv.FormatUint(uint64(body["id"].(float64)), 10)
}

func TestNamespaceDeleteRemovesConfigsAndGrants(t *testing.T) {
	_, store, ts, _ := newRaftServer(t)

	if status, _, _ := do(t, ts, http.MethodPost, "/api/v1/namespaces", adminToken, `{"name":"team-x"}`); status != http.StatusCreated {
		t.Fatalf("create namespace status = %d", status)
	}
	do(t, ts, http.MethodPost, "/api/v1/configs", adminToken,
		`{"namespace":"team-x","key":"k","format":"text","value":"v"}`)
	do(t, ts, http.MethodPost, "/api/v1/roles", adminToken, `{"name":"r1","namespaces":["team-x"]}`)

	// Delete the namespace: its config and the role grant must go too.
	status, raw, _ := do(t, ts, http.MethodDelete, "/api/v1/namespaces/team-x", adminToken, "")
	if status != http.StatusNoContent {
		t.Fatalf("delete namespace status = %d, body = %s", status, raw)
	}
	if _, err := store.Get("team-x", "k"); err != ErrNotFound {
		t.Fatalf("config survived namespace delete: err = %v", err)
	}
	if ns, _ := store.ListNamespaces(); len(ns) != 0 {
		t.Fatalf("namespaces = %v, want empty", ns)
	}
	role, err := store.RoleByName("r1")
	if err != nil {
		t.Fatalf("role missing: %v", err)
	}
	if names := role.NamespaceList(); len(names) != 0 {
		t.Fatalf("role grant survived namespace delete: %v", names)
	}
}

func TestNodeRegistrationThroughRaft(t *testing.T) {
	_, store, ts, _ := newRaftServer(t)

	// An unreachable address must be rejected before it enters the log;
	// otherwise the cluster would start requiring a vote from a dead node.
	status, _, _ := do(t, ts, http.MethodPost, "/api/v1/nodes", adminToken, `{"ip":"127.0.0.1","port":"1"}`)
	if status != http.StatusServiceUnavailable {
		t.Fatalf("unreachable register status = %d, want 503", status)
	}
	if got := opTypes(t, store); len(got) != 0 {
		t.Fatalf("unreachable registration reached the log: %v", got)
	}
	if _, err := store.NodeByAddress("127.0.0.1", "1"); err != ErrNotFound {
		t.Fatalf("unreachable node was stored: err = %v", err)
	}

	// A reachable node (the leader itself, for test purposes) is accepted and
	// replicated through the log.
	status, raw, body := do(t, ts, http.MethodPost, "/api/v1/nodes", adminToken, `{"ip":"127.0.0.1","port":"11451"}`)
	if status != http.StatusOK {
		t.Fatalf("register status = %d, body = %s", status, raw)
	}
	id := uint(body["id"].(float64))
	if _, err := store.NodeByID(id); err != nil {
		t.Fatalf("node not applied: %v", err)
	}

	// Re-registering the same address is idempotent and appends no command.
	before := len(opTypes(t, store))
	if status, _, _ := do(t, ts, http.MethodPost, "/api/v1/nodes", adminToken, `{"ip":"127.0.0.1","port":"11451"}`); status != http.StatusOK {
		t.Fatalf("re-register status = %d", status)
	}
	if after := len(opTypes(t, store)); after != before {
		t.Fatalf("duplicate registration appended a command: %d -> %d", before, after)
	}

	status, raw, _ = do(t, ts, http.MethodDelete, "/api/v1/nodes/"+strconv.FormatUint(uint64(id), 10), adminToken, "")
	if status != http.StatusNoContent {
		t.Fatalf("delete status = %d, body = %s", status, raw)
	}
	if _, err := store.NodeByID(id); err != ErrNotFound {
		t.Fatalf("node still present: err = %v", err)
	}
}

// TestMembershipCommandRefreshesPeers checks that applying a membership command
// updates the Raft node's peer list, which is what lets every node track the
// cluster without the HTTP server doing any extra wiring.
func TestMembershipCommandRefreshesPeers(t *testing.T) {
	store, err := OpenStore(DatabaseConfig{Driver: "sqlite3", DSN: filepath.Join(t.TempDir(), "test.db")})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	node := raft.InitRaftNode("127.0.0.1", nil)

	if _, err := store.RegisterNode("10.0.0.2", "11451"); err != nil {
		t.Fatalf("register node: %v", err)
	}
	cmd, err := newCommand(OpNodeRegister, nodeRegisterPayload{IP: "10.0.0.2", Port: "11451"})
	if err != nil {
		t.Fatalf("build command: %v", err)
	}
	if err := syncRaftPeers(store, node, cmd); err != nil {
		t.Fatalf("sync peers: %v", err)
	}
	peers := node.Nodes()
	if len(peers) != 1 || peers[0] != "10.0.0.2:11451" {
		t.Fatalf("peers = %v, want [10.0.0.2:11451]", peers)
	}

	// A non-membership command must not touch the peer list.
	if err := syncRaftPeers(store, node, raft.Command{OpType: OpConfigPut}); err != nil {
		t.Fatalf("sync peers (non-membership): %v", err)
	}
	if got := node.Nodes(); len(got) != 1 {
		t.Fatalf("peers changed on non-membership command: %v", got)
	}
}
