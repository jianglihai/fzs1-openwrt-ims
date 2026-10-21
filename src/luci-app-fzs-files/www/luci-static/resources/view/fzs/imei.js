'use strict';
'require view';
'require fs';
'require ui';
'require fzs.hist as hist';

var FZS_AT = '/usr/libexec/fzs-at';

var TARGETS = [
	{ v: '1', name: '模组1 (EC200T)' },
	{ v: '2', name: '模组2 (EC200N)' }
];

var CSS = [
	':where(.fzs-card){background:var(--background-color-high, #ffffff);border:1px solid var(--border-color-medium, #d4d8dc);border-radius:6px;overflow:hidden}',
	// ★ 卡片内边距必须由自己给：有的主题把 .cbi-section 的 padding 归零（Argon 就是），
	//   而 :where() 特异性为 0，会被主题压掉 ⇒ 内容贴到卡片边框上（勾选框贴着边）。
	//   这里改用普通类选择器提供内边距——主题不认识 .fzs-card/.fzs-body，不会覆盖。
	'.cbi-section.fzs-card{padding:0}',
	'.cbi-section.fzs-card>h3{padding:.8em 1.1em}',
	'.cbi-section.fzs-card>.fzs-body{padding:.15em 1.1em 1.05em;max-width:680px}',
	'.cbi-section.fzs-card .cbi-section-descr{padding-left:0;padding-right:0}',
	'.fzs-card-h{display:flex;align-items:center;gap:.6em;flex-wrap:wrap}',
	'.fzs-row{display:flex;align-items:center;gap:.8em;padding:.4em 0}',
	'.fzs-row>label{width:7em;flex:none;color:var(--text-color-medium);font-size:.9em}',
	'.fzs-tip{font-size:.8em;margin-top:.35em;min-height:1.2em}',
	'.fzs-tip.ok{color:#43a047}',
	'.fzs-tip.bad{color:#e53935}',
	'.fzs-warn{margin:.9em 0;padding:.7em .9em;border-radius:5px;font-size:.85em;line-height:1.5;background:rgba(244,67,54,.1);border:1px solid rgba(244,67,54,.35)}',
	'.fzs-actions{margin-top:1em;display:flex;gap:.6em;align-items:center;flex-wrap:wrap}',
	'.fzs-status{font-size:.88em}'
].join('\n');

function luhnOk(s) {
	var sum = 0, alt = false;
	for (var i = s.length - 1; i >= 0; i--) {
		var n = parseInt(s.charAt(i), 10);
		if (isNaN(n)) return false;
		if (alt) { n *= 2; if (n > 9) n -= 9; }
		sum += n; alt = !alt;
	}
	return sum % 10 === 0;
}

function luhnComplete(s14) {
	for (var d = 0; d <= 9; d++)
		if (luhnOk(s14 + d)) return s14 + d;
	return null;
}

function readImeis() {
	return fs.exec(FZS_AT, [ 'imei', 'read' ]).then(function(r) {
		try { return JSON.parse((r && r.stdout) || '[]'); } catch (e) { return []; }
	}).catch(function() { return []; });
}

return view.extend({
	load: function() {
		L.env.rpctimeout = 60;
		return null;
	},

	render: function() {
		var cur = {};

		function label(t) {
			return t.name + '　' + (cur[t.v] || '读取中…');
		}

		var sel = E('select', {
			'class': 'cbi-input-select',
			style: 'box-sizing:border-box;width:22em;max-width:100%;font-family:monospace'
		}, TARGETS.map(function(t) { return E('option', { value: t.v }, label(t)); }));

		function loadCur() {
			return readImeis().then(function(list) {
				(list || []).forEach(function(x) {
					if (!x || !x.name) return;
					if (x.name.indexOf('模组1') >= 0) cur['1'] = x.imei || '（读不到）';
					else if (x.name.indexOf('模组2') >= 0) cur['2'] = x.imei || '（读不到）';
				});
				TARGETS.forEach(function(t, i) { sel.options[i].textContent = label(t); });
			});
		}

		var inp = E('input', {
			'class': 'cbi-input-text', type: 'text', maxlength: 15,
			placeholder: '15 位数字（14 位自动补校验位）',
			style: 'box-sizing:border-box;width:22em;max-width:100%;font-family:monospace',
			input: function() { validate(); }
		});
		// 记住以前填过的 IMEI（纯前端，存浏览器本地）
		hist.attach(inp, 'imei');

		var tip = E('div', { 'class': 'fzs-tip' }, '');
		var status = E('div', { 'class': 'fzs-status' }, '');

		function validate() {
			var v = inp.value.replace(/\D/g, '');
			if (inp.value !== v) inp.value = v;
			if (v.length === 0) { tip.textContent = ''; tip.className = 'fzs-tip'; return null; }
			if (v.length < 14) { tip.textContent = '还差 ' + (15 - v.length) + ' 位'; tip.className = 'fzs-tip bad'; return null; }
			if (v.length === 14) {
				var full = luhnComplete(v);
				tip.textContent = '校验位将补全为 ' + full.charAt(14) + ' → ' + full;
				tip.className = 'fzs-tip ok';
				return full;
			}
			if (v.length === 15) {
				if (luhnOk(v)) { tip.textContent = '✓ 校验位正确'; tip.className = 'fzs-tip ok'; return v; }
				tip.textContent = '✗ 校验位错误，建议改为 ' + luhnComplete(v.substr(0, 14));
				tip.className = 'fzs-tip bad';
				return null;
			}
			tip.textContent = '最多 15 位'; tip.className = 'fzs-tip bad';
			return null;
		}

		var btn = E('button', { 'class': 'cbi-button cbi-button-negative' }, '写入 IMEI');
		btn.addEventListener('click', function() {
			var v = validate();
			if (!v) { status.textContent = '请输入合法的 15 位 IMEI'; return; }

			var tgtName = TARGETS[sel.selectedIndex].name;
			var tgtCur = cur[sel.value] || '—';

			ui.showModal('确认写入 IMEI', [
				E('p', {}, '这是对模组 NVM 的【不可逆写入】，写入后模组会重启。'),
				E('table', { 'class': 'table' }, [
					E('tr', {}, [ E('th', {}, '目标模组'), E('td', {}, tgtName) ]),
					E('tr', {}, [ E('th', {}, '当前 IMEI'), E('td', {}, E('b', {}, tgtCur)) ]),
					E('tr', {}, [ E('th', {}, '将写入'), E('td', {}, E('b', {}, v)) ])
				]),
				E('p', { style: 'color:#e53935' }, '⚠️ 请确认这是你本人设备的 IMEI。改成非本机的值在多数国家/地区违法。'),
				E('div', { 'class': 'right' }, [
					E('button', { 'class': 'btn', click: ui.hideModal }, '取消'),
					E('button', {
						'class': 'btn cbi-button-negative',
						click: function() {
							ui.hideModal();
							status.textContent = '写入中…（模组会重启，约 25 秒，请勿断电）';
							fs.exec(FZS_AT, [ 'imei', 'write', v, sel.value, '--yes' ]).then(function(r) {
								var d = null;
								try { d = JSON.parse((r && r.stdout) || ''); } catch (e) {}
								if (d && d.ok) {
									status.textContent = '✅ 写入成功，回读确认 = ' + v;
								} else {
									var err = ((r && r.stderr) || '').trim().split('\n').pop().replace(/^fzs-at:\s*/, '');
									status.textContent = '❌ ' + ((d && d.err) || err || '写入未确认成功');
								}
								return loadCur();
							}).catch(function(e) {
								status.textContent = '❌ 调用失败：' + e;
							});
						}
					}, '确认写入')
				])
			]);
		});

		var card = E('div', { 'class': 'cbi-section fzs-card' }, [
			E('h3', { 'class': 'fzs-card-h' }, 'IMEI 改串'),
			E('div', { 'class': 'cbi-section-node fzs-body' }, [
				E('div', { 'class': 'fzs-row' }, [ E('label', {}, '目标模组'), sel ]),
				E('div', { 'class': 'fzs-row', style: 'align-items:flex-start' }, [
					E('label', {}, '新 IMEI'),
					E('div', {}, [ inp, tip ])
				]),
				E('div', { 'class': 'fzs-warn' }, [
					'本功能用于【恢复本机自己的 IMEI】（刷机/清 NVM 后丢失等场景）。',
					E('br'),
					'随意编造或写入他人设备的 IMEI 在多数国家/地区属于违法行为，请自行确认合法性。'
				]),
				E('div', { 'class': 'fzs-actions' }, [ btn, status ])
			])
		]);

		loadCur();

		return E('div', {}, [ E('style', {}, CSS), card ]);
	},

	handleSave: null,
	handleSaveApply: null,
	handleReset: null
});
