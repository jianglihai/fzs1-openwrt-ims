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

var EARFCN_RANGE = [
	{ b: '1', lo: 0, hi: 599 }, { b: '3', lo: 1200, hi: 1949 },
	{ b: '5', lo: 2400, hi: 2649 }, { b: '8', lo: 3450, hi: 3799 },
	{ b: '34', lo: 36200, hi: 36349 }, { b: '38', lo: 37750, hi: 38249 },
	{ b: '39', lo: 38250, hi: 38649 }, { b: '40', lo: 38650, hi: 39649 },
	{ b: '41', lo: 39650, hi: 41589 }
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
	'.fzs-tip{font-size:.8em;margin-top:.35em;min-height:1.2em}',
	'.fzs-tip.ok{color:#43a047}',
	'.fzs-tip.bad{color:#e53935}',
	'.fzs-warn{margin:.9em 0;padding:.7em .9em;border-radius:5px;font-size:.85em;line-height:1.5;background:rgba(255,152,0,.12);border:1px solid rgba(255,152,0,.4)}',
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

function earfcnBand(n) {
	var v = parseInt(n, 10);
	if (isNaN(v)) return null;
	for (var i = 0; i < EARFCN_RANGE.length; i++)
		if (v >= EARFCN_RANGE[i].lo && v <= EARFCN_RANGE[i].hi) return EARFCN_RANGE[i].b;
	return null;
}

return view.extend({
	load: function() {
		L.env.rpctimeout = 120;
		return null;
	},

	render: function() {
		var status = E('div', { 'class': 'fzs-status' }, '');
		var curTxt = E('span', { 'class': 'fzs-cur' }, '读取中…');
		var tip = E('div', { 'class': 'fzs-tip' }, '');

		var sel = E('select', {
			'class': 'cbi-input-select',
			style: 'box-sizing:border-box;width:22em;max-width:100%;font-family:monospace',
			change: function() { loadCur(); }
		}, TARGETS.map(function(t) { return E('option', { value: t.v }, t.name); }));

		var inpE = E('input', {
			'class': 'cbi-input-text', type: 'text', maxlength: 6, placeholder: '如 38375',
			style: 'box-sizing:border-box;width:22em;max-width:100%;font-family:monospace',
			input: function() { validate(); }
		});
		var inpP = E('input', {
			'class': 'cbi-input-text', type: 'text', maxlength: 3, placeholder: '留空 = 只锁频点',
			style: 'box-sizing:border-box;width:22em;max-width:100%;font-family:monospace'
		});
		// 记住用过的频点 / 小区号：点开就下拉复用（纯前端，存浏览器本地）
		hist.attach(inpE, 'cell_earfcn');
		hist.attach(inpP, 'cell_pci');

		function validate() {
			var v = inpE.value.replace(/\D/g, '');
			if (inpE.value !== v) inpE.value = v;
			if (!v) { tip.textContent = ''; tip.className = 'fzs-tip'; return; }
			var b = earfcnBand(v);
			if (b) { tip.textContent = '→ 频段 B' + b; tip.className = 'fzs-tip ok'; }
			else { tip.textContent = '✗ 该频点不属于本设备支持的频段'; tip.className = 'fzs-tip bad'; }
		}

		function loadCur() {
			curTxt.textContent = '读取中…';
			return call([ 'cell', 'read', sel.value ]).then(function(d) {
				curTxt.textContent = d.ok
					? ('频段 B' + d.band + '　频点 ' + d.earfcn + '　PCI ' + d.pci)
					: (d.err || '读取失败');
				return d;
			});
		}

		function doLock(earfcn, pci, label) {
			var args = [ 'cell', 'lock', sel.value, String(earfcn) ];
			if (pci !== '' && pci != null) args.push(String(pci));
			ui.showModal('确认' + label, [
				E('p', {}, [ '目标：', E('b', {}, TARGETS[sel.selectedIndex].name) ]),
				E('p', {}, [ '频点 EARFCN：', E('b', {}, String(earfcn)), '（频段 B' + (earfcnBand(earfcn) || '?') + '）' ]),
				E('p', {}, [ '小区 PCI：', E('b', {}, (pci === '' || pci == null) ? '不指定（只锁频点）' : String(pci)) ]),
				E('p', { style: 'color:#e53935' }, '锁定后设备会重新搜网，会有十几秒到一分钟的无服务。若锁到没有信号的小区会一直无网，点「解除锁定」即可恢复。'),
				E('div', { 'class': 'right' }, [
					E('button', { 'class': 'btn', click: ui.hideModal }, '取消'),
					E('button', {
						'class': 'btn cbi-button-apply',
						click: function() {
							ui.hideModal();
							status.textContent = '锁定中…（设备重新搜网）';
							call(args).then(function(d) {
								if (d.ok) {
									status.textContent = '✅ 已锁定 频段B' + d.band + ' 频点' + d.earfcn + (d.pci ? (' PCI' + d.pci) : '')
										+ (d.net === 'ok' ? '　网络已恢复' : '　⚠️ 网络未恢复');
									loadCur();
								} else {
									status.textContent = '❌ ' + (d.err || '失败');
									loadCur();
								}
							});
						}
					}, '确认锁定')
				])
			]);
		}

		var card = E('div', { 'class': 'cbi-section fzs-card' }, [
			E('h3', { 'class': 'fzs-card-h' }, '锁定频点 / 小区'),
			E('div', { 'class': 'cbi-section-node fzs-body' }, [
				E('div', { 'class': 'fzs-row' }, [ E('label', {}, '目标模组'), sel ]),
				E('div', { 'class': 'fzs-row' }, [ E('label', {}, '当前服务小区'), curTxt ]),
				E('div', { 'class': 'fzs-row', style: 'align-items:flex-start' }, [
					E('label', {}, '频点 EARFCN'),
					E('div', {}, [ inpE, tip ])
				]),
				E('div', { 'class': 'fzs-row' }, [ E('label', {}, '小区 PCI'), inpP ]),
				E('div', { 'class': 'fzs-warn' }, [
					'「锁定频点」钉死在某个频点（同频点其他小区可接替，较稳）；填了 PCI 就是「锁定小区」（精确锁一个基站，它消失会无网）。',
					E('br'),
					'移动卡想固定用 FDD 频段，用这里锁频点，比在「锁频段」里关掉 TDD 安全得多。'
				]),
				E('div', { 'class': 'fzs-actions' }, [
					E('button', {
						'class': 'cbi-button cbi-button-action',
						click: function() {
							call([ 'cell', 'read', sel.value ]).then(function(d) {
								if (!d.ok) { status.textContent = '读不到当前小区，无法一键锁定'; return; }
								doLock(d.earfcn, d.pci, '锁定当前小区');
							});
						}
					}, '一键锁定当前'),
					E('button', {
						'class': 'cbi-button cbi-button-apply',
						click: function() { doLock(inpE.value, inpP.value, '应用锁定'); }
					}, '应用以上参数'),
					E('button', {
						'class': 'cbi-button cbi-button-negative',
						click: function() {
							status.textContent = '解除中…';
							call([ 'cell', 'unlock', sel.value ]).then(function(d) {
								if (d.ok) {
									status.textContent = '✅ 已解除锁定'
										+ (d.net === 'ok' ? '　网络已恢复' : '　⚠️ 网络未恢复');
									loadCur();
								} else {
									status.textContent = '❌ ' + (d.err || '失败');
									loadCur();
								}
							});
						}
					}, '解除锁定'),
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
