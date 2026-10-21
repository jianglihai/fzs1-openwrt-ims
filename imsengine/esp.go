package main

// esp.go - 用户态 ESP（传输模式 / null 加密 / hmac-sha-1-96）+ AF_PACKET 收包
//
// 契约（2026-09-27 FZS 实测, uvo_ims.py/U3 同源）:
//   出向 ESP SPI = P-CSCF 的 spi-s；入向 ESP SPI = 我方 spi-s
//   完整性密钥 = IK(16B) + 4 零 = 20B；ICV = HMAC-SHA1-96 截 12B
//   ★ 传输模式: ESP 载荷 = 裸 UDP 段（8B UDP 头 + SIP），不是完整 IPv6 包
//   ★ 老内核(MIPS 3.10)没 xfrm，入向 ESP 进不了内核 → 必须 AF_PACKET 链路层抓
//   ★ net.FilePacketConn 包装 AF_PACKET 在该内核不可用 → 裸 syscall fd

import (
	"crypto/hmac"
	"crypto/md5"
	"crypto/sha1"
	"encoding/binary"
	"fmt"
	"hash"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

func htons(x uint16) uint16 { return (x << 8) | (x >> 8) }

// ── v0.9.9m: ESP 完整性算法跟随网络协商（原来写死 sha1）──
// ICV 都是 96bit(12B)，只有底层 hash 不同；由 Security-Server 的 alg= 决定。
var espHashAlg = "sha1"

func setESPAlgFromSIP(alg string) {
	a := strings.ToLower(strings.TrimSpace(alg))
	if a == "" {
		return // 未声明则保持现状, 别把已协商好的算法改错
	}
	switch a {
	case "hmac-md5-96", "hmac-md5":
		espHashAlg = "md5"
	default:
		espHashAlg = "sha1"
	}
	logf("[ESP] 完整性算法 = %s (Security-Server alg=%q)", espHashAlg, alg)
}

func espHashNew() hash.Hash {
	if espHashAlg == "md5" {
		return md5.New()
	}
	return sha1.New()
}

// ---------- ESP 帧构造/校验 ----------

func espWrap(spi uint32, seq int, seg []byte, key []byte) []byte {
	pad := (4 - (len(seg)+2)%4) % 4
	body := append([]byte{}, seg...)
	for i := 0; i < pad; i++ {
		body = append(body, 0)
	}
	body = append(body, byte(pad), 17) // padlen, nexthdr=UDP
	esp := make([]byte, 8)
	binary.BigEndian.PutUint32(esp[0:4], spi)
	binary.BigEndian.PutUint32(esp[4:8], uint32(seq))
	esp = append(esp, body...)
	m := hmac.New(espHashNew, key)
	m.Write(esp)
	return append(esp, m.Sum(nil)[:12]...)
}

func espVerify(esp []byte, key []byte) (seg []byte, ok bool) {
	if len(esp) < 8+12 {
		return nil, false
	}
	m := hmac.New(espHashNew, key)
	m.Write(esp[:len(esp)-12])
	if !hmac.Equal(m.Sum(nil)[:12], esp[len(esp)-12:]) {
		return nil, false
	}
	data := esp[8 : len(esp)-12]
	padlen := int(data[len(data)-2])
	if padlen+2 > len(data) {
		return nil, false
	}
	return data[:len(data)-2-padlen], true
}

// ---------- AF_PACKET ----------

func openPacket(ifindex int) int {
	fd, err := syscall.Socket(syscall.AF_PACKET, syscall.SOCK_RAW, int(htons(3)))
	if err != nil {
		logf("AF_PACKET socket 失败: %v", err)
		return -1
	}
	bind := &syscall.SockaddrLinklayer{Ifindex: ifindex, Protocol: htons(3)}
	if err := syscall.Bind(fd, bind); err != nil {
		logf("AF_PACKET bind 失败: %v", err)
		syscall.Close(fd)
		return -1
	}
	syscall.SetNonblock(fd, true)
	return fd
}

// ---------- ESP 收发 io ----------

// ESPEvent 解密后的 UDP 段（来自 P-CSCF）或明文 SIP
type ESPEvent struct {
	Sport, Dport int
	Payload      []byte
	Protected    bool
}

// ---------- 网侧存活追踪 ----------
//
// ★ 2026-10-20 新增：判断"IMS 注册还活着"的可靠判据是「多久没收到对端任何东西」，
//   而不是墙钟 —— 运营商可以在租期未到时就悄悄回收 SA / 清掉 UDP 会话，此时本地
//   依然认为自己在册，但发短信必丢。任何一次来自 P-CSCF 且通过 HMAC 校验的入站包
//   （含 ESP keepalive 回包、SIP 响应、MT 短信）都刷新这个时间戳。
var (
	netRxMu   sync.Mutex
	lastNetRx time.Time
)

// markNetRx 记录一次有效入站
func markNetRx() {
	netRxMu.Lock()
	lastNetRx = time.Now()
	netRxMu.Unlock()
}

// SinceNetRx 距上次收到网络侧有效报文的时间
func SinceNetRx() time.Duration {
	netRxMu.Lock()
	t := lastNetRx
	netRxMu.Unlock()
	if t.IsZero() {
		return 0
	}
	return time.Since(t)
}

// ResetNetRx 新注册建立时重置，避免刚注册就被判静默
func ResetNetRx() {
	netRxMu.Lock()
	lastNetRx = time.Now()
	netRxMu.Unlock()
}

type ESPio struct {
	uev6, pcscf string

	mu        sync.Mutex
	spiOut    uint32
	spiIn     uint32
	key       []byte
	keyPrev   []byte // 换 key 时旧的留一拍（SA 切换期对端可能仍用旧 key）
	seq       uint32

	raw  net.PacketConn
	pfd  int
	evCh chan ESPEvent
	stop chan struct{}

	noKeySent uint64 // ★ key 未设置时的静默丢弃计数（用于限频打日志, 避免刷屏）
}

func NewESPio(uev6, pcscf string) (*ESPio, error) {
	// ★ 9/27 晚实测: SLAAC 刚配出的地址处于 DAD(tentative) 状态, 立即 bind 报
	//   "cannot assign requested address"。重试等 DAD 完成即可。
	var raw net.PacketConn
	var err error
	for i := 0; i < 10; i++ {
		raw, err = net.ListenPacket("ip6:50", uev6)
		if err == nil {
			break
		}
		if i < 9 {
			time.Sleep(2 * time.Second)
		}
	}
	if err != nil {
		return nil, fmt.Errorf("ip6:50 raw socket: %v", err)
	}
	ifi, err := net.InterfaceByName(gIface) // ★ CPE 版: 网卡名可配（默认 rndis-modem1）
	if err != nil {
		raw.Close()
		return nil, fmt.Errorf("%s: %v", gIface, err)
	}
	pfd := openPacket(ifi.Index)
	if pfd < 0 {
		raw.Close()
		return nil, fmt.Errorf("AF_PACKET 不可用")
	}
	e := &ESPio{uev6: uev6, pcscf: pcscf, raw: raw, pfd: pfd,
		evCh: make(chan ESPEvent, 64), stop: make(chan struct{})}
	go e.rxLoop()
	return e, nil
}

func (e *ESPio) SetSPIs(spiOut, spiIn uint32) {
	e.mu.Lock()
	e.spiOut, e.spiIn = spiOut, spiIn
	e.mu.Unlock()
}

// SetPCSCF 候选链切换候选时同步更新收发锚点。
// ★ 收包过滤按"源 IP = pcscf"、发送按"目标 IP = pcscf"——Reg 侧统一经
//   Reg.setPcscf 调本方法, r.pcscf 与 e.pcscf 永同步 (曾为潜伏 bug, v0.7.6 收敛)。
// ★ v0.7.6: pcscf 读写全走 mu — string 两字赋值非原子, 原先写有锁而
//   handle/Send 无锁读, 换候选瞬间可能读到撕裂串 (race)。
func (e *ESPio) SetPCSCF(ip string) {
	e.mu.Lock()
	e.pcscf = ip
	e.mu.Unlock()
}

// pcscfAddr 锁内读当前锚点 (handle 收包过滤 / Send 目标共用)
func (e *ESPio) pcscfAddr() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.pcscf
}

// SetKey 换 SA 完整性密钥（旧的留到 keyPrev 兜底一拍）
func (e *ESPio) SetKey(key []byte) {
	e.mu.Lock()
	if e.key != nil {
		kp := make([]byte, len(e.key))
		copy(kp, e.key)
		e.keyPrev = kp
	}
	k := make([]byte, len(key))
	copy(k, key)
	e.key = k
	e.mu.Unlock()
}

// Send 发一个 UDP 段（裸段, 含 8B UDP 头）
func (e *ESPio) Send(seg []byte) {
	e.mu.Lock()
	spi, key := e.spiOut, e.key
	dst := e.pcscf // ★ v0.7.7: 已在锁内, 直接读字段 (v0.7.6 在锁内调 pcscfAddr 二次加锁 = 死锁)
	e.seq++
	seq := e.seq
	e.mu.Unlock()
	if key == nil {
		// ★ 限频：注册间隙 / ESP 已关闭期间每秒一次调用会把日志刷爆
		//   （实测 3 天刷了 2000+ 行 "key 未设置", 真正有用的日志全被淹了）
		n := atomic.AddUint64(&e.noKeySent, 1)
		if n == 1 || n%50 == 0 {
			logf("ESP Send: key 未设置! (已静默丢弃 %d 个)", n)
		}
		return
	}
	esp := espWrap(spi, int(seq), seg, key)
	if _, err := e.raw.WriteTo(esp, &net.IPAddr{IP: net.ParseIP(dst)}); err != nil {
		logf("ESP Send 失败: %v", err)
	}
}

func (e *ESPio) rxLoop() {
	buf := make([]byte, 16384)
	for {
		select {
		case <-e.stop:
			return
		default:
		}
		n, _, err := syscall.Recvfrom(e.pfd, buf, 0)
		if err != nil {
			time.Sleep(30 * time.Millisecond)
			continue
		}
		e.handle(buf[:n])
	}
}

func (e *ESPio) handle(b []byte) {
	if len(b) < 54 || binary.BigEndian.Uint16(b[12:14]) != 0x86DD {
		return
	}
	nh := b[20]
	// [诊断] IPv6 分片/截断检测：
	//   nh=44 是 IPv6 分片头（本引擎用户态 ESP 未做重组，长报文会被静默丢弃）
	//   payload_len+40 != 帧长 → 帧被截断
	// 以太网帧: b[0:6]dstMAC b[6:12]srcMAC b[12:14]EtherType b[14:]IPv6
	// IPv6 头: b[14:18]版本/流标签 b[18:20]payload length b[20]next header
	pl := int(binary.BigEndian.Uint16(b[18:20]))
	if nh == 44 {
		logf("[诊断] IPv6 分片帧 nh=44: payload_len=%d 实际=%dB（未重组, 已丢弃）", pl, len(b)-54)
		return
	}
	if pl != len(b)-54 {
		logf("[诊断] IPv6 长度不符: payload_len=%d 实际帧载荷=%dB nh=%d", pl, len(b)-54, nh)
	}
	if net.IP(b[22:38]).String() != e.pcscfAddr() { // ★ v0.7.6: 锁内读, 防撕裂
		return
	}
	switch nh {
	case 50: // ESP
		espB := b[54:len(b)]
		if len(espB) < 8+12 {
			return
		}
		e.mu.Lock()
		key, kp := e.key, e.keyPrev
		e.mu.Unlock()
		seg, ok := espVerify(espB, key)
		if !ok && kp != nil {
			seg, ok = espVerify(espB, kp)
		}
		if !ok {
			spi := binary.BigEndian.Uint32(espB[0:4])
			logf("[ESP] 收到 SPI=%d len=%dB 但 HMAC 校验失败(两把 key)", spi, len(espB))
			return
		}
		if len(seg) < 8 {
			return
		}
		// [诊断] UDP 头自带长度字段：与解密后实际长度不符 ⇒ 报文在到达本进程前已被截断
		if ulen := int(binary.BigEndian.Uint16(seg[4:6])); ulen != len(seg) {
			logf("[诊断] UDP 长度不符: 头部声明=%dB 实际=%dB（差 %d）", ulen, len(seg), ulen-len(seg))
		}
		e.push(ESPEvent{
			Sport:     int(seg[0])<<8 | int(seg[1]),
			Dport:     int(seg[2])<<8 | int(seg[3]),
			Payload:   seg[8:],
			Protected: true,
		})
	case 17: // 明文 UDP 兜底（P-CSCF 偶尔明文回）
		udp := b[54:len(b)]
		if len(udp) < 8 {
			return
		}
		sport := int(udp[0])<<8 | int(udp[1])
		dport := int(udp[2])<<8 | int(udp[3])
		pt := opPcscfPort() // ★ v0.9.9t: 明文兜底端口跟随配置（原写死 5060）
		if sport != pt && dport != pt {
			return
		}
		e.push(ESPEvent{Sport: sport, Dport: dport, Payload: udp[8:], Protected: false})
	}
}

func (e *ESPio) push(ev ESPEvent) {
	// 能走到这里 = 通过了 HMAC 校验的 ESP 包或端口匹配的明文 SIP ⇒ 对端/SA 还活着
	markNetRx()
	select {
	case e.evCh <- ev:
	default: // 满则丢最旧
		select {
		case <-e.evCh:
		default:
		}
		select {
		case e.evCh <- ev:
		default:
		}
	}
}

// WaitEvent 等一个来自 P-CSCF 的事件
func (e *ESPio) WaitEvent(timeout time.Duration) *ESPEvent {
	select {
	case ev := <-e.evCh:
		return &ev
	case <-time.After(timeout):
		return nil
	}
}

func (e *ESPio) Close() {
	select {
	case <-e.stop:
	default:
		close(e.stop)
	}
	syscall.Close(e.pfd)
	e.raw.Close()
}

var _ = os.Exit

// KeepaliveLoop 周期性发送最小 ESP 包, 防止运营商链路会话(防火墙/NAT/SA 闲置)超时被清
// ★ 2026-09-29 根因: 30 分钟无流量的续租必失败(受保护发送无响应 ×3, 抓包确认无回包),
//   而 3 分钟周期连续成功 —— 判定链路上存在会话级超时(典型 15~30min)。
//   IPsec 标准做法即 keepalive(RFC 3947 NAT-T keepalive 同型)。
//   发的是仅含 SPI+seq+padding 的最小 ESP 包, P-CSCF 解密失败丢弃, 无害。
func (e *ESPio) KeepaliveLoop(interval time.Duration, stop <-chan struct{}) {
	t := time.NewTicker(interval)
	defer t.Stop()
	n := 0
	for {
		select {
		case <-stop:
			logf("[keepalive] 停止, 共发送 %d 个", n)
			return
		case <-t.C:
			e.Send(nil)
			n++
		}
	}
}
