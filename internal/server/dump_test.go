package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// 请求采样与失败重放的回归。
//
// 默认行为必须是「不落任何正文」：采样只在显式设置 CODEARTS_DUMP_SAMPLE
// 时开启，且文件权限 0600、体积有上限、滚动有份数上限。

// newDumperAt 构造一个采样率为 every 的采样器（绕过环境变量）。
func newDumperAt(t *testing.T, dir string, every int) *Dumper {
	t.Helper()
	d := &Dumper{dir: dir, rnd: 0x9e3779b97f4a7c15}
	d.every.Store(int64(every))
	d.enabled.Store(true)
	t.Cleanup(d.Close)
	return d
}

// TestDumperDisabledByDefault 未设置环境变量时不采样。
func TestDumperDisabledByDefault(t *testing.T) {
	t.Setenv("CODEARTS_DUMP_SAMPLE", "")
	if d := NewDumper(t.TempDir()); d.Enabled() {
		t.Error("默认不应开启采样")
	}
	// nil 采样器调用 Record 必须安全（请求路径默认就是这条路）。
	var d *Dumper
	d.Record("rid", "/v1/chat/completions", "glm-5.3", false, []byte(`{}`))
	if d.Enabled() {
		t.Error("nil 采样器不应被视为启用")
	}
}

// TestDumperEnabledByEnv 设置后开启，且落在 CODEARTS_DUMP_DIR。
func TestDumperEnabledByEnv(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CODEARTS_DUMP_SAMPLE", "1")
	t.Setenv("CODEARTS_DUMP_DIR", dir)
	d := NewDumper(t.TempDir())
	if !d.Enabled() {
		t.Fatal("设置 CODEARTS_DUMP_SAMPLE=1 后应开启采样")
	}
	d.Record("rid-1", "/v1/chat/completions", "glm-5.3", false,
		[]byte(`{"model":"glm-5.3","messages":[{"role":"user","content":"hi"}]}`))
	d.Close()

	raw, err := os.ReadFile(filepath.Join(dir, "requests-"+time.Now().Format("20060102")+".jsonl"))
	if err != nil {
		t.Fatalf("采样文件没有写出来: %v", err)
	}
	if !strings.Contains(string(raw), "rid-1") {
		t.Errorf("采样文件里没有 request id：%s", raw)
	}
	info, err := os.Stat(filepath.Join(dir, "requests-"+time.Now().Format("20060102")+".jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	// 0600：只允许当前用户读写。Windows 上 Go 的 perm 位不反映 ACL
	// （一律显示 666），只能在 Linux/macOS 上断言。
	if runtime.GOOS != "windows" {
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Errorf("采样文件权限 = %o，期望 600", perm)
		}
	}
}

// TestDumperSampleRate 1/n 采样率：全采时每条都写，1/100 时明显少于全部。
func TestDumperSampleRate(t *testing.T) {
	dir := t.TempDir()
	d := newDumperAt(t, dir, 1)
	for i := 0; i < 5; i++ {
		d.Record("rid", "/v1/chat/completions", "m", false, []byte(`{}`))
	}
	d.Close()
	if got := len(d.ring); got != 5 {
		t.Errorf("全采时记录数 = %d，期望 5", got)
	}

	dir2 := t.TempDir()
	d2 := newDumperAt(t, dir2, 1000)
	for i := 0; i < 500; i++ {
		d2.Record("rid", "/v1/chat/completions", "m", false, []byte(`{}`))
	}
	d2.Close()
	if len(d2.ring) >= 500 {
		t.Errorf("1/1000 采样率下采了 %d 条，几乎等于全采", len(d2.ring))
	}
}

// TestRedactBodyStripsLargeStrings 图片 base64 之类的超长字符串必须被替换，
// 只保留长度信息。
func TestRedactBodyStripsLargeStrings(t *testing.T) {
	huge := strings.Repeat("A", longStringLimit*2)
	body := []byte(`{"messages":[{"role":"user","content":[
		{"type":"text","text":"看图"},
		{"type":"image_url","image_url":{"url":"data:image/png;base64,` + huge + `"}}]}]}`)

	out := RedactBody(body)
	s := string(out)
	if strings.Contains(s, huge) {
		t.Error("超长字符串没有被脱敏")
	}
	if !strings.Contains(s, "[redacted") {
		t.Errorf("脱敏标记缺失: %s", s[:min(len(s), 200)])
	}
	if !strings.Contains(s, "看图") {
		t.Error("普通文本不该被脱敏")
	}
	if !strings.Contains(s, "image_url") {
		t.Error("结构必须保留（排障要知道这是什么字段）")
	}
}

// TestRedactBodyTruncates 整体超限时截断并标记。
func TestRedactBodyTruncates(t *testing.T) {
	body := []byte(strings.Repeat("x", maxDumpBody*2))
	out := RedactBody(body)
	if len(out) > maxDumpBody+64 {
		t.Errorf("截断后仍有 %d 字节", len(out))
	}
	if !strings.Contains(string(out), "truncated") {
		t.Errorf("缺少截断标记: %s", string(out[:min(len(out), 100)]))
	}
}

// TestDumperPrunesWhileRunning 常驻进程（文件一直开着）也必须定期清理旧文件。
func TestDumperPrunesWhileRunning(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	d := newDumperAt(t, dir, 1)
	d.now = func() time.Time { return now }
	d.Record("rid-1", "/v1/chat/completions", "m", false, []byte(`{}`))

	old := filepath.Join(dir, "requests-"+now.AddDate(0, 0, -4).Format("20060102")+".jsonl")
	if err := os.WriteFile(old, []byte(`{"request_id":"old"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// 同一天内再写一次（超过 1 小时的定期清理间隔）。
	now = now.Add(2 * time.Hour)
	d.Record("rid-2", "/v1/chat/completions", "m", false, []byte(`{}`))
	d.Close()

	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Error("运行中的进程也应把过期采样删掉")
	}
}

// TestDumperRollsOverDay 跨天后切到新文件，而不是继续追加昨天的文件。
func TestDumperRollsOverDay(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	d := newDumperAt(t, dir, 1)
	d.now = func() time.Time { return now }
	d.Record("rid-1", "/v1/chat/completions", "m", false, []byte(`{}`))
	yesterday := d.curDay

	now = now.Add(24 * time.Hour)
	d.Record("rid-2", "/v1/chat/completions", "m", false, []byte(`{}`))
	d.Close()

	if d.curDay == yesterday {
		t.Fatalf("跨天后仍写在 %s", yesterday)
	}
	if _, err := os.Stat(filepath.Join(dir, "requests-"+d.curDay+".jsonl")); err != nil {
		t.Errorf("新一天的文件没有建出来: %v", err)
	}
}

// TestDumperFindAcrossRestart 重放要能跨重启：文件回读必须能命中。
func TestDumperFindAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	d := newDumperAt(t, dir, 1)
	d.Record("rid-x", "/v1/chat/completions", "glm-5.3", false,
		[]byte(`{"model":"glm-5.3","messages":[{"role":"user","content":"hi"}]}`))
	d.Close()

	// 新实例（内存 ring 为空），只能靠回读文件。
	fresh := newDumperAt(t, dir, 1)
	e, ok := fresh.Find("rid-x")
	if !ok {
		t.Fatal("回读采样文件失败")
	}
	if e.Model != "glm-5.3" || e.Path != "/v1/chat/completions" {
		t.Errorf("回读内容不对: %+v", e)
	}
	if _, ok := fresh.Find("nope"); ok {
		t.Error("不存在的 id 不该命中")
	}
}

// TestDumperRotatesWithinDay 超过阈值要滚动，且单天文件数有封顶。
func TestDumperRotatesWithinDay(t *testing.T) {
	dir := t.TempDir()
	d := newDumperAt(t, dir, 1)
	// 把滚动阈值压到 2 KiB，否则要真的写满 8 MiB 才能验证滚动。
	d.maxSize = 2048
	body := []byte(`{"model":"glm-5.3","messages":[{"role":"user","content":"hi"}]}`)
	for i := 0; i < 400; i++ {
		d.Record("rid", "/v1/chat/completions", "m", false, body)
	}
	d.Close()

	files, _ := filepath.Glob(filepath.Join(dir, "requests-*.jsonl"))
	if len(files) < 2 {
		t.Fatalf("没有产生第二份文件（滚动没生效），只有 %d 份", len(files))
	}
	if len(files) > maxDumpFilesPerDay {
		t.Errorf("单天文件 %d 份，超过上限 %d", len(files), maxDumpFilesPerDay)
	}
}

// TestDumperPrunesOlderThanKeepDays 超过保留天数的采样整份删除，3 天内保留。
func TestDumperPrunesOlderThanKeepDays(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	write := func(day time.Time, rid string) string {
		p := filepath.Join(dir, "requests-"+day.Format("20060102")+".jsonl")
		line := `{"request_id":"` + rid + `","time":1,"path":"/v1/chat/completions","stream":false,"body":{}}` + "\n"
		if err := os.WriteFile(p, []byte(line), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	tooOld := write(now.AddDate(0, 0, -maxDumpDays-1), "too-old")
	edge := write(now.AddDate(0, 0, -(maxDumpDays-1)), "edge")

	d := newDumperAt(t, dir, 1)
	d.now = func() time.Time { return now }
	d.Record("rid-new", "/v1/chat/completions", "m", false, []byte(`{}`))
	d.Close()

	if _, err := os.Stat(tooOld); !os.IsNotExist(err) {
		t.Errorf("%d 天前的采样文件应被删除", maxDumpDays+1)
	}
	if _, err := os.Stat(edge); err != nil {
		t.Errorf("%d 天前的采样文件应保留: %v", maxDumpDays-1, err)
	}
}

// --- 重放接口 ---

// TestReplayRequiresSampling 未开启采样时重放接口给出明确提示，而不是 404。
func TestReplayRequiresSampling(t *testing.T) {
	s, _ := setupCompatServer(t)
	rec := httptest.NewRecorder()
	s.handleAdminReplay(rec, httptest.NewRequest(http.MethodPost, replayPrefix+"rid", nil))
	if rec.Code != http.StatusConflict {
		t.Fatalf("状态码 = %d，期望 409", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "调试采样") {
		t.Errorf("错误提示应告诉用户去哪开启采样：%s", rec.Body.String())
	}
}

// TestReplayReproducesOriginalFailure 重放必须复现原始失败，而不是「重放就成功」。
//
// 关键点：采样存的是**改写前**的原始请求体，所以原请求因为 role 非法被拒，
// 重放也应当被同样拒绝。
func TestReplayReproducesOriginalFailure(t *testing.T) {
	s, cap := setupCompatServer(t)
	s.dumps = newDumperAt(t, t.TempDir(), 1)

	// 原始请求：未知 role → 400。
	original := `{"model":"glm-5.3","messages":[{"role":"critic","content":"x"}]}`
	rec := postChat(t, s, original)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("原始请求状态码 = %d，期望 400", rec.Code)
	}
	if cap.count() != 0 {
		t.Fatal("原始请求不该打到上游")
	}
	rid := s.dumps.ring[0].RequestID
	if rid == "" {
		t.Fatal("采样里没有 request id")
	}

	replay := httptest.NewRecorder()
	s.handleAdminReplay(replay, httptest.NewRequest(http.MethodPost, replayPrefix+rid, nil))
	if replay.Code != http.StatusOK {
		t.Fatalf("重放接口本身应 200，得到 %d：%s", replay.Code, replay.Body.String())
	}
	var out struct {
		Status int             `json:"status"`
		Body   json.RawMessage `json:"body"`
	}
	if err := json.Unmarshal(replay.Body.Bytes(), &out); err != nil {
		t.Fatalf("重放响应不是合法 JSON: %v", err)
	}
	if out.Status != http.StatusBadRequest {
		t.Errorf("重放结果状态码 = %d，期望 400（必须复现原始失败）", out.Status)
	}
	if !strings.Contains(string(out.Body), "invalid_role") {
		t.Errorf("重放结果应带上原始错误码: %s", out.Body)
	}
}

// TestReplayUnknownRequestID 不存在的 id 返回 404。
func TestReplayUnknownRequestID(t *testing.T) {
	s, _ := setupCompatServer(t)
	s.dumps = newDumperAt(t, t.TempDir(), 1)

	rec := httptest.NewRecorder()
	s.handleAdminReplay(rec, httptest.NewRequest(http.MethodPost, replayPrefix+"nope", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("状态码 = %d，期望 404", rec.Code)
	}
}

// TestReplayRejectsPathTraversal request id 里带路径分隔符必须被拒。
func TestReplayRejectsPathTraversal(t *testing.T) {
	s, _ := setupCompatServer(t)
	s.dumps = newDumperAt(t, t.TempDir(), 1)

	rec := httptest.NewRecorder()
	s.handleAdminReplay(rec, httptest.NewRequest(http.MethodPost, replayPrefix+"../etc/passwd", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("状态码 = %d，期望 400", rec.Code)
	}
}

// TestSamplingDoesNotAffectResponse 采样开启后请求行为必须完全不变。
func TestSamplingDoesNotAffectResponse(t *testing.T) {
	s, cap := setupCompatServer(t)
	s.dumps = newDumperAt(t, t.TempDir(), 1)

	rec := postChat(t, s, `{"model":"glm-5.3","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200", rec.Code)
	}
	if cap.count() != 1 {
		t.Errorf("上游被打了 %d 次，期望 1 次", cap.count())
	}
	if len(s.dumps.ring) != 1 {
		t.Errorf("采样条数 = %d，期望 1", len(s.dumps.ring))
	}
}
