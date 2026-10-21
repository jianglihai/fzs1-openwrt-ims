package main

// log.go - 带时间戳日志 (写 /tmp/imsengine.log)
// ★ v0.5.1: ①去掉硬编码 .UTC()(曾致日志/时间全差 8 小时) ②只写文件不再双写
//   (主引擎 stdout+stderr 被启动脚本重定向到同一日志 → 每行重复两遍)
//   panic 尸检走 stderr → 主引擎由重定向落盘, N 守护的 logf 仍直接写文件, 两不丢
// ★ v0.7.2 (2026-10-04 主人要求: 日志不许无限存, 设备 tmpfs 只有 30MB):
//   引擎日志超过 512KB 自动留尾一半 (丢弃最旧), 防注册重试风暴写爆 tmpfs

import (
	"fmt"
	"os"
	"sync"
	"time"
)

func init() {
	// 设备无 zoneinfo 数据库, Go 的 time.Local 兜底成 UTC → 显式固定北京时间 +8
	// (2026-09-28 主人要求: 转发/日志/收件箱时间统一 +8)
	time.Local = time.FixedZone("CST", 8*3600)
}

var (
	logFile *os.File
	logMu   sync.Mutex // ★ v0.9.9g: 保护 logFile 的读/写/轮转 (原实现多协程并发写+rotate 竞态)
)

const logCapBytes = 512 << 10 // 512KB 上限, 超了留尾 256KB

// logCapRotate 日志轮转 (调用方须已持有 logMu)
func logCapRotate() {
	st, err := logFile.Stat()
	if err != nil || st.Size() < logCapBytes {
		return
	}
	// 留尾 256KB: 从中点截到末尾 (行首可能断, 可接受)
	st2, _ := logFile.Seek(0, 2)
	tail := st2 - logCapBytes/2
	if tail <= 0 {
		return
	}
	buf := make([]byte, st2-tail)
	logFile.ReadAt(buf, tail)
	logFile.Close()
	logFile, _ = os.OpenFile("/tmp/imsengine.log", os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if logFile != nil {
		logFile.Write(buf)
	}
}

func logf(format string, args ...interface{}) {
	ts := time.Now().Format("2006-01-02 15:04:05")
	line := fmt.Sprintf("[%s] %s\n", ts, fmt.Sprintf(format, args...))
	logMu.Lock()
	defer logMu.Unlock()
	if logFile == nil {
		logFile, _ = os.OpenFile("/tmp/imsengine.log", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	}
	if logFile != nil {
		logFile.WriteString(line)
		logFile.Sync()
		logCapRotate()
	}
}
