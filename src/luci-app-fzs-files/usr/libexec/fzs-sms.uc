#!/usr/bin/ucode
// fzs-sms.uc —— 短信解码器与状态机（老 ucode 适配：无 json 模块/typeof，JSON 手写转义）
// 用法:
//   fzs-sms.uc num <号码>            号码 → UCS2 hex（发短信用，CSCS=UCS2 下号码也要 UCS2）
//   fzs-sms.uc txt <文本>            UTF-8 → UCS2 hex
//   fzs-sms.uc poll <cfg> <raw文件>  解析 fzs-at sms list 的原始输出 → 去重/拼接/入库
//   fzs-sms.uc inbox                 收件箱 JSON 对象（msgs + 存储占用 + 守护心跳）

'use strict';
const fs = require('fs');

// ★ 持久化：状态/收件箱放 overlay（/etc/fzs-sms）——重启不丢，恢复出厂才清。
//   瞬态捕获（raw-*/.send/.fifo）仍由调用方放 /tmp，避免 flash 高频写。
const DIR = '/etc/fzs-sms';
const TMP = '/tmp/fzs-sms';
const INBOX = DIR + '/inbox.jsonl';
const OUTBOX = DIR + '/outbox.jsonl';
const ACTIVE = TMP + '/active';        // 前端在短信页时刷新它 → 守护据此进入高频轮询
const INBOX_MAX = 500;
const OUTBOX_MAX = 200;
const SEEN_MAX = 500;
const PARTS_TTL = 3600;
const HEXD = '0123456789ABCDEF';
const HEXL = '0123456789abcdef';

let BYTES = '';
for (let i = 0; i < 256; i++) BYTES += chr(i);

function die(m) { print('fzs-sms.uc: ' + m + '\n'); exit(1); }   // 本版 ucode 无 fprintf；调用方必须检查退出码
function bval(c) { return index(BYTES, c); }
function hexlo(b) { return substr(HEXD, b % 16, 1); }
function hexhi(b) { return substr(HEXD, int(b / 16) % 16, 1); }
function hex2(b) { return hexhi(b) + hexlo(b); }

function hex2bytes(h) {
	let n = length(h), out = [];
	if (n % 2 != 0) return null;
	for (let i = 0; i < n; i += 2) {
		let hi = index(HEXD, substr(h, i, 1));
		if (hi < 0) hi = index(HEXL, substr(h, i, 1));
		let lo = index(HEXD, substr(h, i + 1, 1));
		if (lo < 0) lo = index(HEXL, substr(h, i + 1, 1));
		if (hi < 0 || lo < 0) return null;
		push(out, hi * 16 + lo);
	}
	return out;
}

function utf8enc(cp) {
	if (cp < 0x80) return chr(cp);
	if (cp < 0x800) return chr(0xC0 + int(cp / 64)) + chr(0x80 + cp % 64);
	if (cp < 0x10000) return chr(0xE0 + int(cp / 4096)) + chr(0x80 + int(cp / 64) % 64) + chr(0x80 + cp % 64);
	return chr(0xF0 + int(cp / 262144)) + chr(0x80 + int(cp / 4096) % 64) + chr(0x80 + int(cp / 64) % 64) + chr(0x80 + cp % 64);
}

function utf8dec(s) {
	let out = '', i = 0, n = length(s);
	while (i < n) {
		let b = bval(substr(s, i, 1));
		if (b < 0x80) { out += substr(s, i, 1); i += 1; }
		else if (b < 0xE0) { out += utf8enc((b - 0xC0) * 64 + (bval(substr(s, i + 1, 1)) - 0x80)); i += 2; }
		else if (b < 0xF0) { out += utf8enc((b - 0xE0) * 4096 + (bval(substr(s, i + 1, 1)) - 0x80) * 64 + (bval(substr(s, i + 2, 1)) - 0x80)); i += 3; }
		else { out += utf8enc((b - 0xF0) * 262144 + (bval(substr(s, i + 1, 1)) - 0x80) * 4096 + (bval(substr(s, i + 2, 1)) - 0x80) * 64 + (bval(substr(s, i + 3, 1)) - 0x80)); i += 4; }
	}
	return out;
}

function ucs2dec(hex) {
	let b = hex2bytes(hex);
	if (b == null) return '';
	let out = '';
	for (let i = 0; i + 1 < length(b); i += 2) out += utf8enc(b[i] * 256 + b[i + 1]);
	return out;
}

function utf8_to_ucs2hex(s) {
	let out = '', i = 0, n = length(s);
	while (i < n) {
		let b = bval(substr(s, i, 1));
		let cp;
		if (b < 0x80) { cp = b; i += 1; }
		else if (b < 0xE0) { cp = (b - 0xC0) * 64 + (bval(substr(s, i + 1, 1)) - 0x80); i += 2; }
		else if (b < 0xF0) { cp = (b - 0xE0) * 4096 + (bval(substr(s, i + 1, 1)) - 0x80) * 64 + (bval(substr(s, i + 2, 1)) - 0x80); i += 3; }
		else { cp = (b - 0xF0) * 262144 + (bval(substr(s, i + 1, 1)) - 0x80) * 4096 + (bval(substr(s, i + 2, 1)) - 0x80) * 64 + (bval(substr(s, i + 3, 1)) - 0x80); i += 4; }
		out += hex2(int(cp / 256) % 256) + hex2(cp % 256);
	}
	return out;
}

// GSM 03.38 基本字符集 + 0x1B 扩展表
const GSM7 = [
	'@','£','$','¥','è','é','ù','ì','ò','Ç','\n','Ø','ø','\r','Å','å',
	'Δ','_','Φ','Γ','Λ','Ω','Π','Ψ','Σ','Θ','Ξ','Æ','æ','ß','É',' ',
	'!','"','#','¤','%','&',"'",'(',')','*','+',',','-','.','/',
	'0','1','2','3','4','5','6','7','8','9',':',';','<','=','>','?',
	'¡','A','B','C','D','E','F','G','H','I','J','K','L','M','N','O',
	'P','Q','R','S','T','U','V','W','X','Y','Z','Ä','Ö','Ñ','Ü','§',
	'¿','a','b','c','d','e','f','g','h','i','j','k','l','m','n','o',
	'p','q','r','s','t','u','v','w','x','y','z','ä','ö','ñ','ü','à'
];
const GSM7EXT = [];
GSM7EXT[0x0A] = '\x0C';
GSM7EXT[0x14] = '^';
GSM7EXT[0x28] = '{';
GSM7EXT[0x29] = '}';
GSM7EXT[0x2F] = '\\';
GSM7EXT[0x3C] = '[';
GSM7EXT[0x3D] = '~';
GSM7EXT[0x3E] = ']';
GSM7EXT[0x40] = '|';
GSM7EXT[0x65] = '€';

function gsm7_decode(bytes, nseptets) {
	let out = '', ext = false;
	for (let k = 0; k < nseptets; k++) {
		let bytepos = int(k * 7 / 8);
		let shift = (k * 7) % 8;
		if (bytepos >= length(bytes)) break;
		let v = (bytes[bytepos] >> shift) & 0x7F;
		if (shift > 1 && bytepos + 1 < length(bytes)) v |= (bytes[bytepos + 1] << (8 - shift)) & 0x7F;
		if (ext) { if (GSM7EXT[v] != null) out += GSM7EXT[v]; ext = false; continue; }
		if (v == 0x1B) { ext = true; continue; }
		out += GSM7[v];
	}
	return out;
}

// OA 半八位组数字（0x81/0x91）
function oa_digits(b, len) {
	let out = '';
	for (let i = 0; i < len; i++) {
		let d = (i % 2 == 0) ? b[int(i / 2)] % 16 : int(b[int(i / 2)] / 16);
		if (d == 15) break;
		out += substr(HEXD, d, 1);
	}
	return out;
}

function scts_str(b) {
	if (length(b) < 7) return '';
	function sw(x) { return substr(HEXD, x % 16, 1) + substr(HEXD, int(x / 16), 1); }
	return '20' + sw(b[0]) + '-' + sw(b[1]) + '-' + sw(b[2]) + ' ' + sw(b[3]) + ':' + sw(b[4]) + ':' + sw(b[5]);
}

function parse_deliver(pdu) {
	let i = 0;
	let sclen = pdu[i];
	i += 1 + sclen;
	if (i >= length(pdu)) return null;
	let first = pdu[i]; i += 1;
	if ((first & 3) != 0) return null;   // 只收 DELIVER（状态报告等跳过）
	let udhi = (first & 0x40) != 0;
	let oalen = pdu[i]; i += 1;
	let oaty = pdu[i]; i += 1;
	let onb = int((oalen + 1) / 2);
	if (i + onb > length(pdu)) return null;
	let oabytes = slice(pdu, i, i + onb); i += onb;
	let sender = '';
	// ★★ 2026-10-07 晚 代码审查修复：字母数字发件人（TP-OA 类型 0xD0，
	//   如 "10086"、企业服务号名）的 septet 数**算法写错了**。
	//   规范：`<oalen>` 是**半字节（semi-octet）个数** = 地址字节数 × 2；
	//   而 GSM7 的 septet 数 = floor(半字节数 × 4 / 7)。
	//   旧代码写成 `oalen * 7 / 8`（把半字节数当字节数），
	//   → 传长度不对 → gsm7_decode 读出多余/缺失字符 → **发件人显示乱码**。
	if ((oaty & 0xF0) == 0xD0) sender = gsm7_decode(oabytes, int(oalen * 4 / 7));
	else sender = oa_digits(oabytes, oalen);
	i += 2;   // TP-PID, TP-DCS
	let dcs = pdu[i - 1];
	if (i + 7 > length(pdu)) return null;
	let ts = scts_str(slice(pdu, i, i + 7)); i += 7;
	let udl = pdu[i]; i += 1;
	let ud = slice(pdu, i);
	let udh_len = 0, ref = null, total = 1, seq = 1;
	if (udhi && length(ud) > 0) {
		udh_len = ud[0];
		let udh = slice(ud, 1, 1 + udh_len);
		for (let p = 0; p + 1 < length(udh); ) {
			// ★ 两种「长短信拼接」IE 都要认（2026-10-07 实测踩过）：
			//   旧代码只认 IEI=0x00（8 位引用号、3 字节值），而中国移动 10086 的长回复用的是
			//   IEI=0x08（16 位引用号、4 字节值：ref 2 字节 + 总段数 + 段序号）。
			//   不认 0x08 时：ref=null/total=1 → 每段被当成独立单条 → 用户看到"长短信被砍成两条、
			//   中间与末尾断字"（实测 10086 引用回复复现；原文完整，是我们的 bug）。
			if (udh[p] == 0x08 && p + 5 < length(udh)) {
				// 16 位引用号 → ref 加 256 偏移，避免与 8 位引用号（≤255）在同一 (发送方,总段数)
				// 命名空间里撞键；同时不改变既有 8 位条目的键值（兼容已入库的 seen 记录）
				ref = 256 + udh[p + 2] * 256 + udh[p + 3];
				total = udh[p + 4]; seq = udh[p + 5];
				break;
			}
			if (udh[p] == 0x00 && p + 4 < length(udh)) {
				ref = udh[p + 2]; total = udh[p + 3]; seq = udh[p + 4];
				break;
			}
			p += 2 + udh[p + 1];
		}
		ud = slice(ud, 1 + udh_len);
		if ((dcs & 0x0C) == 0x00) udl -= int(((udh_len + 1) * 8 + 6) / 7);
	}
	let content = '';
	let enc = dcs & 0x0C;
	if (enc == 0x08) {
		let h = '';
		for (let x = 0; x < udl && x < length(ud); x++) h += hex2(ud[x]);
		content = ucs2dec(h);
	} else if (enc == 0x00) {
		content = gsm7_decode(ud, udl);
	} else {
		content = '(二进制内容 ' + length(ud) + ' 字节)';
	}
	return { sender: sender, time: ts, ref: ref, total: total, seq: seq, content: content };
}

function jesc(s) {
	if (s == null) return '';
	let out = '';
	for (let i = 0; i < length(s); i++) {
		let c = substr(s, i, 1);
		if (c == '"') out += '\\"';
		else if (c == '\\') out += '\\\\';
		else if (c == '\n') out += '\\n';
		else if (c == '\r') out += '\\r';
		else if (c == '\t') out += '\\t';
		else out += c;
	}
	return out;
}

function hstr(s) {
	let h = 0;
	for (let i = 0; i < length(s); i++) h = (h * 31 + bval(substr(s, i, 1))) % 1000000007;
	return '' + h;
}

function readlines(p) {
	if (!fs.access(p)) return [];
	let raw = fs.readfile(p);
	let out = [];
	let ls = split(raw, '\n');
	for (let i = 0; i < length(ls); i++) {
		let L = ls[i];
		while (length(L) > 0 && (bval(substr(L, length(L) - 1, 1)) == 13 || substr(L, length(L) - 1, 1) == ' ' || substr(L, length(L) - 1, 1) == '\n')) L = substr(L, 0, length(L) - 1);
		if (L != '') push(out, L);
	}
	return out;
}

// 原子写：临时文件 + rename，杜绝前端读到半截文件（实测踩过：竞态导致 JSON 断裂）
function awrite(p, data) {
	fs.writefile(p + '.tmp', data);
	fs.rename(p + '.tmp', p);
}

function seen_old_has(lines, key) {
	for (let i = 0; i < length(lines); i++) if (lines[i] == key) return true;
	return false;
}

function replace_all(s, a, b) {
	let out = '';
	for (let i = 0; i < length(s); i++) {
		if (substr(s, i, 1) == a) out += b; else out += substr(s, i, 1);
	}
	return out;
}

function upcase(s) {
	let out = '';
	for (let i = 0; i < length(s); i++) {
		let c = substr(s, i, 1);
		let lo = index(HEXL, c);
		out += (lo >= 0) ? substr(HEXD, lo, 1) : c;
	}
	return out;
}

// 老版没有 trim；顺带剥 \n（fs.readfile 的行尾）
function trimc(s) {
	s = '' + s;
	let a = 0, b = length(s);
	while (a < b && (substr(s, a, 1) == ' ' || bval(substr(s, a, 1)) == 13 || bval(substr(s, a, 1)) == 10)) a += 1;
	while (b > a && (substr(s, b - 1, 1) == ' ' || bval(substr(s, b - 1, 1)) == 13 || bval(substr(s, b - 1, 1)) == 10)) b -= 1;
	return substr(s, a, b - a);
}

function cmd_num(n) {
	print(utf8_to_ucs2hex(n) + '\n');
}

function cmd_txt(t) {
	print(utf8_to_ucs2hex(t) + '\n');
}

function cmd_poll(cfg, rawfile) {
	let now = time();
	let raw = fs.readfile(rawfile);
	let lines = split(raw, '\n');
	let msgs = [], usage = '';
	for (let li = 0; li < length(lines); li++) {
		let L = lines[li];
		while (length(L) > 0 && bval(substr(L, length(L) - 1, 1)) == 13) L = substr(L, 0, length(L) - 1);
		if (substr(L, 0, 7) == '+CMGL: ') {
			// 头部第 4 字段 = <length>，但它是【TPDU 长度、不含 SMSC 段】；
			// 完整 PDU = SMSC长度字节(1) + SMSC 段(首字节值) + TPDU。
			// ★ 2026-10-07 实测踩过：直接按 <length> 截断会砍掉尾部 9 字节
			//   → 每条短信末尾少 4~5 个字（10086 长回复 + 那条 DeepSeek 短信都中招）。
			// PDU 行按 2×完整字节数 截取——回显风暴的尾巴（如 "...CAT"）会粘在行尾，
			// 按长度截断正好把垃圾裁掉（实测踩过）。
			let hf = split(substr(L, 7), ',');
			let decl = int(trimc(hf[3]));
			let expect = 2 * (decl + 1);
			for (let lj = li + 1; lj < length(lines); lj++) {
				let t = lines[lj];
				while (length(t) > 0 && bval(substr(t, length(t) - 1, 1)) == 13) t = substr(t, 0, length(t) - 1);
				let clean = '';
				for (let x = 0; x < length(t); x++) { let c = substr(t, x, 1); if (c != ' ') clean += c; }
				if (clean == '') continue;
				// 用 PDU 首字节（SMSC 长度）把 <length> 补全成完整字节数
				if (length(clean) >= 2) { let fb = hex2bytes(substr(clean, 0, 2)); if (fb != null) expect = 2 * (decl + 1 + fb[0]); }
				let chk = (expect > 0 && length(clean) > expect) ? substr(clean, 0, expect) : clean;
				let ishex = true;
				for (let x = 0; x < length(chk); x++) {
					let c = substr(chk, x, 1);
					if (index(HEXD, c) < 0 && index(HEXL, c) < 0) { ishex = false; break; }
				}
				if (ishex) {
					push(msgs, { idx: trimc(hf[0]), stat: trimc(hf[1]), pdu: upcase(chk) });
					li = lj;
				}
				break;
			}
		} else if (substr(L, 0, 7) == '+CPMS: ') {
			let f = split(substr(L, 7), ',');
			if (length(f) >= 3) usage = trimc(f[1]) + '/' + trimc(f[2]);
		}
	}

	// seen：每次以「本轮仍在存储里的 (idx,内容哈希)」全量重建
	// ★ 2026-10-07 性能优化（实测单轮 8 秒 → 目标 0.5 秒）：
	//   ① 每条 PDU 只解码一次并缓存 —— 原先「重建 seen」和「真正解析」各解一遍，
	//      存储里 17 条就要解 34 次，在 580MHz 上要 3~5 秒，直接把轮询拖到 8 秒；
	//   ② stat=2/3 是「我们自己发出去的副本」(STO UNSENT / STO SENT)，
	//      只清不解析（它们本来就不该来占模组存储）。
	let parsed = [];
	let seen = {};
	for (let i = 0; i < length(msgs); i++) {
		parsed[i] = null;
		let st = msgs[i].stat;
		if (st == '2' || st == '3') continue;        // 已发副本：不解析
		let b = hex2bytes(msgs[i].pdu);
		let d = (b != null) ? parse_deliver(b) : null;
		parsed[i] = d;
		if (d == null || d.content == '') continue;
		seen[msgs[i].idx + ':' + hstr(d.sender + '|' + d.content + '|' + (d.ref == null ? '' : '' + d.ref))] = 1;
	}

	let oldseen = readlines(DIR + '/seen-' + cfg);
	let dup_old = [];   // 历史上已入库、却仍留在存储里的索引（待清理）
	let newcnt = 0;
	let inbox_add = [];
	let parts = {};   // key -> {ts, time, sender, total, segs:{seq:ucs2hex}}
	let partsorder = [];

	// 拼接状态：对每个「本轮在存储里的消息」看它是否是某条未完成长短信的一段
	let oldparts = readlines(DIR + '/parts-' + cfg);
	for (let i = 0; i < length(oldparts); i++) {
		let f = split(oldparts[i], '|');
		if (length(f) >= 7) {
			let key = f[0] + '|' + f[1] + '|' + f[2];
			if (parts[key] == null) { parts[key] = { ts: f[3], time: f[4], sender: f[1], total: int(f[2]), segs: {} }; push(partsorder, key); }
			parts[key].segs[int(f[5])] = f[6];
		}
	}

	for (let i = 0; i < length(msgs); i++) {
		let d = parsed[i];                        // 用第一遍的缓存，不再重复解码
		if (d == null || d.content == '') continue;
		let key = msgs[i].idx + ':' + hstr(d.sender + '|' + d.content + '|' + (d.ref == null ? '' : '' + d.ref));
		if (seen_old_has(oldseen, key)) { push(dup_old, msgs[i].idx); continue; }
		newcnt += 1;
		let cfrom = replace_all(d.sender, '|', '/');
		if (d.total > 1 && d.ref != null) {
			let pkey = d.ref + '|' + cfrom + '|' + d.total;
			if (parts[pkey] == null) { parts[pkey] = { ts: now, time: d.time, sender: cfrom, total: d.total, segs: {} }; push(partsorder, pkey); }
			parts[pkey].segs[d.seq] = utf8_to_ucs2hex(d.content);
			let have = 0;
			for (let s = 1; s <= parts[pkey].total; s++) if (parts[pkey].segs[s] != null) have += 1;
			if (have == parts[pkey].total) {
				let full = '';
				for (let s = 1; s <= parts[pkey].total; s++) full += ucs2dec(parts[pkey].segs[s]);
				push(inbox_add, { ts: now, modem: cfg, idx: msgs[i].idx, sender: cfrom, time: parts[pkey].time, content: full });
				delete parts[pkey];
			}
		} else {
			push(inbox_add, { ts: now, modem: cfg, idx: msgs[i].idx, sender: cfrom, time: d.time, content: d.content });
		}
	}

	// 过期拼接丢弃
	for (let i = 0; i < length(partsorder); i++) {
		let k = partsorder[i];
		if (parts[k] != null && now - int(parts[k].ts) > PARTS_TTL) delete parts[k];
	}

	// seen 上限保护
	let seenlines = [];
	for (let k in seen) { push(seenlines, k); if (length(seenlines) >= SEEN_MAX) break; }
	awrite(DIR + '/seen-' + cfg, join('\n', seenlines) + (length(seenlines) > 0 ? '\n' : ''));

	// 拼接状态回写
	let plines = [];
	for (let i = 0; i < length(partsorder); i++) {
		let k = partsorder[i];
		if (parts[k] == null) continue;
		let p = parts[k];
		for (let s in p.segs) push(plines, k + '|' + p.ts + '|' + p.time + '|' + s + '|' + p.segs[s]);
	}
	awrite(DIR + '/parts-' + cfg, join('\n', plines) + (length(plines) > 0 ? '\n' : ''));

	// 入库（追加 + 截断到 200 条）
	if (length(inbox_add) > 0) {
		let old = readlines(INBOX);
		for (let i = 0; i < length(inbox_add); i++) {
			let m = inbox_add[i];
			push(old, '{"ts":' + m.ts + ',"modem":"' + substr(m.modem, 5) + '","idx":' + m.idx + ',"sender":"' + jesc(m.sender) + '","time":"' + jesc(m.time) + '","content":"' + jesc(m.content) + '"}');
		}
		while (length(old) > INBOX_MAX) {
			let nl = [];
			for (let i = length(old) - INBOX_MAX; i < length(old); i++) push(nl, old[i]);
			old = nl;
		}
		awrite(INBOX, join('\n', old) + '\n');

		// ★ 转发队列：每条新短信写一个文件（第 1 行发件人 / 第 2 行时间 / 第 3 行起正文）。
		//   用「一文件一条」而非行式队列 —— 正文里的换行、竖线等一律无需转义，
		//   守护侧 head/sed/tail 就能取字段，零解析、零歧义。
		fs.mkdir(DIR + '/fwdq');
		for (let i = 0; i < length(inbox_add); i++) {
			let m = inbox_add[i];
			awrite(DIR + '/fwdq/' + m.ts + '-' + substr(m.modem, 5) + '-' + m.idx, m.sender + '\n' + m.time + '\n' + m.content);
		}
	}

	awrite(DIR + '/usage-' + cfg, (usage == '' ? '?/?' : usage) + '\n');
	// ★ 已入库条目的存储索引：守护据此从 SIM/模组删除（防存储积满 → 拒收新短信）。
	//   只列「本轮真正入库」的——分片未拼齐的段不删，留待下轮续拼（否则丢分片）。
	//   删除清单 = ① 本轮新入库的 ② 历史上已入库却仍留着的（旧版 keep=1 时期残留，
	//   之后永远不会再清，会一直拖慢每轮解析）③ stat=2/3 的已发副本。
	//   未拼齐的分片不在其中（仍留待续拼）；keep_in_storage=1 时守护不执行删除，故列全无害。
	let delidx = [];
	for (let i = 0; i < length(inbox_add); i++) push(delidx, inbox_add[i].idx);
	for (let i = 0; i < length(dup_old); i++) push(delidx, dup_old[i]);
	for (let i = 0; i < length(msgs); i++) {
		let st = msgs[i].stat;
		if (st == '2' || st == '3') push(delidx, msgs[i].idx);
	}
	print('{"new":' + newcnt + ',"usage":"' + jesc(usage) + '","del":"' + join(',', delidx) + '"}\n');
}

function cmd_inbox() {
	let now = time();
	// ★ 页面活跃标志：前端每次拉取收件箱都会刷新它（见 sms.js 的 10 秒轮询）。
	//   守护据此把轮询间隔从 30 秒降到 5 秒 —— 用户在短信页时收得更快，
	//   离开页面 30 秒后自动回落，不影响面板其它页面的 AT 操作。
	awrite(ACTIVE, '' + now + '\n');
	let lines = readlines(INBOX);
	// ★ 心跳在 /tmp（tmpfs）：它每轮都更新，放 /etc 会磨闪存（见 fzs-smsd 注释）
	let lp = trimc(fs.readfile('/tmp/fzs-sms/lastpoll'));
	if (lp == '' || lp == 'null') lp = '0';
	let u1 = trimc(fs.readfile(DIR + '/usage-modem1'));
	let u2 = trimc(fs.readfile(DIR + '/usage-modem2'));
	if (u1 == '' || u1 == 'null') u1 = '—';
	if (u2 == '' || u2 == 'null') u2 = '—';
	let sent = readlines(OUTBOX);
	print('{"lastpoll":' + lp + ',"now":' + now + ',"usage":{"modem1":"' + jesc(u1) + '","modem2":"' + jesc(u2) + '"},"msgs":[' + join(',', lines) + '],"sent":[' + join(',', sent) + ']}\n');
}

// 清空收件箱（设备库；不动模组/SIM 存储）
function cmd_clear() {
	awrite(INBOX, '');
	print('{"ok":true}\n');
}

// 清空已发送留底
function cmd_clearout() {
	awrite(OUTBOX, '');
	print('{"ok":true}\n');
}

// URL 百分号编码（UTF-8 逐字节）——转发渠道的 URL 参数要用
function cmd_urlenc(s) {
	let out = '';
	for (let i = 0; i < length(s); i++) {
		let b = bval(substr(s, i, 1));
		if ((b >= 48 && b <= 57) || (b >= 65 && b <= 90) || (b >= 97 && b <= 122) || b == 45 || b == 46 || b == 95 || b == 126) {
			out += substr(s, i, 1);
		} else {
			out += '%' + hex2(b);
		}
	}
	print(out + '\n');
}

// 通用 webhook 的 JSON 体
function cmd_fwdjson(sender, t, content) {
	print('{"sender":"' + jesc(sender) + '","time":"' + jesc(t) + '","content":"' + jesc(content) + '"}\n');
}

// ★★ 各推送渠道的请求体构造（2026-10-07 晚加，0.2.5-x 起）
//    背景：钉钉/飞书/企微 群机器人**都要求 POST + application/json**，
//    且 JSON 结构各不相同（见各家官方文档）。设备上原本的 uclient-fetch
//    把 Content-Type 写死成 urlencoded、发不了 JSON，已确认改用 GNU wget-ssl
//    （--header 可自定义，实测通过）。这里只负责**拼出各家要的 JSON**，
//    具体的发送由 shell 侧（fzs-fwd）用 wget 完成 —— 职责分离，加渠道不改这里。
//
//    入参：chan = 渠道名；sender/time/content = 三条原始信息
//    出参：stdout 一行 JSON（已做转义，可直接作为 --post-data）
//    ★ 转义用 jesc()，短信正文里的引号/反斜杠/换行都会被正确转义。
function cmd_fwdbody(chan, sender, t, content) {
	let txt = sender + '\n' + t + '\n' + content;
	let e = jesc(txt);
	let r = '';
	switch (chan) {
	case 'wecom-bot':      // 企业微信群机器人
		r = '{"msgtype":"text","text":{"content":"' + e + '"}}';
		break;
	case 'dingtalk-bot':   // 钉钉群机器人（at 段给出但 isAtAll=false，不发 @）
		r = '{"msgtype":"text","text":{"content":"' + e + '"},"at":{"isAtAll":false}}';
		break;
	case 'feishu-bot':     // 飞书群机器人
		r = '{"msg_type":"text","content":{"text":"' + e + '"}}';
		break;
	case 'webhook':        // 通用 webhook（保持旧格式，兼容已有用户）
		r = '{"sender":"' + jesc(sender) + '","time":"' + jesc(t) + '","content":"' + jesc(content) + '"';
		r += ',"text":"' + e + '"}';
		break;
	default:
		r = '{"text":"' + e + '"}';
		break;
	}
	print(r + '\n');
}

// 从收件箱移除一条记录（按 模组+索引 匹配）。
// 说明：自动清理开启时该条可能已不在存储里，但收件箱记录仍在，所以删除必须落到记录上，
// 否则用户会看到「删了还在」。存储侧的删除由 fzs-at 尽力而为（失败不影响移除记录）。
function cmd_drop(cfgmodem, idx) {
	let md = (substr('' + cfgmodem, 0, 5) == 'modem') ? substr(cfgmodem, 5) : cfgmodem;
	idx = trimc(idx);
	let lines = readlines(INBOX);
	let out = [];
	for (let i = 0; i < length(lines); i++) {
		let L = lines[i];
		if (index(L, '"modem":"' + md + '"') >= 0 && index(L, '"idx":' + idx + ',') >= 0) continue;
		push(out, L);
	}
	awrite(INBOX, (length(out) > 0 ? join('\n', out) + '\n' : ''));
	print('{"ok":true}\n');
}

// 从已发送留底移除一条（按 时间戳+模组+号码 匹配，只删第一条）。
// 说明：留底记录没有唯一索引；时间戳为秒级、同一秒内向同一号码发两条几乎不可能，
// 这三个字段的组合足以定位。只删第一条匹配，避免误删其余记录。
function cmd_dropout(ts, cfgmodem, num) {
	ts = trimc('' + ts);
	let md = (substr('' + cfgmodem, 0, 5) == 'modem') ? substr(cfgmodem, 5) : trimc('' + cfgmodem);
	let nj = jesc('' + num);
	let lines = readlines(OUTBOX);
	let out = [];
	let hit = 0;
	for (let i = 0; i < length(lines); i++) {
		let L = lines[i];
		if (hit == 0 && index(L, '"ts":' + ts + ',') >= 0 && index(L, '"modem":"' + md + '"') >= 0 && index(L, '"to":"' + nj + '"') >= 0) {
			hit = 1;
			continue;
		}
		push(out, L);
	}
	awrite(OUTBOX, (length(out) > 0 ? join('\n', out) + '\n' : ''));
	print('{"ok":true,"removed":' + hit + '}\n');
}

function cmd_outbox(cfg, num, text) {
	let md = (substr(cfg, 0, 5) == 'modem') ? substr(cfg, 5) : cfg;
	let lines = readlines(OUTBOX);
	push(lines, '{"ts":' + time() + ',"modem":"' + md + '","to":"' + jesc(num) + '","content":"' + jesc(text) + '"}');
	while (length(lines) > OUTBOX_MAX) {
		let nl = [];
		for (let i = length(lines) - OUTBOX_MAX; i < length(lines); i++) push(nl, lines[i]);
		lines = nl;
	}
	awrite(OUTBOX, join('\n', lines) + '\n');
}

// PDU 发送：号码半八位组显式编码，零歧义（文本模式 + CSCS=UCS2 的号码编码实测会静默发错）
// csca 非空时显式编入 SMSC 字段（本固件对空 SMSC 的提交回 CMS 304，实测踩过）
// 输出：每行 "<AT+CMGS的字节数> <PDU hex>"，超过 70 字自动分条（每段 65 字 + " (n/m)" 后缀）
function cmd_pdu(num, text, csca) {
	let digits = '' + num, toa = 0x81;
	if (substr(digits, 0, 1) == '+') { toa = 0x91; digits = substr(digits, 1); }
	if (length(digits) < 5) die('号码太短');
	for (let i = 0; i < length(digits); i++) if (index('0123456789', substr(digits, i, 1)) < 0) die('号码含非数字');
	let sw = '';
	for (let i = 0; i < length(digits); i += 2) {
		let a = index('0123456789', substr(digits, i, 1));
		let bc = (i + 1 < length(digits)) ? substr(digits, i + 1, 1) : 'F';
		let b = (bc == 'F') ? 15 : index('0123456789', bc);
		sw += hex2(b * 16 + a);
	}
	let smsc = '';
	if (csca != null && csca != '') {
		let d = replace_all('' + csca, '+', '');
		let dd = '';
		for (let i = 0; i < length(d); i++) { let c = substr(d, i, 1); if (index('0123456789', c) >= 0) dd += c; }
		if (length(dd) > 0) {
			if (length(dd) % 2 == 1) dd += 'F';
			let ssw = '';
			for (let i = 0; i < length(dd); i += 2) {
				let a = index('0123456789', substr(dd, i, 1));
				let b = index('0123456789', substr(dd, i + 1, 1));
				ssw += hex2(b * 16 + a);
			}
			// ★ SMSC 带国家码（86 开头）→ TOA 必须是 0x91（国际），不能复用目标号码的 0x81
			let sto = (substr(d, 0, 2) == '86') ? 0x91 : 0x81;
			smsc = hex2(int(length(dd) / 2) + 1) + hex2(sto) + ssw;
		}
	}
	let chunks = [];
	let n = length(text);
	if (n <= 70) {
		push(chunks, text);
	} else {
		// ★★ 2026-10-07 晚 代码审查修复：分条**必须把后缀算进 70 字上限**。
		//   旧实现固定每条 65 字，再追加 ` (k/total)` 后缀 ——
		//   65 + " (1/2)"(6 字) = **71 字** → UCS2 下 142 字节 > 单条上限 140 字节
		//   → 模组会拒发（CMS 错误），长短信发不出去。
		//   现在：先按「后缀最长可能长度」预留，再算每段可容纳的字数。
		//   后缀形如 ` (10/10)` = 8 字；取 8 为预留量（保守但安全）。
		let suffix_max = 8;
		let per = 70 - suffix_max;              // = 62 字/段（含后缀总长 ≤ 70）
		if (per < 1) per = 1;
		let i = 0;
		while (i < n) { push(chunks, substr(text, i, per)); i += per; }
		let total = length(chunks);
		for (let k = 0; k < total; k++) chunks[k] = chunks[k] + ' (' + (k + 1) + '/' + total + ')';
	}
	for (let k = 0; k < length(chunks); k++) {
		let ud = utf8_to_ucs2hex(chunks[k]);
		let udlen = int(length(ud) / 2);
		// FO=00（无 VP 字段）；SMSC 显式编入（空 SMSC 会被 CMS 304 拒）
		let tpdu = '0100' + hex2(length(digits)) + hex2(toa) + sw + '0008' + hex2(udlen) + ud;
		let pdu = '00' + tpdu;   // \u9996\u5b57\u8282 00 = \u7528\u6a21\u5757\u5185\u914d\u7f6e\u7684\u77ed\u4fe1\u4e2d\u5fc3\uff08\u5b9e\u6d4b\u6700\u7a33\uff09
		// ★ AT+CMGS=<n> 的 n = 纯 TPDU 字节数（GSM 07.05：不含 SMSC 长度字节、也不含 SMSC 地址段）
		print(int(length(tpdu) / 2) + ' ' + pdu + '\n');
	}
}

// 本机 ucode 不给脚本注入 argv → 参数走环境变量：
//   UC_SUB=num|txt|poll|inbox  UC_ARG1/UC_ARG2=子参数
// 发送计划：每行 "charset\t号码参数\t正文参数"（GSM=原文直发；UCS2=号码/正文为 UCS2 hex）
// 实测定案（EC200 BETA 固件）：必须握手式（等 '>' 提示符）；UCS2 号码用国内 11 位格式（+86 前缀会被 CMS 332 拒）
function cmd_sendplan(num, text) {
	num = replace_all('' + num, ' ', '');
	// ★ 统一走 UCS2 路径（2026-10-07 定案，实测依据）：
	//   GSM 字符集路径会被模组把自己的输出残片（"\r\n\r\n" + 响应符 "OK"/提示符 "> "）
	//   当成正文前缀发出去（CMGW 存读回取证：收件人实测收到 "OKYE"）。那是前缀，
	//   无法用填充抵消（退格符实测被当作普通字符存下）。
	//   UCS2 路径的前缀是固定的 3 字节，可用 1 字节填充精确抵消（见 fzs-at 注释）。
	//   代价：每段容量 153 字 → 67 字。取舍理由：垃圾前缀每位收件人可见，容量损失不可见。
	let cs = 'U', limit = 67, na;
	let d = replace_all(num, '+', '');
	if (length(d) == 13 && substr(d, 0, 2) == '86') d = substr(d, 2);
	na = utf8_to_ucs2hex(d);
	let chunks = [];
	let i = 0;
	while (i < length(text)) { push(chunks, substr(text, i, limit)); i += limit; }
	let total = length(chunks);
	for (let k = 0; k < total; k++) {
		let body = (total > 1) ? (chunks[k] + ' (' + (k + 1) + '/' + total + ')') : chunks[k];
		body = replace_all(body, '\t', ' ');
		let arg = (cs == 'U') ? utf8_to_ucs2hex(body) : body;
		print(cs + '\t' + jesc(na) + '\t' + jesc(arg) + '\n');
	}
}

// CSCA? 的返回可能是 UCS2 残留（CSCS=UCS2 状态下）：形如 002B0038... 才解码，否则原样
function cmd_cscadec(s) {
	if (s == null) { print(''); return; }
	let h = upcase('' + s);
	if (length(h) % 4 != 0 || length(h) < 8) { print(s); return; }
	for (let i = 0; i < length(h); i += 4) {
		let pair = substr(h, i, 4);
		if (pair == '002B') continue;
		if (substr(pair, 0, 2) != '00') { print(s); return; }
		if (index('0123456789', substr(pair, 2, 1)) < 0 || index('0123456789', substr(pair, 3, 1)) < 0) { print(s); return; }
	}
	print(ucs2dec(h));
}

let sub = getenv('UC_SUB');
if (sub == null || sub == '') die('用法: UC_SUB=num|txt|poll|inbox ucode fzs-sms.uc');
fs.mkdir(DIR);
fs.mkdir(TMP);
let args = [ sub, getenv('UC_ARG1'), getenv('UC_ARG2'), getenv('UC_ARG3'), getenv('UC_ARG4') ];
switch (args[0]) {
case 'num':
	if (length(args) < 2) die('用法: fzs-sms.uc num <号码>');
	cmd_num(args[1]);
	break;
case 'txt':
	if (length(args) < 2) die('用法: fzs-sms.uc txt <文本>');
	cmd_txt(args[1]);
	break;
case 'poll':
	if (length(args) < 3) die('用法: fzs-sms.uc poll <cfg> <raw文件>');
	cmd_poll(args[1], args[2]);
	break;
case 'inbox':
	cmd_inbox();
	break;
case 'outbox':
	if (length(args) < 4) die('用法: fzs-sms.uc outbox <cfg> <号码> <内容>');
	cmd_outbox(args[1], args[2], args[3]);
	break;
case 'pdu':
	if (length(args) < 3) die('用法: fzs-sms.uc pdu <号码> <文本>');
	cmd_pdu(args[1], args[2], args[3]);
	break;
case 'cscadec':
	cmd_cscadec(args[1]);
	break;
case 'sendplan':
	if (length(args) < 3) die('用法: fzs-sms.uc sendplan <号码> <文本>');
	cmd_sendplan(args[1], args[2]);
	break;
case 'urlenc':
	if (length(args) < 2) die('用法: fzs-sms.uc urlenc <文本>');
	cmd_urlenc(args[1]);
	break;
case 'fwdjson':
	if (length(args) < 4) die('用法: fzs-sms.uc fwdjson <发件人> <时间> <内容>');
	cmd_fwdjson(args[1], args[2], args[3]);
	break;
case 'fwdbody':
	if (length(args) < 5) die('用法: fzs-sms.uc fwdbody <渠道> <发件人> <时间> <内容>');
	cmd_fwdbody(args[1], args[2], args[3], args[4]);
	break;
case 'clear':
	cmd_clear();
	break;
case 'drop':
	if (length(args) < 3) die('用法: fzs-sms.uc drop <模组> <索引>');
	cmd_drop(args[1], args[2]);
	break;
case 'clearout':
	cmd_clearout();
	break;
case 'dropout':
	if (length(args) < 4) die('用法: fzs-sms.uc dropout <时间戳> <模组> <号码>');
	cmd_dropout(args[1], args[2], args[3]);
	break;
default:
	die('未知子命令: ' + args[0]);
}
