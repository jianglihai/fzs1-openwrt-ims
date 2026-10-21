'use strict';
'require view';
'require fs';
'require ui';
'require fzs.hist as hist';

var FZS_AT = '/usr/libexec/fzs-at';

var APN_PRESETS = [ 'auto', 'cmiot', 'cmnet', '3gnet', 'ctnet' ];

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
	'.fzs-card-h .fzs-model{font-size:.8em;font-weight:400;padding:.1em .5em;border-radius:3px;background:var(--background-color-low, #e9ebee);border:1px solid var(--border-color-low, #e5e8eb);color:var(--text-color-medium)}',
	'.fzs-badge{margin-left:auto;font-size:.78em;padding:.15em .6em;border-radius:10px;font-weight:600}',
	'.fzs-badge.ok{background:rgba(76,175,80,.18);color:#4caf50;border:1px solid rgba(76,175,80,.45)}',
	'.fzs-badge.warn{background:rgba(255,152,0,.18);color:#fb8c00;border:1px solid rgba(255,152,0,.45)}',
	'.fzs-badge.bad{background:rgba(244,67,54,.15);color:#e53935;border:1px solid rgba(244,67,54,.4)}',
	'.fzs-row{display:flex;align-items:center;gap:.8em;padding:.4em 0}',
	'.fzs-row>label{width:6.5em;flex:none;color:var(--text-color-medium);font-size:.9em}',
	'.fzs-val{font-family:monospace;font-size:.95em;overflow:hidden;text-overflow:ellipsis;white-space:nowrap}',
	'.fzs-link{font-size:.78em;padding:.15em .5em;border-radius:3px;background:var(--background-color-low, #e9ebee);border:1px solid var(--border-color-low, #e5e8eb);color:var(--text-color-medium)}',
	'.fzs-actions{margin-top:.9em;display:flex;gap:.6em;align-items:center;flex-wrap:wrap}',
	// ★ 真禁用必须"一眼看出禁用"（2026-10-11）：主题给中性按钮灰底灰字，与"禁用态"太像，
	//   用户会误以为可点的中性按钮是禁用的。这里把**真正 disabled** 的按钮明确淡掉 + 禁止光标。
	//   `:where()` 保证特异性为 0，主题若有自己的禁用样式仍可覆盖。
	':where(.fzs-actions button:disabled,.fzs-card button:disabled){opacity:.45;cursor:not-allowed;filter:grayscale(.4)}',
	'.fzs-status{font-size:.85em}',
	'.fzs-err{color:var(--text-color-low);font-size:.85em;min-height:1.2em;margin:.15em 0}',
	'.fzs-hint{font-size:.82em;color:var(--text-color-medium);line-height:1.5;padding:.2em 0 .1em}',
	'.fzs-warn{margin:.9em 0;padding:.7em .9em;border-radius:5px;font-size:.85em;line-height:1.5;background:rgba(255,152,0,.12);border:1px solid rgba(255,152,0,.4)}'
].join('\n');

function fetchAll() {
	return fs.exec(FZS_AT, [ 'apn', 'read' ]).then(function(r) {
		try {
			var a = JSON.parse((r && r.stdout) || '[]');
			return [ a[0] || null, a[1] || null ];
		} catch (e) { return [ null, null ]; }
	}).catch(function() { return [ null, null ]; });
}

function errMsg(r, d) {
	if (d && d.err) return d.err;
	return ((r && r.stderr) || '').trim().split('\n').pop().replace(/^fzs-at:\s*/, '') || '操作未确认成功';
}

function failMsg(e) {
	var s = (e && e.message) ? e.message : String(e);
	return '❌ 调用失败：' + s + '（可点「刷新」查看当前状态）';
}

return view.extend({
	load: function() {
		L.env.rpctimeout = 120;
		return null;
	},

	render: function() {
		var self = this;
		var stamp = E('span', {}, '');

		var cards = [];

		function card(i, name) {
			var badge = E('span', { 'class': 'fzs-badge bad' }, '—');
			var sel = E('select', { 'class': 'cbi-input-select', style: 'box-sizing:border-box;width:14em;max-width:100%' },
				APN_PRESETS.map(function(v) {
					return E('option', { value: v }, v === 'auto' ? 'auto（自动探测）' : v);
				}).concat([ E('option', { value: 'custom' }, '自定义…') ]));
			var inp = E('input', {
				'class': 'cbi-input-text', type: 'text', maxlength: 32,
				placeholder: '例如 cmnet 或 cmiot',
				style: 'box-sizing:border-box;width:14em;max-width:100%;font-family:monospace;display:none'
			});
			// 记住用过的 APN（纯前端，存浏览器本地）
			hist.attach(inp, 'apn');
			var hint = E('div', {
				'class': 'fzs-hint', style: 'display:none;max-width:34em'
			}, 'APN 是一串小写字母 / 数字 / 点。运营商的就是 cmiot、cmnet、3gnet、ctnet 这几样；物联卡的私有 APN 在卡商的发货说明或公告里查（长得像 abc.gd.mnc024.mcc460）。拿不准就先别填，auto 大概率能通。');
			var act = E('span', { 'class': 'fzs-val' }, '—');
			var good = E('span', { 'class': 'fzs-val' }, '—');
			var link = E('span', { 'class': 'fzs-link' }, '—');
			var errline = E('div', { 'class': 'fzs-err' }, '');
			var status = E('span', { 'class': 'fzs-status' }, '');

			var c = { badge: badge, sel: sel, inp: inp, act: act, good: good, link: link, errline: errline, status: status, data: null, name: name };

			sel.addEventListener('change', function() {
				var custom = sel.value === 'custom';
				inp.style.display = custom ? '' : 'none';
				hint.style.display = custom ? '' : 'none';
			});

			function chosenApn() {
				return sel.value === 'custom' ? inp.value.trim() : sel.value;
			}

			var btnApn = E('button', { 'class': 'cbi-button cbi-button-action' }, '应用 APN');
			btnApn.addEventListener('click', function() {
				var apn = chosenApn();
				if (!apn || sel.value === 'custom' && !/^[a-zA-Z0-9._-]+$/.test(apn)) {
					status.textContent = '请先选择或填写合法的 APN';
					return;
				}
				var m = c.data || {};
				var tip = (m.link === 'ppp')
					? '该模组走 PPP 通路：写入后将发起重新拨号，稍后自动生效。'
					: '应用过程中该模组会重新激活数据通路，断网约 10~30 秒。';
				ui.showModal('确认修改 APN', [
					E('table', { 'class': 'table' }, [
						E('tr', {}, [ E('th', {}, '目标'), E('td', {}, m.name || name) ]),
						E('tr', {}, [ E('th', {}, '当前配置'), E('td', {}, m.apn_cfg || '—') ]),
						E('tr', {}, [ E('th', {}, '改为'), E('td', {}, E('b', {}, apn)) ])
					]),
					E('p', {}, tip),
					E('div', { 'class': 'right' }, [
						E('button', { 'class': 'btn', click: ui.hideModal }, '取消'),
						E('button', {
							'class': 'btn cbi-button-action',
							click: function() {
								ui.hideModal();
								status.textContent = '应用中…（请勿关闭页面）';
								fs.exec(FZS_AT, [ 'apn', 'write', m.cfg, apn ]).then(function(r) {
									var d = null;
									try { d = JSON.parse((r && r.stdout) || ''); } catch (e) {}
									if (d && d.ok && d.net === 'ok') status.textContent = '✅ 已用 ' + apn + ' 激活并拿到 IP';
									else if (d && d.ok && d.net === 'wait') status.textContent = '✅ 已写入，重新拨号进行中，稍后自动生效';
									else if (d && d.ok) status.textContent = '⚠️ 已写入，但数据通路未起来，可点「启用数据」重试';
									else status.textContent = '❌ ' + errMsg(r, d);
									return refresh();
								}).catch(function(e) { status.textContent = failMsg(e); return refresh(); });
							}
						}, '确认修改')
					])
				]);
			});

			// ★ 用 cbi-button-action（主色/蓝）：这是**主操作**，且「停用数据」会断网、不能看着像禁用。
			//   裸 `cbi-button` 是中性灰底灰字，视觉上极易被误认为"不可点"。
			var btnToggle = E('button', { 'class': 'cbi-button cbi-button-action' }, '停用数据');
			btnToggle.addEventListener('click', function() {
				var m = c.data || {};
				if (!m.cfg) { status.textContent = '还没读到模组状态，先点「刷新」'; return; }
				if (m.data === 'on') {
					ui.showModal('确认停用该模组数据', [
						E('p', {}, '停用后该卡彻底断网（模组数据通路 + 系统接口同时关掉），流量由另一模组接管（若在线）。'),
						E('p', {}, '重新开启后约 40 秒才会重新参与流量分流。'),
						E('div', { 'class': 'right' }, [
							E('button', { 'class': 'btn', click: ui.hideModal }, '取消'),
							E('button', {
								'class': 'btn cbi-button-negative',
								click: function() {
									ui.hideModal();
									status.textContent = '停用中…';
									fs.exec(FZS_AT, [ 'data', 'off', m.cfg ]).then(function() {
										status.textContent = '✅ 已停用';
										return refresh();
									}).catch(function(e) { status.textContent = failMsg(e); return refresh(); });
								}
							}, '确认停用')
						])
					]);
				} else {
					status.textContent = '启用中…（重新参与分流约需 40 秒）';
					fs.exec(FZS_AT, [ 'data', 'on', m.cfg ]).then(function(r) {
						var d = null;
						try { d = JSON.parse((r && r.stdout) || ''); } catch (e) {}
						if (d && d.ok && d.net === 'ok') status.textContent = '✅ 数据已启用';
						else status.textContent = '⚠️ 已发起启用，但路由未出现（模组可能未驻网），稍后点「刷新」确认';
						return refresh();
					}).catch(function(e) { status.textContent = failMsg(e); return refresh(); });
				}
			});

			var btnFly = E('button', { 'class': 'cbi-button cbi-button-action' }, '飞行模式');
			btnFly.addEventListener('click', function() {
				var m = c.data || {};
				if (!m.cfg) { status.textContent = '还没读到模组状态，先点「立即刷新」'; return; }
				if (m.cfun === '0') {
					status.textContent = '恢复中…（重新驻网约 20~40 秒）';
					fs.exec(FZS_AT, [ 'fun', 'on', m.cfg ]).then(function(r) {
						var d = null;
						try { d = JSON.parse((r && r.stdout) || ''); } catch (e) {}
						if (d && d.ok && d.net === 'ok') status.textContent = '✅ 已退出飞行模式，数据已启用';
						else status.textContent = '⚠️ 已退出飞行模式，模组还在驻网，稍后点「立即刷新」确认';
						return refresh();
					}).catch(function(e) { status.textContent = failMsg(e); return refresh(); });
				} else {
					ui.showModal('确认开启飞行模式', [
						E('p', {}, '开启后该模组射频全部关闭：信号、短信、上网全部断开，基站里查无此卡。'),
						E('p', {}, '关闭飞行模式后需 20~40 秒重新驻网。'),
						E('div', { 'class': 'right' }, [
							E('button', { 'class': 'btn', click: ui.hideModal }, '取消'),
							E('button', {
								'class': 'btn cbi-button-negative',
								click: function() {
									ui.hideModal();
									status.textContent = '开启中…';
									fs.exec(FZS_AT, [ 'fun', 'off', m.cfg ]).then(function(r) {
										var d = null;
										try { d = JSON.parse((r && r.stdout) || ''); } catch (e) {}
										status.textContent = (d && d.ok) ? '✅ 已开启飞行模式' : ('❌ ' + errMsg(r, d));
										return refresh();
									}).catch(function(e) { status.textContent = failMsg(e); return refresh(); });
								}
							}, '确认开启')
						])
					]);
				}
			});

			var body = E('div', { 'class': 'cbi-section-node fzs-body' }, [
				E('div', { 'class': 'fzs-row' }, [ E('label', {}, '配置 APN'), sel, inp ]),
				hint,
				E('div', { 'class': 'fzs-row' }, [ E('label', {}, '数据通道'), act ]),
				E('div', { 'class': 'fzs-row' }, [ E('label', {}, 'auto 记忆'), good ]),
				E('div', { 'class': 'fzs-row' }, [ E('label', {}, '链路模式'), link ]),
				errline,
				E('div', { 'class': 'fzs-actions' }, [ btnApn, btnToggle, btnFly, status ])
			]);

			var cardEl = E('div', { 'class': 'cbi-section fzs-card' }, [
				E('h3', { 'class': 'fzs-card-h' }, [
					E('span', {}, name),
					badge
				]),
				body
			]);
			cards[i] = c;
			c.btnToggle = btnToggle;
			c.btnFly = btnFly;
			return cardEl;
		}

		function apply(c, m) {
			c.data = m;
			if (!m || !m.ok) {
				c.badge.textContent = '—';
				c.btnToggle.disabled = true;
				c.btnFly.disabled = true;
				c.btnToggle.textContent = '启用数据';
				c.errline.textContent = (m && (m.err || '读取失败')) || '读取失败';
				return;
			}
			c.btnToggle.disabled = false;
			c.btnFly.disabled = false;
			if (m.cfun === '0') {
				c.badge.textContent = '飞行模式';
				c.badge.className = 'fzs-badge warn';
				c.btnToggle.disabled = true;
				c.btnToggle.textContent = '启用数据';
				c.btnFly.textContent = '关闭飞行模式';
			} else {
				c.badge.textContent = '数据 ' + (m.data === 'on' ? '开' : '关');
				c.badge.className = 'fzs-badge ' + (m.data === 'on' ? 'ok' : 'bad');
				c.btnToggle.textContent = (m.data === 'on') ? '停用数据' : '启用数据';
				c.btnFly.textContent = '飞行模式';
			}
			c.errline.textContent = m.err || '';
			c.badge.textContent = '数据 ' + (m.data === 'on' ? '开' : '关');
			c.badge.className = 'fzs-badge ' + (m.data === 'on' ? 'ok' : 'bad');
			var preset = APN_PRESETS.indexOf(m.apn_cfg) >= 0 ? m.apn_cfg : 'custom';
			c.sel.value = preset;
			if (preset === 'custom') {
				c.inp.style.display = '';
				c.inp.value = m.apn_cfg;
			} else {
				c.inp.style.display = 'none';
			}
			c.act.textContent = m.ip ? ('已激活 · IP ' + m.ip) : '未激活';
			if (m.apn_act) c.act.setAttribute('title', '网络侧回报通道：' + m.apn_act + '（移动网络固定回报名，与所填 APN 无关）');
			c.good.textContent = m.apn_cfg === 'auto' && m.apn_good ? m.apn_good : '—';
			c.link.textContent = m.link || '—';
		}

		function refresh() {
			btn.disabled = true;
			stamp.textContent = '读取中…';
			return fetchAll().then(function(list) {
				apply(cards[0], list[0]);
				apply(cards[1], list[1]);
				stamp.textContent = '数据读取于 ' + new Date().toLocaleTimeString();
			}).catch(function() {
				stamp.textContent = '读取失败，请重试';
			}).then(function() {
				btn.disabled = false;
			});
		}

		var grid = E('div', { 'class': 'fzs-grid' }, [
			card(0, '模组1 (EC200T)'),
			card(1, '模组2 (EC200N)')
		]);

		var btn = E('button', { 'class': 'cbi-button cbi-button-action', click: refresh }, '立即刷新');
		var head = E('div', { style: 'display:flex;align-items:center;justify-content:flex-end;gap:.8em;margin-bottom:1em' }, [ stamp, btn ]);

		var warn = E('div', { 'class': 'fzs-warn' }, [
			E('b', {}, '数据 关'), ' = 停掉模组数据通路并撤销系统接口，流量由另一模组接管；重新启用后约 ',
			E('b', {}, '40 秒'), '才会重新参与流量分流。',
			E('br'),
			'APN 选 ', E('b', {}, 'auto'), ' 时固件会依次探测 cmiot / cmnet / 3gnet / ctnet（最多约 7 分钟）；',
			E('b', {}, '物联卡请填卡商指定的 APN'), '，本固件仅支持 APN 名称（不含账密认证）。'
		]);

		refresh();

		return E('div', {}, [ E('style', {}, CSS), head, grid, warn ]);
	},

	handleSave: null,
	handleSaveApply: null,
	handleReset: null
});
