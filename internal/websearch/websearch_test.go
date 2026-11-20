package websearch

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// --- 正文抽取 ---

func TestExtractHTML(t *testing.T) {
	page := `<!doctype html><html><head>
<title>  杭州天气 &amp; 预报  </title>
<style>body{color:red}</style>
<script>var x = "<p>不要出现</p>";</script>
</head><body>
<nav>首页 | 关于</nav>
<h1>今天</h1>
<p>晴，25&deg;C。</p>
<div>微风<br>湿度 60%</div>
<noscript>需要 JS</noscript>
<svg><text>图标</text></svg>
</body></html>`

	title, text := extractHTML([]byte(page))
	if title != "杭州天气 & 预报" {
		t.Errorf("title = %q", title)
	}
	for _, want := range []string{"今天", "晴，25°C。", "微风", "湿度 60%"} {
		if !strings.Contains(text, want) {
			t.Errorf("正文缺少 %q：%q", want, text)
		}
	}
	for _, banned := range []string{"color:red", "不要出现", "需要 JS", "图标"} {
		if strings.Contains(text, banned) {
			t.Errorf("正文不应包含 %q：%q", banned, text)
		}
	}
	// 每个块级标签应产生独立行，而不是粘成一句。
	if !strings.Contains(text, "\n") {
		t.Errorf("正文没有换行：%q", text)
	}
}

func TestExtractHTMLPlainText(t *testing.T) {
	title, text := extractHTML([]byte("just text, no tags"))
	if title != "" {
		t.Errorf("title = %q，期望空", title)
	}
	if text != "just text, no tags" {
		t.Errorf("text = %q", text)
	}
}

// --- SSRF 防护 ---

func TestValidateTarget(t *testing.T) {
	cases := []struct {
		url  string
		ok   bool
		hint string
	}{
		{"https://example.com/a", true, ""},
		{"http://example.com:80/a", true, ""},
		{"https://example.com:443/a", true, ""},
		{"ftp://example.com/a", false, "http(s)"},
		{"file:///etc/passwd", false, "http(s)"},
		{"https://user:pass@example.com/", false, "用户名密码"},
		{"http://localhost:8080/", false, "localhost"},
		{"http://127.0.0.1.nip.io/", true, ""}, // 域名形状合法；IP 拦截在拨号时
		{"https://example.com:8443/", false, "端口"},
		{"https://metadata.google.internal/", false, "metadata"},
		{"https://printer.local/", false, ".local"},
	}
	for _, c := range cases {
		u, err := url.Parse(c.url)
		if err != nil {
			t.Fatalf("解析 %s 失败: %v", c.url, err)
		}
		err = validateTarget(u)
		if c.ok && err != nil {
			t.Errorf("%s 应放行，得到 %v", c.url, err)
		}
		if !c.ok {
			if err == nil {
				t.Errorf("%s 应被拒绝", c.url)
			} else if c.hint != "" && !strings.Contains(err.Error(), c.hint) {
				t.Errorf("%s 的错误信息应包含 %q，得到 %q", c.url, c.hint, err)
			}
		}
	}
}

func TestBlockedIP(t *testing.T) {
	blocked := []string{
		"127.0.0.1", "10.0.0.1", "172.16.0.1", "192.168.1.1", "169.254.169.254",
		"100.64.0.1", "0.0.0.0", "198.18.0.1", "240.0.0.1", "255.255.255.255",
		"::1", "fc00::1", "fe80::1",
	}
	for _, s := range blocked {
		if !blockedIP(net.ParseIP(s)) {
			t.Errorf("%s 应被拦截", s)
		}
	}
	for _, s := range []string{"1.1.1.1", "8.8.8.8", "220.181.38.148", "2606:4700::1111"} {
		if blockedIP(net.ParseIP(s)) {
			t.Errorf("%s 不应被拦截", s)
		}
	}
}

// --- 后端解析 ---

func TestSearxngSearch(t *testing.T) {
	var gotQuery url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/search" {
			t.Errorf("path = %s", r.URL.Path)
		}
		gotQuery = r.URL.Query()
		_, _ = io.WriteString(w, `{"results":[
			{"title":"A","url":"https://a.example/1","content":"摘要 A","engine":"bing","score":3},
			{"title":"B","url":"https://b.example/2","content":"摘要 B","engine":"baidu"},
			{"title":"A again","url":"https://a.example/1","content":"重复"}
		]}`)
	}))
	defer srv.Close()

	cfg := Config{Enabled: true, Provider: ProviderSearxng, Endpoint: srv.URL, MaxResults: 5}.Normalize()
	results, err := New(cfg).Search(context.Background(), "杭州天气")
	if err != nil {
		t.Fatal(err)
	}
	if gotQuery.Get("format") != "json" || gotQuery.Get("q") != "杭州天气" {
		t.Errorf("query = %v", gotQuery)
	}
	if len(results) != 2 {
		t.Fatalf("应去重成 2 条，得到 %d：%+v", len(results), results)
	}
	if results[0].Engine != "bing" || results[1].Snippet != "摘要 B" {
		t.Errorf("results = %+v", results)
	}
}

func TestSearxngJSONForbidden(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	cfg := Config{Enabled: true, Provider: ProviderSearxng, Endpoint: srv.URL}.Normalize()
	_, err := New(cfg).Search(context.Background(), "x")
	if err == nil {
		t.Fatal("403 应报错")
	}
	if !strings.Contains(err.Error(), "settings.yml") {
		t.Errorf("错误信息应指路 settings.yml，得到 %q", err)
	}
	var we *Error
	if ok := errors.As(err, &we); !ok || we.APICode() != "websearch_failed" {
		t.Errorf("错误应实现 apierr 规范码，得到 %v", err)
	}
}

func TestTavilySearch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s", r.Method)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer tk" {
			t.Errorf("Authorization = %q", got)
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["query"] != "q" || body["max_results"].(float64) != 3 {
			t.Errorf("body = %v", body)
		}
		_, _ = io.WriteString(w, `{"results":[{"title":"T","url":"https://t.example","content":"C","score":0.9}]}`)
	}))
	defer srv.Close()

	cfg := Config{Enabled: true, Provider: ProviderTavily, Endpoint: srv.URL, APIKey: "tk", MaxResults: 3}.Normalize()
	results, err := New(cfg).Search(context.Background(), "q")
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Title != "T" || results[0].Engine != ProviderTavily {
		t.Errorf("results = %+v", results)
	}
}

func TestBraveSearch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("X-Subscription-Token"); got != "bk" {
			t.Errorf("token = %q", got)
		}
		if r.URL.Query().Get("search_lang") != "zh-hans" {
			t.Errorf("search_lang = %q", r.URL.Query().Get("search_lang"))
		}
		_, _ = io.WriteString(w, `{"web":{"results":[{"title":"B","url":"https://b.example","description":"D"}]}}`)
	}))
	defer srv.Close()

	cfg := Config{Enabled: true, Provider: ProviderBrave, Endpoint: srv.URL, APIKey: "bk"}.Normalize()
	results, err := New(cfg).Search(context.Background(), "q")
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Snippet != "D" || results[0].Engine != ProviderBrave {
		t.Errorf("results = %+v", results)
	}
}

func TestMissingConfig(t *testing.T) {
	cases := []struct{ cfg Config }{
		{Config{Enabled: true, Provider: ProviderSearxng}}, // 没有端点
		{Config{Enabled: true, Provider: ProviderTavily}},  // 没有 key
		{Config{Enabled: true, Provider: ProviderBrave}},   // 没有 key
	}
	for _, c := range cases {
		_, err := New(c.cfg).Search(context.Background(), "q")
		if err == nil {
			t.Errorf("%+v 应报「未配置」", c.cfg)
		}
	}
}

// --- 缓存 / 开关 ---

func TestSearchCache(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		_, _ = io.WriteString(w, `{"results":[{"title":"A","url":"https://a.example"}]}`)
	}))
	defer srv.Close()

	cfg := Config{Enabled: true, Provider: ProviderSearxng, Endpoint: srv.URL,
		CacheTTL: time.Minute}.Normalize()
	s := New(cfg)
	for i := 0; i < 3; i++ {
		if _, err := s.Search(context.Background(), "same"); err != nil {
			t.Fatal(err)
		}
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Errorf("命中缓存时后端应只被调用 1 次，实际 %d 次", got)
	}

	// 配置变更必须清缓存，否则会继续用旧后端的结果。
	s.Update(cfg)
	if _, err := s.Search(context.Background(), "same"); err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt32(&hits); got != 2 {
		t.Errorf("Update 后应重新请求，实际 %d 次", got)
	}
}

func TestSearchDisabledDoesNotHitBackend(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cfg := Config{Enabled: false, Provider: ProviderSearxng, Endpoint: srv.URL}.Normalize()
	_, err := New(cfg).Search(context.Background(), "q")
	if err == nil {
		t.Fatal("未启用应报错（不能静默跳过检索）")
	}
	if atomic.LoadInt32(&hits) != 0 {
		t.Error("未启用时不应请求后端")
	}
}

// --- 正文抓取 ---

func TestFetchPageExtractsText(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, `<html><head><title>标题</title></head>
<body><script>bad()</script><p>第一段</p><p>第二段</p></body></html>`)
	}))
	defer srv.Close()

	// httptest 监听在 127.0.0.1，正式抓取器会（正确地）拦掉它；
	// 这里换一个不做内网拦截的拨号器，只为验证抽取逻辑。
	f := newFetcher(Config{}.Normalize())
	f.allowAnyPort = true
	f.client.Transport = &http.Transport{DialContext: (&net.Dialer{}).DialContext}

	title, text, err := f.page(context.Background(), srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if title != "标题" {
		t.Errorf("title = %q", title)
	}
	if !strings.Contains(text, "第一段") || !strings.Contains(text, "第二段") {
		t.Errorf("text = %q", text)
	}
	if strings.Contains(text, "bad()") {
		t.Errorf("script 不应进入正文：%q", text)
	}
}

// 回归：主页抓取必须真的走内网拦截，否则网关就成了 SSRF 跳板。
func TestFetchPageBlocksLoopback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "should not reach")
	}))
	defer srv.Close()

	f := newFetcher(Config{}.Normalize())
	if _, _, err := f.page(context.Background(), srv.URL); err == nil {
		t.Fatal("抓取 127.0.0.1 必须被拒绝")
	}
}

// --- 注入文本 ---

func TestContextBlock(t *testing.T) {
	results := []Result{
		{Title: "A", URL: "https://a.example", Snippet: "摘要 A"},
		{Title: "", URL: "https://b.example", Content: strings.Repeat("正文", 2000)},
	}
	block := Context(ProviderSearxng, "杭州天气", results, 4000)
	for _, want := range []string{"杭州天气", "searxng", "[1] A", "[2] https://b.example", "摘要 A", "不可信"} {
		if !strings.Contains(block, want) {
			t.Errorf("注入块缺少 %q", want)
		}
	}
	if len([]rune(block)) > 4100 {
		t.Errorf("注入块未按上限截断：%d 字", len([]rune(block)))
	}
}

// --- SearXNG 反向代理 Basic Auth ---

func TestSearxngAuthHeader(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", ""},
		{"   ", ""},
		{"admin:pw", "Basic YWRtaW46cHc="},
		{"  admin:pw  ", "Basic YWRtaW46cHc="},
		{"Basic YWRtaW46cHc=", "Basic YWRtaW46cHc="},
		{"basic abc123", "basic abc123"},
		// 已经 base64 过的凭据（有人直接从 curl -u 的抓包里拷）
		{"YWRtaW46cHc=", "Basic YWRtaW46cHc="},
	}
	for _, c := range cases {
		if got := searxngAuthHeader(c.in); got != c.want {
			t.Errorf("searxngAuthHeader(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// 自己的 SearXNG 通常挂在 Basic Auth 反代后面，请求必须带 Authorization，
// 不带就是 401（用户踩过一次：控制台填了凭据但网关没发头）。
func TestSearxngSendsBasicAuth(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		if r.URL.Query().Get("format") != "json" {
			t.Errorf("format = %q", r.URL.Query().Get("format"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"results":[{"title":"T","url":"https://x.example","content":"C"}]}`)
	}))
	defer srv.Close()

	cfg := Config{Provider: ProviderSearxng, Endpoint: srv.URL, APIKey: "admin:pw"}.Normalize()
	res, err := searchSearxng(context.Background(), cfg, "test")
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 1 || res[0].Title != "T" {
		t.Fatalf("结果 = %+v", res)
	}
	if gotAuth != "Basic YWRtaW46cHc=" {
		t.Errorf("Authorization = %q", gotAuth)
	}

	// 没填凭据时不应凭空造一个头。
	gotAuth = "unset"
	cfg.APIKey = ""
	if _, err := searchSearxng(context.Background(), cfg, "test"); err != nil {
		t.Fatal(err)
	}
	if gotAuth != "" {
		t.Errorf("无凭据时 Authorization = %q，应为空", gotAuth)
	}
}

// 401 的报错要直接告诉用户去哪填，而不是只回一句 HTTP 401。
func TestSearxngUnauthorizedHint(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, "Unauthorized")
	}))
	defer srv.Close()

	cfg := Config{Provider: ProviderSearxng, Endpoint: srv.URL}.Normalize()
	_, err := searchSearxng(context.Background(), cfg, "test")
	if err == nil {
		t.Fatal("预期 401 报错")
	}
	if !strings.Contains(err.Error(), "user:pass") {
		t.Errorf("报错未提示怎么填凭据：%v", err)
	}
}
