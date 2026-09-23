// logfile.go 标准 log 输出的文件镜像 + 轻量轮转。
//
// 为什么需要：容器重建（docker compose up 因镜像/配置变更重建容器）会丢掉
// json-file 日志，断流/降级这类低频告警事后无从回查。把日志额外写进 bind mount
// 的宿主目录即可跨重建留存；同时仍镜像到 stderr，docker logs 行为不变。
//
// 轮转刻意做小：超过阈值把当前文件改名为 path+".1"（覆盖旧备份），只保留一份，
// 避免无界增长撑爆宿主磁盘。并发安全（log 包本身串行写，这里再加锁兜底直接调用）。
package main

import (
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// rotatingFileWriter 追加写文件，超过 max 字节时轮转为 path+".1"（仅一份备份）。
type rotatingFileWriter struct {
	mu   sync.Mutex
	path string
	max  int64
	f    *os.File
	size int64
}

// newRotatingFileWriter 打开（或创建）日志文件并记录当前大小。父目录自动创建。
func newRotatingFileWriter(path string, max int64) (*rotatingFileWriter, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	w := &rotatingFileWriter{path: path, max: max}
	if err := w.open(); err != nil {
		return nil, err
	}
	return w, nil
}

// open 以追加方式打开底层文件并把 size 对齐到已有长度（重启后续写同一文件）。
func (w *rotatingFileWriter) open() error {
	f, err := os.OpenFile(w.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return err
	}
	w.f = f
	w.size = info.Size()
	return nil
}

// rotateLocked 关闭当前文件、把其重命名为 .1 备份，再开新文件。调用方持锁。
func (w *rotatingFileWriter) rotateLocked() {
	_ = w.f.Close()
	_ = os.Remove(w.path + ".1")
	_ = os.Rename(w.path, w.path+".1")
	if err := w.open(); err != nil {
		// 轮转后重开失败也无处可写；把 size 归零让后续 Write 再试，避免永久停写。
		w.f = nil
		w.size = 0
	}
}

// Write 实现 io.Writer：单条写出超过 max 时先轮转，再照常写入（该条可能略超阈值）。
func (w *rotatingFileWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.f == nil {
		if err := w.open(); err != nil {
			return 0, err
		}
	}
	if w.max > 0 && w.size > 0 && w.size+int64(len(p)) > w.max {
		w.rotateLocked()
		if w.f == nil {
			return 0, os.ErrClosed
		}
	}
	n, err := w.f.Write(p)
	w.size += int64(n)
	return n, err
}

// Close 关闭底层文件。
func (w *rotatingFileWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.f == nil {
		return nil
	}
	err := w.f.Close()
	w.f = nil
	return err
}

// setupFileLog 把标准 log 输出镜像到 stderr + path 指向的文件（带轮转），
// 并返回该文件 writer 供调用方另行镜像请求流水（server.SetStatsOutput）。
//   - path 空白：不落盘，返回 (nil, no-op 清理函数, nil)，保持默认纯 stderr 行为。
//   - 打开失败：返回 error，调用方降级为仅 stderr（打 WARN 而非退出）。
//
// 成功时返回的清理函数关闭文件；调用方 defer 调用即可。
func setupFileLog(path string, maxMB int) (io.Writer, func(), error) {
	if strings.TrimSpace(path) == "" {
		return nil, func() {}, nil
	}
	max := int64(maxMB) * 1024 * 1024
	if max <= 0 {
		max = 64 * 1024 * 1024
	}
	w, err := newRotatingFileWriter(path, max)
	if err != nil {
		return nil, nil, err
	}
	log.SetOutput(io.MultiWriter(os.Stderr, w))
	return w, func() { _ = w.Close() }, nil
}
