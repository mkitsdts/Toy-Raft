package configcenter

import (
	"crypto/subtle"
	"embed"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net"
	"net/http"
	"strconv"
	"strings"
)

const maxBodySize = 1 << 20 // 1 MiB

//go:embed web
var webFS embed.FS

// principal is the authenticated caller: either the admin (master token) or a
// role whose token grants read access to a fixed set of namespaces.
type principal struct {
	admin      bool
	role       *Role
	namespaces map[string]bool // role tokens: granted namespaces
}

func (p *principal) canRead(namespace string) bool {
	return p.admin || p.namespaces[namespace]
}

type Server struct {
	store *Store
	token string
	nodes NodeRegistrar
}

// NodeRegistrar receives the full peer address list whenever the set of
// registered Raft nodes changes. *raft.RaftNode implements it, which lets the
// running cluster pick up newly registered nodes without a restart.
type NodeRegistrar interface {
	SetNodes(addrs []string)
}

type nodeRequest struct {
	IP   string `json:"ip"`
	Port string `json:"port"`
}

type registerRequest struct {
	Namespace string `json:"namespace"`
	Key       string `json:"key"`
	Value     string `json:"value"`
	Format    string `json:"format"`
}

type roleRequest struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Namespaces  []string `json:"namespaces"`
}

func NewServer(store *Store, token string) *Server {
	return &Server{store: store, token: token}
}

// SetNodeRegistrar wires the running Raft node so node registrations take
// effect immediately. It may be left unset in tests.
func (s *Server) SetNodeRegistrar(r NodeRegistrar) { s.nodes = r }

func (s *Server) RegisterRoutes(mux *http.ServeMux) {
	// API
	mux.Handle("GET /api/v1/whoami", s.auth(http.HandlerFunc(s.handleWhoami)))
	mux.Handle("GET /api/v1/configs", s.auth(http.HandlerFunc(s.handleListConfigs)))
	mux.Handle("POST /api/v1/configs", s.auth(s.requireAdmin(http.HandlerFunc(s.handleCreateConfig))))
	mux.Handle("GET /api/v1/configs/{namespace}/{key}", s.auth(http.HandlerFunc(s.handleGetConfig)))
	mux.Handle("DELETE /api/v1/configs/{namespace}/{key}", s.auth(s.requireAdmin(http.HandlerFunc(s.handleDeleteConfig))))
	mux.Handle("GET /api/v1/namespaces", s.auth(http.HandlerFunc(s.handleListNamespaces)))
	mux.Handle("GET /api/v1/roles", s.auth(s.requireAdmin(http.HandlerFunc(s.handleListRoles))))
	mux.Handle("POST /api/v1/roles", s.auth(s.requireAdmin(http.HandlerFunc(s.handleCreateRole))))
	mux.Handle("PUT /api/v1/roles/{id}", s.auth(s.requireAdmin(http.HandlerFunc(s.handleUpdateRole))))
	mux.Handle("DELETE /api/v1/roles/{id}", s.auth(s.requireAdmin(http.HandlerFunc(s.handleDeleteRole))))
	mux.Handle("POST /api/v1/roles/{id}/rotate", s.auth(s.requireAdmin(http.HandlerFunc(s.handleRotateRole))))
	mux.Handle("GET /api/v1/nodes", s.auth(s.requireAdmin(http.HandlerFunc(s.handleListNodes))))
	mux.Handle("POST /api/v1/nodes", s.auth(s.requireAdmin(http.HandlerFunc(s.handleRegisterNode))))
	mux.Handle("DELETE /api/v1/nodes/{id}", s.auth(s.requireAdmin(http.HandlerFunc(s.handleDeleteNode))))

	// Frontend (embedded single-page app)
	assets, err := fs.Sub(webFS, "web")
	if err != nil {
		panic(err)
	}
	mux.Handle("GET /", http.FileServer(http.FS(assets)))
}

// ---- whoami -----------------------------------------------------------------

// GET /api/v1/whoami reports what the presented token can do.
func (s *Server) handleWhoami(w http.ResponseWriter, r *http.Request) {
	p := principalFrom(r)
	if p.admin {
		writeJSON(w, http.StatusOK, map[string]any{"type": "admin"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"type": "role",
		"role": map[string]any{
			"id":          p.role.ID,
			"name":        p.role.Name,
			"description": p.role.Description,
		},
		"namespaces": roleNamespaceNames(p.role),
	})
}

// ---- configurations ---------------------------------------------------------

// GET /api/v1/configs?namespace=x lists configurations. Role tokens only ever
// see configurations inside their granted namespaces.
func (s *Server) handleListConfigs(w http.ResponseWriter, r *http.Request) {
	p := principalFrom(r)
	namespace := strings.TrimSpace(r.URL.Query().Get("namespace"))

	var filter []string // nil 表示全部命名空间
	if namespace != "" {
		if !p.canRead(namespace) {
			writeError(w, http.StatusForbidden, "token has no access to namespace "+strconv.Quote(namespace))
			return
		}
		filter = []string{namespace}
	} else if !p.admin {
		// 角色 token 无过滤条件时，只能看到其授权的命名空间
		filter = roleNamespaceNames(p.role)
	}

	configs, err := s.store.ListConfigs(filter)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list configurations")
		return
	}
	writeJSON(w, http.StatusOK, configs)
}

// POST /api/v1/configs registers or updates a configuration. Admin only.
func (s *Server) handleCreateConfig(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodySize)
	defer r.Body.Close()
	var req registerRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	req.Namespace = strings.TrimSpace(req.Namespace)
	req.Key = strings.TrimSpace(req.Key)
	req.Format = strings.ToLower(strings.TrimSpace(req.Format))
	if req.Namespace == "" || req.Key == "" {
		writeError(w, http.StatusBadRequest, "namespace and key are required")
		return
	}

	configuration, err := s.store.Put(req.Namespace, req.Key, req.Value, req.Format)
	if errors.Is(err, ErrInvalidFormat) {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to save configuration")
		return
	}
	writeJSON(w, http.StatusOK, configuration)
}

// GET /api/v1/configs/{namespace}/{key} sends the requested configuration.
// Role tokens may only read inside their granted namespaces.
func (s *Server) handleGetConfig(w http.ResponseWriter, r *http.Request) {
	namespace, key := r.PathValue("namespace"), r.PathValue("key")
	if !principalFrom(r).canRead(namespace) {
		writeError(w, http.StatusForbidden, "token has no access to namespace "+strconv.Quote(namespace))
		return
	}

	configuration, err := s.store.Get(namespace, key)
	if errors.Is(err, ErrNotFound) {
		writeError(w, http.StatusNotFound, "configuration not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to read configuration")
		return
	}
	writeJSON(w, http.StatusOK, configuration)
}

// DELETE /api/v1/configs/{namespace}/{key} removes a configuration. Admin only.
func (s *Server) handleDeleteConfig(w http.ResponseWriter, r *http.Request) {
	if err := s.store.Delete(r.PathValue("namespace"), r.PathValue("key")); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to delete configuration")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---- namespaces -------------------------------------------------------------

// GET /api/v1/namespaces lists namespaces visible to the caller.
func (s *Server) handleListNamespaces(w http.ResponseWriter, r *http.Request) {
	p := principalFrom(r)
	if !p.admin {
		writeJSON(w, http.StatusOK, roleNamespaceNames(p.role))
		return
	}
	namespaces, err := s.store.ListNamespaces()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list namespaces")
		return
	}
	writeJSON(w, http.StatusOK, namespaces)
}

// ---- roles (admin only) -----------------------------------------------------

func (s *Server) handleListRoles(w http.ResponseWriter, r *http.Request) {
	roles, err := s.store.ListRoles()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list roles")
		return
	}
	writeJSON(w, http.StatusOK, roles)
}

func (s *Server) handleCreateRole(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodySize)
	defer r.Body.Close()
	var req roleRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	if strings.TrimSpace(req.Name) == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}
	role, err := s.store.CreateRole(req.Name, req.Description, req.Namespaces)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to create role")
		return
	}
	writeJSON(w, http.StatusCreated, role)
}

func (s *Server) handleUpdateRole(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseUint(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid role id")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxBodySize)
	defer r.Body.Close()
	var req roleRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	if strings.TrimSpace(req.Name) == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}
	role, err := s.store.UpdateRole(uint(id), req.Name, req.Description, req.Namespaces)
	if errors.Is(err, ErrNotFound) {
		writeError(w, http.StatusNotFound, "role not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to update role")
		return
	}
	writeJSON(w, http.StatusOK, role)
}

func (s *Server) handleDeleteRole(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseUint(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid role id")
		return
	}
	if err := s.store.DeleteRole(uint(id)); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to delete role")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// POST /api/v1/roles/{id}/rotate generates a fresh token for the role.
func (s *Server) handleRotateRole(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseUint(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid role id")
		return
	}
	role, err := s.store.RotateRoleToken(uint(id))
	if errors.Is(err, ErrNotFound) {
		writeError(w, http.StatusNotFound, "role not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to rotate token")
		return
	}
	writeJSON(w, http.StatusOK, role)
}

// ---- raft nodes (admin only) ------------------------------------------------

// GET /api/v1/nodes lists the registered Raft cluster members.
func (s *Server) handleListNodes(w http.ResponseWriter, r *http.Request) {
	nodes, err := s.store.ListNodes()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list nodes")
		return
	}
	writeJSON(w, http.StatusOK, nodes)
}

// POST /api/v1/nodes dynamically registers a Raft node (ip + port). The running
// Raft node receives the updated peer list through the registrar.
func (s *Server) handleRegisterNode(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodySize)
	defer r.Body.Close()
	var req nodeRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	req.IP = strings.TrimSpace(req.IP)
	req.Port = strings.TrimSpace(req.Port)
	if req.IP == "" || req.Port == "" {
		writeError(w, http.StatusBadRequest, "ip and port are required")
		return
	}
	if err := validatePort(req.Port); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	node, err := s.store.RegisterNode(req.IP, req.Port)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to register node")
		return
	}
	s.syncNodes()
	writeJSON(w, http.StatusOK, node)
}

// DELETE /api/v1/nodes/{id} removes a registered node and refreshes the peer list.
func (s *Server) handleDeleteNode(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseUint(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid node id")
		return
	}
	if err := s.store.DeleteNode(uint(id)); errors.Is(err, ErrNotFound) {
		writeError(w, http.StatusNotFound, "node not found")
		return
	} else if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to delete node")
		return
	}
	s.syncNodes()
	w.WriteHeader(http.StatusNoContent)
}

// syncNodes pushes the current node list to the running Raft node.
func (s *Server) syncNodes() {
	if s.nodes == nil {
		return
	}
	nodes, err := s.store.ListNodes()
	if err != nil {
		return
	}
	addrs := make([]string, 0, len(nodes))
	for _, n := range nodes {
		addrs = append(addrs, net.JoinHostPort(n.IP, n.Port))
	}
	s.nodes.SetNodes(addrs)
}

func validatePort(port string) error {
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return errors.New("port must be a number between 1 and 65535")
	}
	return nil
}

// ---- auth -------------------------------------------------------------------

type principalContextKey struct{}

// auth resolves the Auth header: the master token grants full access, a role
// token grants read access to that role's namespaces only.
func (s *Server) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		provided := strings.TrimSpace(r.Header.Get("Auth"))
		if provided == "" {
			writeError(w, http.StatusUnauthorized, "invalid or missing Auth header")
			return
		}
		if subtle.ConstantTimeCompare([]byte(provided), []byte(s.token)) == 1 {
			next.ServeHTTP(w, withPrincipal(r, &principal{admin: true}))
			return
		}
		role, err := s.store.RoleByToken(provided)
		if err != nil {
			writeError(w, http.StatusUnauthorized, "invalid or missing Auth header")
			return
		}
		granted := make(map[string]bool, len(role.Namespaces))
		for _, ns := range role.Namespaces {
			granted[ns.Name] = true
		}
		next.ServeHTTP(w, withPrincipal(r, &principal{role: role, namespaces: granted}))
	})
}

// requireAdmin rejects role tokens; only the master token may pass.
func (s *Server) requireAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !principalFrom(r).admin {
			writeError(w, http.StatusForbidden, "admin token required")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func withPrincipal(r *http.Request, p *principal) *http.Request {
	ctx := contextWithPrincipal(r, p)
	return r.WithContext(ctx)
}

// ---- helpers ----------------------------------------------------------------

func roleNamespaceNames(role *Role) []string {
	names := make([]string, 0, len(role.Namespaces))
	for _, ns := range role.Namespaces {
		names = append(names, ns.Name)
	}
	return names
}

func decodeJSON(r *http.Request, v any) error {
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(v); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("multiple JSON values")
	}
	return nil
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
