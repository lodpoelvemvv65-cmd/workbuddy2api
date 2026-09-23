package main

import (
	"bytes"
	"io"
	"log"
	"os"
	"path/filepath"
	"testing"
)

func TestRotatingFileWriterRotates(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "logs", "a.log") // 父目录不存在，验证 MkdirAll
	w, err := newRotatingFileWriter(path, 10)
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	defer w.Close()

	if _, err := w.Write([]byte("1234567890")); err != nil { // 恰好 10，不轮转
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("abcde")); err != nil { // 10+5>10 → 轮转
		t.Fatal(err)
	}

	cur, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(cur) != "abcde" {
		t.Errorf("当前文件=%q want abcde", cur)
	}
	bak, err := os.ReadFile(path + ".1")
	if err != nil {
		t.Fatalf("备份缺失: %v", err)
	}
	if string(bak) != "1234567890" {
		t.Errorf("备份=%q want 1234567890", bak)
	}
}

func TestRotatingFileWriterAppendsAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.log")
	w1, err := newRotatingFileWriter(path, 0)
	if err != nil {
		t.Fatal(err)
	}
	w1.Write([]byte("first\n"))
	w1.Close()

	// 重启后 new 应记录已有长度并追加，而非截断。
	w2, err := newRotatingFileWriter(path, 0)
	if err != nil {
		t.Fatal(err)
	}
	w2.Write([]byte("second\n"))
	w2.Close()

	got, _ := os.ReadFile(path)
	if string(got) != "first\nsecond\n" {
		t.Errorf("got %q", got)
	}
}

func TestSetupFileLogDisabled(t *testing.T) {
	before := log.Writer()
	w, closeFn, err := setupFileLog("  ", 64)
	if err != nil {
		t.Fatalf("empty path should not error: %v", err)
	}
	defer closeFn()
	if w != nil {
		t.Error("空路径应返回 nil writer")
	}
	if log.Writer() != before {
		t.Error("空路径不应改写 log 输出")
	}
}

func TestSetupFileLogMirrorsToFileAndStderr(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "wb2api.log")

	// setupFileLog 显式取当前 os.Stderr 做镜像，故临时换成管道来断言 stderr 侧也收到。
	oldStderr := os.Stderr
	r, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = pw
	defer func() { os.Stderr = oldStderr; log.SetOutput(oldStderr) }()

	_, closeFn, err := setupFileLog(path, 64)
	if err != nil {
		t.Fatalf("setup: %v", err)
	}
	log.Print("hello-truncation")
	closeFn()
	_ = pw.Close()

	file, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(file, []byte("hello-truncation")) {
		t.Errorf("文件未写入: %q", file)
	}
	stderrOut, _ := io.ReadAll(r)
	if !bytes.Contains(stderrOut, []byte("hello-truncation")) {
		t.Errorf("stderr 未镜像: %q", stderrOut)
	}
}
