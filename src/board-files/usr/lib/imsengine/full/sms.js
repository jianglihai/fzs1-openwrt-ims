'use strict';
'require view';
'require fs';
'require poll';
'require ui';
'require uci';
'require fzs.hist as hist';

var FZS_AT = '/usr/libexec/fzs-at';
// 每页条数。长短信单条占高很大，20 条一页会让页面过长、要滚很久；
// 10 条兼顾"一屏能看几条"与"翻页不过于频繁"。
var PER_PAGE = 10;

// ★ IMS 通道（2026-10-20 接入）：imsengine 在用户态实现了 IMS（SIP MESSAGE over IP），
//   短信不经过模组的 AT+CMGS。EC200T 原生 CS 短信有 CMS 304/500 顽疾（固件不含 IMS 栈），
//   所以把 IMS 放在第一项并设为默认；选「模组1 (EC200T)」时 fzs-at 同样转发到 IMS 通道。
var MODEMS = [
	{ v: 'ims', name: 'IMS 通道（推荐 · 走数据通道）' },
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
	// ★★ 卡片头 h3 必须提高特异性（2026-10-11 踩雷，Argon 下标题/说明/按钮挤成一行）：
	//   主题里有更高特异性的规则 `Argon: .cbi-section > h3:first-child{display:block;width:100%}`（0,2,1）
	//   ⇒ 单靠 `.fzs-card-h`（0,1,0）**压不过它**，display:flex 失效 → 退回 block →
	//   `margin-left:auto` 的说明、`flex-wrap` 全失效，标题和按钮黏在一起。
	//   ⇒ 用 `.cbi-section>.fzs-card-h`（0,2,0）+ 本身位置也够，但为稳再叠一层 `:first-child`（0,2,1）**对齐**它的特异性，
	//     并在源码顺序上排在我们 CSS 的**后面**（后者同特异性胜出）。
	'.cbi-section>.fzs-card-h,.cbi-section>.fzs-card-h:first-child{display:flex;align-items:center;gap:.6em;flex-wrap:wrap;line-height:normal}',
	'.fzs-row{display:flex;align-items:center;gap:.8em;padding:.4em 0}',
	// ★★ 左列标签固定宽 —— 但 `.fzs-opt` 也是 <label>，必须**豁免**这条，
	//   否则选项区被按成 6.5em(≈82px)、里面的文字竖排（"开启短信收发"两字一行）。
	//   ⇒ 下面用 `.fzs-row>label:not(.fzs-opt)` 精确排除。
	'.fzs-row>label:not(.fzs-opt){width:6.5em;flex:none;color:var(--text-color-medium);font-size:.9em}',
	'.fzs-tip{font-size:.82em;color:var(--text-color-low);margin-top:.35em;min-height:1.2em}',
	// ★ 2026-10-08：渠道缺组件时的硬性提醒。用**红框 + 淡红底**，让它在表单里"跳出来"
	//   （原来的灰色 tip 离勾选框远、不显眼，而这是"必须先做才能用"的硬条件）。
	//   样式与 apn.js 的 .fzs-warn 同源思路，但红色更重、并带左侧粗边条。
	'.fzs-need{padding:.75em .95em;border-radius:6px;font-size:.86em;line-height:1.7;background:rgba(244,67,54,.1);border:1px solid rgba(244,67,54,.55);border-left:4px solid #f44336;color:var(--text-color-high);margin:.6em 0}',
	'.fzs-need b{color:#f44336}',
	// ★ 包名块：淡红底 + 等宽粗体 + `user-select:all`（点一下整块选中，方便但不强制）。
	'.fzs-need code{background:rgba(244,67,54,.14);padding:.12em .5em;border-radius:3px;font-family:monospace;font-weight:700;user-select:all;cursor:pointer}',
	'.fzs-actions{margin-top:1em;display:flex;gap:.6em;align-items:center;flex-wrap:wrap}',
	// ★ 真禁用要"一眼看出"（主题给中性按钮灰底灰字，容易与禁用态混淆）。`:where()` 不抢主题。
	':where(.fzs-actions button:disabled,.fzs-card button:disabled){opacity:.45;cursor:not-allowed;filter:grayscale(.4)}',
	'.fzs-status{font-size:.88em}',
	// ★ 卡片头里的提示位（「一个框一个提示」时把提示放 h3 里）：h3 是粗体大字，
	//   提示得**小字、常规粗细、右对齐**，否则会跟标题黏成一坨。
	// ★★ 卡片头排布规则（2026-10-11 统一，治"元素横在一条线上、有的贴边有的卡中间"）：
	//   ① 标题在左；
	//   ② **右侧那一组**（说明文字 / 按钮 / 状态）靠在一起、贴右 —— 由**第一个右区元素**吃 `margin-left:auto`；
	//   ③ 其余右区元素**只留小间距**，不再抢 auto。
	//   以前 `.fzs-sub` 与 `.fzs-status` 都写了 `margin-left:auto` ⇒ 两个抢最右，
	//   结果一个贴边、一个卡在中间（用户原话"乱成一团糟"）。
	'.fzs-card-h>*{flex:none}',                                       // 头里元素别被压扁
	'.fzs-card-h .fzs-sub{font-size:.78em;font-weight:400;color:var(--text-color-low)}',
	'.fzs-card-h .fzs-sub,.fzs-card-h>.fzs-status{font-size:.78em;font-weight:400;color:var(--text-color-low);margin-left:auto}',
	'.fzs-card-h .fzs-sub~*,.fzs-card-h>.fzs-status~*{margin-left:0}',  // 只有第一个右区元素吃 auto
	'.fzs-card-h button{margin-left:0}',
	'.fzs-card-h .fzs-sub+.fzs-status,.fzs-card-h button+.fzs-status{margin-left:.5em}',
	'.fzs-actions .fzs-status{margin-left:.3em}',
	'.fzs-msg{display:flex;gap:.8em;padding:.6em 0;border-bottom:1px solid var(--border-color-low, #e5e8eb);align-items:flex-start}',
	'.fzs-msg:last-child{border-bottom:0}',
	'.fzs-mb{flex:none;font-size:.72em;padding:.1em .5em;border-radius:3px;background:var(--background-color-low, #e9ebee);border:1px solid var(--border-color-low, #e5e8eb);color:var(--text-color-medium);margin-top:.1em}',
	'.fzs-mc{flex:1;min-width:0}',
	'.fzs-mh{display:flex;gap:.8em;font-size:.78em;color:var(--text-color-low);margin-bottom:.2em}',
	'.fzs-mh .fzs-from{font-family:monospace;font-weight:600;color:var(--text-color-medium)}',
	'.fzs-mt{white-space:pre-wrap;word-break:break-all;font-size:.92em}',
	'.fzs-del{flex:none;border:0;background:none;color:#e53935;cursor:pointer;opacity:.55;font-size:1em;padding:.1em .3em}',
	'.fzs-del:hover{opacity:1}',
	'.fzs-empty{color:var(--text-color-low);text-align:center;padding:1.5em 1em}',
	'.fzs-use{font-size:.78em;color:var(--text-color-low)}',
	'.fzs-pager{display:flex;align-items:center;justify-content:center;gap:.7em;padding:.9em 0 .1em;font-size:.85em;color:var(--text-color-medium)}',
	'.fzs-pager button{border:1px solid var(--border-color-medium, #d4d8dc);background:var(--background-color-high, #ffffff);border-radius:4px;padding:.2em .8em;cursor:pointer;color:inherit}',
	'.fzs-pager button:disabled{opacity:.4;cursor:default}',
	// ★★ 选项行的三条约束（2026-10-11 踩雷总结）：
	//   ① `.fzs-opt` 是 <label>，在 flex 的 .fzs-row 里会被 `.fzs-row>label{width:6.5em}` 按成 82px
	//      ⇒ 必须 `:not(.fzs-opt)` 豁免 + 这里 `flex:1`、内部文字 `nowrap`；
	//   ② 复选框必须 `flex:none`，否则被 flex 收缩挤成 2×16px（"勾选框不见了"的真因）；
	//   ③ 主题会给 `label > input[type=checkbox]` 加 `top:.4rem`（6.4px 下沉）⇒ 归零。
	'.fzs-opt{display:flex;align-items:center;gap:.4em;font-size:.85em;color:var(--text-color-medium);flex:1;flex-wrap:wrap;min-width:0}',
	'.fzs-opt>span{white-space:nowrap}',
	'.fzs-opt input{flex:none;margin:0}',
	// ★★ 复选框：**用主题原生外观 + 只纠正位移**（2026-10-11 定案，与「锁频段」页一致）。
	//   走过的弯路：曾自绘一个方框（appearance:none + 自己画勾）——在 Argon 下看着还行，
	//   但换到别的主题就与主题风格不搭（灰扑扑、像禁用）。
	//   ⇒ 正解：**不覆盖外观**（各主题的原生勾选框长什么样就什么样），只做两件事：
	//     ① `flex:none` —— 否则在 flex 容器里被收缩挤成一条（曾经"勾选框不见了"的真因）；
	//     ② `top:0; margin:0` —— 抵消主题的 `label > input[type=checkbox]{top:.4rem}`（**6.4px 下沉**）。
	'.fzs-opt input[type=checkbox]{flex:none;top:0;margin:0 .15em 0 0}',
	// ★ 自定义按钮必须显式写 color：主题会给 button 设自己的文字色（openwrt2020 是白色），
	//   而 --text-color-* 在它下面未定义 ⇒ 文字会变成"白底白字"、彻底看不见。
	//   用 inherit 跟正文同色，任何主题都不会瞎。
	'.fzs-hbtn{border:1px solid var(--border-color-medium, #d4d8dc);background:var(--background-color-high, #ffffff);border-radius:4px;padding:.15em .6em;font-size:.78em;cursor:pointer;color:inherit;font-weight:400}',
	'.fzs-hbtn:hover{color:var(--text-color-high)}'
].join('\n');

function errMsg(r, d) {
	if (d && d.err) return d.err;
	return ((r && r.stderr) || '').trim().split('\n').pop().replace(/^fzs-at:\s*/, '') || '操作未确认成功';
}

return view.extend({
	// ★★ 2026-10-07 晚 代码审查发现：本文件曾有**两个 `load` 方法**（另一个在
	//   文件末尾）。JS 对象字面量同名键**后者覆盖前者** → 这里的
	//   `L.env.rpctimeout = 120` 会被静默丢弃，RPC 超时回落 LuCI 默认 20 秒。
	//   后果：长短信分条发送（每片 CMGS 最长等 3.2 秒）可能超 20 秒 →
	//   前端报"失败"，而短信其实已发出 → 误导用户。
	//   现已合并为唯一 load（末尾那个已删除）。
	load: function() {
		L.env.rpctimeout = 120;
		return uci.load('fzs-sms');
	},

	render: function() {
		var self = this;
		self._msgs = [];
		self._sent = [];
		self._pageIn = 1;
		self._pageSent = 1;

		var listEl = E('div', {}, [ E('div', { 'class': 'fzs-empty' }, '读取中…') ]);
		var sentEl = E('div', {}, []);
		var useEl = E('span', { 'class': 'fzs-sub fzs-use' }, '');
		// ★★ 一个框一个提示（2026-10-11 用户要求）：
		//   以前全页共用一个 status，而它挂在最下面「发送短信」旁 ⇒
		//   在上面的框（转发/存储/清空…）点了操作，提示却在几百像素外、根本看不到。
		//   ⇒ 现在**每个卡片各有一个提示位**，提示就显示在你点的那排按钮旁边。
		var stMain = E('span', { 'class': 'fzs-status' }, '');     // 短信中心（启用/关闭）
		var stInbox = E('span', { 'class': 'fzs-status' }, '');    // 收件箱（清空/删除）
		var stSent = E('span', { 'class': 'fzs-status' }, '');     // 已发送（清空/删除）
		var stKeep = E('span', { 'class': 'fzs-status' }, '');     // 存储位置
		var stFwd = E('span', { 'class': 'fzs-status' }, '');      // 短信转发
		var status = E('span', { 'class': 'fzs-status' }, '');     // 发送短信（原有）

		// ---- 分页控件 ----
		function pager(cur, total, go) {
			var pages = Math.max(1, Math.ceil(total / PER_PAGE));
			// 只有一页时不显示翻页按钮（保持简洁），但保留「共 N 条」——
			// 否则用户会以为列表就这么多、分页功能没了。
			if (pages <= 1) return E('div', { 'class': 'fzs-pager' }, [ E('span', {}, '共 ' + total + ' 条') ]);
			var prev = E('button', { click: function() { go(cur - 1); } }, '‹ 上一页');
			var next = E('button', { click: function() { go(cur + 1); } }, '下一页 ›');
			if (cur <= 1) prev.disabled = true;
			if (cur >= pages) next.disabled = true;
			return E('div', { 'class': 'fzs-pager' }, [ prev, E('span', {}, '第 ' + cur + ' / ' + pages + ' 页（共 ' + total + ' 条）'), next ]);
		}

		// ---- 收件箱渲染（倒序 + 分页）----
		function renderInbox() {
			listEl.innerHTML = '';
			var msgs = self._msgs;
			if (!msgs.length) {
				listEl.appendChild(E('div', { 'class': 'fzs-empty' }, '收件箱为空——新短信会自动入库'));
				return;
			}
			var total = msgs.length;
			var pages = Math.max(1, Math.ceil(total / PER_PAGE));
			if (self._pageIn > pages) self._pageIn = pages;
			var start = (self._pageIn - 1) * PER_PAGE;
			var end = Math.min(start + PER_PAGE, total);
			for (var i = start; i < end; i++) (function(m) {
				var del = E('button', { 'class': 'fzs-del', title: '从存储删除' }, '✕');
				del.addEventListener('click', function() {
					ui.showModal('确认删除短信', [
						E('p', {}, '将从收件箱移除（若副本仍在卡上会一并删除，不可恢复）：'),
						E('p', { style: 'font-family:monospace' }, (m.sender || '?') + ' · ' + (m.content || '').substr(0, 40)),
						E('div', { 'class': 'right' }, [
							E('button', { 'class': 'btn', click: ui.hideModal }, '取消'),
							E('button', {
								'class': 'btn cbi-button-negative',
								click: function() {
									ui.hideModal();
									stInbox.textContent = '删除中…';
									fs.exec(FZS_AT, [ 'sms', 'drop', m.modem, '' + m.idx ]).then(function(r) {
										var d2 = null;
										try { d2 = JSON.parse((r && r.stdout) || ''); } catch (e) {}
										stInbox.textContent = (d2 && d2.ok) ? '✅ 已删除' : ('❌ ' + errMsg(r, d2));
										return loadInbox();
									}).catch(function(e) { stInbox.textContent = '❌ ' + e; });
								}
							}, '确认删除')
						])
					]);
				});
				listEl.appendChild(E('div', { 'class': 'fzs-msg' }, [
					E('span', { 'class': 'fzs-mb' }, '模组' + m.modem),
					E('div', { 'class': 'fzs-mc' }, [
						E('div', { 'class': 'fzs-mh' }, [
							E('span', { 'class': 'fzs-from' }, m.sender || '未知号码'),
							E('span', {}, m.time || '')
						]),
						E('div', { 'class': 'fzs-mt' }, m.content || '')
					]),
					del
				]));
			})(msgs[i]);
			var pg = pager(self._pageIn, total, function(p) { self._pageIn = p; renderInbox(); });
			if (pg) listEl.appendChild(pg);
		}

		// ---- 已发送渲染（倒序 + 分页）----
		function renderSent() {
			sentEl.innerHTML = '';
			var sent = self._sent;
			if (!sent.length) {
				sentEl.appendChild(E('div', { 'class': 'fzs-empty' }, '还没有发送记录——发送成功后自动留底'));
				return;
			}
			var total = sent.length;
			var pages = Math.max(1, Math.ceil(total / PER_PAGE));
			if (self._pageSent > pages) self._pageSent = pages;
			var start = (self._pageSent - 1) * PER_PAGE;
			var end = Math.min(start + PER_PAGE, total);
			for (var i = start; i < end; i++) (function(m) {
				var del = E('button', { 'class': 'fzs-del', title: '从留底删除' }, '✕');
				del.addEventListener('click', function() {
					ui.showModal('确认删除发送记录', [
						E('p', {}, '将从已发送留底中移除（已经发出去的短信不受影响，不可恢复）：'),
						E('p', { style: 'font-family:monospace' }, '→ ' + (m.to || '?') + ' · ' + (m.content || '').substr(0, 40)),
						E('div', { 'class': 'right' }, [
							E('button', { 'class': 'btn', click: ui.hideModal }, '取消'),
							E('button', {
								'class': 'btn cbi-button-negative',
								click: function() {
									ui.hideModal();
									stSent.textContent = '删除中…';
									fs.exec(FZS_AT, [ 'sms', 'dropout', '' + m.ts, m.modem, m.to ]).then(function(r) {
										var d2 = null;
										try { d2 = JSON.parse((r && r.stdout) || ''); } catch (e) {}
										stSent.textContent = (d2 && d2.ok) ? '✅ 已删除' : ('❌ ' + errMsg(r, d2));
										return loadInbox();
									}).catch(function(e) { stSent.textContent = '❌ ' + e; });
								}
							}, '确认删除')
						])
					]);
				});
				sentEl.appendChild(E('div', { 'class': 'fzs-msg' }, [
					E('span', { 'class': 'fzs-mb' }, '模组' + m.modem),
					E('div', { 'class': 'fzs-mc' }, [
						E('div', { 'class': 'fzs-mh' }, [
							E('span', { 'class': 'fzs-from' }, '→ ' + m.to),
							E('span', {}, new Date(m.ts * 1000).toLocaleString())
						]),
						E('div', { 'class': 'fzs-mt' }, m.content || '')
					]),
					del
				]));
			})(sent[i]);
			var pg = pager(self._pageSent, total, function(p) { self._pageSent = p; renderSent(); });
			if (pg) sentEl.appendChild(pg);
		}

		function loadInbox() {
			return fs.exec(FZS_AT, [ 'sms', 'inbox' ]).then(function(r) {
				var d = null;
				try { d = JSON.parse((r && r.stdout) || ''); } catch (e) {}
				if (!d || d.msgs == null) {
					listEl.innerHTML = '';
					listEl.appendChild(E('div', { 'class': 'fzs-empty' }, '收件箱读取失败（守护可能未运行，稍后自动重试）'));
					useEl.textContent = '';
					return;
				}
				var ago = (d.lastpoll > 0) ? Math.max(0, Math.floor(d.now - d.lastpoll)) : -1;
				self._hb = (ago >= 0 && ago < (Number(uci_get_interval()) + 45)) ? 'ok' : 'stale';
				// ★ 两个数字要标明各自属于哪个模组，否则看不出谁是谁。
				//   含义：该模组/SIM 卡内当前存着的短信条数 / 卡的上限。
				useEl.textContent = '卡内短信：模组1 ' + (d.usage.modem1 || '?') + ' · 模组2 ' + (d.usage.modem2 || '?')
					+ '　' + (ago < 0 ? '守护未运行' : ago + ' 秒前刷新');
				// 新→旧
				self._msgs = (d.msgs || []).slice().reverse();
				self._sent = (d.sent || []).slice().reverse();
				renderInbox();
				renderSent();
			}).catch(function() {
				listEl.innerHTML = '';
				listEl.appendChild(E('div', { 'class': 'fzs-empty' }, '调用失败——稍后自动重试'));
			});
		}

		function uci_get_interval() {
			return self._interval || 30;
		}

		// ---- 存储策略开关 ----
		// ★ 保存一律直调 uci（不用前端的 uci.set/save）：
		//   前端 set() 是同步的、save() 是异步的，中间那一小段时间 LuCI 会在右上角
		//   弹出「未保存的配置: N」——保存完成后虽会自动消失，但用户看到会以为没保存成功
		//   改为 sh 里逐条 uci set + commit，界面上不再出现该提示。
		function uci_commit(pairs, done, fail) {
			var chain = Promise.resolve();
			pairs.forEach(function(pair) {
				chain = chain.then(function() { return fs.exec('/sbin/uci', [ '-q', 'set', pair ]); });
			});
			return chain
				.then(function() { return fs.exec('/sbin/uci', [ '-q', 'commit', 'fzs-sms' ]); })
				.then(done).catch(fail);
		}
		// ★ 必须用 DOM property 设置选中态：若写成 E('input',{...,checked:false})，
		//   E() 会把它渲染成 HTML 属性 checked="false"，而 HTML 里只要该属性存在
		//   就算"勾选"→ 开关会全部假勾选（本项目 5 个开关曾全中招）。
		var keepChk = E('input', { type: 'checkbox' });
		keepChk.checked = (uci.get('fzs-sms', 'globals', 'keep_in_storage') === '1');
		keepChk.addEventListener('change', function() {
			var v = keepChk.checked ? '1' : '0';
			stKeep.textContent = '保存中…';
			uci_commit([ 'fzs-sms.globals.keep_in_storage=' + v ], function() {
				stKeep.textContent = keepChk.checked
					? '✅ 已在 SIM 卡上也保存一份（注意：卡满会收不到新短信）'
					: '✅ 已改为只存设备上';
			}, function(e) {
				stKeep.textContent = '❌ 保存失败：' + e;
			});
		});

		// ---- 清空收件箱 ----
		var btnClear = E('button', { 'class': 'fzs-hbtn' }, '清空');
		btnClear.addEventListener('click', function() {
			ui.showModal('清空收件箱', [
				E('p', {}, '将清空设备上的收件箱记录（共 ' + self._msgs.length + ' 条）。'),
				E('p', { style: 'color:var(--text-color-low);font-size:.9em' }, '此操作不影响 SIM 卡里的短信，也不可恢复。'),
				E('div', { 'class': 'right' }, [
					E('button', { 'class': 'btn', click: ui.hideModal }, '取消'),
					E('button', {
						'class': 'btn cbi-button-negative',
						click: function() {
							ui.hideModal();
							stInbox.textContent = '清空中…';
							fs.exec(FZS_AT, [ 'sms', 'clear' ]).then(function() {
								self._pageIn = 1;
								stInbox.textContent = '✅ 收件箱已清空';
								return loadInbox();
							}).catch(function(e) { stInbox.textContent = '❌ ' + e; });
						}
					}, '确认清空')
				])
			]);
		});

		// ---- 清空已发送留底 ----
		var btnClearOut = E('button', { 'class': 'fzs-hbtn' }, '清空');
		btnClearOut.addEventListener('click', function() {
			ui.showModal('清空已发送留底', [
				E('p', {}, '将清空设备上的已发送留底（共 ' + self._sent.length + ' 条）。'),
				E('p', {}, '已经发出去的短信不受影响，只是删掉这份记录。'),
				E('div', { 'class': 'right' }, [
					E('button', { 'class': 'btn', click: ui.hideModal }, '取消'),
					E('button', {
						'class': 'btn cbi-button-negative',
						click: function() {
							ui.hideModal();
							stSent.textContent = '清空中…';
							fs.exec(FZS_AT, [ 'sms', 'clearout' ]).then(function() {
								self._pageSent = 1;
								stSent.textContent = '✅ 已发送留底已清空';
								return loadInbox();
							}).catch(function(e) { stSent.textContent = '❌ ' + e; });
						}
					}, '确认清空')
				])
			]);
		});

		// ---- 发送 ----
		var sel = E('select', { 'class': 'cbi-input-select', style: 'box-sizing:border-box;width:16em;max-width:100%' },
			MODEMS.map(function(t) { return E('option', { value: t.v }, t.name); }));
		// [ims-bridge] 默认选中 IMS 通道，避免误选原生模组撞 CMS 304/500
		sel.value = 'ims';
		var num = E('input', {
			'class': 'cbi-input-text', type: 'text', maxlength: 20,
			placeholder: '例如 13800138000 或 +8613800138000',
			style: 'box-sizing:border-box;width:16em;max-width:100%;font-family:monospace'
		});
		// 记住常发的号码（纯前端，存浏览器本地）。短信正文【不记】—— 那是隐私。
		hist.attach(num, 'sms_number');
		var txt = E('textarea', {
			'class': 'cbi-input-text', rows: 3,
			placeholder: '短信内容（统一 UCS2 编码；超过 67 字将自动分条，每条 67 字）',
			style: 'box-sizing:border-box;width:100%;max-width:34em;resize:vertical'
		});
		var counter = E('div', { 'class': 'fzs-tip' }, '');

		txt.addEventListener('input', function() {
			var n = txt.value.length;
			counter.textContent = n > 67 ? ('将自动分 ' + Math.ceil(n / 67) + ' 条发送') : (n + '/67 字');
			counter.textContent += ' · 接收方正文首部会有 1 个异常字符（模组固件限制）';
		});

		var btn = E('button', { 'class': 'cbi-button cbi-button-action' }, '发送短信');
		btn.addEventListener('click', function() {
			var to = num.value.trim();
			var body = txt.value;
			if (!/^\+?[0-9]{5,20}$/.test(to)) { status.textContent = '号码不合法（只允许数字，可带 +）'; return; }
			if (!body) { status.textContent = '内容不能为空'; return; }
			var n = body.length;
			var parts = n > 70 ? Math.ceil(n / 67) : 1;
			ui.showModal('确认发送短信', [
				E('table', { 'class': 'table' }, [
					E('tr', {}, [ E('th', {}, '目标'), E('td', {}, MODEMS[sel.selectedIndex].name) ]),
					E('tr', {}, [ E('th', {}, '接收号码'), E('td', {}, E('b', {}, to)) ]),
					E('tr', {}, [ E('th', {}, '内容'), E('td', {}, body) ]),
					E('tr', {}, [ E('th', {}, '拆条'), E('td', {}, parts + ' 条') ])
				]),
				E('p', { style: 'color:#e53935' }, '发出后无法撤回，请确认号码无误。'),
				E('div', { 'class': 'right' }, [
					E('button', { 'class': 'btn', click: ui.hideModal }, '取消'),
					E('button', {
						'class': 'btn cbi-button-action',
						click: function() {
							// [ims-bridge] 目标 ims 与 1(EC200T) 都由 fzs-at 转发到 IMS 通道
							var viaIMS = (sel.value === 'ims' || sel.value === '1');
							ui.hideModal();
							status.textContent = viaIMS
								? '发送中…（经 IMS 通道发出，等待网络回执约 5~20 秒）'
								: '发送中…（模组应答约需 5~10 秒）';
							fs.exec(FZS_AT, [ 'sms', 'send', sel.value, to, body ]).then(function(r) {
								var d = null;
								try { d = JSON.parse((r && r.stdout) || ''); } catch (e) {}
								if (d && d.ok) {
									status.textContent = viaIMS
										? '✅ 已通过 IMS 通道发出（SIP MESSAGE 已交网络）'
										: '✅ 已交给模组发送（+CMGS 确认）';
									txt.value = '';
									counter.textContent = '';
								} else {
									status.textContent = '❌ ' + errMsg(r, d);
								}
							}).catch(function(e) { status.textContent = '❌ ' + e; });
						}
					}, '确认发送')
				])
			]);
		});

		var composeCard = E('div', { 'class': 'cbi-section fzs-card', style: 'margin-top:1em' }, [
			E('h3', { 'class': 'fzs-card-h' }, '发送短信'),
			E('div', { 'class': 'cbi-section-node fzs-body' }, [
				E('div', { 'class': 'fzs-row' }, [ E('label', {}, '目标模组'), sel ]),
				E('div', { 'class': 'fzs-row' }, [ E('label', {}, '接收号码'), num ]),
				E('div', { 'class': 'fzs-row', style: 'align-items:flex-start' }, [
					E('label', {}, '内容'),
					E('div', { style: 'flex:1;min-width:0' }, [ txt, counter ])
				]),
				E('div', { 'class': 'fzs-actions' }, [ btn, status ])
			])
		]);

		var btnR = E('button', { 'class': 'cbi-button cbi-button-action', click: loadInbox }, '立即刷新');
		var stamp = E('span', {}, '');
		var head = E('div', { style: 'display:flex;align-items:center;justify-content:flex-end;gap:.8em;margin-bottom:1em' }, [ stamp, btnR ]);

		var inboxCard = E('div', { 'class': 'cbi-section fzs-card' }, [
			// 顺序：标题 → 卡内短信(自动右推) → 清空按钮 → 提示位（在**最右**，不挤按钮）
			E('h3', { 'class': 'fzs-card-h' }, [ E('span', {}, '收件箱'), useEl, btnClear, stInbox ]),
			E('div', { 'class': 'cbi-section-node fzs-body' }, [ listEl ])
		]);

		var sentCard = E('div', { 'class': 'cbi-section fzs-card', style: 'margin-top:1em' }, [
			E('h3', { 'class': 'fzs-card-h' }, [ E('span', {}, '已发送'), E('span', { 'class': 'fzs-sub fzs-use' }, '留底最近 200 条 · 重启后仍在'), btnClearOut, stSent ]),
			E('div', { 'class': 'cbi-section-node fzs-body' }, [ sentEl ])
		]);

		var optCard = E('div', { 'class': 'cbi-section fzs-card', style: 'margin-top:1em' }, [
			E('h3', { 'class': 'fzs-card-h' }, [ E('span', {}, '短信存储位置'), stKeep ]),
			E('div', { 'class': 'cbi-section-node fzs-body' }, [
				E('div', { 'class': 'fzs-row' }, [
					E('label', {}, '额外保存'),
					E('label', { 'class': 'fzs-opt' }, [
						keepChk,
						E('span', {}, '同时在 SIM 卡上也保存一份')
					])
				]),
				E('div', { 'class': 'fzs-tip' }, '短信默认只存在设备上：设备空间大，能存 500 条，装满了会自动清掉最旧的，不会丢。打开上面的开关，会同时在 SIM 卡里也存一份——但 SIM 卡一般只能存 50 条，卡存满后就收不到新短信了（这是 SIM 卡本身的容量限制，不是设备的问题）。')
			])
		]);

		// ---- 转发设置 ----
		var g = function(k) { return uci.get('fzs-sms', 'globals', k) || ''; };
		var curCh = g('fwd_channels');

		// ★★ 2026-10-08：检测"本机能否发 JSON"（决定新渠道可不可用）。
		//   企微/钉钉/飞书/Telegram 四个渠道要求 POST + application/json，
		//   需要 GNU wget（wget-ssl 包）。系统自带的 uclient-fetch 发不了 JSON。
		//   主包**零依赖**（离线可装），所以这里可能出现 json=0；
		//   那种情况下**不报错、不打扰**，只在用户真勾了新渠道时给一个
		//   **可点击的**跳转提示（跳到「系统 → 软件包」装 wget-ssl），
		//   而不是丢一行命令让用户去复制。
		// ★★ 2026-10-08：缺组件的提醒（红框，紧贴渠道勾选行）
		//   为什么这样：这是"不装就没法用"的硬条件，所以要醒目；
		//   跳过去可能不知道该装什么（opkg 列表太旧时还得先「更新列表…」再装），
		//   所以**包名作为信息摆在那里**（点一下全选，纯方便），并给个带 ?query= 的跳转按钮，
		//   **不写"复制/粘贴"这类操作指令**。
		var PKG_NAME = 'wget-ssl';
		var capJson = true;                 // 未知时先按"有"渲染，探测回来再纠正
		var needJsonTip = E('div', { 'class': 'fzs-need', style: 'display:none' });
		var newChBoxes = [];                // 依赖 JSON 的渠道勾选框（后填）
		function refreshJsonTip() {
			var want = false;
			for (var i = 0; i < newChBoxes.length; i++)
				if (newChBoxes[i].checked) want = true;
			if (want && !capJson) {
				needJsonTip.style.display = '';
				needJsonTip.innerHTML = '';
				needJsonTip.appendChild(E('b', {}, '⚠ 这几个渠道需要先装一个小组件（约 2.8MB，要能联网）'));
				needJsonTip.appendChild(E('br'));
				// ① 包名：作为**信息**给出（用户可以不记，但要知道装的是什么）。
				//    点一下自动全选，纯粹方便 —— 不要求用户做任何"复制/粘贴"动作。
				needJsonTip.appendChild(E('span', {}, '要装的组件叫：'));
				var codeEl = E('code', {}, PKG_NAME);
				codeEl.addEventListener('click', function() {
					var r = document.createRange();
					r.selectNodeContents(codeEl);
					var s = window.getSelection();
					s.removeAllRanges(); s.addRange(r);
				});
				needJsonTip.appendChild(codeEl);
				needJsonTip.appendChild(E('br'));
				// ② 跳转按钮：带上 ?query=wget-ssl，软件包页会读 `location.search`
				//   里的 `query=` 并**自动填进搜索框**（源码：
				//   `location.search.match(/\bquery=([^=]+)\b/)` → 赋给 filter 输入框的 value）。
				//   它靠 URL 参数工作，**不挑浏览器**（火狐/Chrome/Edge/手机浏览器都一样）。
				//   包名用 encodeURIComponent 拼，防止被中间环节改写导致填充失效。
				needJsonTip.appendChild(E('a', {
					'class': 'btn cbi-button cbi-button-action',
					href: L.url('admin/system/opkg') + '?query=' + encodeURIComponent(PKG_NAME)
				}, '前往「软件包」安装'));
				// ③ 兜底说明：包裹"列表太旧"这个真实情况 —— 认准包名，找不到就先更新列表。
				needJsonTip.appendChild(E('span', {}, '　点进去搜索框已自动填好，点「安装」即可；万一没填上、或列表里找不到它，就认准上面的包名，先点一下页面顶部的「更新列表…」再安装。'));
				needJsonTip.appendChild(E('br'));
				needJsonTip.appendChild(E('span', {}, '（装好后回到本页，勾选就生效了。不想装也可以——只用上面的微信推送等渠道不受影响）'));
			} else {
				needJsonTip.style.display = 'none';
			}
		}
		// 异步探测（不改动页面结构，回来只更新提示显隐）
		fs.exec(FZS_AT, [ 'sms', 'caps' ]).then(function(r) {
			try { var d = JSON.parse((r && r.stdout) || ''); capJson = (d && d.json === 1); }
			catch (e) { capJson = true; }     // 探测失败就当有（不误报）
			refreshJsonTip();
		}).catch(function() { capJson = true; });

		// ★ 同上：一律用 DOM property 设 checked，绝不能在 E() 里传 checked 属性（会假勾选）
		var fwdChk = E('input', { type: 'checkbox' });
		fwdChk.checked = (g('fwd_enabled') === '1');
		var chPp = E('input', { type: 'checkbox' });
		chPp.checked = curCh.indexOf('pushplus') >= 0;
		var chBark = E('input', { type: 'checkbox' });
		chBark.checked = curCh.indexOf('bark') >= 0;
		var chHook = E('input', { type: 'checkbox' });
		chHook.checked = curCh.indexOf('webhook') >= 0;
		// ★ 2026-10-07 晚新增 4 个渠道（国内群机器人 + Telegram）。
		//   请求格式照抄各家官方文档（POST + application/json），见 fzs-fwd 注释。
		var chWecom = E('input', { type: 'checkbox' });
		chWecom.checked = curCh.indexOf('wecom-bot') >= 0;
		var chDing = E('input', { type: 'checkbox' });
		chDing.checked = curCh.indexOf('dingtalk-bot') >= 0;
		var chFeishu = E('input', { type: 'checkbox' });
		chFeishu.checked = curCh.indexOf('feishu-bot') >= 0;
		var chTg = E('input', { type: 'checkbox' });
		chTg.checked = curCh.indexOf('tg-bot') >= 0;
		// 这 4 个渠道依赖 JSON 能力 → 登记 + 勾选变化时刷新提示
		newChBoxes = [ chWecom, chDing, chFeishu, chTg ];
		for (var _bi = 0; _bi < newChBoxes.length; _bi++) {
			newChBoxes[_bi].addEventListener('change', refreshJsonTip);
		}
		// ★ 转发模组选择：可只转某个模组的短信。
		//   默认「两个都是开」——与升级前的「全部转发」行为一致，老用户无感。
		//   配置不存在时（老版本升级上来）也按"开"处理，避免突然收不到转发。
		var md1Chk = E('input', { type: 'checkbox' });
		md1Chk.checked = (g('fwd_modem1') !== '0');
		var md2Chk = E('input', { type: 'checkbox' });
		md2Chk.checked = (g('fwd_modem2') !== '0');
		var ppTok = E('input', {
			'class': 'cbi-input-text', type: 'text', value: g('fwd_pushplus_token'),
			placeholder: 'PushPlus token（pushplus.plus 获取）',
			style: 'box-sizing:border-box;width:24em;max-width:100%;font-family:monospace'
		});
		var barkKey = E('input', {
			'class': 'cbi-input-text', type: 'text', value: g('fwd_bark_key'),
			placeholder: 'Bark key（api.day.app 获取）',
			style: 'box-sizing:border-box;width:24em;max-width:100%;font-family:monospace'
		});
		var hookUrl = E('input', {
			'class': 'cbi-input-text', type: 'text', value: g('fwd_webhook_url'),
			placeholder: 'https://...（POST JSON: sender/time/content）',
			style: 'box-sizing:border-box;width:24em;max-width:100%;font-family:monospace'
		});
		// ---- 新增 4 渠道的输入框（样式与上面统一：等宽、24em）----
		var _iw = 'box-sizing:border-box;width:24em;max-width:100%;font-family:monospace';
		var wecomUrl = E('input', {
			'class': 'cbi-input-text', type: 'text', value: g('fwd_wecom_webhook'),
			placeholder: 'https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=xxx',
			style: _iw
		});
		var dingUrl = E('input', {
			'class': 'cbi-input-text', type: 'text', value: g('fwd_dingtalk_webhook'),
			placeholder: 'https://oapi.dingtalk.com/robot/send?access_token=xxx',
			style: _iw
		});
		var dingKw = E('input', {
			'class': 'cbi-input-text', type: 'text', value: g('fwd_dingtalk_keyword'),
			placeholder: '自定义关键词（钉钉安全设置里填的那个词，可留空）',
			style: _iw
		});
		var feishuUrl = E('input', {
			'class': 'cbi-input-text', type: 'text', value: g('fwd_feishu_webhook'),
			placeholder: 'https://open.feishu.cn/open-apis/bot/v2/hook/xxx',
			style: _iw
		});
		var tgToken = E('input', {
			'class': 'cbi-input-text', type: 'text', value: g('fwd_tg_token'),
			placeholder: 'Bot Token（@BotFather 获取，形如 123456:ABC-xxx）',
			style: _iw
		});
		var tgChat = E('input', {
			'class': 'cbi-input-text', type: 'text', value: g('fwd_tg_chatid'),
			placeholder: 'Chat ID（接收消息的会话 ID）',
			style: _iw
		});
		var tgHost = E('input', {
			'class': 'cbi-input-text', type: 'text', value: g('fwd_tg_apihost'),
			placeholder: 'API 反代域名（国内网络需填，留空=api.telegram.org）',
			style: _iw
		});

		var btnFwdSave = E('button', { 'class': 'cbi-button cbi-button-action' }, '保存转发设置');
		btnFwdSave.addEventListener('click', function() {
			var chans = [];
			if (chPp.checked) chans.push('pushplus');
			if (chBark.checked) chans.push('bark');
			if (chHook.checked) chans.push('webhook');
			if (chWecom.checked) chans.push('wecom-bot');
			if (chDing.checked) chans.push('dingtalk-bot');
			if (chFeishu.checked) chans.push('feishu-bot');
			if (chTg.checked) chans.push('tg-bot');
			stFwd.textContent = '保存中…';
			uci_commit([
				'fzs-sms.globals.fwd_enabled=' + (fwdChk.checked ? '1' : '0'),
				'fzs-sms.globals.fwd_channels=' + chans.join(','),
				'fzs-sms.globals.fwd_pushplus_token=' + ppTok.value.trim(),
				'fzs-sms.globals.fwd_bark_key=' + barkKey.value.trim(),
				'fzs-sms.globals.fwd_webhook_url=' + hookUrl.value.trim(),
				'fzs-sms.globals.fwd_wecom_webhook=' + wecomUrl.value.trim(),
				'fzs-sms.globals.fwd_dingtalk_webhook=' + dingUrl.value.trim(),
				'fzs-sms.globals.fwd_dingtalk_keyword=' + dingKw.value.trim(),
				'fzs-sms.globals.fwd_feishu_webhook=' + feishuUrl.value.trim(),
				'fzs-sms.globals.fwd_tg_token=' + tgToken.value.trim(),
				'fzs-sms.globals.fwd_tg_chatid=' + tgChat.value.trim(),
				'fzs-sms.globals.fwd_tg_apihost=' + tgHost.value.trim(),
				'fzs-sms.globals.fwd_modem1=' + (md1Chk.checked ? '1' : '0'),
				'fzs-sms.globals.fwd_modem2=' + (md2Chk.checked ? '1' : '0')
			], function() {
				stFwd.textContent = '✅ 转发设置已保存（守护下一轮生效）';
			}, function(e) {
				stFwd.textContent = '❌ 保存失败：' + e;
			});
		});

		var btnFwdTest = E('button', { 'class': 'cbi-button' }, '发送测试');
		btnFwdTest.addEventListener('click', function() {
			stFwd.textContent = '发送测试中…（请确认已先保存设置）';
			fs.exec(FZS_AT, [ 'sms', 'fwdtest' ]).then(function(r) {
				var d = null;
				try { d = JSON.parse((r && r.stdout) || ''); } catch (e) {}
				stFwd.textContent = (d && d.ok) ? '✅ 测试消息已发出，请看手机' : ('❌ ' + errMsg(r, d));
			}).catch(function(e) { stFwd.textContent = '❌ ' + e; });
		});

		var fwdCard = E('div', { 'class': 'cbi-section fzs-card', style: 'margin-top:1em' }, [
			E('h3', { 'class': 'fzs-card-h' }, '短信转发'),
			E('div', { 'class': 'cbi-section-node fzs-body' }, [
				E('div', { 'class': 'fzs-row' }, [
					E('label', {}, '启用转发'),
					E('label', { 'class': 'fzs-opt' }, [ fwdChk, E('span', {}, '收到新短信后自动转发') ])
				]),
				E('div', { 'class': 'fzs-row' }, [
					E('label', {}, '转发哪些'),
					E('label', { 'class': 'fzs-opt' }, [
						md1Chk, E('span', {}, '模组1 (EC200T)'),
						md2Chk, E('span', {}, '模组2 (EC200N)')
					])
				]),
				E('div', { 'class': 'fzs-row' }, [
					E('label', {}, '推送渠道'),
					E('label', { 'class': 'fzs-opt' }, [
						chPp, E('span', {}, 'PushPlus(微信)'), chBark, E('span', {}, 'Bark(iPhone)'), chHook, E('span', {}, 'Webhook')
					])
				]),
				// ★ 新增渠道的勾选（第二行，避免一行太挤）
				E('div', { 'class': 'fzs-row' }, [
					E('label', {}, ''),
					E('label', { 'class': 'fzs-opt' }, [
						chWecom, E('span', {}, '企业微信·群机器人'),
						chDing, E('span', {}, '钉钉·群机器人'),
						chFeishu, E('span', {}, '飞书·群机器人'),
						chTg, E('span', {}, 'Telegram')
					])
				]),
				// ★ 位置说明：紧跟「新增渠道勾选」那一行（原来的提示离勾选框太远、
				//   不够醒目）。这里是"不装组件就没法用"的硬条件，必须贴着勾选框。
				needJsonTip,
				E('div', { 'class': 'fzs-row' }, [ E('label', {}, 'PushPlus'), ppTok ]),
				E('div', { 'class': 'fzs-row' }, [ E('label', {}, 'Bark Key'), barkKey ]),
				E('div', { 'class': 'fzs-row' }, [ E('label', {}, 'Webhook'), hookUrl ]),
				E('div', { 'class': 'fzs-row' }, [ E('label', {}, '企微 Webhook'), wecomUrl ]),
				E('div', { 'class': 'fzs-row' }, [ E('label', {}, '钉钉 Webhook'), dingUrl ]),
				E('div', { 'class': 'fzs-row' }, [ E('label', {}, '钉钉关键词'), dingKw ]),
				E('div', { 'class': 'fzs-row' }, [ E('label', {}, '飞书 Webhook'), feishuUrl ]),
				E('div', { 'class': 'fzs-row' }, [ E('label', {}, 'TG Token'), tgToken ]),
				E('div', { 'class': 'fzs-row' }, [ E('label', {}, 'TG ChatID'), tgChat ]),
				E('div', { 'class': 'fzs-row' }, [ E('label', {}, 'TG 反代域名'), tgHost ]),
				E('div', { 'class': 'fzs-actions' }, [ btnFwdSave, btnFwdTest, stFwd ]),
				E('div', { 'class': 'fzs-tip' }, '多个渠道会同时推送；转发内容为「发件人 + 时间 + 正文」。转发失败的短信会自动重试，3 次仍失败则留在设备的 fwdq-failed 目录。转发哪些模组可只选其一；两个都不勾等于不转发。')
			])
		]);

		// ---- 短信中心总开关 ----
		//   关掉后：不再后台轮询短信、也不再收短信（守护停掉）。只影响短信，
		//   锁频段 / 改串 / APN / 概览等不受影响。
		//   ★ 保存后立刻生效：先 set+commit，再 start/stop 守护（否则要重启设备才生效）。
		var muChk = E('input', { type: 'checkbox' });
		muChk.checked = (uci.get('fzs-sms', 'globals', 'enabled') !== '0');   // 缺省=开
		muChk.addEventListener('change', function() {
			var on = muChk.checked;
			stMain.textContent = '保存中…';
			uci_commit([ 'fzs-sms.globals.enabled=' + (on ? '1' : '0') ], function() {
				// ★ 立刻生效：开 → start（把守护拉起来）；关 → stop（停掉守护）。
				//   关掉时如果守护本来就在跑，不 stop 就要等重启设备才真正停。
				return fs.exec('/etc/init.d/fzs-sms', [ on ? 'start' : 'stop' ]).then(function() {
					stMain.textContent = on ? '✅ 短信中心已开启' : '✅ 短信中心已关闭（不再收发短信）';
				}).catch(function() {
					// 服务控制失败不算保存失败：配置已落盘，重启设备后同样生效。
					stMain.textContent = on ? '✅ 已开启（重启设备后生效）' : '✅ 已关闭（重启设备后生效）';
				});
			}, function(e) {
				stMain.textContent = '❌ 保存失败：' + e;
			});
		});
		var mainCard = E('div', { 'class': 'cbi-section fzs-card' }, [
			E('h3', { 'class': 'fzs-card-h' }, [ E('span', {}, '短信中心'), stMain ]),
			E('div', { 'class': 'cbi-section-node fzs-body' }, [
				E('div', { 'class': 'fzs-row' }, [
					// ★ 这一栏原来叫「启用 / 开启短信收发」，而下面还有一张卡叫「短信转发」——
					//   两组词只差一个字，扫一眼容易看走眼。改成"总开关 / 启用短信功能"，
					//   把"发"这个字从这一栏彻底拿掉。
					E('label', {}, '总开关'),
					E('label', { 'class': 'fzs-opt' }, [ muChk, E('span', {}, '启用短信功能') ])
				]),
				E('div', { 'class': 'fzs-tip' }, '关闭后，设备不再后台接收短信，也不做短信转发——适合插了物联卡、根本用不到短信的情况。不影响锁频段、改串号、APN 和概览。')
			])
		]);

		// [ims-bridge-card] v5 — imsengine 补丁标记，勿删（patch-fzs 靠它判断幂等/代次）
		// ---- IMS 通道卡片（2026-10-20 新增）----
		//   背景：本机两块模组里，EC200T（模组1）固件**不含 IMS/VoLTE 栈**，
		//   原生 AT+CMGS 只能走 CS 域 —— 这就是面板发短信报「CMS 错误 500」的根因，
		//   且它自己**读不出 P-CSCF 地址**。EC200N（模组2）固件带原生 IMS，能读到。
		//   所以流程是：把这张卡换插到模组2 → 点「学习 P-CSCF」→ 引擎用原生 IMS 注册
		//   成功后读出 P-CSCF，按 IMSI/ICCID 精确存盘 → 卡插回模组1，之后 EC200T
		//   直接用学到的地址注册（一次命中，不再逐个试 6 个候选）。
		var stIms = E('span', { 'class': 'fzs-status' }, '读取中…');
		var btnImsRefresh = E('button', { 'class': 'fzs-hbtn' }, '刷新状态');
		var btnImsLearn = E('button', { 'class': 'cbi-button' }, '学习 P-CSCF');

		// 秒 → “3 分 20 秒” 这类人话
		function durSec(n) {
			n = parseInt(n, 10);
			if (isNaN(n) || n < 0) return null;
			if (n < 60) return n + ' 秒';
			if (n < 3600) return Math.floor(n / 60) + ' 分';
			return Math.floor(n / 3600) + ' 小时 ' + Math.floor((n % 3600) / 60) + ' 分';
		}

		function imsStatus() {
			stIms.textContent = '读取中…';
			return fs.exec(FZS_AT, [ 'ims', 'status' ]).then(function(r) {
				var d = null;
				try { d = JSON.parse((r && r.stdout) || ''); } catch (e) {}
				if (!d) { stIms.textContent = '❌ ' + errMsg(r, null); return; }
				// ★ 没装 imsengine 时把「IMS 通道」这个下拉项去掉，避免用户选了却发不出。
				//   （fzs-at 侧也有同样保险：/etc/init.d/ims 不存在会回退原生 CS 域）
				if (d.state === '未安装') {
					for (var i = sel.options.length - 1; i >= 0; i--) {
						if (sel.options[i].value === 'ims') sel.remove(i);
					}
					if (sel.value === 'ims') sel.value = '1';
				}
				var t = d.state || '未知';
				if (d.detail) t += ' · ' + d.detail;
				if (d.imsi) t += '（IMSI ' + d.imsi + '）';
				// 租期信息：让人一眼看出"注册稳不稳"，不用去翻日志
				var up = durSec(d.up_sec);
				if (up) t += ' · 已在线 ' + up;
				if (d.next_renew) t += ' · 下次续租 ' + d.next_renew;
				var rx = parseInt(d.net_rx_ago, 10);
				if (!isNaN(rx) && rx >= 0) {
					t += ' · 网络侧最后一次响应 ' + (rx < 5 ? '刚刚' : durSec(rx) + '前');
				}
				stIms.textContent = (d.state === '已注册' ? '✅ ' : '⚠️ ') + t;
			}).catch(function(e) { stIms.textContent = '❌ ' + e; });
		}

		btnImsRefresh.addEventListener('click', function() { imsStatus(); });

		btnImsLearn.addEventListener('click', function() {
			btnImsLearn.disabled = true;
			stIms.textContent = '学习中…（自动找 EC200N 口 → 开原生 IMS → 读 P-CSCF，最长约 90 秒，请勿断电）';
			fs.exec(FZS_AT, [ 'ims', 'learn' ]).then(function(r) {
				var d = null;
				try { d = JSON.parse((r && r.stdout) || ''); } catch (e) {}
				btnImsLearn.disabled = false;
				if (d && d.ok) {
					var a = (d.pcscf && d.pcscf.length) ? d.pcscf.join(' / ') : '(未读到)';
					stIms.textContent = '✅ 已学到 P-CSCF：' + a + ' —— 卡可以插回模组1(EC200T) 了';
				} else {
					stIms.textContent = '❌ ' + ((d && d.err) ? d.err : errMsg(r, d));
				}
				imsStatus();
			}).catch(function(e) {
				btnImsLearn.disabled = false;
				stIms.textContent = '❌ ' + e;
				imsStatus();
			});
		});

		var imsCard = E('div', { 'class': 'cbi-section fzs-card', style: 'margin-top:1em' }, [
			E('h3', { 'class': 'fzs-card-h' }, [ E('span', {}, 'IMS 通道'), stIms, btnImsRefresh ]),
			E('div', { 'class': 'cbi-section-node fzs-body' }, [
				E('div', { 'class': 'fzs-actions' }, [ btnImsLearn ]),
				E('div', { 'class': 'fzs-tip' },
					'短信默认走 IMS 通道（数据通道），不再用模组的 CS 域短信，EC200T 的 CMS 500 报错由此根治。' +
					'「学习 P-CSCF」只在换卡/换运营商后需要做一次：把卡插到模组2(EC200N，固件带原生 IMS) 上点一次，' +
					'学到地址后把卡插回模组1(EC200T) 即可长期复用；同一张卡下次插回来不用再学。')
			])
		]);

		imsStatus();

		// [/ims-bridge-card] 结束标记 —— patch-fzs 靠这对标记定位旧片段边界以便升级，勿删
		function reload() {
			stamp.textContent = '读取中…';
			return loadInbox().then(function() {
				stamp.textContent = '数据读取于 ' + new Date().toLocaleTimeString();
			});
		}

		reload();
		poll.add(reload, 10);

		return E('div', {}, [ E('style', {}, CSS), mainCard, imsCard, head, inboxCard, sentCard, optCard, fwdCard, composeCard ]);
	},

	// ★★ 关键：必须在 render 之前把配置读进来。
	//   render 里是【同步】调用 uci.get() 的；若配置尚未 load，get() 会返回 null，
	//   后果有两层：① 所有开关都按「关」渲染（明明开着却显示关）；
	//   ② 点保存时把这些 null 当成用户选择写回 → 真实配置被默认值【覆盖清空】。
	//   这是本项目踩过的真实事故：转发配置被一次「保存」清掉过。
	//   ⚠️ 注意：`load` 已在文件前部（render 之前）统一定义，**此处不要再定义一遍**
	//   —— 对象字面量同名键后者覆盖前者，会把那里的 `rpctimeout=120` 静默吃掉。

	handleSave: null,
	handleSaveApply: null,
	handleReset: null
});
