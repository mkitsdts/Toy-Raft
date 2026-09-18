package configcenter

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

const adminToken = "admin-test-token"

func newTestServer(t *testing.T) (*Server, *httptest.Server) {
	t.Helper()
	store, err := OpenStore(DatabaseConfig{Driver: "sqlite3", DSN: filepath.Join(t.TempDir(), "test.db")})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	server := NewServer(store, adminToken)
	mux := http.NewServeMux()
	server.RegisterRoutes(mux)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return server, ts
}

// do issues an authenticated request and returns status code and decoded body.
func do(t *testing.T, ts *httptest.Server, method, path, token, body string) (int, string, map[string]any) {
	t.Helper()
	var reader *strings.Reader
	reader = strings.NewReader(body)
	req, err := http.NewRequest(method, ts.URL+path, reader)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if token != "" {
		req.Header.Set("Auth", token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	raw := string(data)
	var decoded map[string]any
	_ = json.Unmarshal(data, &decoded)
	return resp.StatusCode, raw, decoded
}

func TestPutConfigFormats(t *testing.T) {
	_, ts := newTestServer(t)

	cases := []struct {
		name   string
		format string
		value  string
		status int
	}{
		{"json valid", "json", `{"a":1}`, http.StatusOK},
		{"json invalid", "json", `{a:1}`, http.StatusBadRequest},
		{"yaml valid", "yaml", "a: 1\nb: two", http.StatusOK},
		{"yaml invalid", "yaml", "a: [1, 2", http.StatusBadRequest},
		{"text", "text", "anything", http.StatusOK},
		{"empty format defaults text", "", "plain", http.StatusOK},
		{"unsupported format", "xml", "<a/>", http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body, _ := json.Marshal(registerRequest{Namespace: "ns", Key: strings.ReplaceAll(tc.name, " ", "-"), Value: tc.value, Format: tc.format})
			status, _, _ := do(t, ts, http.MethodPost, "/api/v1/configs", adminToken, string(body))
			if status != tc.status {
				t.Fatalf("status = %d, want %d", status, tc.status)
			}
		})
	}
}

func TestRoleTokenNamespaceRestriction(t *testing.T) {
	server, ts := newTestServer(t)

	// Two namespaces with one config each, created by the admin.
	for _, ns := range []string{"team-a", "team-b"} {
		body := `{"namespace":"` + ns + `","key":"k","format":"text","value":"v"}`
		if status, _, _ := do(t, ts, http.MethodPost, "/api/v1/configs", adminToken, body); status != http.StatusOK {
			t.Fatalf("seed %s: status %d", ns, status)
		}
	}

	role, err := server.store.CreateRole("team-a-role", "", []string{"team-a"})
	if err != nil {
		t.Fatalf("create role: %v", err)
	}
	token := role.Token

	t.Run("read granted namespace", func(t *testing.T) {
		status, _, _ := do(t, ts, http.MethodGet, "/api/v1/configs/team-a/k", token, "")
		if status != http.StatusOK {
			t.Fatalf("status = %d, want 200", status)
		}
	})

	t.Run("read other namespace forbidden", func(t *testing.T) {
		status, _, _ := do(t, ts, http.MethodGet, "/api/v1/configs/team-b/k", token, "")
		if status != http.StatusForbidden {
			t.Fatalf("status = %d, want 403", status)
		}
	})

	t.Run("list only sees granted namespace", func(t *testing.T) {
		status, raw, _ := do(t, ts, http.MethodGet, "/api/v1/configs", token, "")
		if status != http.StatusOK {
			t.Fatalf("status = %d, want 200", status)
		}
		if strings.Contains(raw, "team-b") {
			t.Fatalf("role token leaked another namespace: %s", raw)
		}
		if !strings.Contains(raw, "team-a") {
			t.Fatalf("role token missing granted namespace: %s", raw)
		}
	})

	t.Run("list namespaces only granted", func(t *testing.T) {
		status, raw, _ := do(t, ts, http.MethodGet, "/api/v1/namespaces", token, "")
		if status != http.StatusOK || !strings.Contains(raw, "team-a") || strings.Contains(raw, "team-b") {
			t.Fatalf("status=%d body=%s", status, raw)
		}
	})

	t.Run("write forbidden", func(t *testing.T) {
		body := `{"namespace":"team-a","key":"new","format":"text","value":"x"}`
		status, _, _ := do(t, ts, http.MethodPost, "/api/v1/configs", token, body)
		if status != http.StatusForbidden {
			t.Fatalf("status = %d, want 403", status)
		}
	})

	t.Run("role management forbidden", func(t *testing.T) {
		status, _, _ := do(t, ts, http.MethodGet, "/api/v1/roles", token, "")
		if status != http.StatusForbidden {
			t.Fatalf("status = %d, want 403", status)
		}
	})

	t.Run("query filter for foreign namespace forbidden", func(t *testing.T) {
		status, _, _ := do(t, ts, http.MethodGet, "/api/v1/configs?namespace=team-b", token, "")
		if status != http.StatusForbidden {
			t.Fatalf("status = %d, want 403", status)
		}
	})

	t.Run("whoami hides token", func(t *testing.T) {
		status, raw, _ := do(t, ts, http.MethodGet, "/api/v1/whoami", token, "")
		if status != http.StatusOK {
			t.Fatalf("status = %d, want 200", status)
		}
		if strings.Contains(raw, token) {
			t.Fatalf("whoami leaked the token: %s", raw)
		}
	})
}

func TestRoleWithNoNamespacesSeesNothing(t *testing.T) {
	server, ts := newTestServer(t)
	body := `{"namespace":"team-a","key":"k","format":"text","value":"v"}`
	do(t, ts, http.MethodPost, "/api/v1/configs", adminToken, body)

	role, err := server.store.CreateRole("empty", "", nil)
	if err != nil {
		t.Fatalf("create role: %v", err)
	}
	status, raw, _ := do(t, ts, http.MethodGet, "/api/v1/configs", role.Token, "")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if strings.TrimSpace(raw) != "[]" {
		t.Fatalf("expected empty list, got %s", raw)
	}
}

func TestInvalidTokenRejected(t *testing.T) {
	_, ts := newTestServer(t)
	if status, _, _ := do(t, ts, http.MethodGet, "/api/v1/configs", "wrong", ""); status != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", status)
	}
	if status, _, _ := do(t, ts, http.MethodGet, "/api/v1/configs", "", ""); status != http.StatusUnauthorized {
		t.Fatalf("missing token: status = %d, want 401", status)
	}
}

type fakeRegistrar struct{ addrs []string }

func (f *fakeRegistrar) SetNodes(addrs []string) { f.addrs = append([]string{}, addrs...) }

func TestNodeRegistration(t *testing.T) {
	server, ts := newTestServer(t)
	reg := &fakeRegistrar{}
	server.SetNodeRegistrar(reg)

	// Register a node: the address must be pushed to the running Raft node.
	status, _, body := do(t, ts, http.MethodPost, "/api/v1/nodes", adminToken, `{"ip":"10.0.0.2","port":"11451"}`)
	if status != http.StatusOK {
		t.Fatalf("register status = %d, want 200", status)
	}
	if body["ip"] != "10.0.0.2" || body["port"] != "11451" {
		t.Fatalf("unexpected node body: %v", body)
	}
	if len(reg.addrs) != 1 || reg.addrs[0] != "10.0.0.2:11451" {
		t.Fatalf("registrar got %v, want [10.0.0.2:11451]", reg.addrs)
	}

	// Registering the same address twice must not duplicate it.
	if status, _, _ := do(t, ts, http.MethodPost, "/api/v1/nodes", adminToken, `{"ip":"10.0.0.2","port":"11451"}`); status != http.StatusOK {
		t.Fatalf("re-register status = %d, want 200", status)
	}
	status, raw, _ := do(t, ts, http.MethodGet, "/api/v1/nodes", adminToken, "")
	if status != http.StatusOK || strings.Count(raw, `"ip"`) != 1 {
		t.Fatalf("list status=%d body=%s", status, raw)
	}

	// Validation.
	if status, _, _ := do(t, ts, http.MethodPost, "/api/v1/nodes", adminToken, `{"ip":"1.2.3.4","port":"99999"}`); status != http.StatusBadRequest {
		t.Fatalf("invalid port status = %d, want 400", status)
	}
	if status, _, _ := do(t, ts, http.MethodPost, "/api/v1/nodes", adminToken, `{"ip":"","port":"11451"}`); status != http.StatusBadRequest {
		t.Fatalf("missing ip status = %d, want 400", status)
	}

	// Role tokens may not manage nodes.
	role, err := server.store.CreateRole("ops", "", nil)
	if err != nil {
		t.Fatalf("create role: %v", err)
	}
	if status, _, _ := do(t, ts, http.MethodGet, "/api/v1/nodes", role.Token, ""); status != http.StatusForbidden {
		t.Fatalf("role list status = %d, want 403", status)
	}

	// Delete removes the node and refreshes the peer list.
	id := body["id"].(float64)
	if status, _, _ := do(t, ts, http.MethodDelete, fmt.Sprintf("/api/v1/nodes/%.0f", id), adminToken, ""); status != http.StatusNoContent {
		t.Fatalf("delete status = %d, want 204", status)
	}
	if len(reg.addrs) != 0 {
		t.Fatalf("registrar got %v after delete, want empty", reg.addrs)
	}
}
