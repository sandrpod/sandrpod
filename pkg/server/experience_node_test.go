package server

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sandrpod/sandrpod/pkg/sandpod"
	"github.com/sandrpod/sandrpod/pkg/store"
	"github.com/sandrpod/sandrpod/pkg/tunnel"
)

// 体验节点的门控：平台放一台无 workspace 的 worker，任何租户都能用，但每个
// 用户同时只能有一个活跃沙箱。三个事实必须同时成立：
//   - 配置了该节点（SANDRPOD_EXPERIENCE_PODER_IDS）——没配置就没有限制
//   - 请求 pin 到该节点
//   - 调用者不是操作员凭据
func experienceFixture(t *testing.T, existingOwner, envIDs string) *httptest.ResponseRecorder {
	t.Helper()
	t.Setenv("SANDRPOD_EXPERIENCE_PODER_IDS", envIDs)

	stores := store.NewMemoryStores()
	poderStores := stores.Poders
	if _, err := poderStores.Register(&sandpod.RegisterPoderRequest{
		ID: "poder-exp", Name: "experience", Region: "experience",
		ProviderType: "local", Workspace: "",
	}); err != nil {
		t.Fatalf("register experience poder: %v", err)
	}
	if existingOwner != "" {
		if err := stores.Sandboxes.Add(&sandpod.SandboxInfo{
			Name: "exp-existing", State: sandpod.StateRunning,
			PoderID: "poder-exp", Owner: existingOwner,
		}); err != nil {
			t.Fatalf("seed existing sandbox: %v", err)
		}
	}

	// 静态 token 是操作员；tokens 文件里的 user token 是普通租户。
	cfg := Config{
		Token: "admin-tok",
		Tokens: []NamedToken{
			{Name: "tenant1", Token: "tenant-tok", Role: RoleUser},
		},
	}
	handler := New(cfg, stores, tunnel.NewTunnelStore(), tunnel.NewDirectTunnelStore())

	body := `{"name":"exp-new","provider_type":"local","poder_id":"poder-exp","async":true}`
	req := httptest.NewRequest("POST", "/api/v1/sandboxes", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer tenant-tok")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	return w
}

func TestExperienceNodeLimitRefusesSecondSandbox(t *testing.T) {
	w := experienceFixture(t, "tenant1", "poder-exp")
	if w.Code != 403 {
		t.Fatalf("expected 403, got %d (body: %s)", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "one running sandbox per user") {
		t.Errorf("unexpected body: %s", w.Body.String())
	}
}

// 别的用户的记忆沙箱不占我的额度：门控按 Owner 计数，不是按节点全局计数。
func TestExperienceNodeLimitCountsPerUser(t *testing.T) {
	w := experienceFixture(t, "someone-else", "poder-exp")
	if w.Code == 403 {
		t.Fatalf("another user's sandbox must not block this create (body: %s)", w.Body.String())
	}
}

// 没配置体验节点 = 没有限制（自托管的无 workspace worker 是普通共享节点）。
func TestExperienceNodeLimitDisabledWhenUnconfigured(t *testing.T) {
	w := experienceFixture(t, "tenant1", "")
	if w.Code == 403 {
		t.Fatalf("unconfigured experience node must not gate (body: %s)", w.Body.String())
	}
}

// 云 provider 的创建在托管部署里是操作员行为：租户凭据 → 403。
func TestCloudProviderTenantRefused(t *testing.T) {
	t.Setenv("SANDRPOD_EXPERIENCE_PODER_IDS", "")
	stores := store.NewMemoryStores()
	cfg := Config{
		Token:  "admin-tok",
		Tokens: []NamedToken{{Name: "tenant1", Token: "tenant-tok", Role: RoleUser}},
	}
	handler := New(cfg, stores, tunnel.NewTunnelStore(), tunnel.NewDirectTunnelStore())

	body := `{"name":"vm-test","provider_type":"aliyun","region":"cn-hongkong","async":true}`
	req := httptest.NewRequest("POST", "/api/v1/sandboxes", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer tenant-tok")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != 403 {
		t.Fatalf("expected 403 for tenant cloud create, got %d (body: %s)", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "operator-only") {
		t.Errorf("unexpected body: %s", w.Body.String())
	}
}

// 操作员凭据（静态 token = admin）仍然可以创建云 VM。
func TestCloudProviderAdminAllowed(t *testing.T) {
	t.Setenv("SANDRPOD_EXPERIENCE_PODER_IDS", "")
	stores := store.NewMemoryStores()
	cfg := Config{Token: "admin-tok"}
	handler := New(cfg, stores, tunnel.NewTunnelStore(), tunnel.NewDirectTunnelStore())

	body := `{"name":"vm-admin","provider_type":"aliyun","region":"cn-hongkong","async":true}`
	req := httptest.NewRequest("POST", "/api/v1/sandboxes", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer admin-tok")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code == 403 {
		t.Fatalf("admin cloud create must not be refused (body: %s)", w.Body.String())
	}
}
