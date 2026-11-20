package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"codearts2api/internal/apierr"
	"codearts2api/internal/protocol"
	"codearts2api/internal/store"
)

// fakeSearxng 起一个最小可用的 SearXNG JSON 后端。
func fakeSearxng(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/search" {
			http.NotFound(w, r)
			return
		}
		if r.URL.Query().Get("format") != "json" {
			t.Errorf("应以 format=json 请求 SearXNG，得到 %q", r.URL.RawQuery)
		}
		_, _ = io.WriteString(w, `{"results":[
			{"title":"杭州天气","url":"https://weather.example/hz","content":"晴，25℃","engine":"bing"},
			{"title":"浙江气象","url":"https://zj.example","content":"今日多云","engine":"baidu"}
		]}`)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// enableWebSearch 把测试服务器的设置指向假 SearXNG。
func enableWebSearch(t *testing.T, s *Server, endpoint string) {
	t.Helper()
	s.Store.UpdateSettings(func(st *store.Settings) {
		st.WebSearchEnabled = true
		st.WebSearchProvider = "searxng"
		st.WebSearchEndpoint = endpoint
		st.WebSearchMaxResults = 5
		st.WebSearchTimeout = 5
	})
}

func TestApplyChatWebSearchInjectsAndStrips(t *testing.T) {
	srv := fakeSearxng(t)
	s := newTestServer(t, 0, 0)
	enableWebSearch(t, s, srv.URL)

	body := []byte(`{
		"model":"glm-5.3",
		"messages":[{"role":"user","content":"杭州今天天气怎么样"}],
		"tools":[
			{"type":"web_search"},
			{"type":"function","function":{"name":"get_time","parameters":{"type":"object"}}}
		],
		"tool_choice":"auto"
	}`)
	res, err := s.applyChatWebSearch(t.Context(), body)
	if err != nil {
		t.Fatal(err)
	}
	if res == nil {
		t.Fatal("声明了 web_search 应触发检索")
	}
	if !strings.Contains(res.info, "searxng") || !strings.Contains(res.info, "results=2") {
		t.Errorf("info = %q", res.info)
	}

	var out map[string]json.RawMessage
	if err := json.Unmarshal(res.body, &out); err != nil {
		t.Fatal(err)
	}
	var tools []struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(out["tools"], &tools); err != nil {
		t.Fatalf("tools 应保留 function 工具: %v", err)
	}
	if len(tools) != 1 || tools[0].Type != "function" {
		t.Errorf("web_search 应被摘掉，只留 function：%+v", tools)
	}

	var msgs []struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal(out["messages"], &msgs); err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2 || msgs[0].Role != "system" {
		t.Fatalf("应在最前面插入 system：%+v", msgs)
	}
	for _, want := range []string{"杭州天气", "https://weather.example/hz", "晴，25℃", "不可信"} {
		if !strings.Contains(msgs[0].Content, want) {
			t.Errorf("注入的系统提示缺少 %q", want)
		}
	}
	if msgs[1].Role != "user" || msgs[1].Content != "杭州今天天气怎么样" {
		t.Errorf("原消息应原样保留：%+v", msgs[1])
	}
}

// 已有 system 时必须合并进去，而不是再插一条 —— 有些模型只认第一条 system。
func TestApplyChatWebSearchMergesSystem(t *testing.T) {
	srv := fakeSearxng(t)
	s := newTestServer(t, 0, 0)
	enableWebSearch(t, s, srv.URL)

	body := []byte(`{
		"model":"glm-5.3",
		"messages":[
			{"role":"system","content":"你是助手"},
			{"role":"user","content":"查一下天气"}
		],
		"tools":[{"type":"web_search_preview"}]
	}`)
	res, err := s.applyChatWebSearch(t.Context(), body)
	if err != nil || res == nil {
		t.Fatalf("res=%v err=%v", res, err)
	}
	var out map[string]json.RawMessage
	_ = json.Unmarshal(res.body, &out)
	var msgs []struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	}
	_ = json.Unmarshal(out["messages"], &msgs)
	if len(msgs) != 2 {
		t.Fatalf("不应新增 system 条数：%+v", msgs)
	}
	if !strings.Contains(msgs[0].Content, "晴，25℃") || !strings.HasSuffix(msgs[0].Content, "你是助手") {
		t.Errorf("原有 system 应保留在资料之后：%q", msgs[0].Content)
	}
	if _, ok := out["tools"]; ok {
		t.Error("工具被摘空后应删掉 tools 键（空数组上游会报参数错误）")
	}
	if _, ok := out["tool_choice"]; ok {
		t.Error("工具被摘空后应删掉 tool_choice")
	}
}

func TestApplyChatWebSearchDisabled(t *testing.T) {
	s := newTestServer(t, 0, 0)
	body := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"web_search"}]}`)
	_, err := s.applyChatWebSearch(t.Context(), body)
	if err == nil {
		t.Fatal("未启用时应明确拒绝，不能静默跳过检索")
	}
	var ipe *apierr.InvalidParamError
	if !strings.Contains(err.Error(), "未启用") {
		t.Errorf("错误信息应说明未启用：%v", err)
	}
	_ = ipe
	if got := apierr.From(err).Code; got != apierr.CodeWebSearchDisabled {
		t.Errorf("code = %q，期望 %q", got, apierr.CodeWebSearchDisabled)
	}
}

func TestApplyChatWebSearchNoTools(t *testing.T) {
	s := newTestServer(t, 0, 0)
	body := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	res, err := s.applyChatWebSearch(t.Context(), body)
	if err != nil || res != nil {
		t.Errorf("没有内置工具时应原样放过：res=%v err=%v", res, err)
	}
}

func TestApplyResponsesWebSearch(t *testing.T) {
	srv := fakeSearxng(t)
	s := newTestServer(t, 0, 0)
	enableWebSearch(t, s, srv.URL)

	req := &protocol.ResponsesRequest{
		Input: json.RawMessage(`[{"role":"user","content":[{"type":"input_text","text":"杭州天气"}]}]`),
		Tools: []protocol.ResponsesTool{
			{Type: "web_search"},
			{Type: "function", Name: "f", Parameters: json.RawMessage(`{"type":"object"}`)},
		},
		ToolChoice: json.RawMessage(`"auto"`),
	}
	inj, err := s.applyResponsesWebSearch(t.Context(), req)
	if err != nil || inj == nil {
		t.Fatalf("inj=%v err=%v", inj, err)
	}
	if len(req.Tools) != 1 || req.Tools[0].Name != "f" {
		t.Errorf("web_search 应被摘掉：%+v", req.Tools)
	}
	if !strings.Contains(inj.block, "weather.example") {
		t.Errorf("资料块缺少结果：%q", inj.block)
	}
}

func TestApplyAnthropicWebSearch(t *testing.T) {
	srv := fakeSearxng(t)
	s := newTestServer(t, 0, 0)
	enableWebSearch(t, s, srv.URL)

	req := &protocol.Request{
		Messages: []protocol.Message{
			{Role: "user", Content: json.RawMessage(`[{"type":"text","text":"杭州天气"}]`)},
		},
		Tools: []protocol.Tool{
			{Type: "web_search_20250305", Name: "web_search"},
			{Name: "get_time", InputSchema: json.RawMessage(`{"type":"object"}`)},
		},
	}
	inj, err := s.applyAnthropicWebSearch(t.Context(), req)
	if err != nil || inj == nil {
		t.Fatalf("inj=%v err=%v", inj, err)
	}
	if len(req.Tools) != 1 || req.Tools[0].Name != "get_time" {
		t.Errorf("web_search 应被摘掉：%+v", req.Tools)
	}

	// 其它服务端工具必须显式拒绝，而不是变成空参数的 function 转发出去。
	req2 := &protocol.Request{
		Messages: []protocol.Message{{Role: "user", Content: json.RawMessage(`"hi"`)}},
		Tools:    []protocol.Tool{{Type: "computer_20241022", Name: "computer"}},
	}
	if _, err := s.applyAnthropicWebSearch(t.Context(), req2); err == nil {
		t.Fatal("computer 工具应被拒绝")
	} else if got := apierr.From(err).Code; got != apierr.CodeUnsupportedParam {
		t.Errorf("code = %q，期望 %q", got, apierr.CodeUnsupportedParam)
	}
}

func TestHandleSearchEndpoint(t *testing.T) {
	srv := fakeSearxng(t)
	s := newTestServer(t, 0, 0)
	enableWebSearch(t, s, srv.URL)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/search", strings.NewReader(`{"query":"杭州天气"}`))
	s.handleSearch(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d，body = %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Query    string `json:"query"`
		Provider string `json:"provider"`
		Count    int    `json:"count"`
		Results  []struct {
			Title   string `json:"title"`
			URL     string `json:"url"`
			Content string `json:"content"`
		} `json:"results"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Query != "杭州天气" || out.Provider != "searxng" || out.Count != 2 {
		t.Errorf("out = %+v", out)
	}
	if out.Results[0].Content != "晴，25℃" {
		t.Errorf("content 应回落成摘要：%+v", out.Results[0])
	}

	// GET 也要能用（很多客户端 / 浏览器直接拼 query string）。
	rec = httptest.NewRecorder()
	s.handleSearch(rec, httptest.NewRequest(http.MethodGet, "/v1/search?q=杭州&max_results=1", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET status = %d", rec.Code)
	}
}

func TestHandleSearchDisabled(t *testing.T) {
	s := newTestServer(t, 0, 0)
	rec := httptest.NewRecorder()
	s.handleSearch(rec, httptest.NewRequest(http.MethodPost, "/v1/search", strings.NewReader(`{"query":"x"}`)))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d，body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), apierr.CodeWebSearchDisabled) {
		t.Errorf("body = %s", rec.Body.String())
	}
}

func TestHandleSearchEmptyQuery(t *testing.T) {
	s := newTestServer(t, 0, 0)
	rec := httptest.NewRecorder()
	s.handleSearch(rec, httptest.NewRequest(http.MethodPost, "/v1/search", strings.NewReader(`{}`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d", rec.Code)
	}
}

func TestPrependSystemMap(t *testing.T) {
	// 字符串 system：资料在前，原提示在后。
	got := prependSystemMap([]map[string]any{
		{"role": "system", "content": "你是助手"},
		{"role": "user", "content": "hi"},
	}, "资料")
	if len(got) != 2 || got[0]["content"] != "资料\n\n你是助手" {
		t.Errorf("got = %+v", got)
	}

	// 没有 system：插一条。
	got = prependSystemMap([]map[string]any{{"role": "user", "content": "hi"}}, "资料")
	if len(got) != 2 || got[0]["role"] != "system" {
		t.Errorf("got = %+v", got)
	}

	// 分段数组：前置一个 text 段。
	got = prependSystemMap([]map[string]any{
		{"role": "system", "content": []any{map[string]any{"type": "text", "text": "旧"}}},
	}, "资料")
	parts, ok := got[0]["content"].([]any)
	if !ok || len(parts) != 2 {
		t.Errorf("got = %+v", got[0]["content"])
	}
}

// 回归：tool_choice 点名了被摘掉的 web_search 时必须一起清掉，
// 否则上游会说「没有这个工具」，整条请求 400。
func TestApplyChatWebSearchDropsDanglingToolChoice(t *testing.T) {
	srv := fakeSearxng(t)
	s := newTestServer(t, 0, 0)
	enableWebSearch(t, s, srv.URL)

	body := []byte(`{
		"model":"m",
		"messages":[{"role":"user","content":"hi"}],
		"tools":[{"type":"web_search"},
		         {"type":"function","function":{"name":"f","parameters":{"type":"object"}}}],
		"tool_choice":{"type":"function","function":{"name":"web_search"}}
	}`)
	res, err := s.applyChatWebSearch(t.Context(), body)
	if err != nil || res == nil {
		t.Fatalf("res=%v err=%v", res, err)
	}
	var out map[string]json.RawMessage
	_ = json.Unmarshal(res.body, &out)
	if _, ok := out["tool_choice"]; ok {
		t.Error("指向 web_search 的 tool_choice 应被删除")
	}
	if _, ok := out["tools"]; !ok {
		t.Error("其它 function 工具应保留")
	}
}

func TestToolChoiceMentionsWebSearch(t *testing.T) {
	cases := []struct {
		raw  string
		want bool
	}{
		{`{"type":"function","function":{"name":"web_search"}}`, true},
		{`{"type":"tool","name":"web_search_20250305"}`, true},
		{`{"type":"function","function":{"name":"websearch"}}`, true},
		{`{"type":"function","function":{"name":"get_weather"}}`, false},
		{`"auto"`, false},
		{``, false},
	}
	for _, c := range cases {
		if got := toolChoiceMentionsWebSearch(json.RawMessage(c.raw)); got != c.want {
			t.Errorf("%s → %v，期望 %v", c.raw, got, c.want)
		}
	}
}

// 设置页的「检索后端」下拉必须真的生效：切换后服务立即用新后端，且非法值被拒。
func TestAdminSettingsSwitchWebSearchProvider(t *testing.T) {
	s := newTestServer(t, 0, 0)

	rec := httptest.NewRecorder()
	s.handleAdminSettings(rec, httptest.NewRequest(http.MethodPost, "/admin/api/settings",
		strings.NewReader(`{"websearch_enabled":true,"websearch_provider":"Brave","websearch_endpoint":"https://api.search.brave.com"}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d：%s", rec.Code, rec.Body.String())
	}
	st := s.Store.Settings()
	if st.WebSearchProvider != "brave" {
		t.Errorf("后端 = %q，期望 brave（大小写应被归一）", st.WebSearchProvider)
	}
	if got := s.webSearch().Config().Provider; got != "brave" {
		t.Errorf("服务未切换后端：%q", got)
	}

	for _, body := range []string{
		`{"websearch_provider":"google"}`,
		`{"websearch_provider":"duckduckgo"}`,
		`{"websearch_endpoint":"searxng:8080"}`,
		`{"websearch_max_results":50}`,
		`{"websearch_timeout_sec":0}`,
		`{"websearch_cache_ttl_sec":999999}`,
	} {
		rec := httptest.NewRecorder()
		s.handleAdminSettings(rec, httptest.NewRequest(http.MethodPost, "/admin/api/settings", strings.NewReader(body)))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s 应被拒绝，实际 %d：%s", body, rec.Code, rec.Body.String())
		}
	}
	// 被拒的请求不能把设置改脏
	if got := s.Store.Settings().WebSearchProvider; got != "brave" {
		t.Errorf("非法请求改动了设置：%q", got)
	}
}
