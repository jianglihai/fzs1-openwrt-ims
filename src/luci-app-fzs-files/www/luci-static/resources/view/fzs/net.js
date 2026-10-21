'use strict';
'require view';
'require fs';
'require ui';

/*
 * 「上网设置」——三块：上网方式 / 4G 模组 / 网口
 *
 * 实现全部落在设备已有的成熟组件上，**不自己写路由逻辑**：
 *   · 上网方式 + 双卡策略  → mwan3 的 member metric 与策略成员
 *   · 网口角色            → network 的 switch_vlan ports
 *   · 4G 模组开关         → 固件的 mifi.power.enable（改完需重启）
 * 具体动作都在后端 /usr/libexec/fzs-net 里做，本页只负责渲染 + 提交 + 结果。
 *
 * ★ 红线：绝不动模组接口（短信要用）；不碰 14-mifi-route；不写 mifi.power.enable 以外的东西。
 * ★ 文案要求：面向普通用户，简明；不解释实现机制、不描述界面本身的行为。
 * ★ 状态提示用 flash 机制：每次改动都会整页重绘，局部变量里的提示会跟着旧元素一起丢掉。
 */

var FZS_NET = '/usr/libexec/fzs-net';
var FZS_WIFI = '/usr/libexec/fzs-wifi';
var UCI = '/sbin/uci';

// note 是「仅用有线」专用的一句提醒：该模式下 4G 模组仍在，mwan3 会继续周期性探测它
// （会产生少量流量）。只在 4G 模组开启时显示 —— 4G 已关闭时它不成立。
var MODES = [
	{ v: 'only4g',   t: '仅用 4G（默认）', d: '只用 4G 卡上网，网线不参与' },
	{ v: '4gfirst',  t: '4G 优先',         d: '有网线时优先走网线，断了自动回 4G' },
	{ v: 'wanfirst', t: '有线优先',        d: '优先走网线；网线断了自动回 4G' },
	{ v: 'onlywan',  t: '仅用有线',        d: '只走网线；4G 模组保持在线，短信照常',
	  note: '注意：4G 仍会产生少量流量。要完全不用，请关闭下面的「4G 模组」。' }
];

var SIMS = [
	{ v: 'balance', t: '负载均衡（默认）', d: '两张卡同时用' },
	{ v: 'sim1',    t: '模组1 优先',       d: '模组1 断了才走模组2' },
	{ v: 'sim2',    t: '模组2 优先',       d: '模组2 断了才走模组1' }
];

// 物理网口。名称直接用设备面板上的丝印（从左到右），用户对着机器就能对上；
// 不写「左/中/右」——用户分不清左右，靠示意图 + 指示灯对齐。
// chip 是交换机芯片上的口编号（与物理口的对应关系由指示灯自动暴露：插线即亮）。
// 注：面板上三个口都丝印为 LAN，本固件把其中一个口用作 WAN —— 由「角色」那一行体现。
var PORTS = [
	{ v: 'left',  t: 'LAN2', chip: '1' },
	{ v: 'mid',   t: 'LAN1', chip: '2' },
	{ v: 'right', t: 'LAN0', chip: '4' }
];

var CSS = [
	// ★ 区块外壳用 LuCI 标准结构：.cbi-section > h3 + .cbi-section-node。
	//   底色/边框/标题字号**全部交给主题**决定 ⇒ 换任何主题都自动跟随，不再自己画卡片。
	//   （实测：bootstrap 与 openwrt2020 都不给 .cbi-section 画外观，区块感来自
	//     h3 标题 + 区块间距 —— 这正是 LuCI 原生设置页的观感。）
	':where(.fzs-card){background:var(--background-color-high, #ffffff);border:1px solid var(--border-color-medium, #d4d8dc);border-radius:6px;overflow:hidden;margin-bottom:1.6em}',
	// ★ 卡片内边距必须由自己给：有的主题把 .cbi-section 的 padding 归零（Argon 就是），
	//   而 :where() 特异性为 0，会被主题压掉 ⇒ 内容贴到卡片边框上（勾选框贴着边）。
	//   这里改用普通类选择器提供内边距——主题不认识 .fzs-card/.fzs-body，不会覆盖。
	'.cbi-section.fzs-card{padding:0}',
	'.cbi-section.fzs-card>h3{padding:.8em 1.1em}',
	'.cbi-section.fzs-card>.fzs-body{padding:.15em 1.1em 1.05em;max-width:680px}',
	'.cbi-section.fzs-card .cbi-section-descr{padding-left:0;padding-right:0}',
	'.fzs-opt{display:flex;align-items:flex-start;gap:.55em;cursor:pointer;padding:.5em 0}',
	'.fzs-opt input{margin-top:.3em;flex:none}',
	// ★ 复选框一律"主题原生外观 + 只纠正位移"（不自绘）：主题给了
	//   `label > input[type=checkbox]{position:relative;top:.4rem}`（= 6.4px 下沉）⇒ 把 top 归零即可。
	'.fzs-opt input[type=checkbox]{top:0}',
	// ★ 单选圈必须**自绘**，不能用浏览器原生外观：
	//   有的主题（如 openwrt2020）用 `*{border:none;background:none;-webkit-appearance:none}`
	//   抹平全部原生控件，却只给 checkbox 写了恢复规则，radio 一条都没有
	//   ⇒ 原生单选圈被抹掉后没有任何样式画回来，整行变成空白、看不出哪项被选中。
	'.fzs-opt input[type=radio]{-webkit-appearance:none;appearance:none;flex:none;width:1.05em;height:1.05em;margin:.28em .12em 0 0;border:2px solid var(--border-color-high, #b8bec5);border-radius:50%;background:transparent;position:relative;cursor:pointer;transition:border-color .15s}',
	'.fzs-opt input[type=radio]:checked{border-color:var(--primary-color-high, var(--main-bright-color, #1976d2))}',
	'.fzs-opt input[type=radio]:checked::after{content:"";position:absolute;top:50%;left:50%;width:.5em;height:.5em;margin:-.25em 0 0 -.25em;border-radius:50%;background:var(--primary-color-high, var(--main-bright-color, #1976d2))}',
	'.fzs-opt input[type=radio][disabled]{cursor:default;border-color:var(--border-color-low, #e5e8eb)}',
	'.fzs-opt .t{font-weight:600}',
	// 说明文字用 opacity 压淡，不取主题变量 —— 任何主题下都自动跟当前文字色协调。
	'.fzs-opt .d{font-size:.82em;opacity:.8;margin-top:.15em;line-height:1.5}',
	'.fzs-off{opacity:.42}',
	// 网口示意图：照设备背面画 —— 两根天线、DC 电源口、三个网口、SIM 卡槽开口，嵌在一整块"背板"上。
	// 只有三个网口可点（选它作 WAN）；其余元素是纯示意，整块置灰且不可交互。
	// 背板保持"扁"的一条；两根天线**从背板两端向上伸到框外**（靠上方的 margin 留出空间，
	// 不占用背板自身高度 —— 否则背板会被撑成一个高框）。
	'.fzs-panel{position:relative;display:flex;gap:1.3em;flex-wrap:wrap;justify-content:center;align-items:flex-end;padding:1.1em 1.4em .9em;margin:150px 0 2px;border:1px solid var(--border-color-medium, #d4d8dc);border-radius:6px;background:var(--background-color-medium, #f6f7f8)}',
	'.fzs-ant{position:absolute;top:-142px;width:17px;height:204px;border-radius:9px;background:#e3e6e9;border:1px solid #c3c9ce}',
	'.fzs-ant::after{content:"";position:absolute;left:50%;bottom:-14px;width:30px;height:30px;margin-left:-15px;border-radius:50%;background:#e3e6e9;border:1px solid #c3c9ce}',
	'.fzs-ant-l{left:1.1em}',
	'.fzs-ant-r{right:1.1em}',
	'.fzs-static{text-align:center;user-select:none}',
	'.fzs-dc{width:20px;height:20px;border-radius:50%;border:2px solid #c8ced4;background:#ffffff;position:relative;margin:0 auto 8px}',
	'.fzs-dc::after{content:"";position:absolute;left:50%;top:50%;width:7px;height:7px;margin:-3.5px 0 0 -3.5px;border-radius:50%;background:#d5d9dd}',
	'.fzs-sim{width:56px;height:13px;border-radius:2.5px;background:#e9ecef;border:1.5px solid #c8ced4;position:relative;margin:0 auto 12px}',
	'.fzs-sim::after{content:"";position:absolute;left:4px;right:4px;top:3px;bottom:3px;border-radius:1.5px;background:#d5d9dd}',
	'.fzs-sname{font-size:11px;opacity:.55}',
	// 窄屏：把天线/电源口/卡槽收掉，只留三个网口（否则缩成一团更难认）。
	'@media (max-width:560px){.fzs-panel{margin-top:.5em}.fzs-ant,.fzs-static{display:none}}',
	'.fzs-port{position:relative;text-align:center;cursor:pointer;user-select:none}',
	'.fzs-port input{position:absolute;opacity:0;width:1px;height:1px;pointer-events:none}',
	// 网口本体：外框 + 上沿中间的卡扣（::before）+ 内部簧片（::after）
	'.fzs-jack{position:relative;width:58px;height:40px;margin:9px auto;border:2px solid var(--border-color-high, #b8bec5);border-radius:3px;background:var(--background-color-low, #ffffff);transition:border-color .15s}',
	'.fzs-jack::before{content:"";position:absolute;left:50%;top:-9px;width:20px;height:9px;transform:translateX(-50%);border:2px solid var(--border-color-high, #b8bec5);border-bottom:none;border-radius:3px 3px 0 0;background:inherit;transition:border-color .15s}',
	'.fzs-jack::after{content:"";position:absolute;left:7px;right:7px;top:6px;bottom:6px;border-radius:1px;opacity:.85;background:repeating-linear-gradient(74deg, transparent 0 4px, var(--border-color-medium, #ccd1d6) 4px 5.5px)}',
	'.fzs-port:hover .fzs-jack,.fzs-port:hover .fzs-jack::before{border-color:var(--border-color-high, #8f979e)}',
	'.fzs-port.on .fzs-jack,.fzs-port.on .fzs-jack::before{border-color:var(--primary-color-high, var(--main-bright-color, #1976d2))}',
	'.fzs-pname{font-weight:600;font-size:.92em}',
	'.fzs-port.on .fzs-pname{color:var(--primary-color-high, var(--main-bright-color, #1976d2))}',
	'.fzs-prole{font-size:.75em;opacity:.75;margin-top:.15em}',
	'.fzs-port.on .fzs-prole{opacity:1;font-weight:600;color:var(--primary-color-high, var(--main-bright-color, #1976d2))}',
	'.fzs-led{display:inline-block;width:.6em;height:.6em;border-radius:50%;vertical-align:middle;margin-left:.35em;background:var(--border-color-medium, #c9ced3)}',
	'.fzs-led.up{background:#4caf50;box-shadow:0 0 5px rgba(76,175,80,.85)}',
	'.fzs-nolan{margin-top:.8em}',
	'.fzs-grp{margin-top:.9em;padding-top:.7em;border-top:1px solid var(--border-color-low, #e5e8eb)}',
	'.fzs-grp>h4{margin:0 0 .3em;font-size:.9rem;opacity:.8;font-weight:600}',
	'.fzs-warn{margin:.9em 0;padding:.7em .9em;border-radius:5px;font-size:.85em;line-height:1.7;background:rgba(255,152,0,.12);border:1px solid rgba(255,152,0,.4)}',
	'.fzs-actions{margin-top:1.1em;display:flex;gap:.6em;align-items:center;flex-wrap:wrap}',
	// ★ 真禁用要"一眼看出"（主题给中性按钮灰底灰字，容易与禁用态混淆）。`:where()` 不抢主题。
	':where(.fzs-actions button:disabled,.fzs-card button:disabled){opacity:.45;cursor:not-allowed;filter:grayscale(.4)}',
	'.fzs-status{font-size:.88em}'
].join('\n');

function call(args) {
	return fs.exec(FZS_NET, args).then(function(r) {
		var d = null;
		try { d = JSON.parse((r && r.stdout) || ''); } catch (e) {}
		if (!d || !d.ok) return { ok: false, err: (d && d.err) || '执行失败' };
		return d;
	}).catch(function() { return { ok: false, err: '调用失败' }; });
}

// 无线功率：调 /usr/libexec/fzs-wifi（改国家码+功率都在后端做，含"等稳定验证、失败自动还原"）。
// 与 call() 的区别：这里把后端返回的**原样**交出去（ok:false 时也带 err 供页面显示）。
function callWifi(args) {
	return fs.exec(FZS_WIFI, args).then(function(r) {
		var d = null;
		try { d = JSON.parse((r && r.stdout) || ''); } catch (e) {}
		return d || { ok: false, err: '执行失败' };
	}).catch(function() { return { ok: false, err: '调用失败' }; });
}

function uciSet(k, v) {
	return fs.exec(UCI, [ '-q', 'set', k + '=' + v ]).then(function() {
		return fs.exec(UCI, [ '-q', 'commit', 'mifi' ]);
	});
}

function nameOf(list, v) {
	for (var i = 0; i < list.length; i++)
		if (list[i].v === v) return list[i].t.replace('（默认）', '');
	return '未识别';
}

return view.extend({
	load: function() {
		L.env.rpctimeout = 120;
		return Promise.all([
			call([ 'show' ]),
			callWifi([ 'status' ])
		]).then(function(r) {
			return { net: r[0], wifi: r[1] };
		});
	},

	render: function(res) {
		var st = (res && res.net) || { ok: false };    // 上网设置（fzs-net show）
		var wf = (res && res.wifi) || { ok: false };   // 无线功率（fzs-wifi status）
		var wrap = E('div', {});
		var flash = {};          // 每块卡的状态提示；重绘后仍能保留
		var portSel = null;      // 网口块的当前选择（未提交）；重绘后仍能保留，提交成功后复位

		function mInfo(t) { return E('span', {}, t); }
		function mOk(t) { return E('span', {}, '✅ ' + t); }
		function mErr(d) { return E('span', { style: 'color:#e53935' }, '❌ ' + (d.err || '失败')); }
		function mReboot() {
			return E('span', {}, [
				'✅ 已保存，', E('b', {}, '重启后生效'), '：',
				E('a', { href: L.url('admin/system/reboot') }, '去重启')
			]);
		}

		function setMsg(k, node) {
			flash[k] = node;
			var el = document.querySelector('.fzs-status[data-k="' + k + '"]');
			if (el) { el.innerHTML = ''; el.appendChild(node); }
		}

		function statusEl(k) {
			var el = E('div', { 'class': 'fzs-status', 'data-k': k }, '');
			if (flash[k]) el.appendChild(flash[k]);
			return el;
		}

		function draw() {
			while (wrap.firstChild) wrap.removeChild(wrap.firstChild);
			wrap.appendChild(E('style', {}, CSS));
			wrap.appendChild(routeCard());
			wrap.appendChild(powerCard());
			wrap.appendChild(portCard());
			wrap.appendChild(wifiCard());
		}

		function refresh() {
			return call([ 'show' ]).then(function(r) {
				if (r.ok) { st = r; portSel = null; }
				draw();
			});
		}

		// onCancel 用于"控件已经先动了、但用户取消"的场景（如勾选框）——把控件恢复原状。
		function confirmBox(title, lines, onOk, okLabel, onCancel) {
			ui.showModal(title, [
				E('div', {}, lines),
				E('div', { 'class': 'right' }, [
					E('button', { 'class': 'btn', click: function() { ui.hideModal(); if (onCancel) onCancel(); } }, '取消'),
					E('button', {
						'class': 'btn cbi-button cbi-button-apply',
						click: function() { ui.hideModal(); onOk(); }
					}, okLabel || '确认')
				])
			]);
		}

		function pick(name) {
			var e = document.querySelector('input[name="' + name + '"]:checked');
			return e && !e.disabled ? e.value : null;
		}

		function opt(name, v, title, desc, checked, disabled, extra) {
			var i = E('input', { type: 'radio', name: name, value: v });
			if (disabled) i.disabled = true;
			i.checked = !!checked;                       // ★ 设 DOM property，不写 HTML 属性
			return E('label', { 'class': 'fzs-opt' + (disabled ? ' fzs-off' : '') }, [
				i,
				E('span', {}, [
					E('div', { 'class': 't' }, [ title, extra || '' ]),
					desc ? E('div', { 'class': 'd' }, desc) : ''
				])
			]);
		}

		// 区块外壳：LuCI 标准结构（.cbi-section > h3 + .cbi-section-node），外观全交给主题。
		function section(title, body) {
			return E('div', { 'class': 'cbi-section fzs-card' }, [
				E('h3', { 'class': 'fzs-card-h' }, title),
				E('div', { 'class': 'cbi-section-node fzs-body' }, body)
			]);
		}

		// ---------------- ① 上网方式 + 双卡 ----------------
		function routeCard() {
			var fourgOff = (st.fourg === false);
			var simDisabled = fourgOff || st.mode === 'onlywan';
			var stuck = fourgOff && (st.mode === 'only4g' || st.mode === '4gfirst');

			function save() {
				var m = pick('fzs-mode');
				var s = pick('fzs-sim');
				if (!m) return;
				setMsg('route', mInfo('应用中…（约 40 秒内生效）'));
				call([ 'set', m, s || st.sim ]).then(function(r) {
					refresh().then(function() {
						setMsg('route', r.ok ? mOk('已保存，约 40 秒内生效') : mErr(r));
					});
				});
			}

			var body = [ E('div', { 'class': 'cbi-section-descr' }, '当前：' + nameOf(MODES, st.mode)) ];
			if (stuck)
				body.push(E('div', { 'class': 'fzs-warn' },
					'⚠️ 4G 模组已关闭，当前这种方式用不了。请接入网线并改为「仅用有线」，或重新启用 4G 模组。'));
			body.push(E('div', { style: 'margin-top:.5em' }, MODES.map(function(m) {
				var dis = fourgOff && (m.v === 'only4g' || m.v === '4gfirst');
				var desc = m.d;
				if (m.note && !fourgOff) desc = [ m.d, E('br'), m.note ];
				return opt('fzs-mode', m.v, m.t, desc, st.mode === m.v, dis);
			})));
			body.push(E('div', { 'class': 'fzs-grp' }, [
				E('h4', {}, '两张卡先走哪张'),
				E('div', {}, SIMS.map(function(s) {
					return opt('fzs-sim', s.v, s.t, s.d, st.sim === s.v, simDisabled);
				})),
				E('div', { 'class': 'cbi-section-descr' }, '模组1 = EC200T（Cat4）　模组2 = EC200N（Cat1）')
			]));
			body.push(E('div', { 'class': 'fzs-actions' }, [
				E('button', { 'class': 'cbi-button cbi-button-apply', click: save }, '保存'),
				statusEl('route')
			]));
			// 「上网方式」这一栏才是用 MultiWAN 做的 —— 说明与入口就贴在这一栏底下。
			// （以前这段挂在页面最底那张叫「高级」的卡里，说的是"本页"，可 4G 模组 / 网口 / 无线都不是
			//   MultiWAN 做的 ⇒ 一句话错三次；搬到这里归属才对。）
			body.push(E('div', { 'class': 'fzs-grp' }, [
				E('div', { 'class': 'cbi-section-descr' },
					'这一栏是用 MultiWAN 做的；想按目标地址等条件做更细的分流，可以到 MultiWAN 管理器里设。'),
				E('a', {
					'class': 'btn cbi-button',
					href: L.url('admin/network/mwan3')
				}, '打开 MultiWAN 管理器')
			]));

			return section('上网方式', body);
		}

		// ---------------- ② 4G 模组 ----------------
		function powerCard() {
			var isOn = (st.fourg !== false);

			function doSave(v) {
				setMsg('power', mInfo('保存中…'));
				uciSet('mifi.power.enable', v).then(function() {
					setMsg('power', mReboot());
				}).catch(function() { setMsg('power', mErr({ err: '保存失败' })); });
			}

			function save() {
				var v = pick('fzs-4g');
				if (v == null) return;
				if (v !== '0') { doSave('1'); return; }
				var needSwitch = (st.mode === 'only4g' || st.mode === '4gfirst');
				var lines = [ E('div', {}, '关闭后不再使用 4G 上网；短信等功能不受影响。') ];
				if (needSwitch)
					lines.push(E('div', { style: 'margin-top:.5em' },
						'不再有可用的 4G，上网方式将同步改为「仅用有线」。'));
				lines.push(E('div', { style: 'margin-top:.5em;color:#e53935' }, '重启设备后生效。'));
				confirmBox('确认关闭 4G 模组？', lines, function() {
					if (!needSwitch) { doSave('0'); return; }
					setMsg('power', mInfo('处理中…'));
					call([ 'mode', 'onlywan' ]).then(function(r) {
						if (!r.ok) { setMsg('power', mErr(r)); return; }
						doSave('0');
					});
				});
			}

			return section('4G 模组', [
				E('div', { 'class': 'cbi-section-descr' }, '当前：' + (isOn ? '启用' : '关闭')),
				E('div', { style: 'margin-top:.5em' }, [
					opt('fzs-4g', '1', '启用 4G 模组（默认）', '使用 4G 上网。', isOn, false),
					opt('fzs-4g', '0', '关闭 4G 模组', '不再使用 4G（当普通路由器用）；短信等功能不受影响。', !isOn, false)
				]),
				E('div', { 'class': 'cbi-section-descr' }, '修改后需重启设备生效。'),
				E('div', { 'class': 'fzs-actions' }, [
					E('button', { 'class': 'cbi-button cbi-button-apply', click: save }, '保存'),
					statusEl('power')
				])
			]);
		}

		// ---------------- ③ 网口 ----------------
		function portCard() {
			if (portSel == null) portSel = st.port;

			function save() {
				var v = portSel;
				if (!v) return;
				var needSwitch = (v === 'alllan' && (st.mode === 'wanfirst' || st.mode === 'onlywan'));
				var lines = [
					E('div', {}, '切换后这个口的连接会断开。如果你正从这个口访问面板，会立刻掉线。'),
					E('div', { style: 'margin-top:.5em' }, '请从另一个网口或 WiFi 访问。')
				];
				if (needSwitch)
					lines.push(E('div', { style: 'margin-top:.5em' },
						'不再有 WAN 口，上网方式将同步改为「仅用 4G」。'));
				confirmBox('确认切换网口？', lines, function() {
					setMsg('port', mInfo('应用中…（会短暂断网）'));
					var apply = function() {
						call([ 'port', v ]).then(function(r) {
							refresh().then(function() {
								setMsg('port', r.ok ? mOk('已保存') : mErr(r));
							});
						});
					};
					if (!needSwitch) { apply(); return; }
					call([ 'mode', 'only4g' ]).then(function(r) {
						if (!r.ok) { setMsg('port', mErr(r)); return; }
						apply();
					});
				});
			}

			// 一个网口：网口外形 + 名称 + 角色 + 插线灯。点一下 = 把它设为 WAN 口。
			function port(p) {
				var on = (portSel === p.v);
				var up = !!(p.chip && st.links && st.links[p.chip]);
				var i = E('input', { type: 'radio', name: 'fzs-port', value: p.v });
				i.checked = on;                                  // ★ 设 DOM property
				var t = E('div', { 'class': 'fzs-port' + (on ? ' on' : '') }, [
					i,
					E('div', { 'class': 'fzs-jack' }),
					E('div', { 'class': 'fzs-pname' }, [
						p.t,
						E('span', { 'class': 'fzs-led' + (up ? ' up' : ''), title: up ? '已插网线' : '未插网线' })
					]),
					E('div', { 'class': 'fzs-prole' }, on ? 'WAN' : 'LAN')
				]);
				t.addEventListener('click', function() {
					if (portSel !== p.v) { portSel = p.v; draw(); }
				});
				return t;
			}

			// 「不使用 WAN 口」——三个口都作 LAN（同一组选项，用普通选项行，不再抢一个框）
			function noWan() {
				var on = (portSel == 'alllan');
				var i = E('input', { type: 'radio', name: 'fzs-port', value: 'alllan' });
				i.checked = on;
				var t = E('label', { 'class': 'fzs-opt fzs-nolan' }, [
					i,
					E('span', {}, [
						E('div', { 'class': 't' }, '不使用 WAN 口'),
						E('div', { 'class': 'd' }, '三个口都作 LAN')
					])
				]);
				t.addEventListener('click', function() {
					if (portSel != 'alllan') { portSel = 'alllan'; draw(); }
				});
				return t;
			}

			// 背板上的静态装饰（天线 / 电源口 / SIM 卡槽）：纯示意，不可点也不进 Tab 顺序
			var kids = [
				E('span', { 'class': 'fzs-ant fzs-ant-l' }),
				E('span', { 'class': 'fzs-ant fzs-ant-r' }),
				E('div', { 'class': 'fzs-static' }, [
					E('div', { 'class': 'fzs-dc' }),
					E('div', { 'class': 'fzs-sname' }, 'DC')
				])
			];
			for (var pi = 0; pi < PORTS.length; pi++) kids.push(port(PORTS[pi]));
			kids.push(E('div', { 'class': 'fzs-static' }, [
				E('div', { 'class': 'fzs-sim' }),
				E('div', { 'class': 'fzs-sname' }, 'SIM')
			]));

			return section('网口', [
				E('div', { 'class': 'cbi-section-descr' }, '点一下要作 WAN 的口。灯亮表示该口插了网线。'),
				E('div', { 'class': 'fzs-panel' }, kids),
				noWan(),
				E('div', { 'class': 'fzs-warn' },
					'⚠️ 切换会断开该口的网络连接，请从另一个网口或 WiFi 访问。'),
				E('div', { 'class': 'fzs-actions' }, [
					E('button', { 'class': 'cbi-button cbi-button-apply', click: save }, '保存'),
					statusEl('port')
				])
			]);
		}

		// ---------------- ④ 无线网络（2.4G 发射功率） ----------------
		// 全部动作在后端 /usr/libexec/fzs-wifi：改 country + txpower、等无线稳定、不达标自动还原。
		// 这里只负责渲染与提交。
		// ★ 明面上**不写效果承诺、也不写"不及原厂"** —— 那句挪进确认弹窗：不想碰这个开关的人，
		//   不该先被一段文字劝退；真要用的人，在按下确认前把话说清楚。
		function wifiCard() {
			var sup = !!(wf && wf.ok && wf.supported);
			var on = !!(wf && wf.unlocked);
			var cur = (wf && wf.dbm) ? wf.dbm : '';
			var onDbm = (wf && wf.on_dbm) || 23;
			var offDbm = (wf && wf.off_dbm) || 20;

			var chk = E('input', { type: 'checkbox' });
			chk.checked = on;                       // ★ 设 DOM property，绝不写成 HTML 属性

			function apply(want) {
				chk.disabled = true;
				setMsg('wifi', mInfo('应用中…无线会断开重连'));
				callWifi([ want ? 'on' : 'off' ]).then(function(r) {
					if (!r || !r.ok) {
						draw();
						setMsg('wifi', mErr(r || { err: '执行失败' }));
						return;
					}
					wf = r;
					draw();
					setMsg('wifi', mOk(want ? '已开启' : '已关闭，已还原'));
				}).catch(function() {
					// 用户多半正用无线连着面板 —— 换国家码会让无线重连，这次请求可能收不到回包。
					draw();
					setMsg('wifi', mInfo('已下发。无线刚重连过，若上面没更新请刷新页面。'));
				});
			}

			// ★ 只有一个开关，就不设「保存」了 —— 勾上/取消直接弹确认。
			//   面板里"总开关"一贯这么做（短信中心那个总开关也是 change 即生效），
			//   同一个面板里两个总开关不该一个要按保存、一个不用。
			//   ⚠️ 勾选框已经被浏览器先改了，所以「取消」时必须把它恢复原状。
			chk.addEventListener('change', function() {
				var want = chk.checked;
				function undo() { chk.checked = !want; }
				if (want) {
					confirmBox('开启后会断开无线重连', [
						E('div', {}, '· 2.4G 发射功率会从 ' + offDbm + ' dBm 提到 ' + onDbm + ' dBm（本机硬件上限）'),
						E('div', { style: 'margin-top:.4em' },
							'· 无线会断开重连，约 10 秒。如果你正用无线连着这台机器，重连后请刷新本页面。'),
						E('div', { style: 'margin-top:.4em' }, '· 即使开启，也仍不及原厂固件的无线信号。')
					], function() { apply(true); }, '确认开启', undo);
				} else {
					confirmBox('关闭后会断开无线重连', [
						E('div', {}, '· 2.4G 发射功率会还原成 ' + offDbm + ' dBm（出厂设置）'),
						E('div', { style: 'margin-top:.4em' },
							'· 无线会断开重连，约 10 秒。如果你正用无线连着这台机器，重连后请刷新本页面。')
					], function() { apply(false); }, '确认关闭', undo);
				}
			});

			var body;
			if (!sup) {
				body = [ E('div', { 'class': 'cbi-section-descr' }, '这台设备没有可用的 2.4G 无线。') ];
			} else {
				body = [
					E('div', { 'class': 'cbi-section-descr' },
						'当前发射功率：' + (cur ? cur + ' dBm' : '未知')),
					E('label', { 'class': 'fzs-opt' }, [
						chk,
						E('span', {}, [ E('div', { 'class': 't' },
							'提高 2.4G 发射功率（' + offDbm + ' → ' + onDbm + ' dBm）') ])
					]),
					E('div', { style: 'margin-top:.35em' }, [ statusEl('wifi') ]),
					// 名称/密码只能去 LuCI 的无线页改（那一页的编辑框是弹窗，没有可直达的地址）。
					// 形态与「打开 MultiWAN 管理器」一致：灰底导航按钮 —— 面板里跳转就这一种做法，
					// 不引入新元素（文字链接会跟整体风格打架）。
					E('div', { 'class': 'fzs-grp' }, [
						E('a', {
							'class': 'btn cbi-button',
							href: L.url('admin/network/wireless')
						}, '改 WiFi 名称和密码')
					])
				];
			}
			return section('无线网络', body);
		}

		draw();
		return wrap;
	},

	handleSave: null,
	handleSaveApply: null,
	handleReset: null
});
