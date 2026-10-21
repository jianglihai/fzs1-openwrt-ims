package main

// learned.go - P-CSCF 自学习持久化 (v0.6.0)
//
// 背景: EC200T BETA 固件不会向网络申请 P-CSCF 下发 (CGCONTRDP p_cscf 字段恒空,
// 实测 2026-09-28), 运营商也不公开 DNS 发现 → 候选链来自配置预置。
// 本文件让引擎"越用越聪明": 注册成功后记住实际可用的 P-CSCF (按 IMSI 前缀),
// 落盘 /etc/pcscf_learned.json (overlay 持久), 下次注册优先试学习值。
// 效果: 同一地点即插即用; 换新地域第一次靠预置候选, 成功后自动记住。

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

const learnedFile = "/etc/pcscf_learned.json"

func loadLearnedAll() map[string][]string {
	m := map[string][]string{}
	data, err := os.ReadFile(learnedFile)
	if err != nil {
		return m
	}
	_ = json.Unmarshal(data, &m)
	return m
}

// imsiKey 同卡精确的键名（带前缀以区别于运营商前缀键）
func imsiKey(imsi string) string { return "imsi:" + imsi }

// loadLearned 取该 IMSI 可用的学习值:
//   ① 同卡精确 ("imsi:<完整IMSI>") 优先 —— 2026-10-20 借 200N 学到的那一组
//   ② 否则运营商前缀 (前 5 位) 兜底
// 调用方 (dedupCandidates) 会再做去重保序, 这里只负责按优先级收集。
func loadLearned(imsi string) []string {
	all := loadLearnedAll()
	out := []string{}
	if imsi != "" {
		for _, a := range all[imsiKey(imsi)] {
			if a != "" {
				out = append(out, a)
			}
		}
	}
	pfx := imsi
	if len(pfx) > 5 {
		pfx = pfx[:5]
	}
	for k, v := range all {
		if strings.HasPrefix(k, "imsi:") { // 精确键已在 ① 处理
			continue
		}
		if k == pfx || strings.HasPrefix(pfx, k) || strings.HasPrefix(k, pfx) {
			for _, a := range v {
				if a != "" {
					out = append(out, a)
				}
			}
		}
	}
	return out
}

// learnPcscfExact 自学习落盘（2026-10-20 借 200N 原生 IMS 学来）:
//   同卡精确一份 + 运营商前缀一份（换同运营商的卡也能兜底命中）。
// 返回最终写入该 IMSI 的候选列表（精确值在前）。
func learnPcscfExact(imsi, iccid string, addrs []string) []string {
	if imsi == "" || len(addrs) == 0 {
		return nil
	}
	m := loadLearnedAll()

	// ① 同卡精确（ICCID 也存一份：换卡槽/换模组时仍认得这张卡）
	//    ★ 直接覆盖，不并旧值：本次是网络下发的全量主/备 P-CSCF，
	//      并旧值会把上一次的脏数据（如误收的 DNS）永久留下。
	for _, key := range []string{imsiKey(imsi)} {
		lst := []string{}
		seen := map[string]bool{}
		for _, a := range addrs {
			if a == "" || seen[a] {
				continue
			}
			seen[a] = true
			lst = append(lst, a)
		}
		if len(lst) > 8 {
			lst = lst[:8]
		}
		m[key] = lst
	}
	if iccid != "" {
		m["iccid:"+iccid] = m[imsiKey(imsi)]
	}

	// ② 运营商前缀兜底：新值置顶
	pfx := imsi
	if len(pfx) > 5 {
		pfx = pfx[:5]
	}
	lst := []string{}
	seen := map[string]bool{}
	for _, a := range addrs {
		if a == "" || seen[a] {
			continue
		}
		seen[a] = true
		lst = append(lst, a)
	}
	for _, a := range m[pfx] {
		if a == "" || seen[a] {
			continue
		}
		seen[a] = true
		lst = append(lst, a)
	}
	if len(lst) > 8 {
		lst = lst[:8]
	}
	m[pfx] = lst

	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return m[imsiKey(imsi)]
	}
	tmp := learnedFile + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		logf("P-CSCF 学习值写入失败: %v", err)
		return m[imsiKey(imsi)]
	}
	if err := os.Rename(tmp, learnedFile); err != nil {
		logf("P-CSCF 学习值落盘失败: %v", err)
		return m[imsiKey(imsi)]
	}
	logf("P-CSCF 自学习已保存: 卡 %s → %v（并按前缀 %s 兜底）", imsi, addrs, pfx)
	return m[imsiKey(imsi)]
}

// learnPcscf 注册成功后记住实际可用的 P-CSCF (去重, 该前缀置顶)
func learnPcscf(imsi, addr string) {
	if imsi == "" || addr == "" {
		return
	}
	pfx := imsi
	if len(pfx) > 5 {
		pfx = pfx[:5]
	}
	m := loadLearnedAll()
	list := []string{addr}
	for _, a := range m[pfx] {
		if a != addr {
			list = append(list, a)
		}
	}
	// 学习列表封顶 8 个, 防膨胀
	if len(list) > 8 {
		list = list[:8]
	}
	m[pfx] = list
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return
	}
	// ★ v0.9.9g: 内容无变化就不写盘 —— 原实现每次成功注册/全量重注册都重写一遍
	//   (每 ~65 分钟一次 jffs2 写), 纯白磨 NOR flash
	if old, e := os.ReadFile(learnedFile); e == nil && bytes.Equal(old, data) {
		return
	}
	tmp := learnedFile + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		logf("P-CSCF 学习值写入失败: %v", err)
		return
	}
	if err := os.Rename(tmp, learnedFile); err != nil {
		logf("P-CSCF 学习值落盘失败: %v", err)
		return
	}
	logf("P-CSCF 学习值已记录: %s → %s", pfx, addr)
}

// ★ v0.9.2 (主人拍板): unlearnPcscf 剔除机制已整体废除 —— 候选只加不删,
//   失败候选留在库里每次注册照常尝试 (200T 拿地址难, 删是帮倒忙)

// dedupCandidates 学习值置顶 + 预置候选, 去重保序
func dedupCandidates(learned, preset []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, c := range append(append([]string{}, learned...), preset...) {
		if c == "" || seen[c] {
			continue
		}
		seen[c] = true
		out = append(out, c)
	}
	return out
}

var _ = fmt.Sprintf // keep fmt import
