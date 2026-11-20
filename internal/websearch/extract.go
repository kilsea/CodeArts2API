package websearch

import (
	"context"
	"errors"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	defaultUserAgent = "codearts2api-websearch/1.0 (+https://github.com/)"
	// maxPageBytes 是单页最大读取量：超过 1MB 的"文章"通常是异常页面。
	maxPageBytes = 1 << 20
	// pageTimeout 是单页抓取上限（整体超时由调用方的 ctx 控制）。
	pageTimeout = 6 * time.Second
)

// fetcher 抓取第三方网页并抽取正文。
//
// 安全边界（这是网关里唯一会访问"别人给的 URL"的地方）：
//   - 只允许 http / https，端口只允许 80 / 443；
//   - 每一跳连接都校验解析后的 IP，拒绝环回 / 私网 / 链路本地 / CGNAT；
//   - 重定向最多 3 跳，且每一跳重新校验；
//   - 响应体按 Content-Type 与大小裁剪。
//
// 这些规则防的是 SSRF：搜索结果或模型给的 URL 可能指向
// 127.0.0.1、169.254.169.254（云元数据）、docker 内网服务。
type fetcher struct {
	client   *http.Client
	ua       string
	maxChr   int
	maxBytes int64
	// allowAnyPort 只供测试使用：httptest 监听的是随机端口，
	// 正式路径必须限制 80 / 443（防 SSRF 打内网服务端口）。
	allowAnyPort bool
}

func newFetcher(cfg Config) *fetcher {
	tr := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           guardDialContext,
		MaxIdleConns:          8,
		MaxIdleConnsPerHost:   2,
		IdleConnTimeout:       30 * time.Second,
		TLSHandshakeTimeout:   5 * time.Second,
		ResponseHeaderTimeout: 5 * time.Second,
		ForceAttemptHTTP2:     true,
	}
	return &fetcher{
		client: &http.Client{
			Timeout:   pageTimeout,
			Transport: tr,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if len(via) >= 3 {
					return errors.New("重定向次数过多")
				}
				return validateTarget(req.URL)
			},
		},
		ua:       cfg.UserAgent,
		maxChr:   cfg.MaxPageChars,
		maxBytes: maxPageBytes,
	}
}

// guardDialContext 是 SSRF 的最后一道闸：DNS 解析完成后、真正连接之前
// 拿到的是 IP，此刻拒绝内网地址才能防住「域名解析到 127.0.0.1」这类绕过。
func guardDialContext(ctx context.Context, network, address string) (net.Conn, error) {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return nil, fmt.Errorf("无法解析目标地址 %q", host)
	}
	if blockedIP(ip) {
		return nil, fmt.Errorf("拒绝访问内网地址 %s", host)
	}
	return pageDialer.DialContext(ctx, network, address)
}

// pageDialer 复用一个拨号器（保持连接池语义，也避免每次拨号都分配对象）。
var pageDialer = &net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}

// blockedIP 报告一个 IP 是否属于"不该被网关访问"的范围。
func blockedIP(ip net.IP) bool {
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsUnspecified() ||
		ip.IsMulticast() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() {
		return true
	}
	v4 := ip.To4()
	if v4 == nil {
		return false
	}
	switch {
	case v4[0] == 0: // 0.0.0.0/8
		return true
	case v4[0] == 100 && v4[1] >= 64 && v4[1] <= 127: // CGNAT 100.64/10
		return true
	case v4[0] == 192 && v4[1] == 0 && v4[2] == 0: // 192.0.0.0/24
		return true
	case v4[0] == 198 && (v4[1] == 18 || v4[1] == 19): // 基准测试 198.18/15
		return true
	case v4[0] >= 240: // 保留 / 广播
		return true
	}
	return false
}

// validateTarget 校验待抓取的 URL 形状（IP 级检查在拨号时做）。
func validateTarget(u *url.URL) error {
	if err := validateTargetBase(u); err != nil {
		return err
	}
	switch u.Port() {
	case "", "80", "443":
		return nil
	}
	return fmt.Errorf("只允许 80 / 443 端口：%s", u.Port())
}

// validateTargetBase 是端口之外的形状校验。
func validateTargetBase(u *url.URL) error {
	if u == nil {
		return errors.New("URL 为空")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("只允许 http(s)：%s", u.Scheme)
	}
	if u.User != nil {
		return errors.New("URL 不允许带用户名密码")
	}
	host := strings.ToLower(u.Hostname())
	if host == "" {
		return errors.New("URL 缺少主机名")
	}
	if host == "localhost" || host == "metadata.google.internal" || strings.HasSuffix(host, ".local") {
		return fmt.Errorf("拒绝访问 %s", host)
	}
	return nil
}

// page 抓取一个 URL 并返回（标题，正文）。正文失败不影响检索结果本身。
func (f *fetcher) page(ctx context.Context, rawURL string) (string, string, error) {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return "", "", err
	}
	check := validateTarget
	if f.allowAnyPort {
		check = validateTargetBase
	}
	if err := check(u); err != nil {
		return "", "", err
	}
	ctx, cancel := context.WithTimeout(ctx, pageTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", "", err
	}
	req.Header.Set("User-Agent", f.ua)
	req.Header.Set("Accept", "text/html,application/xhtml+xml;q=0.9,text/plain;q=0.8,*/*;q=0.5")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")

	resp, err := f.client.Do(req)
	if err != nil {
		return "", "", err
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		_ = resp.Body.Close()
	}()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	if cl := resp.ContentLength; cl > f.maxBytes {
		return "", "", fmt.Errorf("页面过大（%d 字节）", cl)
	}

	ctype := strings.ToLower(resp.Header.Get("Content-Type"))
	raw, err := io.ReadAll(io.LimitReader(resp.Body, f.maxBytes))
	if err != nil {
		return "", "", err
	}

	switch {
	case strings.Contains(ctype, "html"), strings.Contains(ctype, "xml"), ctype == "":
		title, text := extractHTML(raw)
		return title, truncateRunes(text, f.maxChr), nil
	case strings.HasPrefix(ctype, "text/"):
		return "", truncateRunes(strings.TrimSpace(string(raw)), f.maxChr), nil
	default:
		return "", "", fmt.Errorf("不支持的内容类型 %s", ctype)
	}
}

// blockTags 是"这之后应该换行"的标签。
var blockTags = map[string]bool{
	"p": true, "div": true, "br": true, "li": true, "tr": true, "td": true, "th": true,
	"h1": true, "h2": true, "h3": true, "h4": true, "h5": true, "h6": true,
	"section": true, "article": true, "header": true, "footer": true, "blockquote": true,
	"pre": true, "table": true, "ul": true, "ol": true, "dl": true, "dt": true, "dd": true,
	"figure": true, "figcaption": true, "aside": true, "nav": true, "main": true, "hr": true,
}

// skipTags 里的内容不是正文（脚本、样式、内联图标、模板）。
// 不跳 head：<title> 在 head 里，要单独取出来。
var skipTags = map[string]bool{
	"script": true, "style": true, "noscript": true, "svg": true, "canvas": true,
	"template": true, "iframe": true, "object": true, "embed": true, "form": true,
	"button": true, "select": true, "textarea": true,
}

// extractHTML 用标准库做一次尽量保守的「正文抽取」。
//
// 不追求还原排版：目标是把 HTML 变成「一行一句」的纯文本，供模型阅读。
// 写错的最坏后果是多几行噪音，不会让整页失效 —— 这是不引入第三方
// 解析器的前提条件。
func extractHTML(body []byte) (title, text string) {
	src := string(body)
	var bodyBuf, titleBuf strings.Builder
	inTitle := false
	skipDepth := 0

	for i := 0; i < len(src); {
		lt := strings.IndexByte(src[i:], '<')
		if lt < 0 {
			if skipDepth == 0 {
				if inTitle {
					titleBuf.WriteString(src[i:])
				} else {
					bodyBuf.WriteString(src[i:])
				}
			}
			break
		}
		if skipDepth == 0 {
			if inTitle {
				titleBuf.WriteString(src[i : i+lt])
			} else {
				bodyBuf.WriteString(src[i : i+lt])
			}
		}
		i += lt

		// 注释 / CDATA / doctype 直接跳过。
		if strings.HasPrefix(src[i:], "<!--") {
			end := strings.Index(src[i+4:], "-->")
			if end < 0 {
				break
			}
			i += 4 + end + 3
			continue
		}
		gt := strings.IndexByte(src[i:], '>')
		if gt < 0 {
			break
		}
		tag := strings.TrimSpace(src[i+1 : i+gt])
		i += gt + 1
		if tag == "" || tag[0] == '!' || tag[0] == '?' {
			continue
		}

		closing := tag[0] == '/'
		if closing {
			tag = strings.TrimSpace(tag[1:])
		}
		name := tagName(tag)
		if name == "" {
			continue
		}

		if name == "title" {
			inTitle = !closing
			continue
		}
		if skipTags[name] {
			if closing {
				if skipDepth > 0 {
					skipDepth--
				}
			} else if !strings.HasSuffix(tag, "/") {
				skipDepth++
			}
			continue
		}
		if skipDepth > 0 && !closing {
			continue
		}
		if blockTags[name] && !inTitle {
			bodyBuf.WriteByte('\n')
		}
	}

	title = strings.TrimSpace(strings.Join(strings.Fields(html.UnescapeString(titleBuf.String())), " "))
	return title, normalizeText(bodyBuf.String())
}

// tagName 取标签名（小写）；`br/`、`a href="x"` 都能正确取到首段。
func tagName(tag string) string {
	if i := strings.IndexAny(tag, " \t\r\n/>"); i >= 0 {
		tag = tag[:i]
	}
	return strings.ToLower(tag)
}

// normalizeText 解码实体、压平空白、去掉纯符号行。
func normalizeText(s string) string {
	s = html.UnescapeString(s)
	s = strings.ReplaceAll(s, "\r", "\n")

	var b strings.Builder
	b.Grow(len(s))
	lastBlank := true
	for _, line := range strings.Split(s, "\n") {
		line = strings.Join(strings.Fields(line), " ")
		if line == "" {
			continue
		}
		if len([]rune(line)) < 2 {
			continue
		}
		if !lastBlank {
			b.WriteByte('\n')
		}
		b.WriteString(line)
		lastBlank = false
	}
	return b.String()
}
