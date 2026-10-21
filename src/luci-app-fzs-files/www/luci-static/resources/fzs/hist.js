'use strict';
'require baseclass';

/* LuCI 的模块必须返回一个"类"（框架会 new 它），所以要用 baseclass 包一层。
   返回普通对象会报：factory yields invalid constructor。 */

/*
 * fzs.hist —— 输入框历史记录（纯前端小工具）
 *
 * 目的：把用户输入过、值得复用的值记在浏览器本地，下次点开输入框就能下拉选择，
 *       不用重复敲。典型场景：锁频点 / 锁小区反复试值、老发同一个号码、来回切 APN。
 *
 * 设计取舍（改之前先看）：
 *   - 纯前端：只存 localStorage，不碰设备、不占设备资源、不需要后端配合。
 *     代价是换浏览器或清缓存就没了 —— 这是可接受的，也更干净。
 *   - 不做开关：功能默认就在，不给用户增加决策负担。
 *   - 每个框最多保留最近 10 条，重复值自动提到最前，不会无意义堆积。
 *
 * ★ 位置有讲究：本文件必须放在 resources/fzs/ 下（不能放 resources/view/fzs/），
 *   因为 LuCI 对 `view.*` 路径的模块强制要求"返回视图对象"，工具模块放那里会报错。
 *
 * 用法（顶部）：'require fzs.hist as hist';
 * 用法（代码里）：hist.attach(输入框元素, '唯一键名');
 *
 * 注意：token / 密码类输入框不要用它。
 */

var MAX = 10;

function store_key(k) { return 'fzs.hist.' + k; }

function read(k) {
	try { return JSON.parse(localStorage.getItem(store_key(k)) || '[]') || []; }
	catch (e) { return []; }
}

function write(k, arr) {
	try { localStorage.setItem(store_key(k), JSON.stringify(arr)); } catch (e) {}
}

return baseclass.extend({
	attach: function(input, k) {
		if (!input || !k) return input;

		var listId = 'fzs-hist-' + k.replace(/[^A-Za-z0-9_-]/g, '_');

		/* 把历史值渲染成 datalist 挂到输入框上（浏览器原生的下拉候选） */
		var render = function() {
			var old = document.getElementById(listId);
			if (old && old.parentNode) old.parentNode.removeChild(old);
			var vals = read(k);
			if (!vals.length) { input.removeAttribute('list'); return; }
			var dl = E('datalist', { id: listId });
			vals.forEach(function(v) { dl.appendChild(E('option', { value: v })); });
			document.body.appendChild(dl);
			input.setAttribute('list', listId);
		};

		/* 记住当前值（去重后提到最前，超出上限截断） */
		var remember = function() {
			var v = String(input.value == null ? '' : input.value).trim();
			if (!v) return;
			var vals = read(k).filter(function(x) { return x !== v; });
			vals.unshift(v);
			if (vals.length > MAX) vals = vals.slice(0, MAX);
			write(k, vals);
			render();
		};

		input.addEventListener('change', remember);
		input.addEventListener('blur', remember);   // 有些交互不触发 change，兜底
		input.addEventListener('focus', render);    // 聚焦时重读（别的标签页可能刚写过）

		render();
		return input;
	}
});
