package main

// sms.go —— SMS over IMS（TS 24.341 / 24.011 / 23.040）
// 移植自 fzsagent engine v0.9.x（ims-src），裁剪适配：
//   - 去掉 001B 自有信箱 / 企业微信转发（MT 改走 fzsbridge：fzs-sms 收件箱 + 转发队列）
//   - 短信中心号来源不变：卡 AT+CSCA? → 配置 smsc_psi → 缺省
//   - MO 入口改由 fzsbridge 的 smsReqPoll 驱动（LuCI→fzs-at→init.d submit）
//
// 端口配方自 uvo_ims.py 实测（210 发送成功版）:
//   MO: MESSAGE tel:<smsc> + Route(Service-Route) + From(号码制 IMPU) + body=RP-DATA(SMS-SUBMIT)
//   MT: MESSAGE(vnd.3gpp.sms, RP-MTI=1) → 回 SIP 200 + RP-ACK(MS→N, RP-MTI=2)
//   RP-MTI 分流: 0=RP-DATA(MS→N) 1=RP-DATA(N→MS) 2=RP-ACK(MS→N) 3=RP-ACK(N→MS) 5=ERROR

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"
)

// smscNumberDefault 短信中心缺省号（配置与卡都读不到时）
const smscNumberDefault = "8613010761500"

// readSMSCFromCard AT+CSCA? 读卡内短信中心号（"+8613800100500",145 → +8613800100500; 读不到返回空）
func readSMSCFromCard(at *AT) string {
	ls, err := at.Cmd("AT+CSCA?", 6000)
	if err != nil && len(ls) == 0 {
		logf("AT+CSCA? 失败: %v", err)
		return ""
	}
	for _, ln := range ls {
		if !strings.Contains(ln, "+CSCA") {
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

// telPSI 号码 → SMS over IMS 的短信中心 PSI（tel:+86...；86 开头补 +，国内 11 位补 +86）
func telPSI(num string) string {
	n := strings.TrimSpace(num)
	if n == "" {
		return ""
	}
	if strings.HasPrefix(n, "+") {
		return "tel:" + n
	}
	if strings.HasPrefix(n, "86") && len(n) >= 13 {
		return "tel:+" + n
	}
	return "tel:+86" + n
}

// smscCardLoadedFor 已从卡读过 CSCA 的 IMSI（换卡重读, 同卡不重复打扰 AT 口）
var smscCardLoadedFor string

// initSmscPSI 短信中心来源收口: 卡 AT+CSCA? 优先 → 配置 smsc_psi → 缺省。
// 在 loadConfOrDie 之后调用（gSmscPSI 已就绪作为回退基准）。
func initSmscPSI(at *AT, imsi string) {
	if at == nil || imsi == "" {
		return
	}
	if smscCardLoadedFor == imsi {
		return
	}
	smscCardLoadedFor = imsi
	cardNum := readSMSCFromCard(at)
	if cardNum == "" {
		logf("短信中心: AT+CSCA? 卡未提供 → 用配置 smsc_psi=%s (兜底 %s)", gSmscPSI, smscNumberDefault)
		return
	}
	psi := telPSI(cardNum)
	if psi == gSmscPSI {
		logf("短信中心: AT+CSCA? = %s (与配置一致)", psi)
		return
	}
	logf("短信中心: AT+CSCA? = %s → 覆盖配置 %s (归属省按卡取)", psi, gSmscPSI)
	gSmscPSI = psi
}

func smscDigits() string {
	if gSmscPSI != "" {
		var b strings.Builder
		for _, ch := range gSmscPSI {
			if ch >= '0' && ch <= '9' {
				b.WriteRune(ch)
			}
		}
		if s := b.String(); len(s) >= 6 {
			return s
		}
	}
	return smscNumberDefault
}

var gsm7Table = ("@£$¥èéùìòÇ\nØø\rÅåΔ_ΦΓΛΩΠΨΣΘΞ\x1bÆæßÉ !\"#¤%&'()*+,-./0123456789:;<=>?" +
	"¡ABCDEFGHIJKLMNOPQRSTUVWXYZÄÖÑÜ§¿abcdefghijklmnopqrstuvwxyzäöñüà")

// gsm7Runes: gsm7Table 含多字节 UTF-8 字符 → 解码按 rune 下标（字节下标会从第一个 2 字节字符起全部错位）
var gsm7Runes = []rune(gsm7Table)

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func bcdDecode(b []byte) string {
	out := ""
	for _, by := range b {
		out += fmt.Sprintf("%d", by&0x0F)
		if hi := (by >> 4) & 0x0F; hi != 0xF {
			out += fmt.Sprintf("%d", hi)
		}
	}
	return out
}

func bcdEncode(num string) []byte {
	d := num
	if len(d)%2 == 1 {
		d += "F"
	}
	out := make([]byte, 0, len(d)/2)
	for i := 0; i+1 < len(d); i += 2 {
		lo := d[i] - '0'
		hi := d[i+1]
		var hv byte
		if hi == 'F' || hi == 'f' {
			hv = 0x0F
		} else {
			hv = hi - '0'
		}
		out = append(out, (hv<<4)|lo)
	}
	return out
}

func gsm7Decode(data []byte, nchar int) string {
	return gsm7DecodeAt(data, 0, nchar)
}

// gsm7DecodeAt 从第 skip 个 septet 开始解码（长短信 UDH 占用头部 septet 时跳过）
func gsm7DecodeAt(data []byte, skip, nchar int) string {
	out := ""
	var septet uint32
	bits := 0
	idx := 0
	for _, by := range data {
		septet |= uint32(by) << bits
		bits += 8
		for bits >= 7 {
			c := int(septet & 0x7F)
			septet >>= 7
			bits -= 7
			if idx >= skip {
				if c < len(gsm7Runes) {
					out += string(gsm7Runes[c])
				} else {
					out += "?"
				}
			}
			idx++
			if idx >= skip+nchar {
				return out
			}
		}
	}
	return out
}

// parseSmsDeliver 解析 RP-DATA → SMS-DELIVER/STATUS-REPORT（T 引擎侧）
// ★ RP-DATA 的 OA/DA/UD 长度字段全部来自网络报文, 逐段做边界校验
//   （越界只丢这一条, 绝不允许 panic 打死引擎）
func parseSmsDeliver(body []byte) (sender, text string, reportReq bool, part *SmsPart) {
	if len(body) < 4 {
		return "?", "太短", false, nil
	}
	i := 2 // RP-MTI, RP-MR
	oaLen := int(body[i])
	i++
	if i+oaLen > len(body) {
		return "?", "OA越界", false, nil
	}
	oa := body[i : i+oaLen]
	i += oaLen
	if i >= len(body) {
		return "?", "DA缺段", false, nil
	}
	daLen := int(body[i])
	i++
	if i+daLen > len(body) {
		return "?", "DA越界", false, nil
	}
	i += daLen
	if i >= len(body) {
		return "?", "UD缺段", false, nil
	}
	udLen := int(body[i])
	i++
	if i+udLen > len(body) {
		return "?", "UD越界", false, nil
	}
	tpdu := body[i : i+udLen]
	rpOriginator := "?"
	if len(oa) > 1 {
		rpOriginator = bcdDecode(oa[1:])
	}
	return parseDeliverEx(tpdu, rpOriginator)
}

// parseDeliverTPDU 兼容旧签名, 丢弃分片信息
func parseDeliverTPDU(tpdu []byte, rpOriginator string) (sender, text string, reportReq bool) {
	sender, text, reportReq, _ = parseDeliverEx(tpdu, rpOriginator)
	return
}

// SmsPart 长短信分片信息 (UDH concat 8-bit reference, IEI 0x00)
type SmsPart struct {
	Ref   int
	Total int
	Seq   int
}

// parseDeliverEx 解析 SMS-DELIVER/STATUS-REPORT TPDU, 额外返回长短信分片信息
func parseDeliverEx(tpdu []byte, rpOriginator string) (sender, text string, reportReq bool, part *SmsPart) {
	if len(tpdu) < 1 {
		return "?", "TPDU为空", false, nil
	}
	if tpdu[0]&0x03 == 2 { // SMS-STATUS-REPORT（我方 MO 的送达报告）
		if len(tpdu) < 3 {
			return "STATUS-REPORT", "报告过短", false, nil
		}
		j := 1
		mr := tpdu[j]
		j++
		raSemi := int(tpdu[j])
		j += 2 // len + type
		raBytes := (raSemi + 1) / 2
		if j+raBytes+7 > len(tpdu) {
			return "STATUS-REPORT", fmt.Sprintf("MR=%d 报告截断(收件人/时间戳不全)", mr), false, nil
		}
		ra := bcdDecode(tpdu[j : j+raBytes])
		j += raBytes + 7 // TP-SCTS
		st := byte(0xFF)
		if len(tpdu) > j {
			st = tpdu[j]
		}
		stTxt := "已送达"
		if st != 0 {
			if st < 0x80 {
				stTxt = fmt.Sprintf("暂未送达 0x%02x", st)
			} else {
				stTxt = fmt.Sprintf("送达失败 0x%02x", st)
			}
		}
		return "STATUS-REPORT", fmt.Sprintf("MR=%d 收件人=%s 送达状态: %s", mr, ra, stTxt), false, nil
	}
	j := 0
	j++ // TP-MTI/MMS/VFP/SRI/RD 首字节
	oaSemi := int(tpdu[j])
	j += 2 // len + type
	oaValBytes := (oaSemi + 1) / 2
	if j+oaValBytes > len(tpdu) {
		return "?", "OA越界", false, nil
	}
	oaBcd := tpdu[j : j+oaValBytes]
	j += oaValBytes
	sender = rpOriginator
	if len(oaBcd) > 0 {
		sender = bcdDecode(oaBcd)
	}
	j++ // TP-PID
	if j >= len(tpdu) {
		return sender, "", false, nil
	}
	dcs := tpdu[j]
	j++
	j += 7 // TP-SCTS
	if j >= len(tpdu) {
		return sender, "", false, nil
	}
	udl := int(tpdu[j])
	j++
	// TP-UDHI(bit6): 长短信分片头。不跳过会把 UDHL+UDH 的 6 字节当正文解出乱码
	udhLen := 0
	if tpdu[0]&0x40 != 0 && j < len(tpdu) {
		udhLen = int(tpdu[j])
		if udhLen < 1 || udhLen+1 > udl || j+1+udhLen > len(tpdu) {
			udhLen = 0
		}
	}
	// concat 8-bit reference UDH = [00 03 ref total seq] → 记录分片, 交给 concatSms 重组
	if udhLen >= 5 && tpdu[j+1] == 0x00 && tpdu[j+2] == 0x03 {
		total, seq := int(tpdu[j+4]), int(tpdu[j+5])
		if total >= 2 && seq >= 1 && seq <= total {
			part = &SmsPart{Ref: int(tpdu[j+3]), Total: total, Seq: seq}
		}
	}
	var body2 string
	if dcs&0x0C == 0x08 { // UCS-2
		b := tpdu[j:minInt(j+udl, len(tpdu))]
		if udhLen > 0 {
			b = b[1+udhLen:]
		}
		r := []rune{}
		for k := 0; k+1 < len(b); k += 2 {
			c := rune(uint16(b[k])<<8 | uint16(b[k+1]))
			if c < 0x20 {
				continue // 控制字符 = 解析残留, 不入正文
			}
			r = append(r, c)
		}
		body2 = string(r)
	} else if dcs&0x04 == 0 { // GSM7
		skip := 0
		if udhLen > 0 {
			skip = (8*(udhLen+1) + 6) / 7
		}
		nbytes := (udl*7 + 7) / 8
		body2 = gsm7DecodeAt(tpdu[j:minInt(j+nbytes, len(tpdu))], skip, udl-skip)
	} else {
		b := tpdu[j:minInt(j+udl, len(tpdu))]
		if udhLen > 0 {
			b = b[1+udhLen:]
		}
		body2 = string(b)
	}
	// TP-SRI(bit5): 0=发送方要送达报告 → RP-ACK 须带 DELIVER-REPORT
	reportReq = (tpdu[0]>>5)&1 == 0
	return sender, body2, reportReq, part
}

// ---------- 长短信分片重组 ----------
// 分片按 (发件人, UDH ref) 归组缓存; 全部到齐按序拼接成一条。
// 10 分钟没凑齐的残组直接丢弃 (短信中心有存储转发, 网络会重发)。

var (
	concatMu  sync.Mutex
	concatBuf = map[string]map[int]string{}
	concatAt  = map[string]time.Time{}
)

// concatSms 长短信重组: 返回 (完整文本, 是否可入库)。单条短信 part=nil 原样通过。
func concatSms(slot, sender string, p *SmsPart, text string) (string, bool) {
	if p == nil || p.Total < 2 || p.Seq < 1 || p.Seq > p.Total {
		return text, true
	}
	key := slot + "|" + sender + "|" + strconv.Itoa(p.Ref)
	now := time.Now()
	concatMu.Lock()
	defer concatMu.Unlock()
	for k, t := range concatAt {
		if now.Sub(t) > 10*time.Minute {
			logf("长短信残组超时丢弃: %s (%d/%d 片)", k, len(concatBuf[k]), p.Total)
			delete(concatBuf, k)
			delete(concatAt, k)
		}
	}
	m, ok := concatBuf[key]
	if !ok {
		m = map[int]string{}
		concatBuf[key] = m
	}
	m[p.Seq] = text
	concatAt[key] = now
	if len(m) < p.Total {
		return "", false
	}
	full := ""
	for i := 1; i <= p.Total; i++ {
		full += m[i]
	}
	delete(concatBuf, key)
	delete(concatAt, key)
	return full, true
}

// buildSmsSubmit SMS-SUBMIT TPDU（dcs 恒 8 = UCS-2, 首字节 0x21 = SRR=1 申请送达报告）
// toType: 0x91=国际号 / 0x81=unknown(运营商短号, 用 0x91 会被视为国际号 "+10010" 路由失败)
func buildSmsSubmit(to, text string, toType byte) []byte {
	ud := make([]byte, 0, len([]rune(text))*2)
	for _, r := range []rune(text) {
		ud = append(ud, byte(uint16(r)>>8), byte(uint16(r)))
	}
	return append([]byte{0x21, 0x00, byte(len(to)), toType}, append(bcdEncode(to),
		append([]byte{0x00, 0x08, byte(len(ud))}, ud...)...)...)
}

// buildRpData RP-DATA (MS→N)
func buildRpData(tpdu []byte, scNumber string) []byte {
	rpDa := append([]byte{0x91}, bcdEncode(scNumber)...)
	out := []byte{0x00, 0x00, 0x00, byte(len(rpDa))}
	out = append(out, rpDa...)
	out = append(out, byte(len(tpdu)))
	return append(out, tpdu...)
}

// ---------- Reg 上的 SMS 操作 ----------

// fromURI 号码制 IMPU（MESSAGE 用它, IMSI 制会被 403 —— 210 实测）
func (r *Reg) fromURI() string {
	if len(r.associated) > 0 {
		return r.associated[0]
	}
	return "sip:" + r.impi
}

// SendSMS MO 短信。202/2xx = 已提交（送达与否看后续 STATUS-REPORT）
func (r *Reg) SendSMS(to, text string) error {
	num := ""
	for _, c := range to {
		if c >= '0' && c <= '9' {
			num += string(c)
		}
	}
	// 号码类型（联通 IMS 实测对比）:
	//   手机号: 加 86 前缀 + 0x91 国际 ✓
	//   服务短号(10010/10086): 不加 86 + 0x81 unknown ✓（加 86 会被 IP-SM-GW RP-ERROR 拒绝）
	if len(num) == 11 && strings.HasPrefix(num, "1") {
		num = "86" + num
	}
	if num == "" {
		return fmt.Errorf("号码为空")
	}
	toType := byte(0x91)
	if len(num) <= 8 {
		toType = 0x81
	}
	tpdu := buildSmsSubmit(num, text, toType)
	rpData := buildRpData(tpdu, smscDigits())

	callID := "sms-" + randHex(8) + "@" + r.uev6
	host := "[" + r.uev6 + "]"
	lines := []string{
		fmt.Sprintf("MESSAGE %s SIP/2.0", gSmscPSI),
		fmt.Sprintf("Via: SIP/2.0/UDP %s:%d;branch=z9hG4bK-%s", host, uePortS, randHex(16)),
		"Max-Forwards: 70",
		fmt.Sprintf("From: <%s>;tag=%s", r.fromURI(), randHex(8)),
		fmt.Sprintf("To: <%s>", gSmscPSI),
		"Call-ID: " + callID,
		"CSeq: 1 MESSAGE",
	}
	if r.serviceRoute != "" {
		lines = append(lines, "Route: "+r.serviceRoute)
	}
	lines = append(lines, "Content-Type: application/vnd.3gpp.sms",
		fmt.Sprintf("Content-Length: %d", len(rpData)), "", "")
	msg := strings.Join(lines, "\r\n")
	body := append([]byte(msg), rpData...)

	logf("MO 短信 → %s (原文 %q): TPDU %dB / RP-DATA %dB", num, to, len(tpdu), len(rpData))
	resp, ok, err := r.sendProtectedRaw(body, 0, callID)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("MO 被拒: %s", firstLine(resp))
	}
	logf("★ 已提交（%s）, 等网关 RP-ACK/送达报告", firstLine(resp))
	return nil
}

// HandleEvent 处理来自 P-CSCF 的入站请求（MT 短信 / RP-ACK 等）
// ★ 入站报文完全来自网络, 任何解析意外只许丢这一条, 绝不允许 panic 打死引擎
func (r *Reg) HandleEvent(ev *ESPEvent) {
	defer func() {
		if p := recover(); p != nil {
			logf("!! HandleEvent panic 已隔离: %v", p)
		}
	}()
	payload := ev.Payload
	if bytes.HasPrefix(payload, []byte("SIP/2.0")) {
		return // 响应由各自等待方消费
	}
	headEnd := bytes.Index(payload, []byte("\r\n\r\n"))
	if headEnd < 0 {
		return
	}
	msg := string(payload[:headEnd])
	body := payload[headEnd+4:]

	hVia := sipHeader(msg, "Via")
	hFrom := sipHeader(msg, "From")
	hTo := sipHeader(msg, "To")
	hCallID := sipHeader(msg, "Call-ID")
	hCSeq := sipHeader(msg, "CSeq")

	// 先回 200（响应发往 Via sent-by 端口 = ev.Sport）
	// ★ 所有入站请求一律应答 200 —— P-CSCF 的 OPTIONS 探测/NOTIFY 被静默丢弃,
	//   网络侧可能判会话死亡。
	resp := fmt.Sprintf("SIP/2.0 200 OK\r\nVia: %s\r\nFrom: %s\r\nTo: %s\r\nCall-ID: %s\r\nCSeq: %s\r\nContent-Length: 0\r\n\r\n",
		hVia, hFrom, hTo, hCallID, hCSeq)
	seg := udpPack(net.ParseIP(r.uev6), net.ParseIP(r.pcscf), uePortC, ev.Sport, []byte(resp))[40:]
	r.esp.Send(seg)

	if !bytes.HasPrefix(payload, []byte("MESSAGE")) {
		return // OPTIONS/NOTIFY 等非 MESSAGE 请求: 200 已回, 无后续处理
	}
	if !strings.Contains(sipHeader(msg, "Content-Type"), "vnd.3gpp.sms") || len(body) < 2 {
		return
	}
	// ★ 按 Content-Length 精确截取 SIP body（2026-10-20 实测修复）：
	//   UDP 数据报里 SIP 消息之后常带有多余字节（实测 Content-Length=74 而 body 取到 169B）。
	//   不截取 → RP-DATA 尾部混入噪声，长短信分片(concat)解析错位/UD 越界。
	//   反过来若 body 比 Content-Length 短，则是传输层被截断，记诊断但不强行补。
	if cl := strings.TrimSpace(sipHeader(msg, "Content-Length")); cl != "" {
		if n, e := strconv.Atoi(cl); e == nil {
			if n > 0 && n < len(body) {
				body = body[:n]
			} else if n > len(body) {
				// 收集所有 \r\n\r\n 位置：>1 说明一个 UDP 包里粘了多个 SIP 消息
				pos := []int{}
				for off := 0; len(pos) < 6; {
					p := bytes.Index(payload[off:], []byte("\r\n\r\n"))
					if p < 0 {
						break
					}
					pos = append(pos, off+p)
					off += p + 4
				}
				hls := strings.Split(msg, "\r\n")
				logf("   [诊断] MESSAGE 被截断: CL=%d body=%d payload=%d 头=%dB/%d行 分隔符位置=%v 首行=%q 末行=%q",
					n, len(body), len(payload), headEnd+4, len(hls), pos,
					hls[0], hls[len(hls)-1])
			}
		}
	}
	mti := body[0] & 0x07
	mr := body[1]
	logf("SMS 报文 RP-MTI=%d RP-MR=%d (%dB)", mti, mr, len(body))
	switch mti {
	case 1: // MT 短信 / 送达报告
		sender, text, reportReq, part := parseSmsDeliver(body)
		if sender == "STATUS-REPORT" {
			logf("★ 送达报告: %s", text)
			// STATUS-REPORT 也是 RP-DATA(RP-MTI=1), 必须回 RP-ACK 确认 ——
			// 否则短信中心按重试计划反复重发同一份报告。
			// RP-ACK 不带 DELIVER-REPORT (该 TPDU 只用于 SMS-DELIVER 的妥投回报)。
			ipsmgwSR := strings.Trim(sipHeader(msg, "P-Asserted-Identity"), "<>")
			if ipsmgwSR == "" {
				ipsmgwSR = gSmscPSI
			}
			r.sendRpAck(mr, hCallID, ipsmgwSR, false)
			return
		}
		if sender == "?" {
			// ★ 解析失败：只回 RP-ACK，绝不入库
			//   （借鉴 simadmin sip_listener.rs: 解析失败仍回 ACK，否则网络按重试计划
			//     反复重发同一条；但绝不能把 "UD越界" 这类占位文本写进用户收件箱）
			logf("   [诊断] RP-DATA 解析失败(%s), hex=%s（只回 ACK, 不入库）", text, hex.EncodeToString(body))
			ipsmgw := strings.Trim(sipHeader(msg, "P-Asserted-Identity"), "<>")
			if ipsmgw == "" {
				ipsmgw = gSmscPSI
			}
			r.sendRpAck(mr, hCallID, ipsmgw, false)
			return
		}
		logf("★★ 收到短信 from=%s: %s", sender, text)
		// 长短信重组: 全部到齐才入库+转发 (一条短信一条记录)
		if full, ok := concatSms(imsSlot, sender, part, text); ok {
			text = full
			// ★ 重传去重：网络会重发直到收到 RP-ACK（且重发时 SCTS 可能变），
			//   必须用**拼接后的完整正文**判重——分片正文各不相同，拿它判重必然漏判。
			//   （借鉴 simadmin-volte 的 duplicate_key / recent_seen）
			if mtRecent(sender, text) {
				logf("重传重复(from=%s), 只回 ACK 不再入库", sender)
			} else {
				ts := time.Now().Unix()
				tstr := time.Now().Format("2006-01-02 15:04:05")
				idx := fzsInboxAppend(ts, sender, tstr, text)
				if idx >= 0 {
					fwdqEnqueue(ts, sender, tstr, text, idx)
					mtRemember(sender, text)
					logf("[fzs-bridge] 已入库 fzs-sms 收件箱(模组%s idx=%d) 并转发 → LuCI 短信页/推送渠道可见", imsSlot, idx)
				}
			}
		} else {
			logf("长短信分片 %d/%d 已缓存, 待其余分片到齐后合并入库", part.Seq, part.Total)
		}
		ipsmgw := strings.Trim(sipHeader(msg, "P-Asserted-Identity"), "<>")
		if ipsmgw == "" {
			ipsmgw = gSmscPSI
		}
		r.sendRpAck(mr, hCallID, ipsmgw, reportReq)
	case 3:
		logf("★★★ 网关回执 RP-ACK(N→MS) —— MO 短信已被 IP-SM-GW 接受")
	case 5:
		logf("!! RP-ERROR(N→MS) hex=%x", body)
	}
}

// sendRpAck RP-ACK(MS→N) 确认 MT 短信；reportReq 时带 DELIVER-REPORT TPDU
func (r *Reg) sendRpAck(rpMr byte, inReplyTo, ipsmgw string, reportReq bool) {
	rpAck := []byte{0x02, rpMr}
	if reportReq {
		rpAck = append(rpAck, 0x00, 0x00) // DELIVER-REPORT: MTI=00, FCS=0 妥投
	}
	callID := "ack-" + randHex(8) + "@" + r.uev6
	host := "[" + r.uev6 + "]"
	lines := []string{
		fmt.Sprintf("MESSAGE %s SIP/2.0", ipsmgw),
		fmt.Sprintf("Via: SIP/2.0/UDP %s:%d;branch=z9hG4bK-%s", host, uePortS, randHex(16)),
		"Max-Forwards: 70",
		fmt.Sprintf("From: <%s>;tag=%s", r.fromURI(), randHex(8)),
		fmt.Sprintf("To: <%s>", ipsmgw),
		"Call-ID: " + callID,
		"CSeq: 1 MESSAGE",
	}
	if r.serviceRoute != "" {
		lines = append(lines, "Route: "+r.serviceRoute)
	}
	lines = append(lines, "In-Reply-To: "+inReplyTo,
		"Content-Type: application/vnd.3gpp.sms",
		fmt.Sprintf("Content-Length: %d", len(rpAck)), "", "")
	body := append([]byte(strings.Join(lines, "\r\n")), rpAck...)
	logf("<<< 发 RP-ACK (RP-MR=%d, DELIVER-REPORT=%v)", rpMr, reportReq)
	// RP-ACK 无需等响应(网关回执本就异步), 裸发 —— 避免嵌套等待互吃事件
	seg := udpPack(net.ParseIP(r.uev6), net.ParseIP(r.pcscf), uePortC, r.pcPortS, body)[40:]
	r.esp.Send(seg)
}

// ---- MT 重传去重（借鉴 simadmin-volte: duplicate_key + recent_seen）----
//  网络在收到 RP-ACK 前会按重试计划重发同一条短信（且重发时 TP-SCTS 可能变化），
//  仅靠 SCTS 去重不可靠 ⇒ 用「发件人 + 正文」在 5 分钟窗内判重。
//  注意：只在**完整短信入库后**才记录，长短信的分片正文不记。

var mtSeenMu sync.Mutex
var mtSeen = map[string]time.Time{}

const mtDupWindow = 5 * time.Minute

func mtRecent(sender, text string) bool {
	key := sender + "|" + text
	mtSeenMu.Lock()
	defer mtSeenMu.Unlock()
	now := time.Now()
	for k, t := range mtSeen { // 顺带清理过期项，避免无界增长
		if now.Sub(t) > mtDupWindow {
			delete(mtSeen, k)
		}
	}
	_, ok := mtSeen[key]
	return ok
}

func mtRemember(sender, text string) {
	mtSeenMu.Lock()
	mtSeen[sender+"|"+text] = time.Now()
	mtSeenMu.Unlock()
}
