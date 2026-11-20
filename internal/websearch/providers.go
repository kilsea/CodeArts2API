package websearch

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// 默认端点：SearXNG 必须显式配置（自托管地址各不相同），另两家有官方默认值。
const (
	defaultTavilyEndpoint = "https://api.tavily.com/search"
	defaultBraveEndpoint  = "https://api.search.brave.com/res/v1/web/search"
	// 单次响应体上限：搜索结果本来就小，2MB 只用于兜住异常后端。
	maxResponseBytes = 2 << 20
)

// searchSearxng 调用 SearXNG 的 JSON API。
//
// 前提：实例的 settings.yml 里 search.formats 必须包含 json，
// 否则该实例只回网页（请求会被当成浏览器访问，返回 HTML 或 403）。
func searchSearxng(ctx context.Context, cfg Config, query string) ([]Result, error) {
	if strings.TrimSpace(cfg.Endpoint) == "" {
		return nil, &Error{Provider: ProviderSearxng, Reason: "未配置 SearXNG 地址（控制台 → 联网搜索 → 端点）"}
	}
	u, err := url.Parse(strings.TrimRight(cfg.Endpoint, "/") + "/search")
	if err != nil {
		return nil, &Error{Provider: ProviderSearxng, Reason: "SearXNG 地址不合法：" + err.Error()}
	}
	q := u.Query()
	q.Set("q", query)
	q.Set("format", "json")
	q.Set("language", cfg.Language)
	q.Set("safesearch", "0")
	q.Set("pageno", "1")
	u.RawQuery = q.Encode()

	var headers map[string]string
	if h := searxngAuthHeader(cfg.APIKey); h != "" {
		headers = map[string]string{"Authorization": h}
	}
	body, status, err := getJSON(ctx, cfg, u.String(), headers)
	if err != nil {
		if status == http.StatusUnauthorized {
			return nil, &Error{Provider: ProviderSearxng, Status: status,
				Reason: "SearXNG 需要认证：在「API Key」里填 user:pass（发 Basic Auth）"}
		}
		if status == http.StatusForbidden {
			return nil, &Error{Provider: ProviderSearxng, Status: status,
				Reason: "SearXNG 未开启 JSON 输出：settings.yml 里 search.formats 需要包含 json；" +
					"若开了 limiter，还需要关掉它（网关自己限流）"}
		}
		return nil, &Error{Provider: ProviderSearxng, Status: status, Reason: reasonOf(err, status)}
	}

	var payload struct {
		Results []struct {
			Title   string  `json:"title"`
			URL     string  `json:"url"`
			Content string  `json:"content"`
			Engine  string  `json:"engine"`
			Score   float64 `json:"score"`
		} `json:"results"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, &Error{Provider: ProviderSearxng, Status: status, Reason: "返回体不是合法 JSON：" + err.Error()}
	}
	out := make([]Result, 0, len(payload.Results))
	for _, r := range payload.Results {
		out = append(out, Result{
			Title:   strings.TrimSpace(r.Title),
			URL:     strings.TrimSpace(r.URL),
			Snippet: truncateRunes(strings.TrimSpace(r.Content), maxSnippetChars),
			Score:   r.Score,
			Engine:  r.Engine,
		})
	}
	return out, nil
}

// searxngAuthHeader 把「user:pass」或已编码好的凭据转成 Authorization 头。
//
// SearXNG 本身没有认证，但线上部署普遍在前面挂一层 Basic Auth 反向代理。
// 控制台只有一个多余的「API Key」输入框可用，所以这里兼容三种填法：
//
//	user:pass     → Basic base64(user:pass)
//	Basic xxx     → 原样传
//	其它（base64） → Basic xxx
func searxngAuthHeader(key string) string {
	key = strings.TrimSpace(key)
	if key == "" {
		return ""
	}
	if strings.HasPrefix(strings.ToLower(key), "basic ") {
		return key
	}
	if strings.Contains(key, ":") {
		return "Basic " + base64.StdEncoding.EncodeToString([]byte(key))
	}
	return "Basic " + key
}

// searchTavily 调用 Tavily Search API。
func searchTavily(ctx context.Context, cfg Config, query string) ([]Result, error) {
	if strings.TrimSpace(cfg.APIKey) == "" {
		return nil, &Error{Provider: ProviderTavily, Reason: "未配置 Tavily API Key"}
	}
	endpoint := strings.TrimSpace(cfg.Endpoint)
	if endpoint == "" {
		endpoint = defaultTavilyEndpoint
	}
	payload, _ := json.Marshal(map[string]any{
		"query":        query,
		"max_results":  cfg.MaxResults,
		"search_depth": "basic",
		// 只要结果列表：answer 会掩盖模型自己的综合，raw_content 太长。
		"include_answer":      false,
		"include_raw_content": false,
	})
	// 新版 Tavily 用 Bearer，老版读 body 里的 api_key；两个都带上，省得踩版本。
	headers := map[string]string{
		"Authorization": "Bearer " + cfg.APIKey,
		"Content-Type":  "application/json",
	}
	body, status, err := postJSON(ctx, cfg, endpoint, headers, payload)
	if err != nil {
		return nil, &Error{Provider: ProviderTavily, Status: status, Reason: reasonOf(err, status)}
	}
	var resp struct {
		Results []struct {
			Title   string  `json:"title"`
			URL     string  `json:"url"`
			Content string  `json:"content"`
			Score   float64 `json:"score"`
		} `json:"results"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, &Error{Provider: ProviderTavily, Status: status, Reason: "返回体不是合法 JSON：" + err.Error()}
	}
	out := make([]Result, 0, len(resp.Results))
	for _, r := range resp.Results {
		out = append(out, Result{
			Title:   strings.TrimSpace(r.Title),
			URL:     strings.TrimSpace(r.URL),
			Snippet: truncateRunes(strings.TrimSpace(r.Content), maxSnippetChars),
			Score:   r.Score,
			Engine:  ProviderTavily,
		})
	}
	return out, nil
}

// searchBrave 调用 Brave Web Search API。
func searchBrave(ctx context.Context, cfg Config, query string) ([]Result, error) {
	if strings.TrimSpace(cfg.APIKey) == "" {
		return nil, &Error{Provider: ProviderBrave, Reason: "未配置 Brave API Key"}
	}
	endpoint := strings.TrimSpace(cfg.Endpoint)
	if endpoint == "" {
		endpoint = defaultBraveEndpoint
	}
	u, err := url.Parse(endpoint)
	if err != nil {
		return nil, &Error{Provider: ProviderBrave, Reason: "Brave 端点不合法：" + err.Error()}
	}
	q := u.Query()
	q.Set("q", query)
	q.Set("count", fmt.Sprint(cfg.MaxResults))
	if strings.HasPrefix(strings.ToLower(cfg.Language), "zh") {
		q.Set("search_lang", "zh-hans")
	}
	u.RawQuery = q.Encode()

	headers := map[string]string{
		"X-Subscription-Token": cfg.APIKey,
		"Accept":               "application/json",
	}
	body, status, err := getJSON(ctx, cfg, u.String(), headers)
	if err != nil {
		return nil, &Error{Provider: ProviderBrave, Status: status, Reason: reasonOf(err, status)}
	}
	var resp struct {
		Web struct {
			Results []struct {
				Title       string `json:"title"`
				URL         string `json:"url"`
				Description string `json:"description"`
			} `json:"results"`
		} `json:"web"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, &Error{Provider: ProviderBrave, Status: status, Reason: "返回体不是合法 JSON：" + err.Error()}
	}
	out := make([]Result, 0, len(resp.Web.Results))
	for _, r := range resp.Web.Results {
		out = append(out, Result{
			Title:   strings.TrimSpace(r.Title),
			URL:     strings.TrimSpace(r.URL),
			Snippet: truncateRunes(strings.TrimSpace(r.Description), maxSnippetChars),
			Engine:  ProviderBrave,
		})
	}
	return out, nil
}

const maxSnippetChars = 400

// apiClient 是检索后端共用的 HTTP 客户端。
//
// 单次超时由调用方的 ctx 控制（Service.Search 会按 cfg.Timeout 建 deadline），
// 因此这里不设 Timeout 字段 —— 设了反而会让较长的抓取被提前掐断。
var apiClient = &http.Client{
	Transport: &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		MaxIdleConns:          16,
		MaxIdleConnsPerHost:   4,
		IdleConnTimeout:       60 * time.Second,
		TLSHandshakeTimeout:   5 * time.Second,
		ForceAttemptHTTP2:     true,
		ResponseHeaderTimeout: 10 * time.Second,
	},
}

// getJSON 发一个带默认头的 GET，并读回响应体。
func getJSON(ctx context.Context, cfg Config, rawURL string, headers map[string]string) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", cfg.UserAgent)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	return doJSON(req)
}

// postJSON 发一个 JSON POST。
func postJSON(ctx context.Context, cfg Config, rawURL string, headers map[string]string, payload []byte) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rawURL, bytes.NewReader(payload))
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", cfg.UserAgent)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	return doJSON(req)
}

// doJSON 执行请求并统一处理非 2xx。
//
// 响应体只读前 maxResponseBytes：后端被改坏时不应该把网关内存吃光。
func doJSON(req *http.Request) ([]byte, int, error) {
	resp, err := apiClient.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		_ = resp.Body.Close()
	}()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return nil, resp.StatusCode, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, resp.StatusCode, fmt.Errorf("HTTP %d：%s", resp.StatusCode, snippetForError(body))
	}
	return body, resp.StatusCode, nil
}

// reasonOf 去掉 doJSON 拼进去的「HTTP nnn：」前缀。
//
// Error.Error() 自己会渲染状态码，保留前缀会得到
// 「后端返回 HTTP 403：HTTP 403：…」这种重复文案。
func reasonOf(err error, status int) string {
	s := err.Error()
	if status > 0 {
		s = strings.TrimPrefix(s, fmt.Sprintf("HTTP %d：", status))
	}
	return s
}

// snippetForError 从错误响应里挤出一点可读信息（有的后端回 HTML 错误页）。
func snippetForError(body []byte) string {
	s := strings.TrimSpace(string(body))
	if s == "" {
		return "空响应体"
	}
	s = strings.Join(strings.Fields(s), " ")
	return truncateRunes(s, 200)
}
