package main

// ipv6.go - IPv6 over PPP (proto 0x0057): RS/RA 前缀发现 + 地址组装
// 联通 ims PDN 为纯 IPv6, P-CSCF 亦是 v6

import (
	"encoding/binary"
	"fmt"
	"net"
	"strconv"
	"strings"
)

const (
	protoIPV6 = 0x0057
)

// 自组 IPv6 包 (无扩展头)
func ipv6Pack(src, dst net.IP, nextHdr byte, payload []byte) []byte {
	b := make([]byte, 40)
	b[0] = 0x60 // version
	binary.BigEndian.PutUint16(b[4:6], uint16(len(payload)))
	b[6] = nextHdr
	b[7] = 64 // hop limit
	copy(b[8:24], src.To16())
	copy(b[24:40], dst.To16())
	return append(b, payload...)
}

// 解析 IPv6: 返回 src, dst, nextHdr, payload
func ipv6Parse(b []byte) (src, dst net.IP, next byte, payload []byte) {
	if len(b) < 40 || b[0]>>4 != 6 {
		return nil, nil, 0, nil
	}
	plen := int(binary.BigEndian.Uint16(b[4:6]))
	src = net.IP(append([]byte{}, b[8:24]...))
	dst = net.IP(append([]byte{}, b[24:40]...))
	next = b[6]
	if 40+plen <= len(b) {
		payload = b[40 : 40+plen]
	} else {
		payload = b[40:]
	}
	return
}

// buildRS 构造 Router Solicitation (ICMPv6 type 133)
// option: src link-layer address = interface-id
func buildRS(srcLL []byte) []byte {
	pkt := []byte{133, 0, 0, 0} // type=133 RS, code=0, cksum 占位
	pkt = append(pkt, 0, 0, 0, 0)
	// option 1: source link-layer address (8B)
	pkt = append(pkt, 1, 8)
	pkt = append(pkt, srcLL[:]...)
	// ★ option 0x9D: 3GPP P-CSCF IPv6 Address Request（RFC 3316 / TS 24.301）
	//   不带这个请求，网络不会在 RA 里下发 P-CSCF；
	//   实测 AT+CGCONTRDP 的 ims 承载 P-CSCF 字段为空，只能靠这条请求拿。
	pkt = append(pkt, 0x9D, 1, 0, 0, 0, 0, 0, 0)
	// 校验和: 伪首部 + ICMPv6
	src := linkLocalFromIID(srcLL)
	dst := net.ParseIP("ff02::2")
	cksum := icmpv6Checksum(src, dst, pkt)
	pkt[2] = byte(cksum >> 8)
	pkt[3] = byte(cksum)
	return ipv6Pack(src, dst, 58, pkt)
}

// parseDottedIPv6 解析 Quectel 风格的点分 IPv6（AT+CGCONTRDP 的地址字段）
// 形如 "254.128.0.0.0.0.0.0.0.1.0.1.36.166.36.156" → 16 字节 net.IP
func parseDottedIPv6(s string) net.IP {
	parts := strings.Split(s, ".")
	if len(parts) != 16 {
		return nil
	}
	ip := make(net.IP, 16)
	for i, p := range parts {
		v, err := strconv.Atoi(strings.TrimSpace(p))
		if err != nil || v < 0 || v > 255 {
			return nil
		}
		ip[i] = byte(v)
	}
	return ip
}

// buildEchoReq 构造 ICMPv6 Echo Request (type 128) —— 用于探测链路/路由可达性
func buildEchoReq(src, dst net.IP, id, seq uint16) []byte {
	pkt := []byte{128, 0, 0, 0, byte(id >> 8), byte(id), byte(seq >> 8), byte(seq)}
	pkt = append(pkt, []byte("uvolte-ping-0001")...)
	cksum := icmpv6Checksum(src, dst, pkt)
	pkt[2] = byte(cksum >> 8)
	pkt[3] = byte(cksum)
	return ipv6Pack(src, dst, 58, pkt)
}

func linkLocalFromIID(iid []byte) net.IP {
	ip := make(net.IP, 16)
	ip[0] = 0xFE
	ip[1] = 0x80
	copy(ip[8:], iid)
	return ip
}

// icmpv6Checksum (RFC 4443)
func icmpv6Checksum(src, dst net.IP, icmp []byte) uint16 {
	var sum uint32
	for i := 0; i < 16; i += 2 {
		sum += uint32(src[i])<<8 | uint32(src[i+1])
		sum += uint32(dst[i])<<8 | uint32(dst[i+1])
	}
	// 上层包长度
	sum += uint32(len(icmp))
	sum += 58 // next header ICMPv6
	for i := 0; i < len(icmp); i += 2 {
		if i+1 == len(icmp) {
			sum += uint32(icmp[i]) << 8
		} else {
			sum += uint32(icmp[i])<<8 | uint32(icmp[i+1])
		}
	}
	for sum>>16 != 0 {
		sum = (sum >> 16) + (sum & 0xFFFF)
	}
	return ^uint16(sum)
}

// RAInfo 解析 Router Advertisement: 前缀 + (若带) P-CSCF
type RAInfo struct {
	Prefix net.IP
	PfxLen int
	PCSCF  string   // 3GPP option 0x009D (若 RA 携带)
	Opts   []string // 诊断: 所有选项的 (类型,长度)
}

// parseRA 解析 ICMPv6 RA (type 134)
func parseRA(icmp []byte) *RAInfo {
	if len(icmp) < 16 || icmp[0] != 134 {
		return nil
	}
	ra := &RAInfo{}
	// ★ RA 选项区从第 16 字节开始（不是第 4 字节）：
	//   type(1)+code(1)+cksum(2) = 4B 头
	//   curhoplimit(1)+flags(1)+routerlifetime(2)+reachable(4)+retrans(4) = 12B 固定字段
	//   从 4 开始读会把 curhoplimit 当 option type、flags 当 length(=0) → 立即 break
	i := 16
	for i+2 <= len(icmp) {
		t, l := icmp[i], int(icmp[i+1])*8
		if l == 0 || i+l > len(icmp) {
			break
		}
		opt := icmp[i : i+l]
		ra.Opts = append(ra.Opts, fmt.Sprintf("%d/%d", t, l))
		switch t {
		case 3: // Prefix Information
			if len(opt) >= 32 {
				plen := int(opt[2])
				pfx := net.IP(append([]byte{}, opt[16:32]...))
				ra.Prefix = pfx
				ra.PfxLen = plen
			}
		case 0x9D: // 3GPP P-CSCF IPv6 Address (TS 24.301)
			//   格式: Type(1) + Length(1, 单位8B) + Reserved(2) + 地址(16×N)
			//   故地址从 opt[4] 开始
			for j := 4; j+16 <= l; j += 16 {
				ip := net.IP(append([]byte{}, opt[j:j+16]...))
				if !ip.IsUnspecified() && !ip.IsLinkLocalUnicast() {
					ra.PCSCF = ip.String()
					break
				}
			}
		}
		i += l
	}
	return ra
}

// makeGlobalAddr 前缀 + IID 组全球地址
func makeGlobalAddr(prefix net.IP, plen int, iid []byte) net.IP {
	ip := make(net.IP, 16)
	copy(ip, prefix.To16())
	byteOff := plen / 8
	bitOff := plen % 8
	for i := 0; i < 8 && byteOff+i < 16; i++ {
		ip[16-8+i] = iid[i]
	}
	if bitOff != 0 {
		// 简化: 非整字节前缀少见, 掩码处理
		mask := byte(0xFF) << bitOff
		ip[byteOff] = (ip[byteOff] & mask) | (ip[byteOff] & ^mask)
	}
	return ip
}
