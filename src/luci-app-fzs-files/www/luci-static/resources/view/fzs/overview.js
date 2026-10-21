'use strict';
'require view';
'require fs';
'require poll';
'require ui';

var FZS_AT = '/usr/libexec/fzs-at';

// 「关于」里的跳转目标（仓库已上线，LICENSE 已入库）
var REPO = 'https://github.com/zzt718/fzs1-panel';
var REPO_SIS = 'https://github.com/zzt718/fzs1-decloud';

var CSS = [
	'.fzs-grid{display:grid;grid-template-columns:repeat(auto-fit,minmax(340px,1fr));gap:1em}',
	':where(.fzs-card){background:var(--background-color-high, #ffffff);border:1px solid var(--border-color-medium, #d4d8dc);border-radius:6px;overflow:hidden}',
	// ★ 卡片内边距必须由自己给：有的主题把 .cbi-section 的 padding 归零（Argon 就是），
	//   而 :where() 特异性为 0，会被主题压掉 ⇒ 内容贴到卡片边框上（勾选框贴着边）。
	//   这里改用普通类选择器提供内边距——主题不认识 .fzs-card/.fzs-body，不会覆盖。
	'.cbi-section.fzs-card{padding:0}',
	'.cbi-section.fzs-card>h3{padding:.8em 1.1em}',
	'.cbi-section.fzs-card>.fzs-body{padding:.15em 1.1em 1.05em;max-width:680px}',
	'.cbi-section.fzs-card .cbi-section-descr{padding-left:0;padding-right:0}',
	'.fzs-card-h{display:flex;align-items:center;gap:.6em;flex-wrap:wrap}',
	'.fzs-card-h .fzs-name{font-weight:700;font-size:1.05em}',
	'.fzs-card-h .fzs-model{font-size:.8em;padding:.1em .5em;border-radius:3px;background:var(--background-color-low, #e9ebee);border:1px solid var(--border-color-low, #e5e8eb);color:var(--text-color-medium)}',
	'.fzs-card-h .fzs-badge{margin-left:auto;font-size:.78em;padding:.15em .6em;border-radius:10px;font-weight:600}',
	'.fzs-badge.ok{background:rgba(76,175,80,.18);color:#4caf50;border:1px solid rgba(76,175,80,.45)}',
	'.fzs-badge.warn{background:rgba(255,152,0,.18);color:#fb8c00;border:1px solid rgba(255,152,0,.45)}',
	'.fzs-badge.bad{background:rgba(244,67,54,.15);color:#e53935;border:1px solid rgba(244,67,54,.4)}',
	'.fzs-signal{display:flex;align-items:center;gap:.9em;margin-bottom:1em}',
	'.fzs-rsrp{line-height:1}',
	'.fzs-rsrp b{font-size:1.7em;font-weight:700}',
	'.fzs-rsrp i{font-size:.72em;font-style:normal;color:var(--text-color-medium);margin-left:.15em}',
	'.fzs-bar{flex:1;height:10px;border-radius:5px;background:var(--background-color-low, #e9ebee);overflow:hidden}',
	'.fzs-bar span{display:block;height:100%;border-radius:5px;transition:width .4s}',
	'.fzs-qual{font-size:.85em;font-weight:600;min-width:2.5em;text-align:right}',
	'.fzs-kv{display:grid;grid-template-columns:repeat(3,1fr);gap:.7em .5em;padding:.8em 0;border-top:1px solid var(--border-color-low, #e5e8eb);border-bottom:1px solid var(--border-color-low, #e5e8eb);margin-bottom:.8em}',
	'.fzs-kv>div{min-width:0}',
	'.fzs-kv label{display:block;font-size:.72em;color:var(--text-color-low);margin-bottom:.15em}',
	'.fzs-kv span{font-size:.95em;font-weight:600;white-space:nowrap;overflow:hidden;text-overflow:ellipsis;display:block}',
	'.fzs-id{display:flex;align-items:center;gap:.5em;font-size:.88em;padding:.28em 0}',
	'.fzs-id label{width:3.6em;color:var(--text-color-low);flex:none}',
	'.fzs-id .v{font-family:monospace;letter-spacing:.5px}',
	// ★ 必须写 color:inherit：主题常给 button 设自己的文字色（openwrt2020 是白色），
	//   `👁` 不含变体选择符 ⇒ 按"文本呈现"用 currentColor 画，按钮色一变眼睛就看不见了。
	//   继承父级颜色后，任何主题下都跟正文同色。
	'.fzs-eye{cursor:pointer;border:0;background:none;padding:0 .3em;opacity:.55;font-size:.9em;color:inherit}',
	'.fzs-eye:hover{opacity:1}',
	'.fzs-empty{color:var(--text-color-low);text-align:center;padding:2em 1em}',
	'.fzs-tools{margin-top:1.2em}',
	'.fzs-tools h3{margin:0 0 .6em;font-size:1em}',
	'.fzs-tiles{display:grid;grid-template-columns:repeat(auto-fit,minmax(200px,1fr));gap:.8em}',
	'.fzs-tile{display:flex;flex-direction:column;align-items:center;justify-content:center;text-align:center;gap:.35em;padding:1em .8em;border:1px solid var(--border-color-medium, #d4d8dc);border-radius:6px;background:var(--background-color-high, #ffffff);text-decoration:none;color:inherit;transition:.15s}',
	'.fzs-tile:hover{border-color:var(--primary-color-high);background:var(--background-color-medium, #f4f5f7)}',
	'.fzs-tile .ic{font-size:1.6em;line-height:1}',
	'.fzs-tile .t{font-weight:600}',
	'.fzs-tile .d{font-size:.78em;color:var(--text-color-low)}',
	'.fzs-about{font-size:.78em;opacity:.6;text-align:center;margin-top:1.8em;line-height:1.9}',
	// 链接：优先用主题主色（openwrt2020 用 --main-bright-color），都没有才兜底成蓝色；
	// 一律带下划线 —— 用户要的是"看着就知道能点"。
	'.fzs-about a,.fzs-ab a{color:var(--primary-color-high, var(--main-bright-color, #1976d2));text-decoration:underline}',
	// ── 「关于」弹窗内容 ──
	'.fzs-ab{font-size:.92em;line-height:1.75}',
	'.fzs-ab-h{display:flex;align-items:baseline;gap:.6em}',
	'.fzs-ab-h b{font-size:1.1em}',
	'.fzs-ab-v,.fzs-ab-sub,.fzs-ab-d{color:var(--text-color-low)}',
	'.fzs-ab-t{margin:1.1em 0 .2em;font-size:.85em;color:var(--text-color-low)}',
	'.fzs-ab a.fzs-ab-btn{display:inline-block;margin-top:.5em;padding:.3em .95em;font-size:.92em;border:1px solid var(--border-color-medium, #d4d8dc);border-radius:4px;text-decoration:none;color:inherit}',
	'.fzs-ab a.fzs-ab-btn:hover{border-color:var(--primary-color-high, var(--main-bright-color, #1976d2))}',
	'.fzs-ab-warn{margin-top:1.1em;padding:.6em .8em;border-radius:4px;font-size:.88em;line-height:1.6;background:rgba(255,152,0,.12);border:1px solid rgba(255,152,0,.35);color:var(--text-color-medium, #6b6b6b)}'
].join('\n');

function fetchAll() {
	return fs.exec(FZS_AT, [ 'info' ]).then(function(r) {
		try {
			var a = JSON.parse((r && r.stdout) || '[]');
			return [ a[0] || null, a[1] || null ];
		} catch (e) { return [ null, null ]; }
	}).catch(function() { return [ null, null ]; });
}

// 面板版本号：从安装包的 control 文件读（升级后自动更新，不用改代码）。
// 读不到就留空 —— 这是页脚信息，绝不能影响主内容。
function fetchVer() {
	// ★ 注意：LuCI 的 fs.read() 直接 resolve 出**字符串**（不是 {data:…} 对象）
	return fs.read('/usr/lib/opkg/info/luci-app-fzs.control').then(function(txt) {
		var m = ('' + txt).match(/^Version:\s*(\S+)/m);
		return m ? m[1] : '';
	}).catch(function() { return ''; });
}

function rsrpPct(v) {
	var n = parseInt(v, 10);
	if (isNaN(n)) return 0;
	return Math.max(2, Math.min(100, (n + 120) / 60 * 100));
}

function rsrpInfo(v) {
	var n = parseInt(v, 10);
	if (isNaN(n)) return { txt: '无信号', color: '#9e9e9e' };
	if (n >= -85) return { txt: '很强', color: '#43a047' };
	if (n >= -95) return { txt: '良好', color: '#7cb342' };
	if (n >= -105) return { txt: '一般', color: '#fb8c00' };
	return { txt: '较弱', color: '#e53935' };
}

function regBadge(reg) {
	var cls = 'bad';
	if (reg === '已注册' || reg === '漫游') cls = 'ok';
	else if (reg === '搜网中') cls = 'warn';
	return E('span', { 'class': 'fzs-badge ' + cls }, reg || '未知');
}

function mask(s) {
	s = (s || '').toString();
	if (s.length < 8) return s || '—';
	return s.substr(0, 4) + new Array(s.length - 7).join('*') + s.substr(s.length - 4);
}

function idRow(label, value) {
	var full = (value || '').toString();
	var span = E('span', { 'class': 'v' }, mask(full) || '—');
	var btn = E('button', {
		'class': 'fzs-eye', title: '显示/隐藏',
		click: function() {
			span.textContent = (span.textContent.indexOf('*') >= 0) ? (full || '—') : mask(full);
		}
	}, '👁');
	return E('div', { 'class': 'fzs-id' }, [ E('label', {}, label), span, full ? btn : '' ]);
}

function modemCard(m) {
	if (!m || !m.ok)
		return E('div', { 'class': 'cbi-section fzs-card' }, [
			E('h3', { 'class': 'fzs-card-h' }, [
				E('span', { 'class': 'fzs-name' }, (m && m.name) || '模组'),
				E('span', { 'class': 'fzs-badge bad' }, '未连接')
			]),
			E('div', { 'class': 'cbi-section-node fzs-body' }, [
				E('div', { 'class': 'fzs-empty' }, (m && m.err) || '读取失败')
			])
		]);

	var si = rsrpInfo(m.rsrp);
	var short = (m.name || '模组').replace(/\s*\(.*\)\s*$/, '');

	return E('div', { 'class': 'cbi-section fzs-card' }, [
		E('h3', { 'class': 'fzs-card-h' }, [
			E('span', { 'class': 'fzs-name' }, short),
			E('span', { 'class': 'fzs-model' }, m.model || '?'),
			regBadge(m.reg)
		]),
		E('div', { 'class': 'cbi-section-node fzs-body' }, [
			E('div', { 'class': 'fzs-signal' }, [
				E('div', { 'class': 'fzs-rsrp' }, [
					E('b', { style: 'color:' + si.color }, m.rsrp || '—'),
					E('i', {}, ' dBm')
				]),
				E('div', { 'class': 'fzs-bar' }, [
					E('span', { style: 'width:' + rsrpPct(m.rsrp) + '%;background:' + si.color })
				]),
				E('div', { 'class': 'fzs-qual', style: 'color:' + si.color }, si.txt)
			]),
			E('div', { 'class': 'fzs-kv' }, [
				E('div', {}, [ E('label', {}, '运营商'), E('span', {}, m.operator || '—') ]),
				E('div', {}, [ E('label', {}, '频段'), E('span', {}, m.band ? ('B' + m.band) : '—') ]),
				E('div', {}, [ E('label', {}, '频点 EARFCN'), E('span', {}, m.earfcn || '—') ]),
				E('div', {}, [ E('label', {}, '小区 PCI'), E('span', {}, m.pci || '—') ]),
				E('div', {}, [ E('label', {}, 'SINR'), E('span', {}, m.sinr ? (m.sinr + ' dB') : '—') ]),
				E('div', {}, [ E('label', {}, 'RSRQ'), E('span', {}, m.rsrq ? (m.rsrq + ' dB') : '—') ])
			]),
			idRow('IMEI', m.imei),
			idRow('ICCID', m.iccid),
			idRow('IMSI', m.imsi)
		])
	]);
}

function tile(href, icon, title, desc) {
	return E('a', { 'class': 'fzs-tile', href: L.url(href) }, [
		E('span', { 'class': 'ic' }, icon),
		E('span', { 'class': 't' }, title),
		E('span', { 'class': 'd' }, desc)
	]);
}

function placeholder(name) {
	return E('div', { 'class': 'cbi-section fzs-card' }, [
		E('h3', { 'class': 'fzs-card-h' }, [ E('span', { 'class': 'fzs-name' }, name) ]),
		E('div', { 'class': 'cbi-section-node fzs-body' }, [
			E('div', { 'class': 'fzs-empty' }, '读取中…')
		])
	]);
}

function extLink(href, text) {
	return E('a', { href: href, target: '_blank', rel: 'noopener' }, text);
}

// 「关于」弹窗：适用范围 / 开源许可 / 致谢 / 免责声明。版本号由调用方传入（异步取，可能为空）。
function showAbout(ver) {
	ui.showModal('关于', [
		E('div', { 'class': 'fzs-ab' }, [
			E('div', { 'class': 'fzs-ab-h' }, [
				E('b', {}, '蜂助手面板'),
				E('span', { 'class': 'fzs-ab-v' }, ver ? ('v' + ver) : '')
			]),
			E('div', { 'class': 'fzs-ab-sub' }, 'MIBOX-668M2 蜂窝运维插件（LuCI 应用）'),
			E('div', {}, '面板开发：zzt718'),

			E('div', { 'class': 'fzs-ab-t' }, '适用范围'),
			E('div', {}, '蜂助手 S1 / MIBOX-668M2　·　MT7628　·　双模组 EC200T + EC200N'),
			E('div', {}, 'OpenWrt 22.03 及以上（LuCI）'),

			E('div', { 'class': 'fzs-ab-t' }, '开源许可'),
			E('div', {}, [
				'本面板以 ', extLink(REPO + '/blob/HEAD/LICENSE', 'GPL-3.0-or-later'),
				' 发布，源码与许可证全文见仓库'
			]),
			E('div', {}, [ E('a', { 'class': 'fzs-ab-btn', href: REPO, target: '_blank', rel: 'noopener' }, 'GitHub') ]),

			E('div', { 'class': 'fzs-ab-t' }, '致谢'),
			E('div', {}, [ '羁穗', E('span', { 'class': 'fzs-ab-d' }, '（酷安）· 本机社区固件编译作者') ]),
			E('div', {}, [ 'OpenWrt / LuCI', E('span', { 'class': 'fzs-ab-d' }, ' · 运行平台') ]),
			E('div', {}, [ extLink(REPO_SIS, 'fzs1-decloud'), E('span', { 'class': 'fzs-ab-d' }, ' · 姊妹项目') ]),

			E('div', { 'class': 'fzs-ab-warn' }, '免责声明：修改 IMEI 在多数国家和地区受法律管制，仅限对你本人合法拥有的设备使用。')
		]),
		E('div', { 'class': 'right', style: 'margin-top:1.2em' }, [
			E('button', { 'class': 'btn cbi-button', click: ui.hideModal }, '关闭')
		])
	]);
}

return view.extend({
	load: function() {
		L.env.rpctimeout = 60;
		return null;
	},

	render: function() {
		var grid = E('div', { 'class': 'fzs-grid' }, [
			placeholder('模组1 (EC200T)'), placeholder('模组2 (EC200N)')
		]);
		var stamp = E('span', {}, '读取中…');

		function setCard(i, m) {
			grid.replaceChild(modemCard(m), grid.children[i]);
		}

		function reload() {
			stamp.textContent = '读取中…';
			return fetchAll().then(function(list) {
				setCard(0, list[0]);
				setCard(1, list[1]);
				stamp.textContent = '数据读取于 ' + new Date().toLocaleTimeString();
			});
		}

		var btn = E('button', { 'class': 'cbi-button cbi-button-action', click: reload }, '立即刷新');

		var head = E('div', { style: 'display:flex;align-items:center;justify-content:flex-end;gap:.8em;margin-bottom:1em' }, [ stamp, btn ]);

		var tools = E('div', { 'class': 'fzs-tools' }, [
			E('h3', {}, '功能'),
			E('div', { 'class': 'fzs-tiles' }, [
				tile('admin/services/fzs/net', '🌐', '上网设置', '上网方式 / 4G 模组开关 / 网口'),
				tile('admin/services/fzs/band', '📶', '锁频段', '限定设备可用的频段'),
				tile('admin/services/fzs/cell', '🎯', '锁定频点 / 小区', '钉死在指定频点或基站'),
				tile('admin/services/fzs/imei', '🔢', 'IMEI 改串', '修改 / 恢复模组 IMEI'),
				tile('admin/services/fzs/apn', '📡', 'APN 与数据开关', '接入点设置 / 每模组数据开关'),
				tile('admin/services/fzs/sms', '📱', '短信中心', '双模组收发 / 收件箱')
			])
		]);

		// 页脚：适用硬件 + 版本 + 「关于」入口（版本异步取，取不到只是这一行少一截）
		var verText = '';
		var verEl = E('span', {}, '');
		var aboutLink = E('a', {
			href: '#',
			click: function(ev) { ev.preventDefault(); showAbout(verText); }
		}, '关于');
		var about = E('div', { 'class': 'fzs-about' }, [
			E('span', {}, '适用硬件：蜂助手 S1 / MIBOX-668M2　·　双模组：EC200T + EC200N'),
			verEl,
			E('span', {}, '　·　'),
			aboutLink
		]);
		fetchVer().then(function(v) {
			verText = v || '';
			verEl.textContent = verText ? ('　·　面板 v' + verText) : '';
		});

		reload();
		poll.add(reload, 30);

		return E('div', { 'class': 'fzs-wrap' }, [ E('style', {}, CSS), head, grid, tools, about ]);
	},

	handleSave: null,
	handleSaveApply: null,
	handleReset: null
});
