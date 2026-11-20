package server

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// 请求采样与失败重放。
//
// 默认行为不变：**不记录任何对话正文**。只有在显式设置
// CODEARTS_DUMP_SAMPLE=<n> 时，才按 1/n 的概率把脱敏后的请求体落盘。
//
// 为什么需要它：日志里只有 prompt_len / output_len / model / 耗时 / error_class
// （这是对的，正文不该进日志）。但遇到「上游 400」时无法复现请求体，只能猜 ——
// 尤其当客户端说「我就发了个普通请求」而我们回 400 时，采样是唯一的证据。
//
// 落盘位置：CODEARTS_DUMP_DIR，缺省为状态文件同目录下的 samples/。
// 文件权限 0600，滚动保留最近几份，总量有硬上限。

const (
	// maxDumpFileSize 是单个采样文件的滚动阈值（8 MiB）。
	maxDumpFileSize = 8 << 20
	// maxDumpFiles 是保留的历史采样文件份数。
	maxDumpFiles = 5
	// maxDumpBody 是单条采样里请求体的字节上限（超出截断并标记）。
	maxDumpBody = 64 << 10
	// maxDumpMemory 是内存索引保留的条数（供重放直接取用）。
	maxDumpMemory = 200
	// longStringLimit 是「长字符串」的判定阈值：超过它就当图片 / 附件处理。
	longStringLimit = 2048
)

// DumpEntry 是一条采样记录。
type DumpEntry struct {
	RequestID string `json:"request_id"`
	Time      int64  `json:"time"`
	Path      string `json:"path"`
	Model     string `json:"model,omitempty"`
	Stream    bool   `json:"stream"`
	// Body 是脱敏后的**原始**请求体（网关改写之前）。
	//
	// 必须是原始体：重放要复现客户端的真实行为，包括它发的非法 role /
	// 越界参数 —— 如果存的是网关改写后的体，重放就永远成功，失去意义。
	Body json.RawMessage `json:"body"`
}

// Dumper 是采样器。nil 表示未启用（默认）。
type Dumper struct {
	dir string
	// every 是采样率分母：1 表示全采，n 表示 1/n 概率。
	every int

	// maxSize 是单文件滚动阈值；0 表示用 maxDumpFileSize。
	// 抽成字段是为了让滚动逻辑能被测试（否则要真的写满 8 MiB）。
	maxSize int64

	mu      sync.Mutex
	file    *os.File
	writer  *bufio.Writer
	size    int64
	rotated int
	// ring 是最近若干条采样的内存副本，供重放直接取用（不必回读文件）。
	ring []DumpEntry
	rnd  uint64
}

// NewDumper 按环境变量构造采样器。
//
//	CODEARTS_DUMP_SAMPLE=0（或缺省）  关闭
//	CODEARTS_DUMP_SAMPLE=1            全部采样
//	CODEARTS_DUMP_SAMPLE=100          1% 采样
//
// dir 为空时用 defaultDir/samples。
func NewDumper(defaultDir string) *Dumper {
	raw := strings.TrimSpace(os.Getenv("CODEARTS_DUMP_SAMPLE"))
	if raw == "" {
		return nil
	}
	every, err := strconv.Atoi(raw)
	if err != nil || every <= 0 {
		return nil
	}
	dir := strings.TrimSpace(os.Getenv("CODEARTS_DUMP_DIR"))
	if dir == "" {
		dir = filepath.Join(defaultDir, "samples")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		// 采样是排障辅助，绝不能因为它导致服务起不来。
		return nil
	}
	return &Dumper{dir: dir, every: every}
}

// Enabled 报告采样是否启用。
func (d *Dumper) Enabled() bool { return d != nil }

// sampled 按 1/every 的概率决定是否采样本次请求。
func (d *Dumper) sampled() bool {
	if d == nil || d.every <= 0 {
		return false
	}
	if d.every == 1 {
		return true
	}
	d.mu.Lock()
	// xorshift：不引入 math/rand 的全局锁，也不必播种。
	d.rnd ^= d.rnd << 13
	d.rnd ^= d.rnd >> 7
	d.rnd ^= d.rnd << 17
	v := d.rnd
	d.mu.Unlock()
	return v%uint64(d.every) == 0
}

// Record 记录一条采样。body 会被脱敏与截断后落盘。
//
// 任何 I/O 失败都只是静默放弃这一条：采样不能影响请求本身。
func (d *Dumper) Record(requestID, path, model string, stream bool, body []byte) {
	if d == nil || !d.sampled() {
		return
	}
	entry := DumpEntry{
		RequestID: requestID,
		Time:      time.Now().Unix(),
		Path:      path,
		Model:     model,
		Stream:    stream,
		Body:      RedactBody(body),
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.ring = append(d.ring, entry)
	if len(d.ring) > maxDumpMemory {
		d.ring = d.ring[len(d.ring)-maxDumpMemory:]
	}
	d.writeLocked(entry)
}

// writeLocked 把一条记录追加到当前采样文件，必要时滚动。
func (d *Dumper) writeLocked(entry DumpEntry) {
	line, err := json.Marshal(entry)
	if err != nil {
		return
	}
	line = append(line, '\n')
	if d.writer == nil {
		if !d.openLocked() {
			return
		}
	}
	limit := d.maxSize
	if limit <= 0 {
		limit = maxDumpFileSize
	}
	if d.size+int64(len(line)) > limit {
		d.rotateLocked()
		if d.writer == nil {
			return
		}
	}
	if _, err := d.writer.Write(line); err != nil {
		return
	}
	d.size += int64(len(line))
	// 每条都 flush：崩溃 / 被杀进程时最后一条才可能是现场。
	_ = d.writer.Flush()
}

// openLocked 打开（或创建）当前采样文件。
func (d *Dumper) openLocked() bool {
	path := filepath.Join(d.dir, "requests.jsonl")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return false
	}
	// 显式 chmod：文件可能由旧版本以更宽的权限创建过。
	_ = os.Chmod(path, 0o600)
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return false
	}
	d.file = f
	d.writer = bufio.NewWriterSize(f, 32<<10)
	d.size = info.Size()
	return true
}

// rotateLocked 把当前文件改名归档，并清理过旧的归档。
func (d *Dumper) rotateLocked() {
	if d.writer != nil {
		_ = d.writer.Flush()
	}
	if d.file != nil {
		_ = d.file.Close()
	}
	d.writer, d.file, d.size = nil, nil, 0

	cur := filepath.Join(d.dir, "requests.jsonl")
	// 文件名带时间戳 + 递增序号：同一秒内多次滚动不能互相覆盖
	// （时间戳只到秒，连续滚动会撞名）。
	d.rotated++
	stamp := time.Now().Format("20060102-150405")
	name := "requests-" + stamp + "-" + strconv.Itoa(d.rotated) + ".jsonl"
	_ = os.Rename(cur, filepath.Join(d.dir, name))
	d.pruneLocked()
	_ = d.openLocked()
}

// pruneLocked 只保留最近的 maxDumpFiles 份归档。
func (d *Dumper) pruneLocked() {
	matches, err := filepath.Glob(filepath.Join(d.dir, "requests-*.jsonl"))
	if err != nil || len(matches) <= maxDumpFiles {
		return
	}
	// 文件名里带时间戳，字典序即时间序。
	for _, old := range matches[:len(matches)-maxDumpFiles] {
		_ = os.Remove(old)
	}
}

// Find 按 request_id 找回一条采样（先查内存，再回读文件）。
//
// 回读文件是为了跨重启重放：客户端第二天贴来一个 id，仍然能复现。
func (d *Dumper) Find(requestID string) (DumpEntry, bool) {
	if d == nil || requestID == "" {
		return DumpEntry{}, false
	}
	d.mu.Lock()
	for i := len(d.ring) - 1; i >= 0; i-- {
		if d.ring[i].RequestID == requestID {
			e := d.ring[i]
			d.mu.Unlock()
			return e, true
		}
	}
	d.mu.Unlock()

	// 回读：先当前文件，再从新到旧扫归档。
	files := []string{filepath.Join(d.dir, "requests.jsonl")}
	archives, _ := filepath.Glob(filepath.Join(d.dir, "requests-*.jsonl"))
	for i := len(archives) - 1; i >= 0; i-- {
		files = append(files, archives[i])
	}
	for _, path := range files {
		if e, ok := scanDumpFile(path, requestID); ok {
			return e, true
		}
	}
	return DumpEntry{}, false
}

// scanDumpFile 在单个采样文件里找一条记录。
func scanDumpFile(path, requestID string) (DumpEntry, bool) {
	f, err := os.Open(path)
	if err != nil {
		return DumpEntry{}, false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
	for sc.Scan() {
		line := sc.Bytes()
		// 快速跳过：绝大多数行不含目标 id，没必要解析。
		if !strings.Contains(string(line), requestID) {
			continue
		}
		var e DumpEntry
		if json.Unmarshal(line, &e) != nil {
			continue
		}
		if e.RequestID == requestID {
			return e, true
		}
	}
	return DumpEntry{}, false
}

// Close 关闭当前采样文件。
func (d *Dumper) Close() {
	if d == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.writer != nil {
		_ = d.writer.Flush()
	}
	if d.file != nil {
		_ = d.file.Close()
	}
	d.writer, d.file = nil, nil
}

// RedactBody 脱敏并截断请求体。
//
// 脱敏规则：
//   - 超长字符串（图片 base64、附件正文）替换成 `"<redacted N bytes>"`，
//     只保留长度信息 —— 排障要知道「这里有一张多大的图」，不需要图本身；
//   - 整体超过 maxDumpBody 时截断，并在末尾标记 truncated。
//
// 解析失败时退化为「原样截断」：采样文件里出现坏 JSON 也比丢样本好，
// 至少能看出客户端发了什么。
func RedactBody(body []byte) json.RawMessage {
	if len(body) == 0 {
		return json.RawMessage("null")
	}
	var v any
	if err := json.Unmarshal(body, &v); err != nil {
		return truncateRaw(body)
	}
	red := redactValue(v)
	out, err := json.Marshal(red)
	if err != nil {
		return truncateRaw(body)
	}
	if len(out) > maxDumpBody {
		return truncateRaw(out)
	}
	return out
}

// redactValue 递归脱敏。
func redactValue(v any) any {
	switch x := v.(type) {
	case string:
		if len(x) > longStringLimit {
			return "[redacted " + strconv.Itoa(len(x)) + " bytes]"
		}
		return x
	case []any:
		for i := range x {
			x[i] = redactValue(x[i])
		}
		return x
	case map[string]any:
		for k := range x {
			x[k] = redactValue(x[k])
		}
		return x
	default:
		return v
	}
}

// truncateRaw 把超长请求体截断成一段可读的字符串，并标记已截断。
func truncateRaw(body []byte) json.RawMessage {
	if len(body) <= maxDumpBody {
		return json.RawMessage(body)
	}
	out, err := json.Marshal(string(body[:maxDumpBody]) + "…[truncated]")
	if err != nil {
		return json.RawMessage("null")
	}
	return out
}
