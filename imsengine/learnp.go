package main

// learnp.go - P-CSCF 自学习（借 200N 的原生 IMS 能力给 200T 供地址） 2026-10-20
//
// 背景: 200T (EC200T) 的固件不会向网络申请 P-CSCF 下发, 也没有 AT 能读出;
//       200N (EC200N) 固件自带 IMS 协议栈 (AT+QCFG="ims" 可用)。
//       ⇒ 把 SIM 卡临时插到 200N, 让它跑一次原生 IMS, 从
//         AT+CGCONTRDP=<cid> 的第 8/9 字段 (P_CSCF_prim / P_CSCF_sec) 读出地址,
//         按"同卡精确 + 运营商前缀兜底"落盘 /etc/pcscf_learned.json,
//         卡插回 200T 后 imsengine 启动时优先使用。
//
// 用法: imsengine learn [AT口]        # 省略则自动找 200N 的口
// 输出: 一行 JSON (给 init.d/ims → fzs-at → LuCI 面板消费)

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"go.bug.st/serial"
)

// learnPeerModel 学习源模组的 ATI 特征串（200N）
const learnPeerModel = "EC200N"

// learnFindPorts 列出候选 AT 口（/dev/ttyUSB*），已按名称排序
func learnFindPorts() []string {
	m, _ := filepath.Glob("/dev/ttyUSB*")
	sort.Strings(m)
	return m
}

// learnOccupied 返回被其它进程占用 / 引擎正在用的口，学习必须避开它们
// （踩过的坑: pppd 把口带进数据模式后 AT 命令全部打不进去 → 误判"未插卡"）
func learnOccupied() map[string]bool {
	occ := map[string]bool{}
	for p := range busyPorts() { // busyPorts() 是 map[string]bool, 遍历取 key
		occ[p] = true
	}
	if ep := enginePortFromStatus(); ep != "" {
		occ[ep] = true
	}
	return occ
}

// enginePortFromStatus 从 /tmp/ims_status.txt 读引擎当前 AT 口
func enginePortFromStatus() string {
	b, err := os.ReadFile("/tmp/ims_status.txt")
	if err != nil {
		return ""
	}
	for _, ln := range strings.Split(string(b), "\n") {
		ln = strings.TrimSpace(ln)
		if strings.HasPrefix(ln, "ENGINE_PORT=") {
			return strings.Trim(strings.TrimPrefix(ln, "ENGINE_PORT="), "'\" ")
		}
	}
	return ""
}

// learnProbe 对单个口问型号，返回 (型号串, 是否AT口可用)
func learnProbe(dev string) (string, bool) {
	mode := &serial.Mode{BaudRate: 115200, DataBits: 8, Parity: serial.NoParity, StopBits: serial.OneStopBit}
	p, err := serial.Open(dev, mode)
	if err != nil {
		return "", false
	}
	defer p.Close()
	_ = p.SetReadTimeout(300 * time.Millisecond)
	p.ResetInputBuffer()
	if _, err := p.Write([]byte("ATI\r")); err != nil {
		return "", false
	}
	buf := make([]byte, 512)
	acc := ""
	deadline := time.Now().Add(2500 * time.Millisecond)
	for time.Now().Before(deadline) {
		n, _ := p.Read(buf)
		if n > 0 {
			acc += string(buf[:n])
			if strings.Contains(acc, "\r\nOK") || strings.Contains(acc, "OK\r\n") {
				break
			}
		}
	}
	if !strings.Contains(acc, "OK") {
		return "", false
	}
	model := ""
	for _, ln := range strings.Split(acc, "\n") {
		ln = strings.TrimSpace(ln)
		if strings.Contains(ln, "EC200") || strings.Contains(ln, "EC20") || strings.Contains(ln, "Revision") {
			if model == "" || strings.Contains(ln, "Revision") {
				model = strings.TrimSpace(strings.TrimPrefix(ln, "Revision:"))
			}
		}
	}
	return model, true
}

// ---- AT+CGCONTRDP 解析 ----

// splitCSVLine 按逗号切分，保护双引号内的逗号（APN 可能带）
func splitCSVLine(s string) []string {
	out := []string{}
	cur := ""
	inQ := false
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '"':
			inQ = !inQ
		case ',':
			if inQ {
				cur += ","
			} else {
				out = append(out, cur)
				cur = ""
			}
		default:
			cur += string(s[i])
		}
	}
	out = append(out, cur)
	return out
}

// normAddr 把模组地址字段统一成标准字符串:
//   "36.14.0.248.240.5.0.0.0.0.0.0.0.0.4" (16 段十进制字节) → 240e:f8:f005::4
//   已是冒号格式 / IPv4 点分四段 → 原样返回
func normAddr(s string) string {
	s = strings.Trim(strings.TrimSpace(s), "\"' ")
	if s == "" {
		return ""
	}
	parts := strings.Split(s, ".")
	if len(parts) == 16 {
		b := make([]byte, 0, 16)
		for _, p := range parts {
			n, err := strconv.Atoi(strings.TrimSpace(p))
			if err != nil || n < 0 || n > 255 {
				return ""
			}
			b = append(b, byte(n))
		}
		return net.IP(b).String()
	}
	return s
}

// looksGlobalV6 粗筛: 像是可用的全局 IPv6 地址（排除链路本地/未指定/组播）
func looksGlobalV6(s string) bool {
	ip := net.ParseIP(s)
	if ip == nil || ip.To4() != nil {
		return false // 只要 IPv6: IMS 走 v6
	}
	return !ip.IsLinkLocalUnicast() && !ip.IsUnspecified() && !ip.IsMulticast() && !ip.IsLoopback()
}

// parseContrdp 从 +CGCONTRDP: 行里取 (cid, P-CSCF 列表)
//   TS 27.007 字段序: cid,bearer,apn,local,gw,dns1,dns2,pcscf_prim,pcscf_sec,...
//   → P-CSCF 在下标 7/8。不同固件字段数会有出入, 故再做一次全字段兜底扫描。
func parseContrdp(line string) (string, []string) {
	i := strings.Index(line, "+CGCONTRDP:")
	if i < 0 {
		return "", nil
	}
	f := splitCSVLine(strings.TrimSpace(line[i+len("+CGCONTRDP:"):]))
	if len(f) == 0 {
		return "", nil
	}
	cid := strings.Trim(f[0], "\" ")
	out := []string{}
	seen := map[string]bool{}
	add := func(s string) {
		v := normAddr(s)
		if v == "" || seen[v] {
			return
		}
		seen[v] = true
		out = append(out, v)
	}
	if len(f) > 7 {
		add(f[7]) // P_CSCF_primary
	}
	if len(f) > 8 {
		add(f[8]) // P_CSCF_secondary
	}
	// 兜底: 仅当上面两个位都没给出地址时（不同固件字段数有出入），
	// 才扫描其余字段里像全局 v6 的地址。
	// ⚠ 必须跳过 5/6 —— 那是 DNS1/DNS2，实测电信的 DNS 是 240e:1f:1::1/::33，
	//    同样是全局 v6，不加区分会被当成 P-CSCF 收进来（第一版就踩了这个坑）。
	if len(out) == 0 {
		for idx, v := range f {
			if idx <= 6 || idx == 7 || idx == 8 {
				continue
			}
			if a := normAddr(v); looksGlobalV6(a) {
				add(v)
			}
		}
	}
	return cid, out
}

// ---- 主流程 ----

type learnOut struct {
	Ok      bool     `json:"ok"`
	Err     string   `json:"err,omitempty"`
	Port    string   `json:"port,omitempty"`
	Model   string   `json:"model,omitempty"`
	IMSI    string   `json:"imsi,omitempty"`
	ICCID   string   `json:"iccid,omitempty"`
	CID     string   `json:"cid,omitempty"`
	Pcscf   []string `json:"pcscf,omitempty"`
	Learned []string `json:"learned,omitempty"`
	Hint    string   `json:"hint,omitempty"`
}

func learnCmd(portArg string) int {
	o := learnOut{}
	defer func() {
		b, _ := json.Marshal(o)
		fmt.Println(string(b))
		logf("[learn] 结果: %s", string(b))
	}()

	// ① 选定学习源 AT 口
	port := strings.TrimSpace(portArg)
	if port != "" {
		o.Port = port
	} else {
		occ := learnOccupied()
		found := ""
		for _, d := range learnFindPorts() {
			if occ[d] {
				continue
			}
			model, ok := learnProbe(d)
			if ok && strings.Contains(model, learnPeerModel) {
				found = d
				o.Model = model
				break
			}
		}
		if found == "" {
			o.Err = "未找到可用的 200N (EC200N) AT 口（可能被占用或无此模组）"
			return 1
		}
		port = found
		o.Port = port
	}

	a, err := NewAT(port)
	if err != nil {
		o.Err = "打开 AT 口失败: " + err.Error()
		return 1
	}
	defer a.Close()

	if o.Model == "" {
		if r, _ := a.Cmd("ATI", 3000); len(r) > 0 {
			o.Model = strings.Join(r, " ")
		}
	}

	// ② SIM 必须就绪
	cpin := ""
	if r, _ := a.Cmd("AT+CPIN?", 4000); len(r) > 0 {
		cpin = strings.Join(r, " ")
	}
	if !strings.Contains(cpin, "READY") {
		o.Err = "学习源模组未识别到 SIM 卡（请把卡插入该模组）: " + cpin
		return 1
	}

	// ③ 读卡标识
	if r, _ := a.Cmd("AT+CIMI", 4000); len(r) > 0 {
		for _, ln := range r {
			t := strings.TrimSpace(ln)
			if len(t) >= 15 && t[0] >= '0' && t[0] <= '9' {
				o.IMSI = t
				break
			}
		}
	}
	if r, _ := a.Cmd("AT+QCCID", 4000); len(r) > 0 {
		for _, ln := range r {
			if i := strings.Index(ln, "+QCCID:"); i >= 0 {
				o.ICCID = strings.Trim(strings.TrimSpace(ln[i+7:]), "\" ")
				break
			}
		}
	}
	if o.IMSI == "" {
		o.Err = "读不到 IMSI，无法按卡记录学习值"
		return 1
	}

	// ④ 开原生 IMS 并等它激活
	a.Cmd(`AT+QCFG="ims",1`, 4000)
	activated := false
	for i := 0; i < 20; i++ {
		r, _ := a.Cmd(`AT+QCFG="ims"`, 2500)
		joined := strings.Join(r, " ")
		if strings.Contains(strings.ReplaceAll(joined, " ", ""), `"ims",1,1`) {
			activated = true
			break
		}
		time.Sleep(3 * time.Second)
	}
	if !activated {
		o.Err = "200N 的原生 IMS 未激活（可能该卡未开通 VoLTE，或当前网络不支持）"
		return 1
	}

	// ⑤ 找 IMS 专用承载的 cid（APN 名含 ims，优先 IPV4V6/IPV6）
	cid := ""
	fallbackCid := ""
	if r, _ := a.Cmd("AT+CGDCONT?", 5000); len(r) > 0 {
		for _, ln := range r {
			i := strings.Index(ln, "+CGDCONT:")
			if i < 0 {
				continue
			}
			f := splitCSVLine(strings.TrimSpace(ln[i+9:]))
			if len(f) < 3 {
				continue
			}
			c := strings.Trim(f[0], "\" ")
			pdp := strings.ToUpper(strings.Trim(f[1], "\" "))
			apn := strings.ToUpper(strings.Trim(f[2], "\" "))
			if !strings.Contains(apn, "IMS") {
				continue
			}
			if fallbackCid == "" {
				fallbackCid = c
			}
			if strings.Contains(pdp, "V6") || strings.Contains(pdp, "V4V6") {
				cid = c
			}
		}
	}
	if cid == "" {
		cid = fallbackCid
	}
	if cid == "" {
		o.Err = "200N 未配置 IMS 专用承载（CGDCONT 里找不到 ims APN）"
		return 1
	}
	o.CID = cid

	// ⑥ 读 P-CSCF（未激活则先激活该承载）
	var pcscf []string
	for attempt := 0; attempt < 2; attempt++ {
		r, err := a.Cmd("AT+CGCONTRDP="+cid, 6000)
		if err != nil && len(r) == 0 {
			// 承载未激活 → 试着激活一次再读
			a.Cmd("AT+QIACT="+cid, 15000)
			time.Sleep(3 * time.Second)
			continue
		}
		for _, ln := range r {
			if !strings.Contains(ln, "+CGCONTRDP:") {
				continue
			}
			if _, got := parseContrdp(ln); len(got) > 0 {
				pcscf = append(pcscf, got...)
			}
		}
		if len(pcscf) > 0 {
			break
		}
		// 该 cid 没给 → 全量读一次
		if r2, err2 := a.Cmd("AT+CGCONTRDP", 6000); err2 == nil || len(r2) > 0 {
			for _, ln := range r2 {
				if !strings.Contains(ln, "+CGCONTRDP:") {
					continue
				}
				if _, got := parseContrdp(ln); len(got) > 0 {
					pcscf = append(pcscf, got...)
				}
			}
		}
		break
	}
	if len(pcscf) == 0 {
		o.Err = "200N 已激活 IMS，但 AT+CGCONTRDP 未返回 P-CSCF（该固件可能不下发）"
		return 1
	}
	o.Pcscf = pcscf

	// ⑦ 落盘：同卡精确 + 运营商前缀兜底
	learned := learnPcscfExact(o.IMSI, o.ICCID, pcscf)
	o.Learned = learned
	o.Ok = true
	o.Hint = "已保存。请把卡插回模组1(200T)，重启 ims 服务后即可用该 P-CSCF 注册。"
	return 0
}
