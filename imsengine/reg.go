package main

// reg.go - IMS 注册态机（v0.4: 主机 socket + 用户态 ESP）
//
// 契约来源: uvo_ims.py（U3 实证）+ 2026-09-27 FZS 实测（hims 工具全链 200 OK）
//   ① 明文 REGISTER#1 → [P-CSCF]:5060（本机 5060）→ 401
//   ② AKA: CSIM 008800812210<RAND>10<AUTN> → DB<len><RES> <len><CK> <len><IK> <len><Kc> 9000
//      （SQN 失步回 DC<len><AUTS> → 明文重同步 REGISTER 带 auts=base64 → 新 nonce 重来）
//   ③ Digest AKAv1-MD5: password = RES 二进制
//   ④ 受保护 REGISTER#2: 本机 port-c(2468) → P-CSCF port-s(9900)，Via/Contact 写 port-s(1357)
//      Security-Verify 必须完整镜像 Security-Server
//   ⑤ 续租（ZTE 契约, uvo_ims 9/19 定型）: 无 Sec 头 REGISTER → 401 → 就地 AKA → 同 SA 补发 → 200 OK

import (
	"crypto/md5"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"math/rand"
	"net"
	"strings"
	"time"
)

// plainPort 明文阶段【我方源端口】固定 5060（规范 02）; 目的端口走 opPcscfPort()（配置 pcscf_port, 缺省 5060）
const plainPort = 5060

// ── v0.9.9m: 首个 REGISTER 的 sec-agree 能力声明改为按运营商配置 ──
// 默认值与原硬编码完全一致（电信/联通路径零变化）；移动等对"算法"有要求的运营商可覆盖。
const (
	secAlgDefault  = "hmac-sha-1-96"
	secEalgDefault = "null"
	// 默认值 = 电信/联通现网已实测生效值；配置未提供对应键时用它，保证老运营商零回归
	uaDefault          = "imsengine-v0.4"
	contactTagsDefault = ";+g.3gpp.accesstype=\"3GPP-E-UTRAN-FDD\";+g.3gpp.smsip;+g.3gpp.icsi-ref=\"urn%3Aurn-7%3A3gpp-service.ims.icsi.mmtel\""
	expiresDefault     = 600000
	serverPortDefault  = 9900
)

// ★ v0.9.9n: 原先写死在 reg.go 常量里，按 engine-spec/07-运营商配置规范.md 改为配置驱动
var (
	ueSpiC uint32 = 23456789
	ueSpiS uint32 = 12345678
	uePortC       = 2468 // 我方受保护客户端口（发送源）
	uePortS       = 1357 // 我方受保护服务端口（收 PC 主动请求, Via/Contact 写这个）
)

// applyOpRuntime 把运营商配置里的 IPsec/端口落到运行时（loadOpConf 成功后调用）。
// 未提供的键保持默认（= 原硬编码值），因此老运营商行为不变。
func applyOpRuntime(c *OpConf) {
	if c == nil {
		return
	}
	if c.Ipsec.SpiC != 0 {
		ueSpiC = c.Ipsec.SpiC
	}
	if c.Ipsec.SpiS != 0 {
		ueSpiS = c.Ipsec.SpiS
	}
	if c.Ipsec.UePortTx != 0 {
		uePortC = c.Ipsec.UePortTx
	}
	if c.Ipsec.UePortRx != 0 {
		uePortS = c.Ipsec.UePortRx
	}
	if c.Ipsec.RandomSpi {
		// SimAdmin 同款: 随机 SPI/端口（范围 port_c_lo~port_c_hi, 缺省 40000~60000）
		ueSpiC = 0x1000000 + uint32(rand.Intn(0xF000000))
		ueSpiS = 0x1000000 + uint32(rand.Intn(0xF000000))
		lo, hi := c.Ipsec.PortCLo, c.Ipsec.PortCHi
		if lo <= 0 || hi <= lo {
			lo, hi = 40000, 60000
		}
		uePortC = lo + rand.Intn(hi-lo)
		uePortS = lo + rand.Intn(hi-lo)
		if uePortS == uePortC {
			uePortS++
		}
		logf("[ipsec] random_spi=true → spi-c=%d spi-s=%d port-c=%d port-s=%d", ueSpiC, ueSpiS, uePortC, uePortS)
	}
}

func opExpires() int {
	if gOp != nil && gOp.Registration.Expires > 0 {
		return gOp.Registration.Expires
	}
	return expiresDefault
}

// opPcscfPort 明文 REGISTER 目的端口（配置 pcscf_port; 0 = 5060, 引擎实测验证值）。
// ★ v0.9.9t: 原先配置里躺着但代码写死 5060 —— 接上; 电信/移动/联通配置值均为 5060, 行为零变化。
func opPcscfPort() int {
	if gOp != nil && gOp.PcscfPort > 0 {
		return gOp.PcscfPort
	}
	return plainPort
}

// opDomain IMS 域（配置 domain 优先; 空 = 按 IMSI 推导, 与原行为逐字节一致）。
// ★ v0.9.9t: OpConf.Domain 原是死字段, NewReg 一直 deriveRealm 硬推导 —— 接上。
func opDomain(imsi string) string {
	if gOp != nil {
		if d := strings.TrimSpace(gOp.Domain); d != "" {
			return d
		}
	}
	return deriveRealm(imsi)
}

// opKeepaliveMs OPTIONS/ESP 保活间隔（配置 keepalive.options_interval_ms; 0 = 60000 与原写死一致）
func opKeepaliveMs() int {
	if gOp != nil && gOp.Keepalive.OptionsIntervalMs > 0 {
		return gOp.Keepalive.OptionsIntervalMs
	}
	return 60000
}

func opContactExpires() int {
	if gOp != nil {
		return gOp.Registration.ContactExpires
	}
	return 0
}

// contactExpiresParam Contact 头里的 ;expires=<n>（SimAdmin 的 VoLTE 栈带这个参数）
func contactExpiresParam() string {
	if v := opContactExpires(); v > 0 {
		return fmt.Sprintf(";expires=%d", v)
	}
	return ""
}

func opServerPort() int {
	if gOp != nil && gOp.Ipsec.ServerPort != 0 {
		return gOp.Ipsec.ServerPort
	}
	return serverPortDefault
}

func opUA() string {
	if gOp != nil && gOp.Headers.UserAgent != "" {
		return gOp.Headers.UserAgent
	}
	return uaDefault
}

func opContactTags() string {
	if gOp != nil && gOp.Headers.ContactTags != "" {
		return gOp.Headers.ContactTags
	}
	return contactTagsDefault
}

func opSendSupported() string {
	if gOp != nil {
		return strings.TrimSpace(gOp.Headers.SendSupported)
	}
	return ""
}

func opPanI() string {
	if gOp != nil {
		return strings.TrimSpace(gOp.Headers.PanI)
	}
	return ""
}

// opPPrefIdentity P-Preferred-Identity 的值；"auto" = <sip:IMPI>
func opPPrefIdentity(impi string) string {
	if gOp == nil {
		return ""
	}
	v := strings.TrimSpace(gOp.Headers.PPreferredIdentity)
	if v == "" {
		return ""
	}
	if strings.EqualFold(v, "auto") {
		return "<sip:" + impi + ">"
	}
	return v
}

func opSecAlg() string {
	if gOp != nil && strings.TrimSpace(gOp.Ipsec.Alg) != "" {
		return gOp.Ipsec.Alg
	}
	return secAlgDefault
}

func opSecEalg() string {
	if gOp != nil && strings.TrimSpace(gOp.Ipsec.Ealg) != "" {
		return gOp.Ipsec.Ealg
	}
	return secEalgDefault
}

// opSecAgree 首个 REGISTER 是否声明/要求 Sec-Agree（缺省 true，保持历史行为）
func opSecAgree() bool {
	if gOp != nil && gOp.Ipsec.SecAgree != nil {
		return *gOp.Ipsec.SecAgree
	}
	return true
}

// secClientValue 依配置生成 Security-Client。
//
// ★ v0.9.9m: alg / ealg 都可用逗号分隔；两者都多个时做**笛卡尔积**。
//   3GPP/CM-IMS 要求 UE 在 Security-Client 里一次声明全部支持的完整性×加密组合
//   （HMAC-MD5-96/HMAC-SHA-1-96 × DES-EDE3-CBC/AES-CBC），只声明一个组合会被
//   移动回 403 "Terminal has used different algorithm from initial register"
//   或 421 "Invalid security-client header and IPsecAKA is force on !"。
//   ipsec.sec_client 非空时直接当作原始模板（支持 {spi_c}/{spi_s}/{port_c}/{port_s} 占位符），
//   便于以后不重编译就试验各种组合。
func secClientValue(spiC, spiS uint32, portC, portS int) string {
	raw := ""
	if gOp != nil {
		raw = strings.TrimSpace(gOp.Headers.SecClient)
		if raw == "" {
			raw = strings.TrimSpace(gOp.Ipsec.SecClient)
		}
	}
	if raw != "" {
		r := strings.NewReplacer("{spi_c}", fmt.Sprint(spiC), "{spi_s}", fmt.Sprint(spiS),
			"{port_c}", fmt.Sprint(portC), "{port_s}", fmt.Sprint(portS))
		return r.Replace(raw)
	}
	algs, ealgs := splitCSV(opSecAlg()), splitCSV(opSecEalg())
	var parts []string
	for _, a := range algs {
		for _, e := range ealgs {
			parts = append(parts, fmt.Sprintf(
				"ipsec-3gpp;alg=%s;prot=esp;mod=trans;ealg=%s;spi-c=%d;spi-s=%d;port-c=%d;port-s=%d",
				a, e, spiC, spiS, portC, portS))
		}
	}
	return strings.Join(parts, ",")
}

func splitCSV(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		out = []string{""}
	}
	return out
}

func md5hex(s string) string { h := md5.Sum([]byte(s)); return hex.EncodeToString(h[:]) }

func sipHeader(msg, name string) string {
	for _, ln := range strings.Split(msg, "\r\n") {
		i := strings.Index(ln, ":")
		if i > 0 && strings.EqualFold(strings.TrimSpace(ln[:i]), name) {
			return strings.TrimSpace(ln[i+1:])
		}
	}
	return ""
}

// sipParam 取头字段参数（值兼容 "," 与 ";" 分隔、带引号）
func sipParam(h, key string) string {
	low := strings.ToLower(h)
	k := strings.ToLower(key) + "="
	for i := 0; ; {
		j := strings.Index(low[i:], k)
		if j < 0 {
			return ""
		}
		j += i
		if j == 0 || low[j-1] == ',' || low[j-1] == ';' || low[j-1] == ' ' {
			rest := h[j+len(k):]
			end := strings.IndexAny(rest, ",;")
			v := rest
			if end >= 0 {
				v = rest[:end]
			}
			return strings.Trim(strings.TrimSpace(v), "\"")
		}
		i = j + 1
	}
}

// akav1Md5 AKAv1-MD5 摘要（uvo_ims.py 同款; password=RES 二进制字节）
func akav1Md5(impi, realm string, resBytes []byte, nonce, qop, uri, cnonce string) string {
	ha1 := md5hex(impi + ":" + realm + ":" + string(resBytes))
	ha2 := md5hex("REGISTER:" + uri)
	return md5hex(ha1 + ":" + nonce + ":00000001:" + cnonce + ":" + qop + ":" + ha2)
}

// parseAkaRaw 解析 CSIM AUTHENTICATE 响应
// 成功: DB <len><RES> <len><CK> <len><IK> <len><Kc> 9000 （★ CK/IK/Kc 前只有长度无 tag）
// 失步: DC <len><AUTS> 9000
func parseAkaRaw(hexStr string) (res, ck, ik, auts string, err error) {
	hexStr = strings.ToLower(hexStr)
	if !strings.HasSuffix(hexStr, "9000") {
		tail := hexStr
		if len(tail) > 8 {
			tail = tail[len(tail)-8:]
		}
		return "", "", "", "", fmt.Errorf("SW 异常: ...%s", tail)
	}
	hb := func(s string) byte { var v byte; fmt.Sscanf(s, "%02x", &v); return v }
	switch {
	case strings.HasPrefix(hexStr, "dc"):
		ln := int(hb(hexStr[2:4]))
		if 4+ln*2 > len(hexStr) {
			return "", "", "", "", fmt.Errorf("AUTS 长度异常")
		}
		return "", "", "", hexStr[4 : 4+ln*2], nil
	case strings.HasPrefix(hexStr, "db"):
		resLen := int(hb(hexStr[2:4]))
		pos := 4
		if 4+resLen*2 > len(hexStr)-4 {
			return "", "", "", "", fmt.Errorf("RES 长度异常")
		}
		res = hexStr[pos : pos+resLen*2]
		pos += resLen * 2
		for pos+2 <= len(hexStr)-4 {
			ln := int(hb(hexStr[pos : pos+2]))
			pos += 2
			if ln == 0 || pos+ln*2 > len(hexStr) {
				break
			}
			val := hexStr[pos : pos+ln*2]
			pos += ln * 2
			switch {
			case ck == "":
				ck = val
			case ik == "":
				ik = val
			}
		}
		return res, ck, ik, "", nil
	}
	return "", "", "", "", fmt.Errorf("首 tag 非 DB/DC: %s", hexStr[:2])
}

// ---------- Reg 注册态机 ----------

type Reg struct {
	at *AT

	imsi, domain, impi, impu, uri string
	uev6, pcscf             string

	pcSpiS  uint32
	pcPortS int

	callID, fromTag, cnonce string
	cseq                    int

	esp *ESPio
	udp *net.UDPConn

	serviceRoute string
	associated   []string
	expires      int // 本轮注册的有效期(秒)

	regAt time.Time
}

// readMSISDN AT+CNUM 读本机号码（不少卡没写, 读不到返回空）
func readMSISDN(at *AT) string {
	ls, _ := at.Cmd("AT+CNUM", 6000)
	for _, ln := range ls {
		if !strings.Contains(ln, "+CNUM") {
			continue
		}
		for _, q := range strings.Split(ln, "\"") {
			q = strings.TrimSpace(q)
			if strings.HasPrefix(q, "+") && len(q) >= 8 {
				return q
			}
		}
	}
	return ""
}

func NewReg(at *AT, imsi, uev6, pcscf string, esp *ESPio) *Reg {
	domain := opDomain(imsi) // ★ v0.9.9t: 配置 domain 优先（缺省推导 = 原行为）
	// ★ v0.9.9o: IMPU 制式按配置（impu_style=msisdn → 号码制; 规范 02/A10: 业务 From 必须号码制）
	// ★ v0.9.9p: impu 不带 scheme（buildRegister 统一包 <sip:...>，否则出现 sip:sip: 双前缀 → 移动回 "Invalid User"）
	impu := imsi + "@" + domain
	if gOp != nil && strings.EqualFold(strings.TrimSpace(gOp.ImpuStyle), "msisdn") {
		if ms := readMSISDN(at); ms != "" {
			impu = strings.TrimPrefix(strings.TrimSpace(ms), "sip:") + "@" + domain
			logf("身份 IMPU(impu_style=msisdn): sip:%s (MSISDN=%s)", impu, ms)
		} else {
			logf("impu_style=msisdn 但 AT+CNUM 未取到号码 → 回退 IMSI 制 IMPU")
		}
	}
	return &Reg{
		at:      at,
		imsi:    imsi,
		domain:  domain,
		impi:    imsi + "@" + domain,
		impu:    impu,
		uri:     "sip:" + domain,
		uev6:    uev6,
		pcscf:   pcscf,
		pcPortS: opServerPort(),
		esp:     esp,
		cseq:    0,
		expires: opExpires(),
	}
}

// contactUser Contact 用户名跟随身份制式（v0.9.9t）:
//   impu_style=msisdn → Contact 也用号码制（与 From/To 一致, SimAdmin/真机同款）;
//   缺省 imsi（电信/联通现网报文逐字节不变）。
func (r *Reg) contactUser() string {
	if gOp != nil && strings.EqualFold(strings.TrimSpace(gOp.ImpuStyle), "msisdn") &&
		r.impu != "" && r.impu != r.imsi+"@"+r.domain {
		return r.impu
	}
	return r.imsi
}

// buildRegister 构造 REGISTER
// withSec: 首次注册 true（Security-Client/Require/Proxy-Require）; 续租 false（带 Sec 头 → 500）
func (r *Reg) buildRegister(cseq, viaPort, contactPort int, auth, secVerify string, withSec bool) string {
	branch := "z9hG4bK-" + randHex(16)
	host := "[" + r.uev6 + "]"
	lines := []string{
		fmt.Sprintf("REGISTER %s SIP/2.0", r.uri),
		fmt.Sprintf("Via: SIP/2.0/UDP %s:%d;branch=%s", host, viaPort, branch),
		"Max-Forwards: 70",
		fmt.Sprintf("From: <sip:%s>;tag=%s", r.impu, r.fromTag),
		fmt.Sprintf("To: <sip:%s>", r.impu),
		"Call-ID: " + r.callID,
		fmt.Sprintf("CSeq: %d REGISTER", cseq),
		fmt.Sprintf("Contact: <sip:%s@%s:%d>%s%s", r.contactUser(), host, contactPort, opContactTags(), contactExpiresParam()),
	}
	// ★ v0.9.9n: 以下头域按运营商配置发送（未配置 = 不发，老运营商行为不变）
	if sup := opSendSupported(); sup != "" {
		lines = append(lines, "Supported: "+sup)
	}
	if pani := opPanI(); pani != "" {
		lines = append(lines, "P-Access-Network-Info: "+pani)
	}
	if ppi := opPPrefIdentity(r.impu); ppi != "" {
		lines = append(lines, "P-Preferred-Identity: "+ppi)
	}
	if auth != "" {
		lines = append(lines, "Authorization: "+auth)
	}
	// sec-agree 声明: 首个 REGISTER 是否带由配置决定(便于探测网络真实期望);
	// 受保护 REGISTER#2 必带(secVerify 非空), 否则网络会拒。
	if withSec && (opSecAgree() || secVerify != "") {
		lines = append(lines, "Require: sec-agree", "Proxy-Require: sec-agree")
		lines = append(lines, "Security-Client: "+secClientValue(ueSpiC, ueSpiS, uePortC, uePortS))
		if secVerify != "" {
			lines = append(lines, "Security-Verify: "+secVerify)
		}
	}
	lines = append(lines, fmt.Sprintf("Expires: %d", r.expires),
		"User-Agent: "+opUA(), "Content-Length: 0", "", "")
	return strings.Join(lines, "\r\n")
}

// aka 跑一次 AKA（走引擎 AT 口 CSIM）
func (r *Reg) aka(randHexStr, autnHex string) (res, ik, auts string, err error) {
	// ★ 先在缺省通道 SELECT ADF_USIM（缺省通道的应用选择态可能被
	//   CCHO/CCHC 通道操作清掉 → AUTHENTICATE 直接 6985）
	if _, sw, e := csimAPDU(r.at, "00A4000407"+aidUSIM); e != nil || sw != "9000" {
		logf("SELECT ADF_USIM: sw=%s err=%v (继续尝试 AUTH)", sw, e)
	}
	apdu := fmt.Sprintf("008800812210%s10%s", randHexStr, autnHex)
	data, sw, err := csimAPDU(r.at, apdu)
	if err != nil {
		return "", "", "", fmt.Errorf("CSIM: %v", err)
	}
	raw := hex.EncodeToString(data) + sw
	logf("AKA raw(%dB): %s", len(raw)/2, raw)
	res0, _, ik0, auts0, e0 := parseAkaRaw(raw)
	if e0 == nil {
		return res0, ik0, auts0, nil
	}
	if !strings.Contains(e0.Error(), "6985") {
		return "", "", "", e0
	}
	// 6985(使用条件不满足) → 重试一次（卡应用选择态问题, 实测重试即恢复）
	logf("AKA 6985, 200ms 后重试")
	time.Sleep(200 * time.Millisecond)
	data, sw, err = csimAPDU(r.at, apdu)
	if err != nil {
		return "", "", "", fmt.Errorf("CSIM retry: %v", err)
	}
	raw = hex.EncodeToString(data) + sw
	logf("AKA retry raw(%dB): %s", len(raw)/2, raw)
	r2, _, ik2, auts2, e2 := parseAkaRaw(raw)
	return r2, ik2, auts2, e2
}

// waitPlain 在明文 UDP socket 上等响应（REGISTER#1 / 重同步阶段）
func (r *Reg) waitPlain(timeout time.Duration) (string, error) {
	buf := make([]byte, 8192)
	r.udp.SetReadDeadline(time.Now().Add(timeout))
	n, _, err := r.udp.ReadFrom(buf)
	if err != nil {
		return "", err
	}
	return string(buf[:n]), nil
}

// sendProtectedRaw 受保护发送 + 等响应（重传 3 次, 每窗 8s）
// raw = 完整 SIP 报文字节（可含二进制 body）
// expectCseq: >0 时只接受该 CSeq 的响应 —— 上一事务(REGISTER#1)的 401 会重发好几次,
//   不按 CSeq 过滤会把它们误当本次的拒绝（实测踩到）
// expectCallID: 非空时按 Call-ID 过滤（SMS 每条新事务）
// 返回 (响应文本, 是否 2xx)
func (r *Reg) sendProtectedRaw(raw []byte, expectCseq int, expectCallID string) (string, bool, error) {
	seg := udpPack(net.ParseIP(r.uev6), net.ParseIP(r.pcscf), uePortC, r.pcPortS, raw)[40:]
	var lastResp string
	for attempt := 0; attempt < 3; attempt++ {
		r.esp.Send(seg)
		deadline := time.Now().Add(8 * time.Second)
		for time.Now().Before(deadline) {
			ev := r.esp.WaitEvent(300 * time.Millisecond)
			if ev == nil {
				continue
			}
			resp := string(ev.Payload)
			if !strings.Contains(resp, "SIP/2.0") {
				continue
			}
			// ★ v0.9.9g: CSeq 过滤只对"响应行"(SIP/2.0 <code>)生效 —— 入站请求行
			//   (MESSAGE 等)的 CSeq 由对方独立编号, 可能恰好等于 expectCseq, 原实现
			//   会把它当"旧事务响应"直接丢弃(丢短信)。请求行交给下面 Call-ID 分支处理。
			if strings.HasPrefix(resp, "SIP/2.0 ") {
				if cs := parseCseq(resp); cs != 0 && expectCseq != 0 && cs != expectCseq {
					logf("跳过旧事务响应(CSeq %d≠%d): %s", cs, expectCseq, firstLine(resp))
					continue
				}
			}
		if expectCallID != "" {
			if cid := sipHeader(resp, "Call-ID"); cid != "" && cid != expectCallID {
				// 非本事务: 若是入站请求(MT 短信/OPTIONS 探测等)立即处理, 防止等待期间丢收信
				// ★ v0.9.12 (WP3-3.6): 由"只放行 MESSAGE"改为放行所有请求 —— HandleEvent
				//   对非 MESSAGE 请求回 200, 对 MESSAGE 走短信流程。
				if ev.Protected && !strings.HasPrefix(resp, "SIP/2.0") {
					r.HandleEvent(ev)
				}
				continue
			}
		}
			logf("受保护响应(%s): %s", map[bool]string{true: "ESP", false: "明文"}[ev.Protected], firstLine(resp))
			if code := sipStatus(resp); code >= 200 && code < 300 {
				return resp, true, nil // 2xx 含 202 Accepted(MO 短信)
			}
			lastResp = resp
			if ev.Protected {
				// 受保护的非 200 = 本事务最终裁决, 不用再等
				return resp, false, nil
			}
			// 明文非 200 → 可能是重发, 继续等 ESP 裁决
		}
		logf("受保护发送第 %d 次无响应, 重试", attempt+1)
	}
	if lastResp != "" {
		return lastResp, false, nil
	}
	return "", false, fmt.Errorf("受保护发送无响应")
}

// sipStatus 取响应状态码（0=解析失败）
func sipStatus(resp string) int {
	i := strings.Index(resp, "SIP/2.0 ")
	if i < 0 {
		return 0
	}
	var code int
	fmt.Sscanf(resp[i+8:], "%d", &code)
	return code
}

// parseCseq 从 SIP 消息取 CSeq 序号（0=解析失败）
func parseCseq(msg string) int {
	h := sipHeader(msg, "CSeq")
	var n int
	fmt.Sscanf(h, "%d", &n)
	return n
}

func firstLine(s string) string {
	if i := strings.Index(s, "\r\n"); i >= 0 {
		return s[:i]
	}
	return s
}

// parseChallenge 从 401 响应提取鉴权材料
func (r *Reg) parseChallenge(resp string) (nonce, realm, opaque, qop, secSrv string, err error) {
	www := sipHeader(resp, "WWW-Authenticate")
	nonce = sipParam(www, "nonce")
	realm = sipParam(www, "realm")
	opaque = sipParam(www, "opaque")
	qop = sipParam(www, "qop")
	secSrv = sipHeader(resp, "Security-Server")
	if nonce == "" {
		err = fmt.Errorf("401 无 nonce")
	}
	return
}

// authFor 构造 Authorization 头
func (r *Reg) authFor(realm, nonce, opaque, qop, digest string) string {
	a := fmt.Sprintf("Digest username=\"%s\", realm=\"%s\", nonce=\"%s\", uri=\"%s\", response=\"%s\", algorithm=AKAv1-MD5",
		r.impi, realm, nonce, r.uri, digest)
	if opaque != "" {
		a += fmt.Sprintf(", opaque=\"%s\"", opaque)
	}
	if qop != "" {
		a += fmt.Sprintf(", qop=%s, nc=00000001, cnonce=\"%s\"", qop, r.cnonce)
	}
	return a
}

// do401Round 处理一轮 401: AKA(+失步重同步) → 可选更新 SA key → 返回 Authorization 头
//   updateKey=true  仅用于【初始注册/重协商】
//   updateKey=false 用于【续租】——必须复用现有 SA, 否则与 P-CSCF 密钥不一致导致静默丢包
func (r *Reg) do401Round(nonce, realm, opaque, qop string, updateKey bool) (auth string, nextCseq int, err error) {
	nb, e := base64.StdEncoding.DecodeString(nonce)
	if e != nil || len(nb) != 32 {
		return "", 0, fmt.Errorf("nonce 解码异常 (%dB)", len(nb))
	}
	logf("RAND=%x AUTN=%x", nb[:16], nb[16:])
	res, ik, auts, e := r.aka(hex.EncodeToString(nb[:16]), hex.EncodeToString(nb[16:]))
	if e != nil {
		return "", 0, e
	}
	reg2Cseq := r.cseq + 1
	if auts != "" {
		logf("SIM 报 SQN 失步, AUTS=%s → 发起重同步", auts)
		// ★ v0.9.9g: 续租路径的 r.udp 已在 initialOne 里关闭置空 → 这里按需重开一个
		//   源端口 5060 的明文 socket (重同步是明文阶段, 用完即关)
		if r.udp == nil {
			u, e := net.ListenUDP("udp6", &net.UDPAddr{IP: net.ParseIP(r.uev6), Port: plainPort})
			if e != nil {
				return "", 0, fmt.Errorf("重同步 bind %d: %v", plainPort, e)
			}
			r.udp = u
			defer func() {
				u.Close()
				if r.udp == u {
					r.udp = nil
				}
			}()
		}
		// 空密码占位 Digest + auts(base64)，明文 REGISTER（CSeq 严格递增）
		ha1e := md5hex(r.impi + ":" + realm + ":")
		ha2e := md5hex("REGISTER:" + r.uri)
		respEmpty := md5hex(ha1e + ":" + nonce + ":00000001:" + r.cnonce + ":auth:" + ha2e)
		authAuts := fmt.Sprintf("Digest username=\"%s\", realm=\"%s\", nonce=\"%s\", uri=\"%s\", qop=auth, nc=00000001, cnonce=\"%s\", response=\"%s\", algorithm=AKAv1-MD5, auts=\"%s\"",
			r.impi, realm, nonce, r.uri, r.cnonce, respEmpty,
			base64.StdEncoding.EncodeToString(mustHex(auts)))
		if opaque != "" {
			authAuts += fmt.Sprintf(", opaque=\"%s\"", opaque)
		}
		// ★ 重同步仍是明文阶段: 带 Security-Client/sec-agree（对齐 hims 实测成功报文）
		sync := r.buildRegister(reg2Cseq, plainPort, plainPort, authAuts, "", true)
		if _, e := r.udp.WriteToUDP([]byte(sync), &net.UDPAddr{IP: net.ParseIP(r.pcscf), Port: opPcscfPort()}); e != nil {
			return "", 0, e
		}
		resp, e := r.waitPlain(15 * time.Second)
		if e != nil {
			return "", 0, fmt.Errorf("重同步无响应: %v", e)
		}
		logf("重同步响应: %s", firstLine(resp))
		if !strings.Contains(resp, "nonce=") {
			return "", 0, fmt.Errorf("重同步未拿到新 nonce: %s", firstLine(resp))
		}
		www := sipHeader(resp, "WWW-Authenticate")
		nonce = sipParam(www, "nonce")
		if v := sipParam(www, "realm"); v != "" {
			realm = v
		}
		if v := sipParam(www, "opaque"); v != "" {
			opaque = v
		}
		if v := sipParam(www, "qop"); v != "" {
			qop = v
		}
		nb2, _ := base64.StdEncoding.DecodeString(nonce)
		if len(nb2) != 32 {
			return "", 0, fmt.Errorf("新 nonce 解码异常")
		}
		res, ik, auts, e = r.aka(hex.EncodeToString(nb2[:16]), hex.EncodeToString(nb2[16:]))
		if e != nil || res == "" {
			return "", 0, fmt.Errorf("重同步后 AKA 仍失败: %v", e)
		}
		reg2Cseq++
	}
	if res == "" || ik == "" {
		return "", 0, fmt.Errorf("AKA 无 RES/IK")
	}
	resBytes, _ := hex.DecodeString(res)
	ikBytes, _ := hex.DecodeString(ik)
	key := append(ikBytes, 0, 0, 0, 0) // IK 16B → 20B
	// ★★ 2026-09-29 修复(第二个真 BUG, 与网卡名独立):
	//   续租(re-REGISTER)必须【复用现有 SA】, 不能更新 ESP 密钥。
	//   现象: 续租第一拍收到 401(通道正常, 抓包可见 P-CSCF 有回包) →
	//         第二拍用新 nonce 做 AKA 得到【新 IK】并 SetKey → 我方用新 IK 加密发出,
	//         而 P-CSCF 侧 SA 仍是初始注册时的旧 IK → 解不开 → 静默丢弃 → 无响应。
	//   后果: 续租失败×3 → main.go 走 break → 外层【拆承载重建 CID2】→ 注册彻底失败。
	//   证据: 抓包中续租第二拍 1080B 发出后 P-CSCF 全程无回包; 而重注册(完整协商新 SA)即可恢复。
	//   ⇒ 只有"初始注册/重协商"才允许更新 SA 密钥。
	if updateKey {
		r.esp.SetKey(key)
		logf("SA key 已更新(初始注册): %x", key)
	} else {
		logf("SA key 保持不变(续租复用现有 SA): %x", key)
	}
	digest := akav1Md5(r.impi, realm, resBytes, nonce, qop, r.uri, r.cnonce)
	return r.authFor(realm, nonce, opaque, qop, digest), reg2Cseq, nil
}

// Initial 完整初始注册。成功后 Reg 进入已注册态。
// ★ v0.6.0: 学习值置顶 (自学习), 成功后记住实际可用的 P-CSCF
// ★ v0.9.2 (主人拍板): P-CSCF 候选【只加不删】—— 失败的候选留在库里,
//   每次注册挨个尝试谁能通就跟谁建立; 200T 拿地址本来就难, 删是帮倒忙
//   (锁频事故实锤: 剔除机制把 46001 的 8 个地址全清空)。
func (r *Reg) Initial(pcscfCandidates []string) error {
	var err error
	learned := loadLearned(r.imsi)
	cands := dedupCandidates(learned, pcscfCandidates)
	for _, cand := range cands {
		if e := r.initialOne(cand); e != nil {
			logf("P-CSCF 候选 [%s] 失败: %v", cand, e)
			err = e
			continue
		}
		learnPcscf(r.imsi, cand)
		return nil
	}
	return err
}

// setPcscf P-CSCF 锚点唯一写入口 (v0.7.6): r.pcscf 与 e.pcscf 在这里强制同步,
// 候选链遍历/重注册永不脱钩 (esp.go SetPCSCF 的"候选[0]发往候选[1]"潜伏 bug 根治)。
func (r *Reg) setPcscf(pcscf string) {
	r.pcscf = pcscf
	if r.esp != nil {
		r.esp.SetPCSCF(pcscf)
	}
}

func (r *Reg) initialOne(pcscf string) error {
	r.setPcscf(pcscf)
	laddr := &net.UDPAddr{IP: net.ParseIP(r.uev6), Port: plainPort}
	udp, err := net.ListenUDP("udp6", laddr)
	if err != nil {
		return fmt.Errorf("bind [%s]:5060: %v", r.uev6, err)
	}
	r.udp = udp
	// ★ v0.9.9g: 关闭时同步把 r.udp 置空 —— 续租路径若遇 SIM SQN 失步需要明文重同步,
	//   会检查 r.udp==nil 再按需重开; 原实现只 Close 不置空 → 重同步写到已关闭 socket,
	//   必失败(靠全量重注册兜底, 白丢一拍)
	defer func() {
		udp.Close()
		if r.udp == udp {
			r.udp = nil
		}
	}()

	// ★ v0.9.9p: 每个候选 P-CSCF 都用全新对话（Call-ID/From-tag/CSeq 从 1 起）。
	//   原实现只在首轮生成 → 8 个候选共用一个 Call-ID 且 CSeq 一直递增，真机不会这样；
	//   S-CSCF 会把"同一对话换 P-CSCF/换参数"当异常（移动 403 "different algorithm
	//   from initial register" 与此措辞吻合）。注意：同一候选内 REGISTER#1→401→#2 仍复用
	//   同一 Call-ID（规范 09-A3），不受影响。
	r.callID = "eng-" + randHex(8) + "@" + r.uev6
	r.fromTag = randHex(8)
	r.cnonce = randHex(8)
	r.cseq = 0

	// ---- REGISTER#1（明文, 5060→5060）----
	r.cseq++
	reg1 := r.buildRegister(r.cseq, plainPort, plainPort, "", "", true)
	logf("REGISTER#1 → [%s]:%d (%dB)", pcscf, opPcscfPort(), len(reg1))
	// ★ v0.9.9n: 把实际发出的 Security-Client 打进日志，便于确认配置是否生效（不用抓包）
	if opSecAgree() {
		logf("  Security-Client = %s", secClientValue(ueSpiC, ueSpiS, uePortC, uePortS))
	}
	for _, h := range []string{"Supported", "P-Access-Network-Info", "P-Preferred-Identity"} {
		if v := sipHeader(reg1, h); v != "" {
			logf("  %s = %s", h, v)
		}
	}
	if _, err := udp.WriteToUDP([]byte(reg1), &net.UDPAddr{IP: net.ParseIP(pcscf), Port: opPcscfPort()}); err != nil {
		return err
	}
	resp1, err := r.waitPlain(15 * time.Second)
	if err != nil {
		return fmt.Errorf("15s 无响应")
	}
	logf("响应: %s", firstLine(resp1))
	// ★ v0.9.9n: 423 Interval Too Brief → 按服务器给的 Min-Expires 提高租期后重发
	//   （SimAdmin 的 VoLTE 栈同样处理: "plain REGISTER retrying with server Min-Expires"）
	for i := 0; i < 2 && strings.Contains(firstLine(resp1), "423"); i++ {
		var v int
		fmt.Sscanf(sipHeader(resp1, "Min-Expires"), "%d", &v)
		if v <= 0 || v <= r.expires {
			logf("收到 423, 但 Min-Expires=%d 无效/未增加 → 不重发", v)
			break
		}
		logf("收到 423: 按服务器 Min-Expires=%d 重发 REGISTER#1 (原租期 %d)", v, r.expires)
		r.expires = v
		r.cseq++
		reg1 = r.buildRegister(r.cseq, plainPort, plainPort, "", "", true)
		if _, e2 := udp.WriteToUDP([]byte(reg1), &net.UDPAddr{IP: net.ParseIP(pcscf), Port: opPcscfPort()}); e2 != nil {
			return e2
		}
		resp1, err = r.waitPlain(15 * time.Second)
		if err != nil {
			return fmt.Errorf("15s 无响应")
		}
		logf("响应: %s", firstLine(resp1))
	}
	if !strings.Contains(resp1, " 401 ") {
		// ★ v0.9.9m: 非 401 时把完整响应头也打进日志 —— 运营商拒绝原因就写在 Warning 里
		//   移动实测: Warning: 399 ... "Terminal has used different algorithm from initial register"
		for _, ln := range strings.Split(strings.TrimRight(resp1, "\r\n"), "\r\n") {
			if strings.TrimSpace(ln) != "" {
				logf("  ← %s", ln)
			}
		}
		return fmt.Errorf("未收到 401: %s", firstLine(resp1))
	}
	nonce, realm, opaque, qop, secSrv, err := r.parseChallenge(resp1)
	if err != nil {
		return err
	}
	fmt.Sscanf(sipParam(secSrv, "spi-s"), "%d", &r.pcSpiS)
	fmt.Sscanf(sipParam(secSrv, "port-s"), "%d", &r.pcPortS)
	if r.pcSpiS == 0 {
		return fmt.Errorf("Security-Server 无 spi-s")
	}
	setESPAlgFromSIP(sipParam(secSrv, "alg")) // ★ v0.9.9m: ESP 完整性算法跟随网络协商
	logf("P-CSCF: spi-s=%d port-s=%d", r.pcSpiS, r.pcPortS)

	// ---- AKA + SA key ----
	r.cseq = 1 // 重同步从 2 起
	auth, nextCseq, err := r.do401Round(nonce, realm, opaque, qop, true)
	if err != nil {
		return err
	}

	// ---- 受保护 REGISTER#2 ----
	if r.esp == nil {
		return fmt.Errorf("esp 未初始化")
	}
	r.esp.SetSPIs(r.pcSpiS, ueSpiS)
	reg2 := r.buildRegister(nextCseq, uePortS, uePortS, auth, secSrv, true)
	r.cseq = nextCseq
	logf("REGISTER#2(受保护) [%s]:%d → [%s]:%d (%dB)", r.uev6, uePortC, pcscf, r.pcPortS, len(reg2))
	resp2, ok200, err := r.sendProtectedRaw([]byte(reg2), nextCseq, r.callID)
	if err != nil {
		return err
	}
	if !ok200 {
		return fmt.Errorf("受保护注册被拒: %s", firstLine(resp2))
	}
	if sr := sipHeader(resp2, "Service-Route"); sr != "" {
		if !strings.HasPrefix(sr, "<") {
			sr = "<" + sr + ">"
		}
		r.serviceRoute = sr
	}
	if pau := sipHeader(resp2, "P-Associated-URI"); pau != "" {
		r.associated = nil
		for _, part := range strings.Split(pau, ",") {
			part = strings.TrimSpace(part)
			if i := strings.Index(part, "<"); i >= 0 {
				if j := strings.Index(part, ">"); j > i {
					r.associated = append(r.associated, part[i+1:j])
				}
			}
		}
	}
	if v := sipHeader(resp2, "Expires"); v != "" {
		fmt.Sscanf(v, "%d", &r.expires)
	}
	r.regAt = time.Now()
	logf("★ 200 OK: expires=%ds serviceRoute=%s associated=%v", r.expires, r.serviceRoute, r.associated)
	return nil
}

// Renew 保活续租（ZTE 契约: 无 Sec 头 REGISTER → 401 → 就地 AKA → 同 SA 补发 → 200 OK）
func (r *Reg) Renew() error {
	r.cseq++
	msg := r.buildRegister(r.cseq, uePortS, uePortS, "", "", false)
	resp, ok200, err := r.sendProtectedRaw([]byte(msg), r.cseq, r.callID)
	if err != nil {
		return err
	}
	if ok200 {
		if v := sipHeader(resp, "Expires"); v != "" {
			fmt.Sscanf(v, "%d", &r.expires)
		}
		r.regAt = time.Now()
		return nil
	}
	// 401 → 新 nonce → AKA → 同 SA 补发
	if !strings.Contains(resp, " 401 ") {
		return fmt.Errorf("续租被拒: %s", firstLine(resp))
	}
	nonce, realm, opaque, qop, _, err := r.parseChallenge(resp)
	if err != nil {
		return err
	}
	auth, nextCseq, err := r.do401Round(nonce, realm, opaque, qop, false)
	if err != nil {
		return err
	}
	r.cseq = nextCseq
	msg2 := r.buildRegister(r.cseq, uePortS, uePortS, auth, "", false)
	resp2, ok200, err := r.sendProtectedRaw([]byte(msg2), r.cseq, r.callID)
	if err != nil {
		return err
	}
	if !ok200 {
		return fmt.Errorf("续租补发被拒: %s", firstLine(resp2))
	}
	if v := sipHeader(resp2, "Expires"); v != "" {
		fmt.Sscanf(v, "%d", &r.expires)
	}
	r.regAt = time.Now()
	return nil
}

// PhoneNo 从 P-Associated-URI 取号码制身份（+86...）
func (r *Reg) PhoneNo() string {
	for _, u := range r.associated {
		if strings.HasPrefix(u, "sip:+") {
			if i := strings.Index(u, "@"); i > 5 {
				return u[5:i]
			}
		}
	}
	return ""
}

func mustHex(s string) []byte {
	b, _ := hex.DecodeString(s)
	return b
}

// SendOptions OPTIONS 保活（学 U3 options_interval_ms=60000）:
//   ① 保持运营商链路 UDP 会话（30min 无流量会被清 → 续租包全被丢弃的根因）
//   ② 探测 P-CSCF 是否仍认识我们（防 SA 被网络侧回收）
//   经 ESP 受保护发送（与续租同路径），P-CSCF 回 200/401 均证明链路活着。
func (r *Reg) SendOptions() {
	r.cseq++
	branch := randHex(8)
	msg := fmt.Sprintf(
		"OPTIONS %s SIP/2.0\r\n"+
			"Via: SIP/2.0/UDP [%s]:%d;branch=z9hG4bK%s\r\n"+
			"From: <%s>;tag=%s\r\n"+
			"To: <%s>\r\n"+
			"Call-ID: %s\r\n"+
			"CSeq: %d OPTIONS\r\n"+
			"Max-Forwards: 70\r\n"+
			"Content-Length: 0\r\n\r\n",
		r.uri, r.uev6, uePortS, branch, r.impi, r.fromTag, r.uri, r.callID, r.cseq)
	r.esp.Send(udpPack(net.ParseIP(r.uev6), net.ParseIP(r.pcscf), uePortC, r.pcPortS, []byte(msg))[40:])
}
