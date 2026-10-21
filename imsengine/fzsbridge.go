package main

// fzsbridge.go —— imsengine ↔ fzs-sms（厂商短信管理系统）桥接层
//
// 把 IMS 通道模拟成 fzs-sms 的"模组3"，全部复用现成机制：
//   MT 收信 → inbox.jsonl（flock 同锁）+ fwdq 队列（fzs-smsd 的 fwd_flush 自动转发）
//   MO 发信 → /tmp/ims-sms/req-*.txt（LuCI→fzs-at→init.d 提交）→ 引擎发送 → outbox.jsonl
//   双向删除 → fzs-at sms drop/dropout 按 modem:"3" 匹配，原样可用
//
// 契约逐字段实证自 /usr/libexec/fzs-sms.uc：
//   inbox.jsonl 行: {"ts":<epoch>,"modem":"<N>","idx":<n>,"sender":<jesc>,"time":"<YYYY-MM-DD HH:MM:SS>","content":<jesc>}
//   outbox.jsonl 行: {"ts":<epoch>,"modem":"<N>","to":<jesc>,"content":<jesc>}
//   fwdq 文件: /etc/fzs-sms/fwdq/<ts>-<模组号>-<idx>，内容 = 发件人\n时间\n正文…
//   jesc: 只转义 " \ \n \r \t（与 uc jesc 逐字符一致，保证 JSON.parse 兼容）
//   容量: inbox 500 / outbox 200（保留最新，uc INBOX_MAX/OUTBOX_MAX）
//   跨进程锁: /tmp/fzs-sms-state.lock（fzs-smsd 解析入库、页面 clear/drop 都用它）

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	fzsDir     = "/etc/fzs-sms"
	fzsInbox   = fzsDir + "/inbox.jsonl"
	fzsOutbox  = fzsDir + "/outbox.jsonl"
	fzsFwdq    = fzsDir + "/fwdq"
	fzsLock    = "/tmp/fzs-sms-state.lock"
	fzsInboxCap = 500  // uc INBOX_MAX
	fzsOutboxCap = 200 // uc OUTBOX_MAX
	imsSlot    = "3"   // IMS 在 fzs-sms 里的虚拟模组号（页面显示"模组3"）

	reqDir = "/tmp/ims-sms" // MO 发送请求目录（init.d ims submit 写入，引擎轮询）
)

var (
	fzsBridgeMu sync.Mutex // 进程内串行化（跨进程锁之外再防自己的重入）
	imsIdxNext  int64      // IMS 条目 idx 分配（启动时从 inbox 现有 max+1 起）
)

// fzsJesc 与 uc jesc 逐字符同语义：仅转义 " \ \n \r \t
func fzsJesc(s string) string {
	var b strings.Builder
	for _, c := range s {
		switch c {
		case '"':
			b.WriteString("\\\"")
		case '\\':
			b.WriteString("\\\\")
		case '\n':
			b.WriteString("\\n")
		case '\r':
			b.WriteString("\\r")
		case '\t':
			b.WriteString("\\t")
		default:
			b.WriteRune(c)
		}
	}
	return b.String()
}

// fzsLocked 跨进程文件锁（fzs-smsd / fzs-at 页面操作共用同一把）
func fzsLocked(fn func()) error {
	lf, err := os.OpenFile(fzsLock, os.O_CREATE|os.O_RDWR, 0644)
	if err != nil {
		return err
	}
	if err := syscall.Flock(int(lf.Fd()), syscall.LOCK_EX); err != nil {
		lf.Close()
		return err
	}
	defer lf.Close()
	defer syscall.Flock(int(lf.Fd()), syscall.LOCK_UN)
	fn()
	return nil
}

// fzsAtomicWrite 写临时文件再 rename（防掉电半写；权限与 uc awrite 一致 0644）
func fzsAtomicWrite(path, content string) error {
	tmp := path + ".ims.tmp"
	if err := os.WriteFile(tmp, []byte(content), 0644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// fzsReadLines 读 jsonl 全部非空行
func fzsReadLines(path string) []string {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var lines []string
	for _, ln := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(ln) != "" {
			lines = append(lines, ln)
		}
	}
	return lines
}

// fzsIdxInit 扫 inbox.jsonl 里现有所有行（不限模组）的最大 idx，IMS 从 max+1 起分配
// —— 保证 drop(模组,idx) 匹配语义与模组1/2 不撞（本进程每次启动调一次）
func fzsIdxInit() {
	fzsBridgeMu.Lock()
	defer fzsBridgeMu.Unlock()
	var maxIdx int64
	for _, ln := range fzsReadLines(fzsInbox) {
		// 行内取 "idx":<n>
		p := strings.Index(ln, `"idx":`)
		if p < 0 {
			continue
		}
		rest := ln[p+6:]
		end := strings.IndexAny(rest, ",}")
		if end < 0 {
			continue
		}
		var n int64
		if _, err := fmt.Sscanf(rest[:end], "%d", &n); err == nil && n > maxIdx {
			maxIdx = n
		}
	}
	imsIdxNext = maxIdx + 1
	logf("[fzs-bridge] inbox 现有最大 idx=%d, IMS 条目从 %d 起分配", maxIdx, imsIdxNext)
}

// fzsInboxAppend 收件箱追加一条 IMS 短信（锁内读-改-写），返回分配的 idx
func fzsInboxAppend(ts int64, sender, timeStr, content string) int64 {
	fzsBridgeMu.Lock()
	defer fzsBridgeMu.Unlock()
	var idx int64
	err := fzsLocked(func() {
		lines := fzsReadLines(fzsInbox)
		idx = imsIdxNext
		imsIdxNext++
		lines = append(lines, fmt.Sprintf(
			`{"ts":%d,"modem":"%s","idx":%d,"sender":"%s","time":"%s","content":"%s"}`,
			ts, imsSlot, idx, fzsJesc(sender), fzsJesc(timeStr), fzsJesc(content)))
		// 容量 500：保留最新（与 uc "保留尾部 cap 条" 同口径）
		if len(lines) > fzsInboxCap {
			lines = lines[len(lines)-fzsInboxCap:]
		}
		fzsAtomicWrite(fzsInbox, strings.Join(lines, "\n")+"\n")
	})
	if err != nil {
		logf("!! [fzs-bridge] inbox 入库失败(锁): %v", err)
		return -1
	}
	return idx
}

// fzsOutboxAppend 已发送留底追加一条 IMS 发送成功记录
func fzsOutboxAppend(to, content string) {
	fzsBridgeMu.Lock()
	defer fzsBridgeMu.Unlock()
	err := fzsLocked(func() {
		lines := fzsReadLines(fzsOutbox)
		lines = append(lines, fmt.Sprintf(
			`{"ts":%d,"modem":"%s","to":"%s","content":"%s"}`,
			time.Now().Unix(), imsSlot, fzsJesc(to), fzsJesc(content)))
		if len(lines) > fzsOutboxCap {
			lines = lines[len(lines)-fzsOutboxCap:]
		}
		fzsAtomicWrite(fzsOutbox, strings.Join(lines, "\n")+"\n")
	})
	if err != nil {
		logf("!! [fzs-bridge] outbox 留底失败(锁): %v", err)
	}
}

// fwdqEnqueue 写 fzs-smsd 的转发队列文件（一文件一条：发件人\n时间\n正文…）
// fzs-smsd 每轮 fwd_flush 扫目录自动转发（渠道/重试/失败封顶全部复用现成机制）
func fwdqEnqueue(ts int64, sender, timeStr, content string, idx int64) {
	name := fmt.Sprintf("%d-%s-%d", ts, imsSlot, idx)
	tmp := filepath.Join(fzsFwdq, name+".ims.tmp")
	dst := filepath.Join(fzsFwdq, name)
	if err := os.MkdirAll(fzsFwdq, 0755); err != nil {
		logf("!! [fzs-bridge] fwdq 目录创建失败: %v", err)
		return
	}
	if err := os.WriteFile(tmp, []byte(sender+"\n"+timeStr+"\n"+content), 0644); err != nil {
		logf("!! [fzs-bridge] fwdq 写入失败: %v", err)
		os.Remove(tmp)
		return
	}
	if err := os.Rename(tmp, dst); err != nil {
		logf("!! [fzs-bridge] fwdq 提交失败: %v", err)
		os.Remove(tmp)
		return
	}
	logf("[fzs-bridge] 已入转发队列: %s", name)
}

// ---------- MO 发送请求（/tmp/ims-sms/req-<id>.txt：第1行号码, 第2行起正文） ----------

// smsTestCmd 桥接自验入口：imsengine smstest <发件人> <正文>
// 不依赖真实来话，直接走一遍 inbox 入库 + 转发队列（接手方/现场无测试号码时用）
func smsTestCmd(sender, text string) {
	fzsIdxInit()
	ts := time.Now().Unix()
	tstr := time.Now().Format("2006-01-02 15:04:05")
	idx := fzsInboxAppend(ts, sender, tstr, text)
	if idx < 0 {
		fmt.Println("FAIL: inbox 入库失败")
		os.Exit(1)
	}
	fwdqEnqueue(ts, sender, tstr, text, idx)
	fmt.Printf("OK idx=%d\n", idx)
}

// smsReqPoll 处理来自 LuCI（fzs-at sms send ims）的发送请求。
// 请求文件由 /etc/init.d/ims submit 写入（原子 rename）；
// 发完写 req-<id>.done（ok / err:…），submit 等它做同/异步入执。
func smsReqPoll(reg *Reg) {
	entries, err := os.ReadDir(reqDir)
	if err != nil {
		return
	}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, "req-") || strings.HasSuffix(name, ".done") {
			continue
		}
		path := filepath.Join(reqDir, name)
		data, err := os.ReadFile(path)
		if err != nil {
			continue // 可能正被 rename 替换中，下一轮再看
		}
		os.Remove(path) // 先取销，防重启重复发送
		id := strings.TrimSuffix(strings.TrimPrefix(name, "req-"), ".txt")

		nl := strings.IndexByte(string(data), '\n')
		if nl < 0 {
			writeReqDone(id, "err:请求格式错误（缺正文）")
			continue
		}
		to := strings.TrimSpace(string(data[:nl]))
		text := strings.TrimLeft(string(data[nl+1:]), "\n")
		if to == "" || text == "" {
			writeReqDone(id, "err:号码或内容为空")
			continue
		}
		// IMS 单条上限：TPDU UDL 是 1 字节，UCS2 每字 2B → 140B（70 个 Unicode 字符）
		if len([]rune(text))*2 > 140 {
			writeReqDone(id, "err:IMS 通道单条上限 70 个 Unicode 字符（暂不支持自动分条）")
			continue
		}
		logf("[fzs-bridge][MO] 网页发送请求 → %s (%d 字符)", to, len([]rune(text)))
		if err := reg.SendSMS(to, text); err != nil {
			logf("[fzs-bridge][MO] 发送失败: %v", err)
			writeReqDone(id, "err:"+err.Error())
			continue
		}
		fzsOutboxAppend(to, text)
		writeReqDone(id, "ok")
		logf("[fzs-bridge][MO] 已提交并留底 → %s", to)
	}
}

// writeReqDone 写回执文件（submit 侧轮询等待，最多 20s）
func writeReqDone(id, result string) {
	os.MkdirAll(reqDir, 0755)
	fzsAtomicWrite(filepath.Join(reqDir, "req-"+id+".done"), result+"\n")
}
