'use strict';
'require view';
'require fs';
'require ui';

var FZS_AT = '/usr/libexec/fzs-at';

var TARGETS = [
	{ v: '1', name: '模组1 (EC200T)' },
	{ v: '2', name: '模组2 (EC200N)' }
];

var FDD = [
	{ b: '1', f: '2100M' }, { b: '3', f: '1800M' }, { b: '5', f: '850M' }, { b: '8', f: '900M' }
];
var TDD = [
	{ b: '34', f: '2000M' }, { b: '38', f: '1900M' }, { b: '39', f: '1900M' },
	{ b: '40', f: '2300M' }, { b: '41', f: '2500M' }
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
	'.fzs-cur{font-family:monospace}',
	'.fzs-grp{padding:.5em 0 .2em}',
	'.fzs-grp>h4{margin:0 0 .4em;font-size:.85em;color:var(--text-color-medium);font-weight:600}',
	'.fzs-bands{display:flex;flex-wrap:wrap;gap:.4em 1.2em}',
	'.fzs-bands label{display:flex;align-items:center;gap:.35em;font-size:.92em;cursor:pointer}',
	// ★ 复选框对齐（2026-10-11）：主题常给 `label > input[type=checkbox]{position:relative;top:.4rem}`
	//   （Argon 就是，0.4rem = 6.4px）⇒ 复选框相对文字下沉。这里把位移归零，
	//   **不碰外观**（各主题的原生勾选框样式都保留），只消除偏移 ⇒ 换主题也不会错乱。
	'.fzs-bands label>input[type=checkbox]{position:relative;top:0;margin:0;flex:none}',
	'.fzs-warn{margin:.9em 0;padding:.7em .9em;border-radius:5px;font-size:.85em;line-height:1.5;background:rgba(255,152,0,.12);border:1px solid rgba(255,152,0,.4)}',
	'.fzs-danger{background:rgba(244,67,54,.1);border-color:rgba(244,67,54,.35)}',
	'.fzs-actions{margin-top:1em;display:flex;gap:.6em;align-items:center;flex-wrap:wrap}',
	'.fzs-status{font-size:.88em}'
].join('\n');

function call(args) {
	return fs.exec(FZS_AT, args).then(function(r) {
		var out = (r && r.stdout) || '';
		var err = ((r && r.stderr) || '').trim().split('\n').pop().replace(/^fzs-at:\s*/, '');
		var d = null;
		try { d = JSON.parse(out); } catch (e) {}
		if (!d || typeof d !== 'object') {
			return { ok: false, err: err || (out.trim() ? '返回格式错误' : ('执行失败（退出码 ' + ((r && r.code) != null ? r.code : '?') + '）')) };
		}
		if (!d.ok && !d.err) d.err = err || '执行失败';
		return d;
	}).catch(function() { return { ok: false, err: '调用失败' }; });
}

return view.extend({
	load: function() {
		L.env.rpctimeout = 120;
		return null;
	},

	render: function() {
		var boxes = {};
		var status = E('div', { 'class': 'fzs-status' }, '');
		var curTxt = E('span', { 'class': 'fzs-cur' }, '读取中…');

		var sel = E('select', {
			'class': 'cbi-input-select',
			style: 'box-sizing:border-box;width:22em;max-width:100%;font-family:monospace',
			change: function() { loadCur(); }
		}, TARGETS.map(function(t) { return E('option', { value: t.v }, t.name); }));

		function mkBoxes(list) {
			return list.map(function(x) {
				var cb = E('input', { type: 'checkbox', value: x.b });
				boxes[x.b] = cb;
				return E('label', {}, [ cb, 'B' + x.b + ' (' + x.f + ')' ]);
			});
		}

		function setBands(str) {
			var on = (str || '').split(',');
			Object.keys(boxes).forEach(function(b) {
				boxes[b].checked = on.indexOf(b) >= 0;
			});
		}

		function loadCur() {
			curTxt.textContent = '读取中…';
			return call([ 'band', 'read', sel.value ]).then(function(d) {
				if (d.ok) {
					curTxt.textContent = d.bands ? d.bands.split(',').map(function(b) { return 'B' + b; }).join(' ') : '（无）';
					setBands(d.bands);
				} else {
					curTxt.textContent = d.err || '读取失败';
				}
				return d;
			});
		}

		function selected() {
			return Object.keys(boxes).filter(function(b) { return boxes[b].checked; }).sort(function(a, c) { return a - c; });
		}

		function doWrite(list, label) {
			if (!list.length) { status.textContent = '请至少勾选一个频段'; return; }
			ui.showModal('确认' + label, [
				E('p', {}, [ '目标：', E('b', {}, TARGETS[sel.selectedIndex].name) ]),
				E('p', {}, [ '将允许的频段：', E('b', {}, list.map(function(b) { return 'B' + b; }).join(' ')) ]),
				E('p', { style: 'color:#e53935' }, '应用后设备会重新驻网，会有几秒到十几秒的无服务。'),
				E('div', { 'class': 'right' }, [
					E('button', { 'class': 'btn', click: ui.hideModal }, '取消'),
					E('button', {
						'class': 'btn cbi-button-apply',
						click: function() {
							ui.hideModal();
							status.textContent = '应用中…（设备重新驻网）';
							call([ 'band', 'write', sel.value, list.join(',') ]).then(function(d) {
								if (d.ok) {
									status.textContent = '✅ 已应用（' + d.value + '）'
										+ (d.net === 'ok' ? '　网络已自动恢复'
											: '　⚠️ 网络未恢复 —— 建议点「恢复出厂频段」，必要时重启设备');
									loadCur();
								} else {
									status.textContent = '❌ ' + (d.err || '失败');
									loadCur();
								}
							});
						}
					}, '确认应用')
				])
			]);
		}

		var card = E('div', { 'class': 'cbi-section fzs-card' }, [
			E('h3', { 'class': 'fzs-card-h' }, '锁频段'),
			E('div', { 'class': 'cbi-section-node fzs-body' }, [
				E('div', { 'class': 'fzs-row' }, [ E('label', {}, '目标模组'), sel ]),
				E('div', { 'class': 'fzs-row' }, [ E('label', {}, '当前允许'), curTxt ]),
				E('div', { 'class': 'fzs-grp' }, [ E('h4', {}, 'FDD-LTE'), E('div', { 'class': 'fzs-bands' }, mkBoxes(FDD)) ]),
				E('div', { 'class': 'fzs-grp' }, [ E('h4', {}, 'TDD-LTE'), E('div', { 'class': 'fzs-bands' }, mkBoxes(TDD)) ]),
				E('div', { 'class': 'fzs-warn fzs-danger' }, [
					'⚠️ 至少勾选一个频段。',
					E('br'),
					'⚠️ ',
					E('b', {}, '移动卡用户注意'),
					'：',
					E('b', {}, '模组1（EC200T）'),
					'的固件有个已知缺陷 —— ',
					E('b', {}, 'TDD 频段一个都不勾'),
					'时，模组会反复重启、一直稳定不下来。想固定用 FDD，',
					E('b', {}, '保留任意一个 TDD 频段勾选'),
					'即可避开，或改用「锁定频点/小区」。'
				]),
				E('div', { 'class': 'fzs-actions' }, [
					E('button', { 'class': 'cbi-button cbi-button-apply', click: function() { doWrite(selected(), '应用频段'); } }, '应用勾选'),
					// ★ 主操作 ⇒ action（蓝）。裸 cbi-button 是中性灰，容易被误当成"不可点"。
					E('button', { 'class': 'cbi-button cbi-button-action', click: function() { doWrite([ '1', '3', '5', '8', '34', '38', '39', '40', '41' ], '恢复出厂频段'); } }, '恢复出厂频段'),
					status
				])
			])
		]);

		loadCur();

		return E('div', {}, [ E('style', {}, CSS), card ]);
	},

	handleSave: null,
	handleSaveApply: null,
	handleReset: null
});
