package server

// 独立的联网检索端点。
//
//   POST /v1/search          （用户密钥鉴权，响应形状与 Tavily 对齐）
//   POST /admin/api/websearch/test （控制台「测试」按钮）
//
// 有它才能把「搜索」当成一个独立服务卖给客户端：Dify / Cherry Studio /
// n8n 这类客户端自带 Tavily 插件，改个 base_url 就能直接调用，
// 不必经过 chat 链路，也不必让模型先「决定要不要搜」。

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"codearts2api/internal/apierr"
	"codearts2api/internal/websearch"
)

// searchRequest 是 /v1/search 的请求体（尽量兼容常见写法）。
type searchRequest struct {
	Query string `json:"query"`
	// Q 是 OpenAI / 各家客户端常见的简写，等同于 query。
	Q string `json:"q"`
	// MaxResults 缺省用控制台配置。
	MaxResults int `json:"max_results"`
	// IncludeContent 显式要求正文（默认遵循控制台「抓取正文」开关）。
	IncludeContent *bool `json:"include_content"`
}

// searchResultItem 是响应里的一条结果（Tavily 同名字段 + snippet 别名）。
type searchResultItem struct {
	Title   string  `json:"title"`
	URL     string  `json:"url"`
	Content string  `json:"content"`
	Snippet string  `json:"snippet"`
	Score   float64 `json:"score,omitempty"`
	Engine  string  `json:"engine,omitempty"`
}

// handleSearch 实现 POST/GET /v1/search。
func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost && r.Method != http.MethodGet {
		writeRequestErr(w, http.StatusMethodNotAllowed, "仅支持 POST / GET", "invalid_request_error", "", "")
		return
	}

	var req searchRequest
	if r.Method == http.MethodGet {
		q := r.URL.Query()
		req.Query = firstNonEmptyString(q.Get("query"), q.Get("q"))
		req.MaxResults = atoiSafe(q.Get("max_results"))
	} else {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeRequestErr(w, http.StatusBadRequest, "请求体解析失败: "+err.Error(),
				"invalid_request_error", apierr.CodeInvalidValue, "")
			return
		}
	}
	query := strings.TrimSpace(firstNonEmptyString(req.Query, req.Q))
	if query == "" {
		writeRequestErr(w, http.StatusBadRequest, "query 不能为空",
			"invalid_request_error", apierr.CodeInvalidValue, "query")
		return
	}

	s.searchResults(w, r, query, req.MaxResults, req.IncludeContent != nil && *req.IncludeContent)
}

// handleAdminWebSearchTest 是控制台的联通性测试：用真实后端搜一次。
func (s *Server) handleAdminWebSearchTest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "仅支持 POST", "invalid_request_error")
		return
	}
	var req searchRequest
	_ = json.NewDecoder(r.Body).Decode(&req)
	query := strings.TrimSpace(firstNonEmptyString(req.Query, req.Q, "OpenAI"))
	s.searchResults(w, r, query, req.MaxResults, req.IncludeContent != nil && *req.IncludeContent)
}

// searchResults 是两条入口的公共实现。
func (s *Server) searchResults(w http.ResponseWriter, r *http.Request, query string, maxResults int, wantContent bool) {
	svc := s.webSearch()
	cfg := svc.Config()
	if !cfg.Enabled {
		writeRequestErr(w, http.StatusServiceUnavailable,
			"网关未启用联网搜索：请先在控制台「联网搜索」里配置后端并勾选启用",
			"server_error", apierr.CodeWebSearchDisabled, "")
		return
	}
	if wantContent && !cfg.FetchPages {
		// 单次请求要求正文：临时开一个抓正文的服务，不改全局设置。
		cfg.FetchPages = true
		svc = websearch.New(cfg)
	}

	start := time.Now()
	results, err := svc.SearchWithOptions(r.Context(), query, websearch.SearchOptions{MaxResults: maxResults})
	if err != nil {
		e := apierr.From(err)
		writeRequestErr(w, e.Status, err.Error(), e.OpenAI, e.Code, e.Param)
		return
	}

	items := make([]searchResultItem, 0, len(results))
	for _, res := range results {
		items = append(items, searchResultItem{
			Title:   res.Title,
			URL:     res.URL,
			Content: firstNonEmptyString(res.Content, res.Snippet),
			Snippet: res.Snippet,
			Score:   res.Score,
			Engine:  res.Engine,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"query":         query,
		"provider":      svc.Config().Provider,
		"count":         len(items),
		"response_time": time.Since(start).Seconds(),
		"results":       items,
	})
}

// firstNonEmptyString 返回第一个非空（trim 后）的值。
func firstNonEmptyString(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// atoiSafe 解析非负整数，失败返回 0（表示「用默认值」）。
func atoiSafe(s string) int {
	n := 0
	for _, r := range strings.TrimSpace(s) {
		if r < '0' || r > '9' {
			return 0
		}
		n = n*10 + int(r-'0')
		if n > 100 {
			return 100
		}
	}
	return n
}
