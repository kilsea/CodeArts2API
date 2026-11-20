package server

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
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
	// maxDumpDays 是采样文件保留天数（含今天）。超过的文件整份删除。
	//
	// 排障要看的是「最近这几天发了什么」，再早的正文没有保留价值，
	// 而且这是唯一会把对话内容写到磁盘上的开关 —— 必须有硬性上限。
	maxDumpDays = 3
	// maxDumpFilesPerDay 是单天文件数兜底（防 8 MiB 滚动出几百份）。
	maxDumpFilesPerDay = 32
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

// Dumper 是采样器。nil 表示不可用（建不了目录），此时 Record / Find 都退化为空操作。
//
// 是否真的采样由 enabled 决定，可以在运行时开关（控制台的「调试采样」开关），
// 不必重启网关 —— 排障时想立刻开始记录，重启会丢掉现场。
type Dumper struct {
	dir string
	// every 是采样率分母：1 表示全采，n 表示 1/n 概率。
	every atomic.Int64
	// enabled 是运行时开关；默认关闭（与环境变量无关时为 false）。
	enabled atomic.Bool

	// maxSize 是单文件滚动阈值；0 表示用 maxDumpFileSize。
	// 抽成字段是为了让滚动逻辑能被测试（否则要真的写满 8 MiB）。
	maxSize int64
	// now 是时钟（测试注入用）；nil 表示 time.Now。
	now func() time.Time

	mu     sync.Mutex
	file   *os.File
	writer *bufio.Writer
	size   int64
	// curDay / curSeq 定位当前正在写的文件：requests-<YYYYMMDD>[-seq].jsonl。
	// 按天分文件，过期清理就能整份删，不必逐行 compaction。
	curDay string
	curSeq int
	// lastPrune 是上次过期清理的时间：常驻进程不会反复 open，
	// 否则旧文件要等到跨天/重启才会被删。
	lastPrune time.Time
	// ring 是最近若干条采样的内存副本，供重放直接取用（不必回读文件）。
	ring []DumpEntry
	rnd  uint64
}

// NewDumper 按环境变量构造采样器。
//
//	CODEARTS_DUMP_SAMPLE=0（或缺省）  初始关闭（可在控制台打开）
//	CODEARTS_DUMP_SAMPLE=1            初始开启，全部采样
//	CODEARTS_DUMP_SAMPLE=100          初始开启，1% 采样
//
// dir 为空时用 defaultDir/samples。
//
// 环境变量只决定**启动时**的初始值；之后以控制台设置为准（启动后
// ApplyDebug 会用持久化设置覆盖）。返回 nil 仅在目录建不了时。
func NewDumper(defaultDir string) *Dumper {
	dir := strings.TrimSpace(os.Getenv("CODEARTS_DUMP_DIR"))
	if dir == "" {
		dir = filepath.Join(defaultDir, "samples")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		// 采样是排障辅助，绝不能因为它导致服务起不来。
		return nil
	}
	d := &Dumper{dir: dir}
	d.every.Store(1)
	if raw := strings.TrimSpace(os.Getenv("CODEARTS_DUMP_SAMPLE")); raw != "" {
		if every, err := strconv.Atoi(raw); err == nil && every > 0 {
			d.every.Store(int64(every))
			d.enabled.Store(true)
		}
	}
	return d
}

// Enabled 报告采样是否已开启。
func (d *Dumper) Enabled() bool { return d != nil && d.enabled.Load() }

// SetEnabled 运行时开关采样。
func (d *Dumper) SetEnabled(on bool) {
	if d == nil {
		return
	}
	d.enabled.Store(on)
}

// SetSampleRate 运行时调整采样率分母（1 = 全采）。
func (d *Dumper) SetSampleRate(every int) {
	if d == nil {
		return
	}
	if every <= 0 {
		every = 1
	}
	d.every.Store(int64(every))
}

// SampleRate 返回当前采样率分母。
func (d *Dumper) SampleRate() int {
	if d == nil {
		return 0
	}
	return int(d.every.Load())
}

// Dir 返回采样目录（控制台展示用）。
func (d *Dumper) Dir() string {
	if d == nil {
		return ""
	}
	return d.dir
}

// SampleMeta 是采样列表里的一行（不含正文）。
type SampleMeta struct {
	RequestID string `json:"request_id"`
	Time      int64  `json:"time"`
	Path      string `json:"path"`
	Model     string `json:"model,omitempty"`
	Stream    bool   `json:"stream"`
}

// Recent 返回最近 limit 条采样，**最新的在前**。
//
// 只看内存 ring：采样文件可能到几十 MB，翻文件会把控制台拖死；
// 更早的样本用 request id 直接查（Find 会回读文件）。
func (d *Dumper) Recent(limit int) []SampleMeta {
	if d == nil {
		return nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if limit <= 0 || limit > len(d.ring) {
		limit = len(d.ring)
	}
	out := make([]SampleMeta, 0, limit)
	for i := len(d.ring) - 1; i >= 0 && len(out) < limit; i-- {
		e := d.ring[i]
		out = append(out, SampleMeta{
			RequestID: e.RequestID,
			Time:      e.Time,
			Path:      e.Path,
			Model:     e.Model,
			Stream:    e.Stream,
		})
	}
	return out
}

// sampled 按 1/every 的概率决定是否采样本次请求。
func (d *Dumper) sampled() bool {
	if d == nil || !d.enabled.Load() {
		return false
	}
	every := int(d.every.Load())
	if every <= 0 {
		return false
	}
	if every == 1 {
		return true
	}
	d.mu.Lock()
	// xorshift：不引入 math/rand 的全局锁，也不必播种。
	d.rnd ^= d.rnd << 13
	d.rnd ^= d.rnd >> 7
	d.rnd ^= d.rnd << 17
	v := d.rnd
	d.mu.Unlock()
	return v%uint64(every) == 0
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

// writeLocked 把一条记录追加到当前采样文件，必要时跨天 / 滚动。
func (d *Dumper) writeLocked(entry DumpEntry) {
	line, err := json.Marshal(entry)
	if err != nil {
		return
	}
	line = append(line, '\n')

	now := d.clock()
	// 常驻进程不会反复 open，所以跨天必须在这里切文件：
	// 否则 requests-<昨天>.jsonl 会一直被追加，过期清理也永远跑不到。
	if d.writer == nil || d.curDay != now.Format("20060102") {
		d.closeLocked()
		if !d.openLocked() {
			return
		}
	} else if now.Sub(d.lastPrune) > time.Hour {
		// 同一天内也要定期清理：昨天的文件过零后就该在 72h 窗口外了。
		d.pruneLocked(now)
		d.lastPrune = now
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

// clock 返回当前时间（可被测试注入）。
func (d *Dumper) clock() time.Time {
	if d.now != nil {
		return d.now()
	}
	return time.Now()
}

// dayFile 返回某天第 seq 份采样文件的路径（seq<=1 时不带序号）。
func (d *Dumper) dayFile(day time.Time, seq int) string {
	name := "requests-" + day.Format("20060102")
	if seq > 1 {
		name += "-" + strconv.Itoa(seq)
	}
	return filepath.Join(d.dir, name+".jsonl")
}

// latestSeqLocked 返回某天已存在的最大文件序号（无文件时为 0）。
func (d *Dumper) latestSeqLocked(day time.Time) int {
	matches, err := filepath.Glob(filepath.Join(d.dir, "requests-"+day.Format("20060102")+"*.jsonl"))
	if err != nil {
		return 0
	}
	max := 0
	for _, p := range matches {
		seq := seqOfDumpFile(filepath.Base(p))
		if seq > max {
			max = seq
		}
	}
	return max
}

// seqOfDumpFile 从文件名里解析序号（requests-20261120-3.jsonl → 3）。
// 无序号（当天第一份）返回 1；解析不了返回 0。
func seqOfDumpFile(base string) int {
	name := strings.TrimSuffix(strings.TrimPrefix(base, "requests-"), ".jsonl")
	if len(name) < 8 {
		return 0
	}
	rest := name[8:]
	if rest == "" {
		return 1
	}
	if !strings.HasPrefix(rest, "-") {
		return 0
	}
	seq, err := strconv.Atoi(rest[1:])
	if err != nil || seq <= 0 {
		return 0
	}
	return seq
}

// openLocked 打开（或创建）当天的采样文件。
//
// 重启后要**追加**到当天已有的文件，而不是另起一份 —— 否则一天内重启几次
// 就会散出一堆小文件，控制台列表也会重复。
func (d *Dumper) openLocked() bool {
	day := d.clock()
	today := day.Format("20060102")
	if d.curDay != today {
		d.curDay, d.curSeq = today, 0
	}
	if d.curSeq <= 0 {
		seq := d.latestSeqLocked(day)
		if seq <= 0 {
			seq = 1
		}
		d.curSeq = seq
	}
	// 先清理过期文件：启动后第一次写、跨天、滚动时都会走到这里。
	d.pruneLocked(day)
	d.lastPrune = day

	path := d.dayFile(day, d.curSeq)
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

// rotateLocked 当前文件写满时切到当天下一份。
func (d *Dumper) rotateLocked() {
	d.closeLocked()
	d.curSeq++
	_ = d.openLocked()
}

// closeLocked 关闭当前文件（不清 curDay/curSeq）。
func (d *Dumper) closeLocked() {
	if d.writer != nil {
		_ = d.writer.Flush()
	}
	if d.file != nil {
		_ = d.file.Close()
	}
	d.writer, d.file, d.size = nil, nil, 0
}

// pruneLocked 删除超过 maxDumpDays 的采样文件，并给单天文件数封顶。
func (d *Dumper) pruneLocked(now time.Time) {
	// 保留「今天 + 前 2 天」= 3 天。文件名里的日期是字典序可比的，
	// 直接字符串比较即可，不用解析时间。
	cutoff := now.AddDate(0, 0, -(maxDumpDays - 1)).Format("20060102")

	matches, err := filepath.Glob(filepath.Join(d.dir, "requests-*.jsonl"))
	if err != nil {
		return
	}
	type dumpFile struct {
		path string
		day  string
		seq  int
	}
	files := make([]dumpFile, 0, len(matches))
	for _, p := range matches {
		base := filepath.Base(p)
		name := strings.TrimSuffix(strings.TrimPrefix(base, "requests-"), ".jsonl")
		if len(name) < 8 {
			continue
		}
		day := name[:8]
		// 只认纯数字日期，防止把别的手工文件删掉。
		if _, err := strconv.Atoi(day); err != nil {
			continue
		}
		files = append(files, dumpFile{path: p, day: day, seq: seqOfDumpFile(base)})
	}

	kept := make(map[string][]dumpFile)
	for _, f := range files {
		if f.day < cutoff {
			_ = os.Remove(f.path)
			continue
		}
		kept[f.day] = append(kept[f.day], f)
	}
	// 单天文件数封顶：文件名带序号，字典序即写入顺序，删最老的几份。
	//
	// 留一格给「马上要创建的那份」：prune 在 open 之前跑，若按上限硬砍，
	// 当天永远会多出第 maxDumpFilesPerDay+1 份。
	keep := maxDumpFilesPerDay - 1
	for day, group := range kept {
		if len(group) <= keep {
			continue
		}
		sort.Slice(group, func(i, j int) bool { return group[i].seq < group[j].seq })
		for _, f := range group[:len(group)-keep] {
			_ = os.Remove(f.path)
		}
		_ = day
	}

	// 旧版本遗留的 requests.jsonl：没有日期，按 mtime 判。
	legacy := filepath.Join(d.dir, "requests.jsonl")
	if info, err := os.Stat(legacy); err == nil {
		if info.ModTime().Before(now.AddDate(0, 0, -maxDumpDays)) {
			_ = os.Remove(legacy)
		}
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

	// 回读：当前文件 → 当天的其它份 → 旧版遗留文件。
	// 文件名里的日期 + 序号是字典序可比的时间序，倒着扫就是新的优先。
	var files []string
	if d.curDay != "" && d.curSeq > 0 {
		files = append(files, d.dayFile(d.clock(), d.curSeq))
	}
	archives, _ := filepath.Glob(filepath.Join(d.dir, "requests-*.jsonl"))
	sort.Sort(sort.Reverse(sort.StringSlice(archives)))
	files = append(files, archives...)
	files = append(files, filepath.Join(d.dir, "requests.jsonl"))
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
	d.closeLocked()
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
