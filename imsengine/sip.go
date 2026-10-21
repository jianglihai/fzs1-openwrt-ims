package main

// sip.go - SIP 底层工具（UDP 打包/解析/随机串）
//
// ★ v0.9.9t: 原 SipCfg/buildRegister1 是 v0.4 时代的死代码（无人调用）,
//   里面还留着一套旧硬编码（UA stick-volte / 租期 600000 / alg 在前的 Security-Client）,
//   极易误导维护 —— 已整体删除。REGISTER 构造唯一入口 = reg.go buildRegister（配置驱动）。

import (
	"crypto/rand"
	"encoding/hex"
	"net"
)

func randHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// udpPack 组 UDP+IPv6
func udpPack(src, dst net.IP, sport, dport int, payload []byte) []byte {
	udp := make([]byte, 8)
	binaryEnc := func(b []byte, v uint16) { b[0] = byte(v >> 8); b[1] = byte(v) }
	binaryEnc(udp[0:2], uint16(sport))
	binaryEnc(udp[2:4], uint16(dport))
	binaryEnc(udp[4:6], uint16(8+len(payload)))
	udp[6] = 0
	udp[7] = 0
	full := append(udp, payload...)
	// UDP 校验和 (IPv6 必填): 伪首部
	var sum uint32
	for i := 0; i < 16; i += 2 {
		sum += uint32(src[i])<<8 | uint32(src[i+1])
		sum += uint32(dst[i])<<8 | uint32(dst[i+1])
	}
	sum += uint32(len(full))
	sum += 17
	for i := 0; i < len(full); i += 2 {
		if i+1 == len(full) {
			sum += uint32(full[i]) << 8
		} else {
			sum += uint32(full[i])<<8 | uint32(full[i+1])
		}
	}
	for sum>>16 != 0 {
		sum = (sum >> 16) + (sum & 0xFFFF)
	}
	ck := ^uint16(sum)
	full[6] = byte(ck >> 8)
	full[7] = byte(ck)
	return ipv6Pack(src, dst, 17, full)
}

// udpParse 解析 IPv6/UDP: 返回 sport,dport,payload
func udpParse(b []byte) (sport, dport int, payload []byte, ok bool) {
	_, _, next, pl := ipv6Parse(b)
	if next != 17 || len(pl) < 8 {
		return 0, 0, nil, false
	}
	sport = int(pl[0])<<8 | int(pl[1])
	dport = int(pl[2])<<8 | int(pl[3])
	return sport, dport, pl[8:], true
}
