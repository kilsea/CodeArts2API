package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"codearts2api/internal/codearts"
)

// TestKeysListStaysMasked 确认列表永远只给脱敏值 ——
// 控制台截图、浏览器会话恢复、反代访问日志里都不该出现可直接用的密钥。
func TestKeysListStaysMasked(t *testing.T) {
	s := newAdminTestServer(t)
	full := s.Store.AddKey("我的密钥").Key

	rec := httptest.NewRecorder()
	s.handleAdminKeys(rec, httptest.NewRequest(http.MethodGet, "/admin/api/keys", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var body struct {
		Keys []struct {
			Key  string `json:"key"`
			Name string `json:"name"`
		} `json:"keys"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Keys) != 1 {
		t.Fatalf("keys = %d", len(body.Keys))
	}
	if body.Keys[0].Key == full {
		t.Fatal("列表里出现了完整密钥明文")
	}
	if body.Keys[0].Key != codearts.Mask(full) {
		t.Errorf("key = %q，期望脱敏值 %q", body.Keys[0].Key, codearts.Mask(full))
	}
}

// TestKeyRevealReturnsPlaintext 覆盖控制台「复制」按钮的后端：
// 用脱敏值反查明文（前端手上只有脱敏值）。
func TestKeyRevealReturnsPlaintext(t *testing.T) {
	s := newAdminTestServer(t)
	full := s.Store.AddKey("我的密钥").Key
	masked := codearts.Mask(full)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/admin/api/keys?reveal="+masked, nil)
	s.handleAdminKeys(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Key string `json:"key"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Key != full {
		t.Errorf("reveal 返回 %q，期望 %q", body.Key, full)
	}
}

// TestKeyRevealUnknownRef 确认取不到就是 404，绝不把未知引用当成密钥回显。
func TestKeyRevealUnknownRef(t *testing.T) {
	s := newAdminTestServer(t)
	s.Store.AddKey("我的密钥")

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/admin/api/keys?reveal=nope...xxxx", nil)
	s.handleAdminKeys(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d，期望 404", rec.Code)
	}
	if body := rec.Body.String(); len(body) > 0 && json.Valid([]byte(body)) {
		var m map[string]any
		_ = json.Unmarshal([]byte(body), &m)
		if e, ok := m["error"].(map[string]any); ok && e["key"] != nil {
			t.Fatalf("404 响应里不应带 key 字段: %s", body)
		}
	}
}

// TestKeyRevealAcceptsFullKey 确认传入完整密钥（老前端的用法）也能命中。
func TestKeyRevealAcceptsFullKey(t *testing.T) {
	s := newAdminTestServer(t)
	full := s.Store.AddKey("我的密钥").Key

	rec := httptest.NewRecorder()
	s.handleAdminKeys(rec, httptest.NewRequest(http.MethodGet, "/admin/api/keys?reveal="+full, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
}

// TestAdminResponsesAreNotCacheable 覆盖「管理接口响应不得缓存」。
//
// `?reveal=` 返回 API 密钥明文，状态接口含账号与登录态摘要。
// GET + 200 的 JSON 在某些浏览器 / 中间代理上会被缓存 ——
// 一个明文密钥就这么留在了磁盘里。
func TestAdminResponsesAreNotCacheable(t *testing.T) {
	s := newAdminTestServer(t)
	full := s.Store.AddKey("我的密钥").Key

	h := s.withAdminAuth(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"key": full})
	})

	req := httptest.NewRequest(http.MethodGet, "/admin/api/keys?reveal=x", nil)
	req.RemoteAddr = "127.0.0.1:5555" // 未设管理密码时管理接口只允许本机直连
	rec := httptest.NewRecorder()
	h(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if cc := rec.Header().Get("Cache-Control"); !strings.Contains(cc, "no-store") {
		t.Errorf("Cache-Control = %q，期望包含 no-store", cc)
	}
	if p := rec.Header().Get("Pragma"); p != "no-cache" {
		t.Errorf("Pragma = %q", p)
	}
}
