package main

// aka.go - SIM/USIM/ISIM APDU 通道与身份读取
//
// 双通道（自动择优，上层 API 一致）:
//   ccho - AT+CCHO(开逻辑通道) + AT+CGLA(传 APDU)   [EC200N 实测支持]
//   csim - AT+CSIM(直接传 APDU)                      [EC200T 固件不支持 CCHO 时的退路]
//
// ★ 目标模组是 EC200T（外置卡槽，用户态 IMS 对象）；EC200N 是内置贴片卡（原生支持 VoLTE）。

import (
	"encoding/hex"
	"fmt"
	"strings"
)

const (
	aidUSIM = "A0000000871002" // ADF_USIM (3GPP TS 31.101)
	aidISIM = "A0000000871004" // ADF_ISIM (3GPP TS 31.103)
)

// ============ 底层通道 A: CCHO / CGLA ============

// apduOnce 经 CGLA 透传一次（不处理 61XX）
func apduOnce(a *AT, ch int, apduHex string) (data []byte, sw string, err error) {
	cmd := fmt.Sprintf("AT+CGLA=%d,%d,\"%s\"", ch, len(apduHex), strings.ToUpper(strings.ReplaceAll(apduHex, " ", "")))
	lines, err := a.Cmd(cmd, 5000)
	if err != nil {
		return nil, "", err
	}
	for _, l := range lines {
		if strings.HasPrefix(l, "+CGLA:") {
			v := strings.TrimSpace(strings.TrimPrefix(l, "+CGLA:"))
			parts := strings.SplitN(v, ",", 2)
			if len(parts) == 2 {
				hexStr := strings.Trim(parts[1], "\" ")
				raw, e := hex.DecodeString(hexStr)
				if e != nil || len(raw) < 2 {
					return nil, "", fmt.Errorf("bad CGLA payload")
				}
				return raw[:len(raw)-2], hex.EncodeToString(raw[len(raw)-2:]), nil
			}
		}
	}
	return nil, "", fmt.Errorf("no CGLA response")
}

// apdu 经 CGLA 透传，自动处理 SW=61XX (GET RESPONSE, 最多 3 跳)
func apdu(a *AT, ch int, apduHex string) (data []byte, sw string, err error) {
	data, sw, err = apduOnce(a, ch, apduHex)
	if err != nil {
		return
	}
	for hop := 0; hop < 3 && strings.HasPrefix(sw, "61"); hop++ {
		var more []byte
		le := sw[2:]
		more, sw, err = apduOnce(a, ch, "00C00000"+le)
		if err != nil {
			return
		}
		data = append(data, more...)
	}
	return
}

// ============ 底层通道 B: CSIM ============

// csimAPDU 用 AT+CSIM 直接发 APDU（无需开逻辑通道）
func csimAPDU(a *AT, apduHex string) (data []byte, sw string, err error) {
	apduHex = strings.ToUpper(strings.ReplaceAll(apduHex, " ", ""))
	// Quectel: <length> = 命令的 hex 字符数
	cmd := fmt.Sprintf("AT+CSIM=%d,\"%s\"", len(apduHex), apduHex)
	lines, err := a.Cmd(cmd, 10000)
	if err != nil {
		return nil, "", err
	}
	for _, l := range lines {
		if strings.HasPrefix(l, "+CSIM:") {
			v := strings.TrimSpace(strings.TrimPrefix(l, "+CSIM:"))
			parts := strings.SplitN(v, ",", 2)
			if len(parts) != 2 {
				continue
			}
			hexStr := strings.Trim(parts[1], "\" ")
			raw, e := hex.DecodeString(hexStr)
			if e != nil || len(raw) < 2 {
				return nil, "", fmt.Errorf("bad CSIM payload: %s", l)
			}
			return raw[:len(raw)-2], hex.EncodeToString(raw[len(raw)-2:]), nil
		}
	}
	return nil, "", fmt.Errorf("no CSIM response")
}

// ============ 统一通道抽象 ============

type simCh struct {
	a    *AT
	mode string // "ccho" | "csim"
	ch   int
}

// openSim 打开 SIM 应用并确定可用通道；返回通道对象与 ADF 名
//
// ★ 判定只看响应内容，不看 Cmd 的 error：
//   at.go 的 Cmd 读到 OK/ERROR 就返回（即使同时报 timeout partial），
//   若把 error 当成失败条件，会把「已拿到 +CCHO: n」的情况误判为失败。
func openSim(a *AT) (*simCh, string, error) {
	// ① 先试 CCHO（EC200T/EC200N 实测均支持，响应 +CCHO: <会话号>）
	for _, aid := range []struct{ hex, name string }{{aidUSIM, "USIM"}, {aidISIM, "ISIM"}} {
		lines, e := a.Cmd(fmt.Sprintf("AT+CCHO=\"%s\"", aid.hex), 6000)
		if e != nil && len(lines) == 0 {
			logf("CCHO %s: 无响应 (%v)", aid.name, e)
			continue
		}
		logf("CCHO %s: %v", aid.name, lines)
		for _, l := range lines {
			if strings.HasPrefix(l, "+CCHO:") {
				var ch int
				fmt.Sscanf(strings.TrimPrefix(l, "+CCHO:"), "%d", &ch)
				if ch > 0 {
					logf("SIM 通道: CCHO (channel %d, %s)", ch, aid.name)
					return &simCh{a: a, mode: "ccho", ch: ch}, aid.name, nil
				}
			}
		}
	}
	// ② 退回 CSIM（EC200T 固件不支持，保留以便其他模组）
	for _, aid := range []struct{ hex, name string }{{aidUSIM, "USIM"}, {aidISIM, "ISIM"}} {
		data, sw, e := csimAPDU(a, "00A4000407"+aid.hex)
		logf("CSIM 试 %s: sw=%s data=%dB err=%v", aid.name, sw, len(data), e)
		if sw == "9000" {
			logf("SIM 通道: CSIM (SELECT %s 成功)", aid.name)
			return &simCh{a: a, mode: "csim"}, aid.name, nil
		}
	}
	return nil, "", fmt.Errorf("SIM 访问通道不可用 (CCHO 与 CSIM 均失败)")
}

func (s *simCh) send(apduHex string) ([]byte, string, error) {
	if s.mode == "ccho" {
		return apdu(s.a, s.ch, apduHex)
	}
	return csimAPDU(s.a, apduHex)
}

func (s *simCh) close() {
	if s.mode == "ccho" {
		s.a.Cmd(fmt.Sprintf("AT+CCHC=%d", s.ch), 2000)
	}
}

// ============ 身份读取与派生 ============

// decodeIMSI USIM EF_IMSI (6F07) 的 BCD 解码
// 格式: b[0]=后续有效字节数, 之后每字节 低半字节=前一位/高半字节=后一位, 0xF 为奇数位补位
func decodeIMSI(b []byte) string {
	if len(b) < 2 {
		return ""
	}
	n := int(b[0])
	if n+1 > len(b) {
		n = len(b) - 1
	}
	var sb strings.Builder
	for i := 1; i <= n; i++ {
		lo := b[i] & 0x0F
		hi := (b[i] >> 4) & 0x0F
		if lo <= 9 {
			sb.WriteByte(byte('0' + lo))
		}
		if hi <= 9 {
			sb.WriteByte(byte('0' + hi))
		}
	}
	return sb.String()
}

// deriveRealm 由 IMSI 派生 IMS 域: 46001xxxxx -> ims.mnc001.mcc460.3gppnetwork.org
func deriveRealm(imsi string) string {
	if len(imsi) < 6 {
		return ""
	}
	mcc := imsi[:3]
	mnc := imsi[3:5] // 中国为 2 位 MNC
	return fmt.Sprintf("ims.mnc0%s.mcc%s.3gppnetwork.org", mnc, mcc)
}

// ============ 主流程 ============

// akaProbe 打开 SIM → 读 IMSI → 返回 (IMSI, 摘要, error)
func akaProbe(a *AT) (string, string, error) {
	sc, adf, err := openSim(a)
	if err != nil {
		return "", "", err
	}
	defer sc.close()

	var summary []string
	imsi := ""

	// ★ IMSI 优先用 AT+CIMI（模组已正确解析 BCD）
	//   实测交叉验证：EF_IMSI(6F07) 直读返回 08 49 06 10 47 35 25 02 92
	//   （CRSM 与 CGLA 两条通道结果一致，FCI 确认文件就是 6F07/9B），
	//   但按标准 BCD（低半字节先）解得 9460017453522029，多一个前导 9，
	//   与 AT+CIMI 的 460017453522029 不符 → 以 AT+CIMI 为准。
	if ls, e := a.Cmd("AT+CIMI", 3000); e == nil {
		for _, l := range ls {
			l = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(l), "+CIMI:"))
			if isNum(l) && len(l) >= 14 {
				imsi = l
				summary = append(summary, "IMSI(AT+CIMI)="+l)
			}
		}
	}

	if adf == "ISIM" {
		// SELECT EF_IMPI (6F02)，读内容
		if _, sw, e := sc.send("00A4000402" + "6F02"); e == nil && sw == "9000" {
			if data, _, e := sc.send("00B0000018"); e == nil {
				summary = append(summary, "IMPI="+printable(data))
			}
		} else {
			summary = append(summary, "EF_IMPI select sw="+sw)
		}
		// ISIM 下也尝试取 IMSI（部分卡 ADF 内可见）
		if _, sw, e := sc.send("00A4000402" + "6F07"); e == nil && sw == "9000" {
			if d2, _, e2 := sc.send("00B0000009"); e2 == nil {
				imsi = decodeIMSI(d2)
				summary = append(summary, "EF_IMSI="+imsi)
			}
		}
	} else {
		// USIM: ★ 必须先选 DF_GSM(7F20) 再选 EF_IMSI(6F07)
		//   路径 3F00/7F20/6F07 —— 直选 6F07 会命中错误文件（同样返回 9000，但内容不是 IMSI）
		if _, sw, e := sc.send("00A4000402" + "7F20"); e == nil && sw == "9000" {
			if _, sw2, e2 := sc.send("00A4000402" + "6F07"); e2 == nil && sw2 == "9000" {
				if data, _, e3 := sc.send("00B0000009"); e3 == nil {
					ref := decodeIMSI(data)
					summary = append(summary, fmt.Sprintf("EF_IMSI引用(%dB hex=%X → %s)", len(data), data, ref))
					// ★ 只在 AT+CIMI 未取到时兜底，不覆盖（两者实测不符，以 AT+CIMI 为准）
					if imsi == "" {
						imsi = ref
					}
				}
			} else {
				summary = append(summary, "EF_IMSI select sw="+sw2)
			}
		} else {
			summary = append(summary, "DF_GSM(7F20) select sw="+sw)
		}
		// 兜底: USIM 读不到就用 AT+CIMI
		if imsi == "" {
			if ls, e := a.Cmd("AT+CIMI", 3000); e == nil {
				for _, l := range ls {
					l = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(l), "+CIMI:"))
					if isNum(l) && len(l) >= 14 {
						imsi = l
						summary = append(summary, "IMSI(AT+CIMI)="+l)
					}
				}
			}
		}
	}

	// EF_DIR 摘要 (2FE2) 证明数据通路
	if data, sw, e := sc.send("00A4000402" + "2FE2"); e == nil && sw == "9000" {
		summary = append(summary, fmt.Sprintf("EF_DIR %dB", len(data)))
	}
	if len(summary) == 0 {
		return "", "", fmt.Errorf("no APDU data")
	}
	return imsi, strings.Join(summary, ", "), nil
}

// printable 取可打印 ASCII 片段
func printable(b []byte) string {
	var out []byte
	for _, c := range b {
		if c >= 0x20 && c <= 0x7e {
			out = append(out, c)
		}
	}
	if len(out) == 0 {
		return hex.EncodeToString(b)
	}
	return string(out)
}

func oneLine(lines []string, prefix string) string {
	for _, l := range lines {
		if strings.HasPrefix(l, prefix) {
			return strings.TrimSpace(strings.TrimPrefix(l, prefix))
		}
	}
	return "?"
}
