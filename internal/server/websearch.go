package server

// 联网搜索（Web Search）在网关侧的接线。
//
// 分工：
//   - internal/websearch 负责「检索 + 抓正文」（可替换后端、SSRF 防护）；
//   - 本文件负责「识别客户端声明的内置搜索工具 → 执行检索 →
//     把资料注入上下文并把那个工具从请求里摘掉」；
//   - 三条协议（Chat / Responses / Anthropic）共用这里的注入逻辑。
//
// 为什么是「注入」而不是「工具循环」：上游只接受 type=function，网关若把
// 搜索结果当成一次 tool 调用回灌，需要多打一轮上游（多花积分、多一次延迟），
// 而且主流客户端（Dify / Cherry Studio / Claude Code）对内置搜索的预期就是
// 「一问一答、答案里带引用」。需要多轮深挖的，客户端自己声明 function 工具即可。

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"codearts2api/internal/apierr"
	"codearts2api/internal/protocol"
	"codearts2api/internal/store"
	"codearts2api/internal/websearch"
)

// webSearchConfig 把持久化设置翻译成 websearch 配置。
func webSearchConfig(st store.Settings) websearch.Config {
	return websearch.Config{
		Enabled:      st.WebSearchEnabled,
		Provider:     st.WebSearchProvider,
		Endpoint:     st.WebSearchEndpoint,
		APIKey:       st.WebSearchAPIKey,
		MaxResults:   st.WebSearchMaxResults,
		Timeout:      time.Duration(st.WebSearchTimeout) * time.Second,
		FetchPages:   st.WebSearchFetchPages,
		MaxPageChars: st.WebSearchMaxPageChars,
		CacheTTL:     time.Duration(st.WebSearchCacheTTL) * time.Second,
		// 上游语言头是 zh-cn（小写），SearXNG 认 zh-CN；不映射会退化成「所有语言」。
		Language: languageForSearch(st.Language),
	}
}

// languageForSearch 把控制台的语言设置映射成检索后端认识的值。
func languageForSearch(lang string) string {
	if strings.HasPrefix(strings.ToLower(strings.TrimSpace(lang)), "zh") {
		return "zh-CN"
	}
	return lang
}

// webSearch 返回检索服务；设置变更时热更新（缓存随配置一起清空）。
func (s *Server) webSearch() *websearch.Service {
	cfg := webSearchConfig(s.Store.Settings())
	fp := cfg.Fingerprint()

	s.searchMu.Lock()
	defer s.searchMu.Unlock()
	if s.searchSvc == nil {
		s.searchSvc = websearch.New(cfg)
		s.searchFP = fp
		return s.searchSvc
	}
	if s.searchFP != fp {
		s.searchSvc.Update(cfg)
		s.searchFP = fp
	}
	return s.searchSvc
}

// isWebSearchToolType 判断客户端声明的工具是否属于「网关代执行」的联网搜索。
//
// 覆盖各家的写法：
//   - OpenAI Chat / Responses：web_search、web_search_preview、web_search_preview_2025_03_11
//   - Anthropic：web_search_20250305
//   - 某些客户端直接叫 websearch
func isWebSearchToolType(t string) bool {
	t = strings.ToLower(strings.TrimSpace(t))
	return t == "websearch" || strings.HasPrefix(t, "web_search")
}

// toolChoiceMentionsWebSearch 判断 tool_choice 是否点名了内置搜索工具。
//
// 两种形状都要认：
//   - Chat：{"type":"function","function":{"name":"web_search"}}
//   - Anthropic：{"type":"tool","name":"web_search"}
func toolChoiceMentionsWebSearch(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}
	var tc struct {
		Name     string `json:"name"`
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if err := json.Unmarshal(raw, &tc); err != nil {
		return false
	}
	name := tc.Function.Name
	if name == "" {
		name = tc.Name
	}
	return name != "" && isWebSearchToolType(name)
}

// webSearchDisabledErr 是「客户端要联网、但网关没开」的明确拒绝。
//
// 不静默摘掉工具：那会让客户端以为模型查过了，实际只凭训练数据作答。
func webSearchDisabledErr(param string) error {
	return &apierr.InvalidParamError{
		Param: param,
		Code:  apierr.CodeWebSearchDisabled,
		Reason: "网关未启用联网搜索：请先在控制台「联网搜索」里配置后端（推荐自托管 SearXNG）。" +
			"上游本身不支持内置 web_search 工具，联网必须由网关代执行。",
	}
}

// lastUserText 从 Chat 的 messages 里取最后一条用户文本作为检索词。
//
// 取一个「意图句」就够了：搜索 API 对长 query 的效果反而更差；
// 这里上限 500 字，避免把整段代码贴进搜索后端。
func lastUserText(raw json.RawMessage) string {
	var msgs []struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(raw, &msgs); err != nil {
		return ""
	}
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role != "user" {
			continue
		}
		if txt := contentText(msgs[i].Content); txt != "" {
			return truncateText(txt, 500)
		}
	}
	return ""
}

// contentText 把 content（字符串或分段数组）压成纯文本。
func contentText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return strings.TrimSpace(s)
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err != nil {
		return ""
	}
	var b strings.Builder
	for _, p := range parts {
		if p.Text == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		b.WriteString(p.Text)
	}
	return strings.TrimSpace(b.String())
}

// truncateText 按字符截断。
func truncateText(s string, n int) string {
	rs := []rune(s)
	if n <= 0 || len(rs) <= n {
		return s
	}
	return string(rs[:n]) + "…"
}

// chatWebSearchResult 是一次请求的检索结果。
type chatWebSearchResult struct {
	body []byte
	// info 写进 X-Gateway-Web-Search（客户端 / 日志一眼能看出「这次真的联网了」）。
	info string
}

// applyChatWebSearch 在转发上游之前执行客户端声明的联网搜索。
//
// 返回 nil 表示请求里没有内置搜索工具，调用方原样转发。
func (s *Server) applyChatWebSearch(ctx context.Context, body []byte) (*chatWebSearchResult, error) {
	// 先做一次廉价的子串预筛：绝大多数请求没有内置搜索工具，
	// 不该为一个可选功能把可能几十 MB 的请求体再解析一遍。
	if !bytes.Contains(body, []byte("web_search")) && !bytes.Contains(body, []byte("websearch")) {
		return nil, nil
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, nil
	}
	rawTools, ok := m["tools"]
	if !ok {
		return nil, nil
	}
	var tools []json.RawMessage
	if err := json.Unmarshal(rawTools, &tools); err != nil {
		return nil, nil
	}

	kept := make([]json.RawMessage, 0, len(tools))
	found := false
	for _, t := range tools {
		var head struct {
			Type string `json:"type"`
			Name string `json:"name"`
		}
		_ = json.Unmarshal(t, &head)
		if isWebSearchToolType(head.Type) || (head.Type == "" && strings.EqualFold(head.Name, "web_search")) {
			found = true
			continue
		}
		kept = append(kept, t)
	}
	if !found {
		return nil, nil
	}

	svc := s.webSearch()
	if !svc.Enabled() {
		return nil, webSearchDisabledErr("tools")
	}
	query := lastUserText(m["messages"])
	if query == "" {
		return nil, &apierr.InvalidParamError{
			Param:  "messages",
			Code:   apierr.CodeInvalidValue,
			Reason: "要用联网搜索，messages 里至少需要一条非空的 user 文本",
		}
	}

	start := time.Now()
	results, err := svc.Search(ctx, query)
	if err != nil {
		return nil, err
	}
	elapsed := time.Since(start)

	block := websearch.Context(svc.Config().Provider, query, results, 0)
	msgs, err := prependSystemRaw(m["messages"], block)
	if err != nil {
		return nil, &apierr.InvalidParamError{Param: "messages", Code: apierr.CodeInvalidValue,
			Reason: "无法把搜索资料注入 messages：" + err.Error()}
	}
	m["messages"] = msgs

	if len(kept) == 0 {
		// 工具被摘空了就把 keys 一起删掉：空的 tools 数组会被上游当成非法参数。
		delete(m, "tools")
		delete(m, "tool_choice")
	} else if toolChoiceMentionsWebSearch(m["tool_choice"]) {
		// 工具还在、但 tool_choice 指向被摘掉的 web_search：留着上游会说「没有这个工具」。
		delete(m, "tool_choice")
	} else {
		keptRaw, err := json.Marshal(kept)
		if err != nil {
			return nil, err
		}
		m["tools"] = keptRaw
	}

	out, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	return &chatWebSearchResult{
		body: out,
		info: fmt.Sprintf("%s; results=%d; %dms", svc.Config().Provider, len(results), elapsed.Milliseconds()),
	}, nil
}

// prependSystemRaw 把资料块注入 messages。
//
// 优先合并进已存在的第一条 system（保持「只有一条 system」的干净形状），
// 没有就新插一条；段的数组形状（多模态）则前置一个 text 段。
func prependSystemRaw(rawMessages json.RawMessage, block string) (json.RawMessage, error) {
	var msgs []json.RawMessage
	if err := json.Unmarshal(rawMessages, &msgs); err != nil {
		return nil, err
	}
	if len(msgs) == 0 {
		return nil, fmt.Errorf("messages 为空")
	}

	var first map[string]json.RawMessage
	if err := json.Unmarshal(msgs[0], &first); err == nil {
		var role string
		_ = json.Unmarshal(first["role"], &role)
		if role == "system" {
			merged, err := mergeSystemContent(first["content"], block)
			if err != nil {
				return nil, err
			}
			first["content"] = merged
			b, err := json.Marshal(first)
			if err != nil {
				return nil, err
			}
			msgs[0] = b
			return json.Marshal(msgs)
		}
	}

	sys, err := json.Marshal(map[string]string{"role": "system", "content": block})
	if err != nil {
		return nil, err
	}
	msgs = append([]json.RawMessage{sys}, msgs...)
	return json.Marshal(msgs)
}

// mergeSystemContent 把资料块并入 system 的 content（字符串或分段数组）。
func mergeSystemContent(raw json.RawMessage, block string) (json.RawMessage, error) {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		if strings.TrimSpace(s) == "" {
			return json.Marshal(block)
		}
		return json.Marshal(block + "\n\n" + s)
	}
	var parts []map[string]any
	if err := json.Unmarshal(raw, &parts); err != nil {
		return nil, err
	}
	parts = append([]map[string]any{{"type": "text", "text": block}}, parts...)
	return json.Marshal(parts)
}

// prependSystemMap 是 prependSystemRaw 的 map 版本（Responses / Anthropic 转换后使用）。
func prependSystemMap(msgs []map[string]any, block string) []map[string]any {
	if len(msgs) == 0 {
		return []map[string]any{{"role": "system", "content": block}}
	}
	if role, _ := msgs[0]["role"].(string); role == "system" {
		switch c := msgs[0]["content"].(type) {
		case string:
			if strings.TrimSpace(c) == "" {
				msgs[0]["content"] = block
			} else {
				msgs[0]["content"] = block + "\n\n" + c
			}
			return msgs
		case []map[string]any:
			msgs[0]["content"] = append([]map[string]any{{"type": "text", "text": block}}, c...)
			return msgs
		case []any:
			msgs[0]["content"] = append([]any{map[string]any{"type": "text", "text": block}}, c...)
			return msgs
		}
	}
	return append([]map[string]any{{"role": "system", "content": block}}, msgs...)
}

// searchInjection 是「已执行检索、等待注入」的结果。
type searchInjection struct {
	block string
	info  string
}

// applyResponsesWebSearch 处理 Responses 请求里的内置 web_search 工具。
//
// Responses 的 tools 是扁平结构（{"type":"web_search"}），OpenAI 官方由服务端执行；
// 这里由网关代执行，再把结果注入 messages —— 客户端拿到的仍是普通答案 + 引用。
func (s *Server) applyResponsesWebSearch(ctx context.Context, req *protocol.ResponsesRequest) (*searchInjection, error) {
	kept := make([]protocol.ResponsesTool, 0, len(req.Tools))
	found := false
	for _, t := range req.Tools {
		if isWebSearchToolType(t.Type) {
			found = true
			continue
		}
		kept = append(kept, t)
	}
	if !found {
		return nil, nil
	}
	svc := s.webSearch()
	if !svc.Enabled() {
		return nil, webSearchDisabledErr("tools")
	}
	query := responsesQuery(req.Input)
	if query == "" {
		return nil, &apierr.InvalidParamError{
			Param:  "input",
			Code:   apierr.CodeInvalidValue,
			Reason: "要用联网搜索，input 里至少需要一条非空的 user 文本",
		}
	}

	start := time.Now()
	results, err := svc.Search(ctx, query)
	if err != nil {
		return nil, err
	}
	req.Tools = kept
	if len(kept) == 0 || toolChoiceMentionsWebSearch(req.ToolChoice) {
		// 工具被摘空（tool_choice=required 会报错），或 tool_choice 点名了 web_search。
		req.ToolChoice = nil
	}
	return &searchInjection{
		block: websearch.Context(svc.Config().Provider, query, results, 0),
		info: fmt.Sprintf("%s; results=%d; %dms",
			svc.Config().Provider, len(results), time.Since(start).Milliseconds()),
	}, nil
}

// applyAnthropicWebSearch 处理 Anthropic 的 server tool：
// {"type":"web_search_20250305","name":"web_search"}。
//
// 顺带把其它服务端工具（computer / bash / text_editor …）显式拒绝：
// 以前它们会被当成空参数的 function 工具转发给上游，模型照着幻觉调用，
// 客户端却收不到任何结果 —— 静默失败最难排查。
func (s *Server) applyAnthropicWebSearch(ctx context.Context, req *protocol.Request) (*searchInjection, error) {
	kept := make([]protocol.Tool, 0, len(req.Tools))
	found := false
	for i, t := range req.Tools {
		if isWebSearchToolType(t.Type) {
			found = true
			continue
		}
		if t.Type != "" && t.Type != "custom" {
			return nil, &apierr.InvalidParamError{
				Param:  fmt.Sprintf("tools[%d].type", i),
				Code:   apierr.CodeUnsupportedParam,
				Reason: "服务端工具类型 " + t.Type + " 不支持（只支持自定义工具与 web_search，后者由网关代执行）",
			}
		}
		kept = append(kept, t)
	}
	if !found {
		return nil, nil
	}
	svc := s.webSearch()
	if !svc.Enabled() {
		return nil, webSearchDisabledErr("tools")
	}
	query := anthropicQuery(req.Messages)
	if query == "" {
		return nil, &apierr.InvalidParamError{
			Param:  "messages",
			Code:   apierr.CodeInvalidValue,
			Reason: "要用联网搜索，messages 里至少需要一条非空的 user 文本",
		}
	}

	start := time.Now()
	results, err := svc.Search(ctx, query)
	if err != nil {
		return nil, err
	}
	req.Tools = kept
	if toolChoiceMentionsWebSearch(req.ToolChoice) {
		req.ToolChoice = nil
	}
	return &searchInjection{
		block: websearch.Context(svc.Config().Provider, query, results, 0),
		info: fmt.Sprintf("%s; results=%d; %dms",
			svc.Config().Provider, len(results), time.Since(start).Milliseconds()),
	}, nil
}

// responsesQuery 从 Responses 的 input 里取最后一条用户文本。
func responsesQuery(raw json.RawMessage) string {
	msgs, err := protocol.ResponsesInputMessages(raw)
	if err != nil {
		return ""
	}
	for i := len(msgs) - 1; i >= 0; i-- {
		if role, _ := msgs[i]["role"].(string); role != "user" {
			continue
		}
		if txt := anyContentText(msgs[i]["content"]); txt != "" {
			return truncateText(txt, 500)
		}
	}
	return ""
}

// anthropicQuery 从 Anthropic messages 里取最后一条用户文本。
func anthropicQuery(msgs []protocol.Message) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role != "user" {
			continue
		}
		if txt := contentText(msgs[i].Content); txt != "" {
			return truncateText(txt, 500)
		}
	}
	return ""
}

// anyContentText 处理「字符串 / 内容块数组 / 已解析的 map」三种形状。
func anyContentText(v any) string {
	switch c := v.(type) {
	case string:
		return strings.TrimSpace(c)
	case []any:
		var b strings.Builder
		for _, item := range c {
			switch it := item.(type) {
			case string:
				if b.Len() > 0 {
					b.WriteString("\n")
				}
				b.WriteString(it)
			case map[string]any:
				if txt, _ := it["text"].(string); txt != "" {
					if b.Len() > 0 {
						b.WriteString("\n")
					}
					b.WriteString(txt)
				}
			}
		}
		return strings.TrimSpace(b.String())
	}
	return ""
}
