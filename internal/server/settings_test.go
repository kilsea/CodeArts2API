package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"codearts2api/internal/store"
)

// postSettings 直接调用设置接口（绕过 HTTP 鉴权，只测字段语义）。
func postSettings(t *testing.T, s *Server, body string) map[string]any {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/admin/api/settings", strings.NewReader(body))
	req.RemoteAddr = "127.0.0.1:5555"
	rec := httptest.NewRecorder()
	s.handleAdminSettings(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，body = %s", rec.Code, rec.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("响应解析失败: %v", err)
	}
	return out
}

// TestAdminPasswordEmptyDoesNotClear 回归：空的 admin_password 不能把密码抹掉。
//
// 回归背景：以前 `if body.AdminPassword != nil` 直接赋值，于是任何带
// `"admin_password": ""` 的请求都会清除管理密码，让管理接口退回
// 「本机直连」兜底 —— 配合反向代理就是一个静默提权链。
func TestAdminPasswordEmptyDoesNotClear(t *testing.T) {
	s := newTestServer(t, 0, 1)
	s.Store.UpdateSettings(func(st *store.Settings) { st.AdminPassword = "keepme" })

	postSettings(t, s, `{"admin_password":""}`)
	if got := s.Store.Settings().AdminPassword; got != "keepme" {
		t.Errorf("空字符串清除了管理密码: %q", got)
	}

	postSettings(t, s, `{"admin_password":"   "}`)
	if got := s.Store.Settings().AdminPassword; got != "keepme" {
		t.Errorf("空白字符串清除了管理密码: %q", got)
	}

	// 不带该字段（null）也不应改动
	postSettings(t, s, `{"default_model":"GLM-5.2"}`)
	if got := s.Store.Settings().AdminPassword; got != "keepme" {
		t.Errorf("未携带该字段却改动了管理密码: %q", got)
	}
}

// TestAdminPasswordSetAndClear 设置与显式清除。
func TestAdminPasswordSetAndClear(t *testing.T) {
	s := newTestServer(t, 0, 1)

	postSettings(t, s, `{"admin_password":"  s3cret  "}`)
	if got := s.Store.Settings().AdminPassword; got != "s3cret" {
		t.Errorf("设置管理密码失败（应去掉两端空白）: %q", got)
	}

	// 不带 clear 标志的 false 不应清除
	postSettings(t, s, `{"clear_admin_password":false}`)
	if got := s.Store.Settings().AdminPassword; got != "s3cret" {
		t.Errorf("clear_admin_password=false 却清除了密码: %q", got)
	}

	postSettings(t, s, `{"clear_admin_password":true}`)
	if got := s.Store.Settings().AdminPassword; got != "" {
		t.Errorf("显式清除失败: %q", got)
	}
}

// TestSanitizedSettingsExposesCreditPriority 回归：新增设置必须同步加进
// sanitizedSettings 白名单，否则 GET /admin/api/settings 缺字段，
// 控制台回显成默认值，用户看不到真实状态。
//
// 回归背景：credit_priority 上线时漏加白名单，接口里读不到该字段。
func TestSanitizedSettingsExposesCreditPriority(t *testing.T) {
	st := store.DefaultSettings()
	st.CreditPriority = false
	out := sanitizedSettings(st)
	if out["credit_priority"] != false {
		t.Errorf("credit_priority = %v，期望 false（设置白名单漏了新字段）", out["credit_priority"])
	}
}

// TestSanitizedSettingsHidesAdminPassword 下发给前端的设置里不能出现密码本体。
func TestSanitizedSettingsHidesAdminPassword(t *testing.T) {
	st := store.DefaultSettings()
	st.AdminPassword = "topsecret"
	out := sanitizedSettings(st)

	if _, ok := out["admin_password"]; ok {
		t.Error("sanitizedSettings 泄露了 admin_password 字段")
	}
	if out["admin_password_set"] != true {
		t.Errorf("admin_password_set = %v，期望 true", out["admin_password_set"])
	}
	raw, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "topsecret") {
		t.Error("序列化后的设置里包含管理密码明文")
	}
}

// TestSettingsAppliesClaimSchedule 回归：活动福利的「每天自动领取」开关与
// 领取时刻曾经**根本没接进设置接口** —— 前端把 auto_claim /
// claim_after_hour / claim_after_minute 都发出去了，后端请求体 struct 里
// 没有这几个字段，于是保存返回 200、界面回显「已保存」，刷新后又回到默认
// 00:05，后台调度永远按默认时刻跑。
func TestSettingsAppliesClaimSchedule(t *testing.T) {
	s := newTestServer(t, 0, 1)

	postSettings(t, s, `{"auto_claim":false,"claim_after_hour":7,"claim_after_minute":30}`)
	st := s.Store.Settings()
	if st.AutoClaim {
		t.Error("auto_claim=false 没有落盘")
	}
	if st.ClaimAfterHour != 7 || st.ClaimAfterMinute != 30 {
		t.Errorf("领取时刻 = %02d:%02d，期望 07:30（设置接口丢弃了字段）",
			st.ClaimAfterHour, st.ClaimAfterMinute)
	}

	// 0 点是合法值，不能被当成「未提供」而忽略。
	postSettings(t, s, `{"auto_claim":true,"claim_after_hour":0,"claim_after_minute":0}`)
	st = s.Store.Settings()
	if !st.AutoClaim {
		t.Error("auto_claim=true 没有落盘")
	}
	if st.ClaimAfterHour != 0 || st.ClaimAfterMinute != 0 {
		t.Errorf("领取时刻 = %02d:%02d，期望 00:00", st.ClaimAfterHour, st.ClaimAfterMinute)
	}

	// 不携带这几个字段时保持原值（指针 nil = 不修改）。
	postSettings(t, s, `{"default_model":"GLM-5.2"}`)
	if st := s.Store.Settings(); st.ClaimAfterHour != 0 || st.ClaimAfterMinute != 0 {
		t.Errorf("未携带领取时刻却改动了它：%02d:%02d", st.ClaimAfterHour, st.ClaimAfterMinute)
	}
}

// TestSettingsRejectsOutOfRangeClaimTime 越界时刻必须 400，而不是静默夹回默认值
// —— 静默夹回会让用户以为「我设的 25:00 生效了」，实际每天仍在 00:05 领。
func TestSettingsRejectsOutOfRangeClaimTime(t *testing.T) {
	s := newTestServer(t, 0, 1)
	for _, body := range []string{
		`{"claim_after_hour":24}`,
		`{"claim_after_hour":-1}`,
		`{"claim_after_minute":60}`,
		`{"claim_after_minute":-1}`,
	} {
		req := httptest.NewRequest(http.MethodPost, "/admin/api/settings", strings.NewReader(body))
		req.RemoteAddr = "127.0.0.1:5555"
		rec := httptest.NewRecorder()
		s.handleAdminSettings(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s 返回 %d，期望 400", body, rec.Code)
		}
	}
	if st := s.Store.Settings(); st.ClaimAfterHour != 0 || st.ClaimAfterMinute != 5 {
		t.Errorf("越界值被写进了设置：%02d:%02d", st.ClaimAfterHour, st.ClaimAfterMinute)
	}
}

// TestSettingsPatchCoversAllSettings 是「设置项三处对齐」的守卫：
//
//	store.Settings 的字段（落盘）↔ settingsPatch（POST 能不能写进来）↔ sanitizedSettings（GET 能不能读回去）
//
// 任何一处漏字段，表现都是「界面上改了、提示已保存、刷新后变回去」这类
// 查不出原因的怪现象，而且只有真人在浏览器里点一遍才看得见。
// 历史上 credit_priority 漏在 sanitizedSettings、auto_claim 与领取时刻
// 漏在 settingsPatch，各自都修过一次。
func TestSettingsPatchCoversAllSettings(t *testing.T) {
	// 这两个是密钥，按设计只下发「是否已设置」与脱敏串：
	// admin_password → admin_password_set，websearch_api_key → websearch_api_key_set /
	// websearch_api_key_masked；清除分别走 clear_admin_password / clear_websearch_api_key。
	exempt := map[string]bool{"admin_password": true, "websearch_api_key": true}

	storeKeys := jsonKeysOf(t, store.Settings{})
	patchKeys := jsonKeysOf(t, settingsPatch{})
	sanitized := sanitizedSettings(store.DefaultSettings())

	for k := range storeKeys {
		if exempt[k] {
			continue
		}
		if !patchKeys[k] {
			t.Errorf("store.Settings 有 %q，但 settingsPatch 里没有同名字段："+
				"POST /admin/api/settings 会静默丢弃它（界面上改了等于没改）", k)
		}
		if _, ok := sanitized[k]; !ok {
			t.Errorf("store.Settings 有 %q，但 sanitizedSettings 没下发："+
				"控制台读不到真实值，会回显成默认值", k)
		}
	}

	// 反向：patch 里的每个字段都必须对应一个真实设置项（拼错的字段名没人会读）。
	want := map[string]bool{}
	for k := range storeKeys {
		want[k] = true
	}
	// 这些是动作标志，本身不对应 store.Settings 的字段。
	actions := map[string]bool{
		"clear_admin_password":    true,
		"clear_websearch_api_key": true,
	}
	for k := range patchKeys {
		if want[k] || actions[k] {
			continue
		}
		t.Errorf("settingsPatch 有 %q，但 store.Settings 没有对应字段（拼写错误？）", k)
	}
}

// jsonKeysOf 取出结构体所有字段的 json tag 名。
func jsonKeysOf(t *testing.T, v any) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	rt := reflect.TypeOf(v)
	for i := 0; i < rt.NumField(); i++ {
		name, _, _ := strings.Cut(rt.Field(i).Tag.Get("json"), ",")
		if name == "" || name == "-" {
			continue
		}
		out[name] = true
	}
	return out
}
