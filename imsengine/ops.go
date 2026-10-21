package main

// ops.go - 运营商配置加载（v0.9.9w: 外挂优先 + 内嵌兜底)
//
// 🔴 铁律（2026-09-28 主人重申, 此前 main.go 硬编码联通 P-CSCF 被骂）:
//   P-CSCF/短信中心等网络参数一律来自运营商配置, 源码里不得出现任何具体地址。
//   仓库事实源为 conf/operators/*.json, gen_pkg 打包时校验两处 md5 一致防漂移。
//
// ★ v0.9.9s（2026-10-07 主人指令）: 废除设备 /etc/operators/ 参数外挂 ——
//   注册参数只认配置文件（放的都是验证过的运营商参数）; 可外挂的只有
//   P-CSCF 地址表（学习机制 pcscf_learned.json, 属动态数据不是参数）。
//
// ★ v0.9.9w（2026-10-07 主人指令: "清理旧的, 把验证过的放一份, 优先走外挂,
//   不行走内置的兜底"）: 恢复外挂覆盖层, 但做成【外挂优先 + 内嵌兜底】——
//   外挂文件不存在/不合规/不完整时自动回退go:embed 的验证版本, 改坏也注册得上。
//   为什么这次敢开: v0.9.9s 关掉它的真实原因是"盘上留着旧实验文件, 会静默盖掉
//   验证过的参数"(实测 /etc/operators/46000.json 是 3320B 的早期实验版, 而验证
//   版本只有 2061B)。现在用两条护栏根治:
//     ① 只接受"与内嵌副本同源"的干净文件: 属主必须 root、group/other 不可写、
//        且必须能解析出完整配置(缺 P-CSCF 或短信中心 = 不完整 = 拒绝加载);
//     ② 每次加载都打日志说明用的是外挂还是内嵌, 杜绝"盘上一套、跑起来另一套"。
//
// ★ v0.9.9z（主人拍板"肯定要啊, 要不然外挂不等于摆设了吗"）: 新增首刷自动铺种
//   seedOverrides() —— 全新刷机的设备上 /etc/operators 本来是空的, 外挂层形同摆设。
//   现在首刷启动时自动把内嵌验证版铺进去(0600 root) 并落标记 .seeded,
//   之后任何升级都【不覆盖】该目录, 主人为某地运营商做的调试永远不会被冲。
//   想强制重新铺一份: 删掉 .seeded 即可(会丢当前修改, 明确写进标记文件里)。
//
// ★ 漫游 / 非 460 开头(境外卡)暂不处理(主人 2026-10-07 决定): 纯 IMSI 前缀
//   原理上区分不了漫游(漫游时 IMSI 显示的是归属运营商), 遇到非 460 卡会
//   "无匹配配置 → 60s 退出 → watchdog 无限重拉"。要支持得读 visited PLMN。
//
// 按 IMSI 前缀选运营商; 无匹配配置 = 拒绝注册并退出,
// 由 toolbox_start.sh 守护重拉 → 重拉后重新探测读卡 → 换卡自动适配（SIM 自适应）。

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

//go:embed operators/46000.json
var opConf46000 []byte

//go:embed operators/46001.json
var opConf46001 []byte

//go:embed operators/46011.json
var opConf46011 []byte

// OpConf 引擎消费的运营商字段（JSON 里其余字段留给 PC 侧/未来扩展）
type OpConf struct {
	Name          string   `json:"name"`
	ImsiPrefix    string   `json:"imsi_prefix"`                     // 旧: 单前缀（保留兼容）
	ImsiPrefixes  []string `json:"imsi_prefixes"`                   // 新: 同运营商全部 IMSI 子号段（如电信 46011/46003/46005）
	Domain        string   `json:"domain"`
	PcscfFallback []string `json:"pcscf_fallback"`
	PcscfPort     int      `json:"pcscf_port"` // ★ v0.9.9t: 明文 REGISTER 目的端口（0=5060，引擎实测验证值）
	SmscPSI       string   `json:"smsc_psi"`
	ImpuStyle     string   `json:"impu_style"`   // ★ v0.9.9o: imsi | msisdn（规范 02/A10: 业务 From 必须号码制）
	Ipsec         OpIpsec  `json:"ipsec"`        // ★ v0.9.9m: 首个 REGISTER 的 sec-agree 能力声明
	Headers       OpHeaders `json:"headers"`     // ★ v0.9.9n: 头域（Supported/PANI/P-Preferred-Identity/Sec-Client 模板）
	Registration  OpReg     `json:"registration"` // ★ v0.9.9n: 租期等
	Keepalive     OpKeepalive `json:"keepalive"` // ★ v0.9.9t: 保活间隔
}

// OpKeepalive 保活节奏（0 = 引擎缺省，与原写死值一致）
type OpKeepalive struct {
	OptionsIntervalMs int `json:"options_interval_ms"` // OPTIONS 保活间隔（0=60000）
}

// OpHeaders 只挑"引擎需要、且老运营商配置里没有对应键"的头域，
// 这样老运营商（电信/联通）行为零变化，新运营商（移动）可逐项开关。
type OpHeaders struct {
	SecClient           string `json:"sec_client"`              // Security-Client 原始模板（也兼容 ipsec.sec_client）
	SendSupported       string `json:"send_supported"`          // 非空 → 发 Supported: <值>
	PanI                string `json:"pani"`                    // 非空 → 发 P-Access-Network-Info: <值>
	PPreferredIdentity  string `json:"p_preferred_identity"`    // "auto"=用 IMPI；非空 → 发 P-Preferred-Identity
	UserAgent           string `json:"user_agent"`              // 空 = imsengine-v0.4
	ContactTags         string `json:"contact_tags"`            // 空 = 原写死标签
}

// OpReg 注册租期相关
type OpReg struct {
	Expires        int `json:"expires"`         // 申请租期(秒); 0 = 600000
	ContactExpires int `json:"contact_expires"` // >0 → Contact 里带 ;expires=<n>
}

// OpIpsec 首个 REGISTER 里 Security-Client 声明的能力（原来是 reg.go 里的硬编码常量）
//
// ★ v0.9.9m 动机（2026-10-07 主人插移动卡实测）: 移动 P-CSCF 直接回
//   403 Forbidden + Warning "Terminal has used different algorithm from initial register"
//   —— 即"算法与其记录的不一致", 连 401 挑战都不给。算法/加密/是否要 sec-agree
//   必须能按运营商调, 不能再写死。
type OpIpsec struct {
	Alg        string `json:"alg"`         // 完整性算法; 可用逗号分隔多个(如 "hmac-sha-1-96,hmac-md5-96"), 空=默认 hmac-sha-1-96
	Ealg       string `json:"ealg"`        // 加密算法; 可用逗号分隔多个(如 "des-ede3-cbc,aes-cbc"), 空=null
	SecAgree   *bool  `json:"sec_agree"`   // 指针: 缺省(nil)=true; false=首个 REGISTER 完全不带 Require/Proxy-Require/Security-Client(用于探测网络真实期望)
	SecClient  string `json:"sec_client"`  // 原始覆盖串(最高优先级); 支持 {spi_c}/{spi_s}/{port_c}/{port_s} 占位符
	SpiC       uint32 `json:"spi_c"`       // 0 = 23456789
	SpiS       uint32 `json:"spi_s"`       // 0 = 12345678
	UePortTx   int    `json:"ue_port_tx"`  // 0 = 2468
	UePortRx   int    `json:"ue_port_rx"`  // 0 = 1357
	ServerPort int    `json:"server_port"` // 0 = 9900（实际以 Security-Server 的 port-s 为准）
	RandomSpi  bool   `json:"random_spi"`  // true = 随机生成 SPI/端口（SimAdmin 同款）
	PortCLo    int    `json:"port_c_lo"`   // 随机端口下限, 0 = 40000
	PortCHi    int    `json:"port_c_hi"`   // 随机端口上限, 0 = 60000
}

// gOp 当前运营商配置（loadOpConf 成功后有效）
var gOp *OpConf

// gSmscPSI 短信中心 PSI（来自配置, 替代旧硬编码 const）
var gSmscPSI string

// opOverrideDir 运行时覆盖层目录: 人工可改, 首刷时由 seedOverrides 自动铺一份验证版。
//   只保留这一个目录 —— 包内那份随升级解到 /tmp/aos_tmp/operators (tmpfs, 重启即失,
//   且 OEM 解压清单 /usr/aos/package 里根本没有 operators), 不能当权威来源。
var opOverrideDir = "/etc/operators"

// seedMarker 铺种标记。存在 = 已铺过, 之后任何升级都不再动这个目录。
//   想强制重新铺一份(把新验证参数同步到外挂): 删掉这个文件即可, 下次启动会重铺。
const seedMarker = "/etc/operators/.seeded"

// opFiles 内嵌副本 → 外挂文件名(文件名 = IMSI 前缀, 加载时按前缀取)
var opFiles = []struct {
	name string
	raw  []byte
}{
	{"46000.json", opConf46000},
	{"46001.json", opConf46001},
	{"46011.json", opConf46011},
}

// seedOverrides 首刷时把内嵌的验证版参数铺到 /etc/operators/, 让外挂能力开箱可用。
//
//	为什么必须有这一步: 不铺的话全新刷机的设备上该目录是空的, 外挂层等于摆设,
//	所有设备都得人工放一次才生效。
//
//	★ 铁律: 只在标记文件不存在时铺, 且三份全写成功才落标记 ——
//	  升级/换固件一律不再覆盖这个目录, 主人为某地运营商做的调试永远不会被冲掉。
//	  铺完的是内嵌验证副本, 所以"外挂优先"与"内嵌兜底"内容一致, 只是外形上多一条
//	  可编辑的落地路径; 一旦你改了某份, 日志会明确显示用的是【外挂】。
func seedOverrides() {
	if _, err := os.Stat(seedMarker); err == nil {
		return // 已铺过 —— 绝不覆盖
	}
	if err := os.MkdirAll(opOverrideDir, 0755); err != nil {
		logf("运营商配置: 建 %s 失败, 本次不铺种(仍走内嵌兜底): %v", opOverrideDir, err)
		return
	}
	seeded := 0
	for _, f := range opFiles {
		path := filepath.Join(opOverrideDir, f.name)
		if err := os.WriteFile(path, f.raw, 0600); err != nil {
			logf("运营商配置: 铺 %s 失败(已写的保留), 本次不落标记: %v", path, err)
			return
		}
		os.Chmod(path, 0600) // 文件可能已存在且沿用旧权限
		seeded++
	}
	// 三份都成功才落标记 —— 否则下次启动重铺, 不会出现"标记在但文件缺"的半拉子状态
	if err := os.WriteFile(seedMarker, []byte("本目录由 imsengine 首刷铺种(内嵌验证副本)。\n"+
		"可直接编辑本目录里的 <IMSI前缀>.json 调整运营商参数, 改完触发一次重注册生效。\n"+
		"删掉本文件 = 下次启动重新铺一份覆盖(会丢失你的修改)。\n升级固件不会覆盖本目录。\n"),
		0600); err != nil {
		logf("运营商配置: 铺种完成但写标记失败(下次启动会重铺, 无害): %v", err)
		return
	}
	logf("运营商配置: 首刷铺种完成 → %s (%d 份验证参数, 0600 root); 之后升级不再覆盖, "+
		"改完触发一次重注册即生效", opOverrideDir, seeded)
}

// readOverride 读取并校验一份外挂配置。校验不过一律拒绝(返回 err), 绝不"凑合用":
//   ① 必须是普通文件 (拒符号链接/管道/设备, 防指向别处)
//   ② 属主必须 root —— 能改这个文件 = 能把 IMS 流量指到任意 P-CSCF, 权限必须收死
//   ③ group/other 均不可写
// 路径由 IMSI 前缀推导(不接受外部输入), 故无穿越面。
func readOverride(path string) ([]byte, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("不是普通文件 (mode=%s)", fi.Mode())
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return nil, fmt.Errorf("取不到 stat")
	}
	if st.Uid != 0 {
		return nil, fmt.Errorf("属主不是 root (uid=%d)", st.Uid)
	}
	if fi.Mode().Perm()&0o022 != 0 {
		return nil, fmt.Errorf("权限过宽 (mode=%s, group/other 不可写)", fi.Mode().Perm())
	}
	return os.ReadFile(path)
}

// loadOpConf 按 IMSI 前缀匹配运营商配置。
// ★ v0.9.9w: 外挂优先, 内嵌兜底。外挂文件按"内嵌配置声明的前缀"去找同名文件,
//   解析或校验不过就跳过并回退内嵌 —— 改坏一个 json 最坏结果是"用回验证版本",
//   不会出现注册不上(而这正是 v0.9.9s 关掉外挂时想避免的风险)。
func loadOpConf(imsi string) (*OpConf, error) {
	// ---- 1) 外挂覆盖层 (人工/随包放置, 可改) ----
	seen := map[string]bool{}
	for _, raw := range [][]byte{opConf46000, opConf46001, opConf46011} {
		var meta OpConf
		if json.Unmarshal(raw, &meta) != nil {
			continue
		}
		for _, pfx := range append([]string{meta.ImsiPrefix}, meta.ImsiPrefixes...) {
			pfx = strings.TrimSpace(pfx)
			if pfx == "" || seen[pfx] {
				continue
			}
			seen[pfx] = true
			path := filepath.Join(opOverrideDir, pfx+".json")
			rawOv, err := readOverride(path)
			if err != nil {
				// 文件不存在 = 常态(没铺种/被手工删了), 不打日志刷屏;
				// 被护栏拒绝(权限过宽/属主不对/不是普通文件)必须打出来 ——
				// 否则运维只看到"未找到合规外挂文件", 完全不知道是被权限挡了。
				if !os.IsNotExist(err) {
					logf("运营商配置: 外挂 %s 被拒 — %v (已跳过, 继续找)", path, err)
				}
				continue
			}
			if !opPrefixMatch(rawOv, imsi) {
				// 区分"不是本卡运营商"(正常, 静默) 与 "文件本身坏了"(必须报警):
				// 解析失败必须说出来, 否则运维改了配置写成烂 JSON 却毫无察觉。
				var probe OpConf
				if jerr := json.Unmarshal(rawOv, &probe); jerr != nil {
					logf("运营商配置: 外挂 %s 不是合法 JSON, 已跳过 — %v", path, jerr)
				}
				continue
			}
			// 前缀对得上 → 再判完整性(缺 P-CSCF / 缺短信中心 = 不合格)
			if c := matchOpConf(rawOv, imsi); c != nil {
				logf("运营商配置: 采用【外挂】%s (%s · %d 个 P-CSCF 候选 · 内嵌副本为兜底)",
					path, strings.TrimSpace(c.Name), len(c.PcscfFallback))
				return c, nil
			}
			logf("运营商配置: 外挂 %s 前缀匹配但配置不合格(缺 P-CSCF 或短信中心), 继续找下一份", path)
		}
	}

	// ---- 2) 内嵌兜底 (go:embed 的验证版本, 永远保底) ----
	for _, raw := range [][]byte{opConf46000, opConf46001, opConf46011} {
		if c := matchOpConf(raw, imsi); c != nil {
			logf("运营商配置: 采用【内嵌兜底】%s (%s) —— 未找到合规外挂文件",
				strings.TrimSpace(c.Name), strings.Join(prefixList(raw), "/"))
			return c, nil
		}
	}
	pfx := imsi
	if len(pfx) > 5 {
		pfx = pfx[:5]
	}
	return nil, fmt.Errorf("IMSI %s... 无匹配运营商配置 (支持: 移动 46000/46002/46004/46007/46008 · 联通 46001/46006/46009 · 电信 46011/46003/46005)", pfx)
}

// opPrefixMatch 只判断这份配置是否声明了本 IMSI 的前缀, 不做完整性校验。
// 用途: 把"这不是本卡运营商"(正常跳过) 与 "本卡配置坏了"(要报警) 区分开。
func opPrefixMatch(raw []byte, imsi string) bool {
	var c OpConf
	if json.Unmarshal(raw, &c) != nil {
		return false
	}
	if p := strings.TrimSpace(c.ImsiPrefix); p != "" && strings.HasPrefix(imsi, p) {
		return true
	}
	for _, p := range c.ImsiPrefixes {
		if p = strings.TrimSpace(p); p != "" && strings.HasPrefix(imsi, p) {
			return true
		}
	}
	return false
}

// prefixList 仅用于日志: 从内嵌副本里取出该配置覆盖的前缀段
func prefixList(raw []byte) []string {
	var c OpConf
	if json.Unmarshal(raw, &c) != nil {
		return nil
	}
	var out []string
	if p := strings.TrimSpace(c.ImsiPrefix); p != "" {
		out = append(out, p)
	}
	for _, p := range c.ImsiPrefixes {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// matchOpConf 解析并匹配一份配置; 不匹配或配置不完整返回 nil
func matchOpConf(raw []byte, imsi string) *OpConf {
	var c OpConf
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil
	}
	matched := false
	if c.ImsiPrefix != "" && strings.HasPrefix(imsi, c.ImsiPrefix) {
		matched = true
	}
	for _, p := range c.ImsiPrefixes {
		if p != "" && strings.HasPrefix(imsi, p) {
			matched = true
			break
		}
	}
	if !matched {
		return nil
	}
	if len(c.PcscfFallback) == 0 {
		return nil
	}
	if c.SmscPSI == "" {
		return nil
	}
	return &c
}
