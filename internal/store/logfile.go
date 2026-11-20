package store

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// 请求日志的 append-only 落盘。
//
// 为什么日志不放在 state.json 里：
//  1. state.json 每次保存都是**全量重写**，几百条日志会让「改个设置」也多写几十 KB，
//     还要和 STS 凭据抢同一个文件的写锁；
//  2. 日志是「高频追加、低频读取」，JSONL 追加才是对的形态；
//  3. 之前 record() 不触发 persist()，重启就丢日志 —— 而崩溃前的那几条恰恰是
//     重启后最需要看的。
//
// 文件与状态文件同目录，命名 <state>.logs.jsonl；超过 8 MiB 滚动，保留 3 份。

const (
	logSuffix      = ".logs.jsonl"
	maxLogFileSize = 8 << 20
	maxLogFiles    = 3
)

// logSink 是日志文件写入器（nil 表示不可用，所有方法都是空操作）。
type logSink struct {
	mu   sync.Mutex
	path string
	file *os.File
	size int64
}

func newLogSink(statePath string) *logSink {
	return &logSink{path: statePath + logSuffix}
}

// archivePath 返回第 n 份归档的路径（n>=1）。
func (l *logSink) archivePath(n int) string {
	return l.path + "." + strconv.Itoa(n)
}

// Load 读取最近 keep 条日志（按时间正序返回）。
//
// 归档文件按序号从旧到新排列，所以先读最老的归档、最后读当前文件。
func (l *logSink) Load(keep int) []LogEntry {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	var files []string
	for n := maxLogFiles - 1; n >= 1; n-- {
		if _, err := os.Stat(l.archivePath(n)); err == nil {
			files = append(files, l.archivePath(n))
		}
	}
	files = append(files, l.path)

	var out []LogEntry
	for _, path := range files {
		f, err := os.Open(path)
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
		for sc.Scan() {
			line := sc.Bytes()
			if len(line) == 0 {
				continue
			}
			var e LogEntry
			if json.Unmarshal(line, &e) != nil {
				// 半行（进程被杀时正在写）直接跳过，不要因此丢掉整个文件。
				continue
			}
			out = append(out, e)
		}
		_ = f.Close()
	}
	if keep > 0 && len(out) > keep {
		out = out[len(out)-keep:]
	}
	return out
}

// Append 追加一条日志并立即刷盘。
func (l *logSink) Append(e LogEntry) {
	if l == nil {
		return
	}
	line, err := json.Marshal(e)
	if err != nil {
		return
	}
	line = append(line, '\n')

	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file == nil {
		if !l.openLocked() {
			return
		}
	}
	if l.size+int64(len(line)) > maxLogFileSize {
		l.rotateLocked()
		if l.file == nil {
			return
		}
	}
	if _, err := l.file.Write(line); err != nil {
		return
	}
	l.size += int64(len(line))
	// 每条都 flush：崩溃/被杀进程时最后一条才可能是现场。
	_ = l.file.Sync()
}

func (l *logSink) openLocked() bool {
	// 目录只给本人：日志里含模型名、账号备注与错误原文。
	if err := os.MkdirAll(filepath.Dir(l.path), 0o700); err != nil {
		return false
	}
	f, err := os.OpenFile(l.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return false
	}
	_ = os.Chmod(l.path, 0o600)
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return false
	}
	l.file = f
	l.size = info.Size()
	return true
}

// rotateLocked 把当前文件滚到 .1，旧的依次后移，最多保留 maxLogFiles 份。
func (l *logSink) rotateLocked() {
	if l.file != nil {
		_ = l.file.Close()
		l.file = nil
	}
	// .N 已存在则先删最老的，再从大到小后移。
	_ = os.Remove(l.archivePath(maxLogFiles))
	for n := maxLogFiles - 1; n >= 1; n-- {
		_ = os.Rename(l.archivePath(n), l.archivePath(n+1))
	}
	_ = os.Rename(l.path, l.archivePath(1))
	l.size = 0
	_ = l.openLocked()
}

// Truncate 清空日志文件（控制台「清空日志」用）。
func (l *logSink) Truncate() {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file != nil {
		_ = l.file.Close()
		l.file = nil
	}
	for n := 1; n <= maxLogFiles; n++ {
		_ = os.Remove(l.archivePath(n))
	}
	_ = os.Remove(l.path)
	l.size = 0
}

// Close 关闭文件句柄。
func (l *logSink) Close() {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file != nil {
		_ = l.file.Close()
		l.file = nil
	}
}

// readJSONLines 从若干文件里读 JSONL（迁移旧 state.json 里的日志用）。
func readJSONLines(files []string) []LogEntry {
	var out []LogEntry
	for _, path := range files {
		f, err := os.Open(path)
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
		for sc.Scan() {
			if len(sc.Bytes()) == 0 {
				continue
			}
			var e LogEntry
			if json.Unmarshal(sc.Bytes(), &e) == nil {
				out = append(out, e)
			}
		}
		_ = f.Close()
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Time < out[j].Time })
	return out
}

// isLogFile 判断文件名是否是日志文件（含归档）。
func isLogFile(name string) bool {
	return strings.Contains(name, logSuffix)
}
