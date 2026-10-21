package main

// main.go - imsengine-cpe：OpenWrt CPE 移植版（MIBOX / EC200T）
//
// 移植自 fzsagent engine v0.9.x，裁剪适配：
//   - 去掉 001B 面板/机器人/IMEI/网络模式生态（bot/toolbox/netmode/forward/inbox）
//   - IMS 承载按 CPE 实证：ims 走 CID3 + AT+QNETDEVCTL=2,<cid>,1（op=2 语义，
//     与 001B 的 op=1 不同），RA/SLAAC 从 rndis-modem1 拿 IMS /64
//   - 引擎自维护一个稳定注册地址（CPE 的 SLAAC 地址是 mngtmpaddr 会轮换，
//     轮换触发无谓重注册；SA 绑定也要求地址稳定）
//   - 用户态 ESP（内核无 XFRM）+ AF_PACKET 收包，绕开模组 QIOPEN socket 的
//     vendor SIP Guard（实证：同字节 REGISTER 模组 socket 回假 400，
//     主机侧回真 401）
//
// 用法（OpenWrt 包imsengine 的 init.d 注入环境变量）：
//   IMS_IFACE=rndis-modem1  IMS_CID=3  IMS_AT_PORT=auto
//   imsengine              常驻: 注册 + 保活（短信阶段2接入）
//
// 状态文件: /tmp/ims_status.txt（KEY='值' 格式, LuCI/诊断可读）

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	statusFile = "/tmp/ims_status.txt"
	version    = "cpe-0.1.0"
)

var (
	gIface = "rndis-modem1" // RNDIS 网卡名（IMS 承载所在）
	cid    = 3              // IMS PDP 的 CID（CPE 实证: cid2 不可激活, ims=cid3）

	metaMu                                                     sync.Mutex
	gState, gDetail, gAtPort, gImsi                            string
	mCSQ, mCell, mReg, mICCID, mIMEI, mCOPS, mModel, mFw, mTemp string
)

// init 读环境变量（init.d 由 uci 转换注入），便于不改源码调参
func init() {
	if v := os.Getenv("IMS_IFACE"); v != "" {
		gIface = v
	}
	if v := os.Getenv("IMS_CID"); v != "" {
		if n, e := strconv.Atoi(v); e == nil && n >= 1 && n <= 15 {
			cid = n
		}
	}
}

// ---------- 状态文件 ----------

// writeStatus 写状态文件并打日志（状态"发生变迁"时用）
func writeStatus(state, detail string) {
	writeStatusFile(state, detail)
	logf("状态 -> %s | %s", state, detail)
}

// writeStatusQuiet 只更新状态文件、不打日志。
// ★ metaRefresh 每 30s 刷新一次 IMSI/CSQ/温度等元数据，若也走 writeStatus，
//   日志里就塞满 "状态 -> 已注册 | ims 注册成功" 心跳，真正有价值的行（重注册、探活、失败）
//   全被淹没 —— 排查"注册到底保不保持得住"时尤其致命。
func writeStatusQuiet(state, detail string) {
	writeStatusFile(state, detail)
}

func writeStatusFile(state, detail string) {
	metaMu.Lock()
	gState, gDetail = state, detail
	tmp := fmt.Sprintf("STATE='%s'\nDETAIL='%s'\nENGINE_PORT='%s'\n", state, detail, gAtPort)
	tmp += fmt.Sprintf("T_IMSI='%s'\nT_CSQ='%s'\nT_REG='%s'\nT_ICCID='%s'\nT_IMEI='%s'\nT_TEMP='%s'\n",
		gImsi, mCSQ, mReg, mICCID, mIMEI, mTemp)
	// ---- 注册租期 / 存活信息（回答"注册还能维持多久 / 是不是已经悄悄失效了"）----
	if !regSince.IsZero() {
		tmp += fmt.Sprintf("REG_SINCE='%s'\nUP_SEC='%d'\n",
			regSince.Format("15:04:05"), int64(time.Since(regSince).Seconds()))
	}
	if !nextRenewAt.IsZero() {
		tmp += fmt.Sprintf("NEXT_RENEW='%s'\n", nextRenewAt.Format("15:04:05"))
	}
	if r := SinceNetRx(); r > 0 {
		tmp += fmt.Sprintf("NET_RX_AGO='%ds'\n", int64(r.Seconds()))
	}
	metaMu.Unlock()
	tf := statusFile + ".tmp"
	if err := os.WriteFile(tf, []byte(tmp), 0644); err != nil {
		logf("写状态文件失败: %v", err)
		return
	}
	os.Rename(tf, statusFile)
}

// 注册起始时刻与下次续租时刻（供状态文件与诊断读取）
var (
	regSince    time.Time
	nextRenewAt time.Time
)

// ---------- 小工具 ----------

func isNum(s string) bool {
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return len(s) > 0
}

func firstOf(lines []string, prefix string) string {
	for _, l := range lines {
		if strings.HasPrefix(l, prefix) {
			return l
		}
	}
	return ""
}

func firstRe(lines []string, digits int) string {
	for _, l := range lines {
		l = strings.TrimSpace(l)
		if len(l) != digits {
			continue
		}
		ok := true
		for _, c := range l {
			if c < '0' || c > '9' {
				ok = false
				break
			}
		}
		if ok {
			return l
		}
	}
	return ""
}

func qTempVal(lines []string) string {
	for _, l := range lines {
		if !strings.Contains(l, "+QTEMP") {
			continue
		}
		toks := []int{}
		n := -1
		for _, c := range l + " " {
			if c >= '0' && c <= '9' {
				if n < 0 {
					n = 0
				}
				n = n*10 + int(c-'0')
			} else if n >= 0 {
				toks = append(toks, n)
				n = -1
			}
		}
		for _, t := range toks {
			if t != 0 && t < 250 {
				return fmt.Sprintf("%d", t)
			}
		}
	}
	return ""
}

func cpinReady(lines []string) bool {
	for _, l := range lines {
		if strings.Contains(l, "READY") {
			return true
		}
	}
	return false
}

// brkReason 从「保活+收信」内层循环退出的原因，决定外层是只重注册还是连承载一起重建
type brkReason int

const (
	brkSIM brkReason = iota // SIM 拔出 / AT 口失联 → 外层等卡重来
	brkBearer               // 承载或地址失效 → 必须重建 CID
	brkSIP                  // 仅 SIP 注册失效 → 承载保留，只重注册
)

// isSimGone 判断 AT 口是否「明确」报告 SIM 不在。
// ★ 只有模组自己说 SIM 没了才算拔卡；超时/无响应不能算，否则偶发抖动就是一次误杀。
func isSimGone(lines []string, cmdErr error) bool {
	if cmdErr != nil {
		return false // 超时/无响应 ≠ 拔卡
	}
	for _, l := range lines {
		u := strings.ToUpper(l)
		if strings.Contains(u, "+CME ERROR: 10") ||
			strings.Contains(u, "SIM NOT INSERTED") ||
			strings.Contains(u, "SIM NOT PRESENT") ||
			strings.Contains(u, "NO SIM") {
			return true
		}
	}
	return false
}

// 存活检测相关参数（可用同名环境变量覆盖，便于现场调参不用重编）
func envDur(name string, def time.Duration) time.Duration {
	if v := os.Getenv(name); v != "" {
		var n int
		if _, e := fmt.Sscanf(v, "%d", &n); e == nil && n > 0 {
			return time.Duration(n) * time.Second
		}
	}
	return def
}

var (
	// pinProbeSec 探卡周期；maxPinMiss 连续几次异常才判 AT 口失联
	// （20s × 6 = 2 分钟内持续收不到 CPIN 才动手，滤掉偶发串扰）
	pinProbeSec    = 20 * time.Second
	maxPinMiss     = 6
	// silenceProbeSec 多久没收到网络侧任何东西就开始探活
	// probeInterval 探活包间隔；maxProbeMiss 连续几次无回应就判注册失效并重注册
	// 最坏 180 + 20×3 = 240s（4 分钟）发现失效，远快于原先 25 分钟的定期续租
	silenceProbeSec = 180 * time.Second
	probeInterval   = 20 * time.Second
	maxProbeMiss    = 3
)

// initTimeline 允许用环境变量现场调参（不必重新编译）：
//
//	IMS_PIN_PROBE_SEC / IMS_PIN_MAX_MISS / IMS_SILENCE_SEC / IMS_PROBE_SEC / IMS_PROBE_MAX
func initTimeline() {
	pinProbeSec = envDur("IMS_PIN_PROBE_SEC", pinProbeSec)
	if v := os.Getenv("IMS_PIN_MAX_MISS"); v != "" {
		fmt.Sscanf(v, "%d", &maxPinMiss)
	}
	silenceProbeSec = envDur("IMS_SILENCE_SEC", silenceProbeSec)
	probeInterval = envDur("IMS_PROBE_SEC", probeInterval)
	if v := os.Getenv("IMS_PROBE_MAX"); v != "" {
		fmt.Sscanf(v, "%d", &maxProbeMiss)
	}
}

// hasAddrIn 判断接口上是否还挂着指定 IPv6（注册地址被内核清掉时用于及早发现）
func hasAddrIn(ifname, addr string) bool {
	out, err := exec.Command("ip", "-6", "addr", "show", "dev", ifname, "scope", "global").Output()
	if err != nil {
		return false
	}
	return strings.Contains(string(out), addr+"/")
}

// ---------- 元数据采集（喂状态文件, 低频） ----------

func metaRefresh(at *AT) {
	for {
		csqr, _ := at.Cmd("AT+CSQ", 5000)
		regr, _ := at.Cmd("AT+CEREG?", 5000)
		iccidr, _ := at.Cmd("AT+QCCID", 5000)
		tempr, _ := at.Cmd("AT+QTEMP", 4000)
		metaMu.Lock()
		mCSQ = firstOf(csqr, "+CSQ")
		mReg = firstOf(regr, "+CEREG")
		mICCID = firstOf(iccidr, "+QCCID")
		mTemp = qTempVal(tempr)
		metaMu.Unlock()
		// 只刷新元数据到状态文件，不打日志（否则每 30s 一行心跳淹掉有用信息）
		writeStatusQuiet(gState, gDetail)
		time.Sleep(30 * time.Second)
	}
}

// ---------- AT 口探测（CPE 单实例简化版） ----------

// busyPorts 扫 /proc/*/fd，返回被其他进程占用的 ttyUSB 名单（避开 vendor firmware 抢口）
func busyPorts() map[string]bool {
	out := map[string]bool{}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return out
	}
	self := fmt.Sprintf("%d", os.Getpid())
	for _, e := range entries {
		if !isNum(e.Name()) || e.Name() == self {
			continue
		}
		cmdline, _ := os.ReadFile("/proc/" + e.Name() + "/cmdline")
		s := string(cmdline)
		// pppd/chat = ims-ppp 的拨号进程（2026-10-20 教训：它占着 ttyUSB 进 PPP 数据模式后
		// 本引擎 AT 命令全部超时 → CIMI 五连败误报"未插卡"→ IMS 假注册。必须避开）
		if !strings.Contains(s, "firmware") &&
			!strings.Contains(s, "fzs") &&
			!strings.Contains(s, "pppd") &&
			!strings.Contains(s, "chat") {
			continue
		}
		fds, err := os.ReadDir("/proc/" + e.Name() + "/fd")
		if err != nil {
			continue
		}
		for _, fd := range fds {
			link, err := os.Readlink("/proc/" + e.Name() + "/fd/" + fd.Name())
			if err == nil && strings.Contains(link, "ttyUSB") {
				out[filepath.Base(link)] = true
			}
		}
	}
	return out
}

// openAT 探测 AT 口：优先 EC200T，其次任意能读 IMSI 的
// （MIBOX 上 EC200T=外置卡槽目标模组; vendor firmware 进程会周期性用 AT，
//  探测到的口若被占用，Cmd 会 timeout —— 用 busyPorts 预先避开）
func openAT() (*AT, string, error) {
	if p := os.Getenv("IMS_AT_PORT"); p != "" && p != "auto" {
		a, err := NewAT(p)
		if err == nil && a.ping() {
			return a, p, nil
		}
		if a != nil {
			a.Close()
		}
		// 显式口失效不退出: 回退自动探测（USB 重枚举后编号可能漂移）
		logf("指定 AT 口 %s 无响应, 回退自动探测", p)
	}
	for round := 0; round < 5; round++ {
		devs, _ := filepath.Glob("/dev/ttyUSB*")
		sort.Strings(devs)
		busy := busyPorts()
		var fallback *AT
		var fallbackDev string
		for _, dev := range devs {
			if busy[filepath.Base(dev)] {
				logf("AT 探测: 跳过 %s (被 firmware 占用)", dev)
				continue
			}
			a, err := NewAT(dev)
			if err != nil {
				continue
			}
			lines, err := a.Cmd("ATI", 2000)
			if err != nil {
				logf("AT 探测: %s ATI 无响应 (%v)", dev, err)
				a.Close()
				continue
			}
			model := "?"
			for _, l := range lines {
				switch {
				case strings.Contains(l, "EC200T"):
					model = "EC200T"
				case strings.Contains(l, "EC200N"):
					model = "EC200N"
				}
			}
			imsi := ""
			if ls, e := a.Cmd("AT+CIMI", 3000); e == nil {
				if v := firstRe(ls, 15); v != "" {
					imsi = v
				}
				for _, l := range ls {
					l = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(l), "+CIMI:"))
					if isNum(l) && len(l) >= 14 {
						imsi = l
					}
				}
			}
			logf("AT 候选 %s: 型号=%s IMSI=%q", dev, model, imsi)
			if model == "EC200T" && imsi != "" {
				return a, dev, nil
			}
			if imsi != "" && fallback == nil {
				fallback, fallbackDev = a, dev
				continue
			}
			a.Close()
		}
		if fallback != nil {
			logf("未找到 EC200T 有卡, 退用 %s", fallbackDev)
			return fallback, fallbackDev, nil
		}
		logf("AT 口探测第 %d 轮无果, 5s 后重试", round+1)
		time.Sleep(5 * time.Second)
	}
	return nil, "", fmt.Errorf("AT 口探测失败: 无可用口")
}

// readIMSI 直读 AT+CIMI（不走 openSim 的 CCHO/CCHC —— 会污染卡的应用选择态，
// 导致后续 AUTHENTICATE 报 6985）
func readIMSI(at *AT) string {
	for retry := 0; retry < 5; retry++ {
		ls, _ := at.Cmd("AT+CIMI", 6000)
		if v := firstRe(ls, 15); v != "" {
			return v
		}
		for _, l := range ls {
			l = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(l), "+CIMI:"))
			if isNum(l) && len(l) >= 14 {
				return l
			}
		}
		logf("CIMI 第 %d 次未取到, 8s 后重试", retry+1)
		time.Sleep(8 * time.Second)
	}
	return ""
}

// ---------- IMS 承载（CPE 实证姿势） ----------

// cidHasAddr 查 CID 是否已激活且有全球 v6
func cidHasAddr(at *AT) bool {
	ls, _ := at.Cmd(fmt.Sprintf("AT+CGPADDR=%d", cid), 5000)
	for _, l := range ls {
		if !strings.HasPrefix(l, "+CGPADDR:") {
			continue
		}
		for _, f := range strings.Split(strings.TrimPrefix(l, "+CGPADDR:"), ",") {
			f = strings.Trim(f, "\" ")
			if strings.Count(f, ":") >= 2 && !strings.HasPrefix(f, "::") {
				return true
			}
		}
	}
	return false
}

// netdevBound QNETDEVCTL? 读回绑定状态（"0,0,0,0" = 未绑）
func netdevBound(at *AT) bool {
	r, err := at.Cmd("AT+QNETDEVCTL?", 5000)
	if err != nil {
		return false
	}
	for _, ln := range r {
		ln = strings.TrimSpace(ln)
		if strings.HasPrefix(ln, "+QNETDEVCTL:") {
			f := strings.TrimSpace(strings.TrimPrefix(ln, "+QNETDEVCTL:"))
			return f != "" && f != "0,0,0,0"
		}
	}
	return false
}

func respHasOk(r []string) bool {
	for _, ln := range r {
		if strings.TrimSpace(ln) == "OK" {
			return true
		}
	}
	return false
}

// bearerUp 建/恢复 IMS 承载（CPE 实证）:
//   QICSGP=cid,"ims" → QIACT=cid → 等 CGPADDR → QNETDEVCTL=2,cid,1 绑 RNDIS
// ★ 幂等判据 = QNETDEVCTL 已绑 + 网卡有全球 v6 + 有 v6 默认路由
//   （CGPADDR 有值 ≠ PDP 健康: 半死 PDP 时 QNETDEVCTL 回 ERROR 但地址还在）
func bearerUp(at *AT) error {
	if netdevBound(at) && currentGlobal(gIface) != "" && hasV6DefaultRoute(gIface) {
		logf("承载已就绪 (QNETDEVCTL 已绑 + %s 有全球地址 + 有默认路由), 跳过重建", gIface)
		return nil
	}
	writeStatus("注册中", fmt.Sprintf("重建 ims 承载 (CID%d)", cid))
	if r, err := at.Cmd(fmt.Sprintf("AT+CGACT=0,%d", cid), 15000); err != nil {
		logf("CGACT=0,%d (可能本就未激活): %v", cid, err)
	} else {
		logf("CGACT=0,%d resp=%v", cid, r)
	}
	// ★ 去激活后立刻清掉旧 IPv6：运营商下次激活会再给一个新 /64 前缀，
	//   旧地址不清会一直堆在接口上（并最终把 currentGlobal 带沟里）
	flushGlobalV6(gIface)
	if r, err := at.Cmd(fmt.Sprintf(`AT+QICSGP=%d,2,"ims","","",1`, cid), 8000); err != nil {
		logf("QICSGP: %v (继续, 可能已配置)", err)
	} else {
		logf("QICSGP resp=%v", r)
	}
	writeStatus("注册中", fmt.Sprintf("激活 ims 承载 (QIACT CID%d)", cid))
	if r, err := at.Cmd(fmt.Sprintf("AT+QIACT=%d", cid), 30000); err != nil {
		logf("QIACT: %v", err)
	} else {
		logf("QIACT resp=%v", r)
	}
	ok := false
	for i := 0; i < 15 && !ok; i++ {
		time.Sleep(2 * time.Second)
		ok = cidHasAddr(at)
	}
	if !ok {
		return fmt.Errorf("CID%d 激活失败 (CGPADDR 无全球 v6)", cid)
	}
	logf("CID%d(ims) 已激活, 有全球 v6", cid)
	writeStatus("注册中", fmt.Sprintf("绑 CID%d 到 RNDIS (QNETDEVCTL)", cid))
	var lastErr error
	for i := 0; i < 3; i++ {
		// ★ CPE 实证语法: op=2（把 CID 绑到 RNDIS 网口）, cid, state
		r, err := at.Cmd(fmt.Sprintf("AT+QNETDEVCTL=2,%d,1", cid), 10000)
		if err == nil && respHasOk(r) {
			logf("QNETDEVCTL resp=%v ✓", r)
			lastErr = nil
			break
		}
		lastErr = fmt.Errorf("err=%v resp=%v", err, r)
		logf("QNETDEVCTL 第 %d 次未成功: %v (5s 后重试)", i+1, lastErr)
		time.Sleep(5 * time.Second)
		if lastErr != nil && i < 2 {
			at.Cmd(fmt.Sprintf("AT+QIACT=%d", cid), 30000)
			for j := 0; j < 10; j++ {
				time.Sleep(2 * time.Second)
				if cidHasAddr(at) {
					break
				}
			}
		}
	}
	if lastErr != nil {
		return fmt.Errorf("QNETDEVCTL 绑卡失败: %v", lastErr)
	}
	// 绑定后网卡可能处 DOWN, 内核不在 DOWN 口上收 RA → 必须拉起
	ensureIfaceUp(gIface)
	return nil
}

func diagnoseBearer(at *AT) {
	for _, c := range []string{
		fmt.Sprintf("AT+CGPADDR=%d", cid), "AT+CGACT?", "AT+QNETDEVCTL?", `AT+QCFG="usbnet"`,
	} {
		if r, err := at.Cmd(c, 8000); err != nil {
			logf("[diag] %s -> err=%v", c, err)
		} else {
			logf("[diag] %s -> %v", c, r)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// ---------- 主机侧地址管理 ----------

// currentGlobal 取接口上一个「真正可用」的全球 IPv6。
//
// ★ 2026-10-20 关键修复：原实现直接返回 net.Interface.Addrs() 的第一个全球 v6，
//   而 Addrs() 的返回顺序就是内核链表顺序 —— 承载每重建一次运营商就换一个新 /64
//   前缀，旧地址从不清（实测堆到 14 个前缀），它们只是变成 deprecated 仍留在链上，
//   于是 Addrs()[0] 常年是某个早已失效前缀上的地址。后果：
//     ① ensureStableAddr 拿错前缀派生稳定地址 → 注册源地址网络侧不可达
//     ② inner 循环的 prefix64Of 比对必然不等 → 每 30s 判一次"前缀变化"→ 无限自我重注册
//   这里改为读 `ip -6 addr` 文本，显式跳过 tentative(DAD 未完成) 与 deprecated(即将失效)。
func currentGlobal(ifname string) string {
	out, err := exec.Command("ip", "-6", "addr", "show", "dev", ifname, "scope", "global").Output()
	if err != nil {
		return ""
	}
	type cand struct {
		addr    string
		dynamic bool
	}
	var cands []cand
	for _, ln := range strings.Split(string(out), "\n") {
		f := strings.Fields(ln)
		if len(f) < 2 || f[0] != "inet6" {
			continue
		}
		low := strings.ToLower(ln)
		if strings.Contains(low, "deprecated") || strings.Contains(low, "tentative") {
			continue
		}
		ip := net.ParseIP(strings.Split(f[1], "/")[0])
		if ip == nil || ip.To4() != nil || !ip.IsGlobalUnicast() || ip.IsLinkLocalUnicast() {
			continue
		}
		cands = append(cands, cand{ip.String(), strings.Contains(low, "dynamic")})
	}
	if len(cands) == 0 {
		return ""
	}
	// 优先「永久地址」（我们自己 ip addr add 配的稳定注册地址, 不带 dynamic 标志）；
	// 只有在还没有稳定地址时（刚开机 / waitSLAAC 阶段）才退而用内核 SLAAC 的 dynamic 地址。
	for _, c := range cands {
		if !c.dynamic {
			return c.addr
		}
	}
	return cands[0].addr
}

// flushGlobalV6 删掉接口上所有 global-scope IPv6（保留 link-local）。
// ★ 承载重建会换前缀，旧地址不清理会越堆越多（见 currentGlobal 注释里的故障链）。
//   调用时机必须是「CGACT 去激活之后、QIACT 重新激活之前」—— 此时旧地址已失效，
//   删掉不会误伤正在用的；而紧接着 QIACT 会触发新 RA/SLAAC，无需等待。
func flushGlobalV6(ifname string) int {
	out, err := exec.Command("ip", "-6", "addr", "show", "dev", ifname, "scope", "global").Output()
	if err != nil {
		return 0
	}
	n := 0
	for _, ln := range strings.Split(string(out), "\n") {
		f := strings.Fields(ln)
		if len(f) < 2 || f[0] != "inet6" {
			continue
		}
		if err := exec.Command("ip", "-6", "addr", "del", f[1], "dev", ifname).Run(); err == nil {
			n++
		}
	}
	if n > 0 {
		logf("清理 %s 上 %d 个失效 IPv6 地址（承载重建, 前缀已变）", ifname, n)
	}
	return n
}

func ensureIfaceUp(ifname string) {
	ifi, err := net.InterfaceByName(ifname)
	if err != nil {
		logf("ensureIfaceUp: InterfaceByName(%s): %v", ifname, err)
		return
	}
	if ifi.Flags&net.FlagUp != 0 {
		return
	}
	if err := exec.Command("ip", "link", "set", ifname, "up").Run(); err != nil {
		logf("ensureIfaceUp: ip link set %s up: %v", ifname, err)
		return
	}
	logf("%s 原为 DOWN, 已自动 UP", ifname)
}

func hasV6DefaultRoute(ifc string) bool {
	out, err := exec.Command("ip", "-6", "route", "show", "default", "dev", ifc).Output()
	if err != nil {
		return false
	}
	return len(strings.TrimSpace(string(out))) > 0
}

// waitSLAAC 等 RA/SLAAC 拿到全球 v6（CPE 上 vendor 固件默认 disable_ipv6=1,
// 必须先打开 accept_ra/disable_ipv6 —— 这一步之前是手工做的, 现在引擎自管）
func waitSLAAC(ifname string, timeout time.Duration) (string, error) {
	ensureIfaceUp(ifname)
	os.WriteFile("/proc/sys/net/ipv6/conf/all/disable_ipv6", []byte("0"), 0644)
	os.WriteFile("/proc/sys/net/ipv6/conf/default/accept_ra", []byte("2"), 0644)
	os.WriteFile("/proc/sys/net/ipv6/conf/"+ifname+"/accept_ra", []byte("2"), 0644)
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if v := currentGlobal(ifname); v != "" {
			// 刚配出的地址处于 DAD(tentative), 等 3s 确认仍在（租期异常短会被撤）
			time.Sleep(3 * time.Second)
			if v2 := currentGlobal(ifname); v2 != "" {
				return v2, nil
			}
			continue
		}
		time.Sleep(2 * time.Second)
	}
	return "", fmt.Errorf("%s SLAAC 超时 (%v 内无全球 v6)", ifname, timeout)
}

// stableIID 从 IMSI 派生固定接口标识符（U/L 位置0, 保证全局唯一且重启不变）
func stableIID(imsi string) []byte {
	var sum [16]byte
	h := func(b byte) { sum[15] ^= b; sum[15] = sum[15]<<1 | sum[15]>>7 }
	for i := 0; i < len(imsi); i++ {
		h(imsi[i])
		sum[i%8] ^= imsi[i]
	}
	iid := make([]byte, 8)
	copy(iid, sum[:8])
	iid[0] &^= 0x02 // 清 U/L 位（本地管理位）
	return iid
}

// prefix64Of 取一个全球 v6 地址的 /64 前缀
func prefix64Of(addr string) string {
	ip := net.ParseIP(addr)
	if ip == nil || ip.To4() != nil {
		return ""
	}
	b := ip.To16()
	return net.IP(b[:8]).String()
}

// ensureStableAddr 确保 iface 上有稳定注册地址（prefix(IID from IMSI)）。
// CPE 的 SLAAC 地址是 mngtmpaddr 会轮换, 轮换会触发无谓重注册;
// 注册/SA 绑定全部用这个稳定地址。已存在则幂等跳过。
func ensureStableAddr(slaacAddr, imsi string) (string, error) {
	if slaacAddr == "" {
		return "", fmt.Errorf("无 SLAAC 地址, 无法派生稳定地址")
	}
	iid := stableIID(imsi)
	ip := net.ParseIP(slaacAddr).To16()
	stable := net.IP(append(append([]byte{}, ip[:8]...), iid...)).String()
	ifi, err := net.InterfaceByName(gIface)
	if err != nil {
		return "", err
	}
	addrs, _ := ifi.Addrs()
	for _, a := range addrs {
		if ipn, ok := a.(*net.IPNet); ok && ipn.IP.String() == stable {
			return stable, nil
		}
	}
	if out, err := exec.Command("ip", "-6", "addr", "add", stable+"/64", "dev", gIface).CombinedOutput(); err != nil {
		return "", fmt.Errorf("addr add: %v %s", err, out)
	}
	logf("稳定注册地址: %s (来自 SLAAC %s + 固定 IID)", stable, slaacAddr)
	return stable, nil
}

// ---------- 注册主流程 ----------

func loadConfOrDie(imsi string) {
	conf, err := loadOpConf(imsi)
	if err != nil {
		writeStatus("注册失败", err.Error()+" · 60s 后重试")
		logf("%v", err)
		time.Sleep(60 * time.Second)
		os.Exit(1)
	}
	gOp = conf
	gSmscPSI = conf.SmscPSI
	applyOpRuntime(conf)
	logf("运营商: %s · P-CSCF 候选 %d 个", conf.Name, len(conf.PcscfFallback))
}

func renewInterval(expires int) time.Duration {
	if v := os.Getenv("IMS_RENEW_SEC"); v != "" {
		var n int
		if _, e := fmt.Sscanf(v, "%d", &n); e == nil && n > 0 {
			return time.Duration(n) * time.Second
		}
	}
	d := time.Duration(expires/2) * time.Second
	if d > 25*time.Minute {
		d = 25 * time.Minute
	}
	if d < 60*time.Second {
		d = 60 * time.Second
	}
	return d
}

func backoff(streak int) time.Duration {
	d := time.Duration(streak) * 2 * time.Minute
	if d > 10*time.Minute {
		d = 10 * time.Minute
	}
	return d
}

func daemon() {
	initTimeline()
	logf("imsengine-cpe %s 启动 (iface=%s cid=%d)", version, gIface, cid)
	logf("存活检测参数: 探卡 %v×%d, 静默 %v 后每 %v 探活 ×%d",
		pinProbeSec, maxPinMiss, silenceProbeSec, probeInterval, maxProbeMiss)
	writeStatus("启动", "探测 AT 口")
	at, atPort, err := openAT()
	if err != nil {
		writeStatus("注册失败", "AT 口探测失败: "+err.Error())
		logf("%v", err)
		time.Sleep(60 * time.Second)
		os.Exit(1)
	}
	gAtPort = atPort
	logf("AT 口: %s", atPort)
	go metaRefresh(at)

	imsi := readIMSI(at)
	if imsi == "" {
		writeStatus("未插卡", "未检测到 SIM 卡, 等待插卡 (30s 轮询)")
	}
	gImsi = imsi
	if imsi != "" {
		loadConfOrDie(imsi)
		initSmscPSI(at, imsi) // 短信中心: 卡 AT+CSCA? 优先(归属省按卡取) → 配置 → 缺省
		fzsIdxInit()          // IMS 条目 idx 与 fzs-sms 现有记录错峰
	} else {
		writeStatus("未插卡", "等待 SIM 卡")
	}

	regFailStreak := 0
	registered := false
	// needBearer=false 表示承载还在（只是 SIP 层丢了注册），外层跳过 bearerUp 只重注册。
	// ★ 重建承载是最重的手段：它会换 IPv6 前缀、重写 SA、中断一切，能不用就不用。
	needBearer := true
	sipBreaks := 0
	for {
		// 未插卡 → 原地等卡
		if gImsi == "" {
			time.Sleep(30 * time.Second)
			if v := readIMSI(at); v != "" {
				gImsi = v
				imsi = v
				writeStatus("注册中", "检测到 SIM 卡, 继续")
				loadConfOrDie(imsi)
			} else {
				continue
			}
			needBearer = true
		}
		// 运行中拔卡 → 停注册等卡
		if pinr, _ := at.Cmd("AT+CPIN?", 3000); !cpinReady(pinr) {
			gImsi = ""
			needBearer = true
			writeStatus("未插卡", "检测到 SIM 卡拔出, 等待插卡")
			time.Sleep(30 * time.Second)
			continue
		}

		if needBearer {
			if err := bearerUp(at); err != nil {
				writeStatus("注册失败", err.Error())
				regFailStreak++
				time.Sleep(backoff(regFailStreak))
				continue
			}
		} else {
			logf("承载保持 (仅 SIP 层失效, 不重建 CID%d)", cid)
		}
		writeStatus("注册中", "等主机 SLAAC ("+gIface+")")
		uev6, err := waitSLAAC(gIface, 120*time.Second)
		if err != nil {
			writeStatus("注册失败", err.Error())
			logf("[diag] SLAAC 超时, 承载诊断:")
			diagnoseBearer(at)
			regFailStreak++
			needBearer = true
			time.Sleep(backoff(regFailStreak))
			continue
		}
		logf("SLAAC 地址: %s", uev6)

		stable, err := ensureStableAddr(uev6, gImsi)
		if err != nil {
			writeStatus("注册失败", "稳定地址: "+err.Error())
			regFailStreak++
			needBearer = true
			time.Sleep(backoff(regFailStreak))
			continue
		}

		esp, err := NewESPio(stable, gOp.PcscfFallback[0])
		if err != nil {
			writeStatus("注册失败", "ESP io 初始化失败: "+err.Error())
			regFailStreak++
			needBearer = true
			time.Sleep(backoff(regFailStreak))
			continue
		}
		// keepalive: 防运营商链路 UDP 会话超时（30min 无流量续租必失败根因）
		// kstop: 退出 inner 循环时必须 close —— 否则 goroutine 永存,
		// 每 30s 往已关闭的 socket 上发 → "ESP Send 失败 … closed network connection" 刷屏
		kstop := make(chan struct{})
		go esp.KeepaliveLoop(time.Duration(opKeepaliveMs())*time.Millisecond, kstop)
		reg := NewReg(at, gImsi, stable, gOp.PcscfFallback[0], esp)
		if err := reg.Initial(gOp.PcscfFallback); err != nil {
			writeStatus("注册失败", "注册失败: "+err.Error())
			regFailStreak++
			esp.Close()
			time.Sleep(backoff(regFailStreak))
			continue
		}
		regFailStreak = 0
		needBearer = false
		sipBreaks = 0
		ResetNetRx() // 注册成功, 重置"多久没收到网络侧消息"的基线
		regSince = time.Now()
		if registered {
			// 之前注册过 → 这次是失效后的自动恢复，标注出来便于统计"多久失效一次"
			writeStatus("已注册", "ims 注册成功 (会话自动恢复)")
		} else {
			writeStatus("已注册", "ims 注册成功")
			registered = true
		}

		// ---- 保活 + 收信循环 ----
		renewAt := time.Now().Add(renewInterval(reg.expires))
		nextRenewAt = renewAt
		failCount := 0
		prefix := prefix64Of(stable)
		lastPin := time.Now()
		lastAddrChk := time.Now()
		lastProbe := time.Time{}
		probeMiss := 0
		pinMiss := 0
		reason := brkBearer
	inner:
		for {
			time.Sleep(1 * time.Second)

			// ---- SIM 探卡 ----
			// ★ 2026-10-20 关键修复：原实现是「一次 CPIN? 不返回 READY 就 break」，
			//   而 IMS 会话建立后 AT 口完全空闲，任何一次偶发超时/串扰（AT 口被别的进程
			//   短暂占用、模组返回慢）都会被当成拔卡。实测每 8~15 分钟误触发一次，
			//   每次都把承载推倒重建（换新 IPv6 前缀）→ 地址越堆越多 → 最终再也注册不上。
			//   这里改为：① 明确报 SIM 不在的错误码才立刻判拔卡
			//            ② 其余异常必须连续多次才判 AT 口失联
			if time.Since(lastPin) > pinProbeSec {
				lastPin = time.Now()
				pinr, perr := at.Cmd("AT+CPIN?", 3000)
				switch {
				case cpinReady(pinr):
					if pinMiss > 0 {
						logf("探卡恢复正常 (此前连续异常 %d 次, 注册未受影响)", pinMiss)
					}
					pinMiss = 0
				case isSimGone(pinr, perr):
					logf("SIM 卡已拔出 (模组明确报错), 停止注册等待插卡")
					reason = brkSIM
					break inner
				default:
					pinMiss++
					logf("探卡异常 %d/%d (AT 口忙或超时, 不影响已建立的 IMS 会话): %v",
						pinMiss, maxPinMiss, pinr)
					if pinMiss >= maxPinMiss {
						logf("探卡连续异常 %d 次 → 判定 AT 口失联, 重建会话", pinMiss)
						reason = brkSIM
						break inner
					}
				}
			}

			// 网页下发的 IMS 发送请求（LuCI→fzs-at sms send ims→init.d submit→/tmp/ims-sms）
			smsReqPoll(reg)
			// 收包（MT 短信 / 送达报告 → HandleEvent）
			for {
				ev := esp.WaitEvent(0)
				if ev == nil {
					break
				}
				reg.HandleEvent(ev)
			}

			// ---- 注册地址 / 前缀体检（30s 一次, ip 命令不宜每秒跑）----
			if time.Since(lastAddrChk) > 30*time.Second {
				lastAddrChk = time.Now()
				if !hasAddrIn(gIface, stable) {
					logf("注册地址 %s 已从接口消失, 需重建承载", stable)
					reason = brkBearer
					break inner
				}
				// 承载被网络侧重建会换前缀 → 稳定地址失效, 整个重来
				if cur := currentGlobal(gIface); cur != "" && prefix64Of(cur) != prefix {
					logf("IMS 前缀变化 %s → %s, 需重建承载后重注册", prefix, prefix64Of(cur))
					reason = brkBearer
					break inner
				}
			}

			// ---- 注册存活探测：真正的"防止注册悄悄过期" ----
			// 网络侧完全可能在租期未到时就回收 SA / 清掉 UDP 会话：此时本地仍显示"已注册"，
			// 但发出去的短信全部石沉大海 —— 这正是用户观察到的"过一会儿就收发不了"。
			// 判据不用墙钟，用「多久没收到 P-CSCF 的任何东西」（SinceNetRx），这也是
			// 运营商侧是否还认我们这条 SA 的唯一客观信号。
			if d := SinceNetRx(); d >= silenceProbeSec {
				if time.Since(lastProbe) >= probeInterval {
					lastProbe = time.Now()
					probeMiss++
					reg.SendOptions()
					logf("已 %ds 未收到网络侧任何报文 → 第 %d/%d 次 OPTIONS 探活",
						int(d.Seconds()), probeMiss, maxProbeMiss)
					if probeMiss >= maxProbeMiss {
						logf("OPTIONS 探活连续 %d 次无回应 ⇒ 注册已失效, 立即重新注册", probeMiss)
						writeStatus("注册中", "检测到注册失效, 正在重新注册")
						if err := reg.Renew(); err != nil {
							logf("探活后续租失败: %v → 改走全量重注册", err)
							if e2 := reg.Initial(gOp.PcscfFallback); e2 != nil {
								logf("探活后全量重注册也失败: %v", e2)
								reason = brkSIP
								break inner
							}
						}
						logf("★ 注册已自动恢复")
						ResetNetRx()
						probeMiss = 0
						failCount = 0
						renewAt = time.Now().Add(renewInterval(reg.expires))
						nextRenewAt = renewAt
						writeStatus("已注册", "ims 注册成功 (自动恢复)")
					}
				}
			} else {
				probeMiss = 0 // 有入站流量 ⇒ 链路活着
			}

			// 续租 / 临近 SA 死线(65min)主动全量重注册
			if time.Now().After(renewAt) {
				if time.Since(reg.regAt) >= 65*time.Minute {
					logf("距上次全量注册 %d 分钟(临近 SA 死线), 主动全量重注册",
						int(time.Since(reg.regAt).Minutes()))
					if err := reg.Initial(gOp.PcscfFallback); err != nil {
						failCount++
						logf("主动全量重注册失败: %v", err)
					} else {
						failCount = 0
						ResetNetRx()
					}
				} else if err := reg.Renew(); err != nil {
					failCount++
					logf("定期续租失败(%d): %v → 改走完整重注册", failCount, err)
					if e2 := reg.Initial(gOp.PcscfFallback); e2 != nil {
						logf("完整重注册也失败: %v", e2)
						if failCount >= 2 {
							reason = brkSIP
							break inner
						}
					} else {
						failCount = 0
						ResetNetRx()
					}
				} else {
					failCount = 0
					ResetNetRx()
				}
				renewAt = time.Now().Add(renewInterval(reg.expires))
				nextRenewAt = renewAt
				writeStatus("已注册", "保活 "+time.Now().Format("15:04:05"))
			}
		}
		close(kstop)
		esp.Close()

		// ---- 退出 inner 后的分级处置 ----
		writeStatus("注册中", "重建立 IMS 会话")
		if reason == brkSIP {
			sipBreaks++
			if sipBreaks >= 3 {
				logf("SIP 层连续失败 %d 次 → 升级为重建承载", sipBreaks)
				needBearer = true
				sipBreaks = 0
			} else {
				needBearer = false // 承载还在, 别动它
				time.Sleep(15 * time.Second)
			}
		} else {
			sipBreaks = 0
			needBearer = true
		}
		if reason == brkSIM {
			gImsi = "" // 让外层进入"等卡"分支
		}
	}
}

// ensureSingleInstance 单实例文件锁。
// ★ 2026-10-20 血的教训：误执行 `imsengine -h`（那时 -h 会被当成普通参数进 daemon）
//   拉起了第二个实例。两个实例共享同一个 AT 口和同一个 IPv6/raw socket：
//     · 互相收到对方的 ESP 包 → "HMAC 校验失败(两把 key)" 刷屏
//     · 一个刚 ensureStableAddr 的地址被另一个 rebuild 后 flush 掉 → 反复重注册
//     · AT 口争抢 → 探卡随机失败 → 又触发重建
//   用 flock 从根上杜绝：第二个实例直接拒绝启动。
func ensureSingleInstance() error {
	f, err := os.OpenFile("/var/run/imsengine.pid", os.O_CREATE|os.O_RDWR, 0644)
	if err != nil {
		return nil // /var/run 不可写则放弃加锁, 不阻止启动
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		old, _ := os.ReadFile("/var/run/imsengine.pid")
		f.Close()
		return fmt.Errorf("已有 imsengine 实例在运行 (pid=%s)", strings.TrimSpace(string(old)))
	}
	// 不 Close：锁需持有到进程退出。f 逃逸到包级变量防止被 GC 关闭 fd
	pidFile = f
	fmt.Fprintf(f, "%d\n", os.Getpid())
	return nil
}

var pidFile *os.File

const imsUsage = `imsengine —— OpenWrt CPE 用户态 IMS 引擎 (SMS over IMS)

用法:
  imsengine                  以守护模式运行（正常用法，由 /etc/init.d/ims 拉起）
  imsengine learn [端口]     P-CSCF 自学习（借 200N 原生 IMS 给 200T 取地址）
  imsengine smstest <号> <文> 桥接自验：直接往 fzs-sms 收件箱写一条并转发
  imsengine -h | --help      显示本帮助
  imsengine -v | --version   显示版本

环境变量（引擎本体）:
  IMS_IFACE    承载网卡       默认 rndis-modem1
  IMS_CID      IMS 承载 CID   默认 3
  IMS_AT_PORT  AT 口          默认自动探测
  IMS_RENEW_SEC              覆盖租期一半算出的续租间隔（秒）

环境变量（注册存活检测调参，不必重编译）:
  IMS_PIN_PROBE_SEC  探卡周期(秒)        默认 20
  IMS_PIN_MAX_MISS   连续异常几次判失联  默认 6
  IMS_SILENCE_SEC    静默多久开始探活    默认 180
  IMS_PROBE_SEC      探活包间隔          默认 20
  IMS_PROBE_MAX      连续几次无回应重注册 默认 3
`

func main() {
	// -h/--help / -v/--version：必须先于任何分支处理。
	// ★ 曾经因为没有这两个分支，`imsengine -h` 直接落进 daemon() 把守护拉起来，
	//   排查脚本里的一次手误就制造了双实例。
	if len(os.Args) >= 2 {
		switch os.Args[1] {
		case "-h", "--help", "help":
			fmt.Print(imsUsage)
			return
		case "-v", "--version", "version":
			fmt.Printf("imsengine %s\n", version)
			return
		}
	}
	// 桥接自验入口（不启动引擎，直接走一遍 fzs-sms 收件箱入库+转发队列）:
	//   imsengine smstest 10000 "测试正文"
	if len(os.Args) == 4 && os.Args[1] == "smstest" {
		smsTestCmd(os.Args[2], os.Args[3])
		return
	}
	// P-CSCF 自学习入口（借 200N 原生 IMS 拿地址，给 200T 用）:
	//   imsengine learn [/dev/ttyUSB4]
	// 一次性命令，不启动守护；结果以一行 JSON 打到 stdout。
	if len(os.Args) >= 2 && os.Args[1] == "learn" {
		port := ""
		if len(os.Args) >= 3 {
			port = os.Args[2]
		}
		os.Exit(learnCmd(port))
	}
	if err := ensureSingleInstance(); err != nil {
		logf("启动中止: %v", err)
		fmt.Fprintf(os.Stderr, "imsengine: %v\n", err)
		os.Exit(3)
	}
	daemon()
}
