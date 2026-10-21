package main

// at.go - 串口 AT 客户端 (go.bug.st/serial)

import (
	"bufio"
	"fmt"
	"strings"
	"sync"
	"time"

	"go.bug.st/serial"
)

type AT struct {
	port serial.Port
	sc   *bufio.Scanner
	mu   sync.Mutex
}

func NewAT(dev string) (*AT, error) {
	mode := &serial.Mode{
		BaudRate: 115200,
		DataBits: 8,
		Parity:   serial.NoParity,
		StopBits: serial.OneStopBit,
	}
	p, err := serial.Open(dev, mode)
	if err != nil {
		return nil, err
	}
	if err := p.SetReadTimeout(200 * time.Millisecond); err != nil {
		return nil, err
	}
	a := &AT{port: p}

	// ① 先直接试探：状态正常时立即返回（避免每次都付恢复序列的代价）
	if a.ping() {
		return a, nil
	}

	// ② 残留在 PPP 数据态 → 循环恢复（+++ 需前后各 1s 静默，单次常失败）
	//    上一轮引擎异常退出（os.Exit）不会挂断 PPP，模组会把 AT 当字节流吞掉。
	for i := 0; i < 3; i++ {
		time.Sleep(1 * time.Second)
		p.Write([]byte("+++"))
		time.Sleep(1200 * time.Millisecond)
		p.Write([]byte("ATH\r"))
		time.Sleep(800 * time.Millisecond)
		p.ResetInputBuffer()
		if a.ping() {
			logf("AT 口 %s 恢复成功 (第 %d 轮)", dev, i+1)
			return a, nil
		}
	}
	logf("AT 口 %s 恢复序列未确认（继续尝试）", dev)
	p.ResetInputBuffer()
	return a, nil
}

// ping 发一条 AT 并确认收到 OK（用于判断模组是否在命令态）
func (a *AT) ping() bool {
	a.port.ResetInputBuffer()
	if _, err := a.port.Write([]byte("AT\r")); err != nil {
		return false
	}
	buf := make([]byte, 256)
	deadline := time.Now().Add(1200 * time.Millisecond)
	acc := ""
	for time.Now().Before(deadline) {
		n, _ := a.port.Read(buf)
		if n > 0 {
			acc += string(buf[:n])
			if strings.Contains(acc, "OK") {
				return true
			}
		}
	}
	return false
}

// Cmd 发送 AT 命令, 收集响应直到 OK/ERROR/超时; 超时也返回已收数据
func (a *AT) Cmd(cmd string, timeoutMs int) ([]string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.port.ResetInputBuffer()
	if _, err := a.port.Write([]byte(cmd + "\r")); err != nil {
		return nil, err
	}
	deadline := time.Now().Add(time.Duration(timeoutMs) * time.Millisecond)
	var lines []string
	buf := make([]byte, 1024)
	acc := ""
	for time.Now().Before(deadline) {
		n, err := a.port.Read(buf)
		if err == nil && n > 0 {
			acc += string(buf[:n])
			for {
				idx := strings.IndexAny(acc, "\r\n")
				if idx < 0 {
					break
				}
				line := strings.TrimSpace(acc[:idx])
				acc = acc[idx+1:]
				if line == "" || line == cmd {
					continue
				}
				lines = append(lines, line)
				if line == "OK" || strings.HasPrefix(line, "ERROR") || strings.HasPrefix(line, "+CME ERROR") || strings.HasPrefix(line, "+CMS ERROR") {
					return lines, nil
				}
			}
		} else {
			time.Sleep(20 * time.Millisecond)
		}
	}
	if len(lines) > 0 {
		return lines, fmt.Errorf("timeout (partial)")
	}
	return lines, fmt.Errorf("timeout waiting for response to %q", cmd)
}

// Dial 拨号专用: CONNECT 也作为终止条件
func (a *AT) Dial(cmd string, timeoutMs int) ([]string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.port.ResetInputBuffer()
	if _, err := a.port.Write([]byte(cmd + "\r")); err != nil {
		return nil, err
	}
	deadline := time.Now().Add(time.Duration(timeoutMs) * time.Millisecond)
	var lines []string
	buf := make([]byte, 1024)
	acc := ""
	for time.Now().Before(deadline) {
		n, err := a.port.Read(buf)
		if err == nil && n > 0 {
			acc += string(buf[:n])
			for {
				idx := strings.IndexAny(acc, "\r\n")
				if idx < 0 {
					break
				}
				line := strings.TrimSpace(acc[:idx])
				acc = acc[idx+1:]
				if line == "" || line == cmd {
					continue
				}
				lines = append(lines, line)
				if line == "OK" || strings.HasPrefix(line, "CONNECT") || strings.HasPrefix(line, "ERROR") ||
					strings.HasPrefix(line, "+CME ERROR") || strings.HasPrefix(line, "NO CARRIER") || strings.HasPrefix(line, "NO ANSWER") {
					return lines, nil
				}
			}
		} else {
			time.Sleep(20 * time.Millisecond)
		}
	}
	if len(lines) > 0 {
		return lines, fmt.Errorf("dial timeout (partial)")
	}
	return lines, fmt.Errorf("dial timeout")
}

// LockWrite 加锁裸写 (CMGS 交互等非标准流)
func (a *AT) LockWrite(s string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.port.ResetInputBuffer()
	a.port.Write([]byte(s))
}

// ReadRaw 裸读 (与 LockWrite 配对)
func (a *AT) ReadRaw(buf []byte) (int, error) {
	return a.port.Read(buf)
}

// ---- v0.9.9g: 独占会话 (cmgsSend 这类多步 AT 交互专用) ----
//
// Lock/Unlock 直接暴露底层互斥: 持锁期间其它 goroutine 的 Cmd/LockWrite 会阻塞等待,
// 保证 "写命令 → 等 > 提示符 → 写正文 → 读响应" 整串不会被并发 AT 命令插队抢读
// (原实现用 LockWrite 写完即解锁 + 裸 ReadRaw, 与 toolbox 1s 轮询并发时会互相抢字节)。
// ⚠ 持锁期间【禁止】再调 a.Cmd/LockWrite (会自我死锁), 用下面的 Raw* 方法。

// Lock 独占 AT 会话
func (a *AT) Lock() { a.mu.Lock() }

// Unlock 释放独占会话
func (a *AT) Unlock() { a.mu.Unlock() }

// ResetInput 清空串口输入缓冲 (须持锁)
func (a *AT) ResetInput() { a.port.ResetInputBuffer() }

// WriteRaw 裸写 (须持锁)
func (a *AT) WriteRaw(s string) (int, error) { return a.port.Write([]byte(s)) }

// ReadBuf 裸读 (须持锁; 底层 Read 自带 200ms 超时, 超时返回 0, nil)
func (a *AT) ReadBuf(buf []byte) (int, error) { return a.port.Read(buf) }

// Raw 暴露底层串口 (PPP 模式: 引擎直控字节流)
func (a *AT) Raw() serial.Port {
	return a.port
}

func (a *AT) Close() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.port.Close()
}
