/* Beian2API 前端：原生 JS，无依赖，约 4KB。 */

/* ---- 两个 IIFE 共用的小工具 ----
   页面拆成「查询台」和「备案号校验器」两块各自独立初始化，但两者都要
   带 API Key 请求。这些函数必须在两者之外定义，否则后一个块调前者块里的
   局部函数会直接 ReferenceError（这正是校验器坏了这么久的原因）。

   KEY_STORE 也必须在这一层：之前它只写在 b2aKey 的闭包里（叫 STORE），
   而「保存 Key」那段代码引用的是 KEY_STORE——一个从来没声明过的名字。
   赋值语句抛 ReferenceError，恰好被外层的 try/catch 吞掉，于是：
   Key 没存进去、输入框不消失、下一次刷新全站 401，而界面上一点错都不报。
   开了鉴权的部署，网页端等于完全不可用。 */
var KEY_STORE = 'beian2api.key';

var b2aKey = function () {
  try { return localStorage.getItem(KEY_STORE) || ''; } catch (e) { return ''; }
};

function setKey(v) {
  try {
    if (v) localStorage.setItem(KEY_STORE, v);
    else localStorage.removeItem(KEY_STORE);
    return true;
  } catch (e) {
    return false; // 隐私模式下 localStorage 写不了，只对本次会话有效
  }
}

/* copyText 复制文本，返回 Promise。
   navigator.clipboard 只在安全上下文（https 或 localhost）里存在。
   用局域网 IP 打开自己部署的服务时它是 undefined，直接调 .writeText 会抛
   TypeError——按钮点下去毫无反应，也不报错。所以降级到 execCommand。 */
function copyText(text) {
  if (navigator.clipboard && navigator.clipboard.writeText) {
    return navigator.clipboard.writeText(text).catch(function () {
      // 权限被拒（非用户手势、iframe、浏览器策略）时再试老办法，
      // 两条都失败才算真失败。
      return legacyCopy(text);
    });
  }
  return legacyCopy(text);
}

function legacyCopy(text) {
  return new Promise(function (resolve, reject) {
    var ta = document.createElement('textarea');
    ta.value = text;
    ta.setAttribute('readonly', '');
    ta.style.cssText = 'position:fixed;top:-1000px;left:-1000px;opacity:0';
    document.body.appendChild(ta);
    ta.select();
    ta.setSelectionRange(0, text.length);
    var ok = false;
    try { ok = document.execCommand('copy'); } catch (e) { ok = false; }
    document.body.removeChild(ta);
    ok ? resolve() : reject(new Error('浏览器不允许自动复制'));
  });
}

/* showCopyBox 在 host 里摊出一段已选中的文本，让人手动 Ctrl+C。
   自动复制失败时只说一句「复制失败」等于把人丢在原地：徽章代码、
   JSON 都不是屏幕上现成能选的东西，用户没有别的办法拿到它。 */
function showCopyBox(host, text, label) {
  var old = host.querySelector('.copy-box');
  if (old) old.remove();
  var box = document.createElement('div');
  box.className = 'copy-box';
  var tip = document.createElement('div');
  tip.className = 'copy-tip';
  tip.textContent = '自动复制被浏览器拦下了，下面是' + (label || '内容') + '，全选后按 Ctrl/⌘+C 复制：';
  var ta = document.createElement('textarea');
  ta.readOnly = true;
  ta.value = text;
  ta.rows = Math.min(6, text.split('\n').length + 1);
  box.appendChild(tip);
  box.appendChild(ta);
  host.appendChild(box);
  ta.focus();
  ta.select();
}

function withKey(path) {
  var k = b2aKey();
  if (!k) return path;
  return path + (path.indexOf('?') >= 0 ? '&' : '?') + 'key=' + encodeURIComponent(k);
}

(function () {
  'use strict';
  var $ = function (s, r) { return (r || document).querySelector(s); };
  var form = $('#form'), input = $('#q'), goBtn = $('#go'), result = $('#result');

  function el(tag, cls, text) {
    var n = document.createElement(tag);
    if (cls) n.className = cls;
    if (text != null) n.textContent = text;
    return n;
  }
  function num(n) { return (n == null ? 0 : n).toLocaleString('zh-CN'); }

  /* ---- API Key ----
     Key 的读取与拼接放在文件最外层（见上面的 b2aKey / withKey），
     因为备案号校验器是另一个 IIFE。之前 withKey 定义在这个 IIFE 里，
     校验器里调它就抛 ReferenceError——点「校验」永远停在「校验中…」，
     连错误提示都没有（异常发生在 fetch 之前，走不到 .catch）。 */
  // apiKey 是本块内的短名字，实体在最外层。
  function apiKey() { return b2aKey(); }

  function needKey() {
    var box = $('#keybox');
    if (!box) return;
    box.hidden = false;
    var f = $('#k');
    if (f && !f.value) f.value = apiKey();
  }

  function api(path, opts) {
    return fetch(withKey(path), opts).then(function (r) {
      return r.json().then(function (j) {
        if (r.status === 401) needKey();
        if (!r.ok || j.ok === false) {
          var m = (j.error && j.error.message) || ('HTTP ' + r.status);
          throw new Error(m);
        }
        return j.data;
      });
    });
  }

  var kform = $('#kform');
  if (kform) {
    kform.addEventListener('submit', function (e) {
      e.preventDefault();
      var v = $('#k').value.trim();
      var stored = setKey(v);
      // 这里原先无条件 `hidden = !!v`：填了 Key 就把整个框收起来，而下面那句
      // 「✓ Key 有效」写进的是框里的提示行——框都没了，谁看得见？结果填对了
      // 和填错了在界面上长得一模一样，都只剩一个不动的搜索框，用户只能靠
      // 「怎么还是没数据」来猜。现在框留到验证结果出来再收。
      var tip = $('#keytip');
      if (tip) {
        tip.textContent = v
          ? (stored ? '已保存，正在校验…' : '当前浏览器不允许保存（隐私模式），本次访问有效。正在校验…')
          : '已清除 Key。';
      }
      refreshRSS();
      // 保存完立刻验一次：Key 填错了跟没填一样，而「保存成功」的提示会让人
      // 以为已经好了，直到下一次刷新才发现还是 401。
      api('/api/v1/stats').then(function () {
        var msg = v ? '✓ Key 有效，已保存到本浏览器。' : '已清除 Key。';
        if (tip) tip.textContent = msg;
        loadStats(); loadWatch(); loadChanges(); loadNotify(); loadProviders();
        if (input.value.trim()) submit();
        // 先让人看见「✓ 有效」，再收框。停留久一点也没关系：这段时间里
        // 再点一次保存会因为 tip 文案变了而重新计时，不会错收一个失败的框。
        // 清空 Key 时反而不收框：清掉之后这实例仍然需要 Key，框得留着。
        var box = $('#keybox');
        if (!v) return;
        setTimeout(function () {
          if (box && tip && tip.textContent === msg) box.hidden = true;
        }, 1400);
      }).catch(function (err) {
        if (tip) tip.textContent = '✗ Key 无效：' + err.message;
      });
    });
  }

  function refreshRSS() {
    var a = $('#rss-link');
    if (a) a.href = withKey('/api/v1/changes.rss');
  }

  function showError(msg) {
    result.innerHTML = '';
    var box = el('div', 'err-box fade');
    box.appendChild(el('strong', null, '查询失败：'));
    box.appendChild(document.createTextNode(msg));
    result.appendChild(box);
  }

  function kv(list) {
    var dl = el('dl', 'kv');
    list.forEach(function (p) {
      if (!p[1]) return;
      dl.appendChild(el('dt', null, p[0]));
      var dd = el('dd');
      if (p[2]) { var s = el('strong', null, p[1]); dd.appendChild(s); }
      else dd.textContent = p[1];
      dl.appendChild(dd);
    });
    return dl;
  }

  function render(d) {
    result.innerHTML = '';
    var card = el('div', 'card res-card fade');
    var head = el('div', 'res-head');
    head.appendChild(el('h2', null, d.domain));

    var b;
    if (!d.found) b = el('span', 'badge warn', '未备案');
    else if (d.stale) b = el('span', 'badge gray', '缓存快照');
    else b = el('span', 'badge', '已备案');
    head.appendChild(b);
    if (d.cached) head.appendChild(el('span', 'badge gray', '缓存命中'));
    var meta = el('span', 'meta', (d.cost_ms || 0).toFixed(1) + ' ms · ' + (d.source || '—'));
    head.appendChild(meta);
    card.appendChild(head);

    if (d.found && d.records && d.records.length) {
      d.records.forEach(function (r, i) {
        if (i > 0) card.appendChild(el('hr', null));
        card.appendChild(kv([
          ['备案号', r.icp_code, true],
          ['主办单位', r.unit_name],
          ['单位性质', r.unit_type],
          ['网站名称', r.site_name],
          ['备案省份', r.province],
          ['审核时间', r.audit_time]
        ]));
      });
    } else {
      card.appendChild(el('p', null, '该域名在公开备案库中没有记录。若刚提交备案，通常需要 1–3 个工作日同步。'));
    }

    if (d.notice) card.appendChild(el('div', 'notice', d.notice));

    var acts = el('div', 'actions');
    var copy = el('button', 'ghost small', '复制 JSON');
    copy.onclick = function () {
      copyText(JSON.stringify(d, null, 2)).then(function () {
        copy.textContent = '已复制 ✓';
        setTimeout(function () { copy.textContent = '复制 JSON'; }, 1600);
      }).catch(function () {
        // 复制失败必须给出替代方案：静默失败的话按钮什么都不变，
        // 用户以为点漏了，反复点。
        showCopyBox(card, JSON.stringify(d, null, 2), 'JSON');
        copy.textContent = '手动复制 ↓';
      });
    };
    acts.appendChild(copy);

    if (d.found) {
      var his = el('button', 'ghost small', '查看历史');
      his.onclick = function () { loadHistory(d.domain, card); };
      acts.appendChild(his);

      var badge = el('button', 'ghost small', '徽章代码');
      badge.onclick = function () {
        var src = location.origin + '/badge/icp.svg?domain=' + encodeURIComponent(d.domain);
        // 原来这里是「调用完立刻改文案」——无论复制成功与否都显示「已复制 ✓」。
        // 在 http + 局域网 IP 下 clipboard 根本不存在，异常被丢掉，
        // 于是用户看到的是一句谎话，粘贴出来是空的。
        copyText('![' + d.domain + ' 备案](' + src + ')').then(function () {
          badge.textContent = '已复制 ✓';
          setTimeout(function () { badge.textContent = '徽章代码'; }, 1600);
        }).catch(function () {
          showCopyBox(card, '![' + d.domain + ' 备案](' + src + ')', '徽章代码');
          badge.textContent = '手动复制 ↓';
        });
      };
      acts.appendChild(badge);

      var watchBtn = el('button', 'ghost small', '加入监控');
      watchBtn.onclick = function () {
        api('/api/v1/watch', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ domain: d.domain })
        }).then(function () {
          watchBtn.textContent = '监控中 ✓';
          watchBtn.disabled = true;
          loadWatch();
        }).catch(function (e) { watchBtn.textContent = '失败：' + e.message; });
      };
      acts.appendChild(watchBtn);
    }
    card.appendChild(acts);
    result.appendChild(card);
  }

  function loadHistory(domain, card) {
    api('/api/v1/history?domain=' + encodeURIComponent(domain)).then(function (d) {
      var old = $('.hist', card);
      if (old) { old.remove(); return; }
      var box = el('div', 'hist');
      box.style.marginTop = '16px';
      if (!d.history || !d.history.length) {
        box.appendChild(el('p', null, '暂无历史快照。'));
      } else {
        var t = el('table');
        var thead = el('thead'), tr = el('tr');
        ['时间', '备案号', '主办单位', '变化'].forEach(function (h) { tr.appendChild(el('th', null, h)); });
        thead.appendChild(tr); t.appendChild(thead);
        var tb = el('tbody');
        d.history.slice().reverse().forEach(function (n) {
          var row = el('tr');
          var r = n.record;
          row.appendChild(el('td', null, new Date(r.fetched_at * 1000).toLocaleDateString('zh-CN')));
          row.appendChild(el('td', null, r.icp_code || '—'));
          row.appendChild(el('td', null, r.unit_name || '—'));
          row.appendChild(el('td', null, n.diff && n.diff.length ? n.diff.map(function (x) { return x.field; }).join('、') : '—'));
          tb.appendChild(row);
        });
        t.appendChild(tb);
        box.appendChild(t);
      }
      card.appendChild(box);
    }).catch(function (e) { showError(e.message); });
  }

  function submit() {
    var v = input.value.trim();
    if (!v) return;
    goBtn.disabled = true;
    goBtn.innerHTML = '<span class="spin"></span>查询中';
    var t0 = performance.now();
    api('/api/v1/query?domain=' + encodeURIComponent(v))
      .then(function (d) {
        render(d);
        history.replaceState(null, '', '?domain=' + encodeURIComponent(d.domain));
      })
      .catch(function (e) { showError(e.message); })
      .finally(function () {
        goBtn.disabled = false;
        goBtn.textContent = '查询';
        var el2 = $('.meta');
        if (el2) el2.textContent = el2.textContent + ' · 端到端 ' + (performance.now() - t0).toFixed(0) + ' ms';
        loadStats();
      });
  }

  form.addEventListener('submit', function (e) { e.preventDefault(); submit(); });
  document.querySelectorAll('.chips button').forEach(function (b) {
    b.addEventListener('click', function () { input.value = b.dataset.d; submit(); });
  });

  function loadStats() {
    api('/api/v1/stats').then(function (s) {
      $('#s-hit').textContent = s.requests.hit_rate;
      $('#s-req').textContent = '命中 ' + num(s.requests.cache_hits) + ' / 回源 ' + num(s.requests.cache_misses);
      $('#s-cache').textContent = num(s.counts.cache);
      $('#s-up').textContent = fmtDur(s.uptime_sec);
      $('#s-ver').textContent = '版本 ' + s.version;
      $('#s-prov').textContent = (s.providers || []).length + ' 个';
      $('#s-prov').title = (s.providers || []).join(', ');
      if (s.storage) {
        $('#s-size').textContent = fmtSize(s.storage.size_bytes);
        var rc = s.storage.retain_changes;
        $('#s-retain').textContent = rc > 0 ? '变更保留 ' + num(rc) + ' 条' : '变更不裁剪';
        var tips = [];
        tips.push(s.storage.retain_history > 0
          ? '每个域名保留最近 ' + num(s.storage.retain_history) + ' 条历史快照'
          : '历史快照不裁剪');
        if (s.storage.retain_history_total > 0) {
          tips.push('历史快照全局最多 ' + num(s.storage.retain_history_total) + ' 条');
        }
        if (s.storage.stale_window > 0) {
          tips.push('超过 ' + fmtDur(s.storage.stale_window) + '没更新的缓存会被清理');
        }
        $('#s-retain').title = tips.join('；');
      }
    }).catch(function () {});
  }
  function fmtSize(b) {
    if (!b) return '–';
    if (b < 1024) return b + ' B';
    if (b < 1024 * 1024) return (b / 1024).toFixed(1) + ' KB';
    return (b / 1024 / 1024).toFixed(1) + ' MB';
  }
  function fmtDur(s) {
    if (s < 60) return s + ' 秒';
    if (s < 3600) return Math.floor(s / 60) + ' 分';
    if (s < 86400) return Math.floor(s / 3600) + ' 时 ' + Math.floor(s % 3600 / 60) + ' 分';
    return Math.floor(s / 86400) + ' 天';
  }

  /* ---- 巡检面板：监控列表 + 最近变更 ---- */
  var wList = $('#w-list'), cList = $('#c-list');

  function when(ts) {
    if (!ts) return '—';
    var diff = Math.floor(Date.now() / 1000) - ts;
    // 未来时间（下次巡检）不能走"刚刚"那一支，否则永远显示"刚刚"。
    if (diff < 0) {
      var ahead = -diff;
      if (ahead < 60) return '1 分钟内';
      if (ahead < 3600) return Math.round(ahead / 60) + ' 分钟后';
      if (ahead < 86400) return Math.round(ahead / 3600) + ' 小时后';
      return new Date(ts * 1000).toLocaleDateString('zh-CN');
    }
    if (diff < 60) return '刚刚';
    if (diff < 3600) return Math.floor(diff / 60) + ' 分钟前';
    if (diff < 86400) return Math.floor(diff / 3600) + ' 小时前';
    if (diff < 86400 * 30) return Math.floor(diff / 86400) + ' 天前';
    return new Date(ts * 1000).toLocaleDateString('zh-CN');
  }

  var lastStatus = null;
  function rhythm(s) {
    var b = $('#w-rhythm');
    if (!b || !s) return;
    if (!s.enabled) { b.textContent = '巡检未启用'; return; }
    if (s.running) { b.textContent = '巡检中…'; return; }
    var parts = [];
    if (s.last_run_at) parts.push('上次 ' + when(s.last_run_at));
    else parts.push('待首轮');
    if (s.next_run_at) parts.push('下次 ' + when(s.next_run_at));
    b.textContent = parts.length ? parts.join(' · ') : '巡检待首轮';
    b.title = s.interval ? '每 ' + fmtDur(s.interval) + ' 巡检一轮' : '';
  }

  /* 监控列表分页：首页每 60 秒轮询一次，域名上千时全量拉一遍只为渲染
     屏幕上那十几行。默认先要 20 条，点「展开」再多要一页，
     展开后的条数记在 wWant 里，轮询时按同样的条数刷新，
     否则每次轮询都会把用户刚展开的列表收回去。 */
  var wWant = 20, wShowAll = false;

  function watchRow(it, box) {
    var row = el('div', 'row');
    var dom = el('div', 'dom');
    var btn = el('button', null, it.domain);
    btn.type = 'button';
    btn.onclick = function () { input.value = it.domain; submit(); };
    dom.appendChild(btn);
    row.appendChild(dom);

    var sub = el('div', 'sub', (it.changes ? it.changes + ' 次变更' : '暂无变更') + ' · 检查于 ' + when(it.checked_at));
    row.appendChild(sub);
    if (it.last_error) {
      // 连续失败次数要写出来：显示「上游异常」但不告诉你挂了几轮，
      // 就没法判断是一次抖动还是已经挂了一整天。
      var b = el('span', 'badge err', it.fail_streak > 1 ? '连续 ' + it.fail_streak + ' 轮失败' : '上游异常');
      b.title = it.last_error;
      row.appendChild(b);
    } else if (it.last_icp) {
      row.appendChild(el('span', 'badge', it.last_icp));
    } else if (it.last_fp) {
      var nb = el('span', 'badge warn', '无备案');
      nb.title = '上一次巡检时未查到备案记录';
      row.appendChild(nb);
    }

    var grow = el('div', 'grow');
    var del = el('button', 'drop', '移出');
    del.type = 'button';
    del.onclick = function () {
      api('/api/v1/watch?domain=' + encodeURIComponent(it.domain), { method: 'DELETE' })
        .then(loadWatch)
        .catch(function (e) { del.textContent = '失败'; del.title = e.message; });
    };
    grow.appendChild(del);
    row.appendChild(grow);
    box.appendChild(row);
  }

  /* fetchWatch 按需翻页，凑够 want 条（want=0 表示全要）后回调。 */
  function fetchWatch(want, cb, fail) {
    var items = [], offset = 0, meta = null;
    function step() {
      var limit = 200;
      if (want > 0) limit = Math.min(200, Math.max(1, want - items.length));
      api('/api/v1/watch/status?offset=' + offset + '&limit=' + limit).then(function (d) {
        if (!meta) meta = d;
        items = items.concat(d.items || []);
        offset += (d.items || []).length;
        var needMore = (want === 0 || items.length < want) && d.has_more && (d.items || []).length > 0;
        if (needMore) { step(); return; }
        meta.loaded = items.length;
        cb(items, meta);
      }).catch(fail);
    }
    step();
  }

  /* 巡检关掉（watch.enabled=false）时，「立即巡检」按下去只会拿到 503。
     按钮留着不变，用户会以为是自己点错了或者服务坏了。这里直接把它按死，
     并把原因写在 title 上——按钮的可用状态本身就是一句说明。 */
  function syncRunButton(d) {
    var btn = $('#w-run');
    if (!btn) return;
    if (d && d.enabled === false) {
      btn.disabled = true;
      btn.textContent = '巡检已关闭';
      btn.title = '配置里 watch.enabled = false，服务不会自动比对。改回 true 并重启即可恢复。';
    } else if (btn.textContent === '巡检已关闭') {
      btn.disabled = false;
      btn.textContent = '立即巡检';
      btn.title = '';
    }
  }

  function loadWatch() {
    if (!wList) return;
    fetchWatch(wShowAll ? 0 : wWant, function (items, d) {
      lastStatus = d;
      $('#w-total').textContent = num(d.total) + ' 个';
      // 「有几个正在报错」是巡检面板最该一眼看到的东西，之前只能逐行去数。
      // 上百个域名时，一条红标藏在列表第 40 行，等于没有。
      //
      // 但这个数字只覆盖**已加载的那部分**：接口分页返回（默认 20 条），
      // 一个域名排在第 25 位报错时，pill 上写「0 个异常」——比不显示更糟，
      // 它是句假话。所以分两种情况：
      //   - 还有没加载的：数字后面加「+」，并用虚线边提示可点，点一下加载全部；
      //   - 全都在手上：给准确的数字。
      var bad = items.filter(function (x) { return x.last_error; }).length;
      var wb = $('#w-bad');
      if (wb) {
        var more = d.has_more || items.length < d.total;
        wb.hidden = bad === 0 && !more;
        wb.className = 'pill err-pill' + (more ? ' partial' : '');
        wb.textContent = bad + (more ? '+' : '') + ' 个异常';
        wb.title = more
          ? '已加载的 ' + items.length + ' / ' + num(d.total) + ' 个域名里有 ' + bad
            + ' 个回源失败。点击加载全部再统计'
          : d.total + ' 个域名里有 ' + bad + ' 个最近一次回源失败，点行内红标看原因';
        wb.disabled = false;
      }
      rhythm(d);
      syncRunButton(d);
      wList.innerHTML = '';
      if (!items.length) {
        wList.appendChild(el('div', 'hint small-hint', '还没有监控域名。查询到结果后点「加入监控」，备案变更会推送给你。'));
        return;
      }
      var box = el('div', 'row-list');
      items.forEach(function (it) { watchRow(it, box); });
      wList.appendChild(box);
      var left = d.total - items.length;
      if (left > 0) {
        // 剩多少要写出来：只说「加载更多」，用户不知道点下去还要点几次。
        var more = el('button', 'ghost small more', '展开更多（还有 ' + num(left) + ' 个）');
        more.type = 'button';
        more.onclick = function () {
          more.disabled = true;
          more.textContent = '加载中…';
          wWant = items.length + 50;
          if (wWant >= d.total) wShowAll = true;
          loadWatch();
        };
        wList.appendChild(more);
      }
    }, function (e) {
      wList.innerHTML = '';
      wList.appendChild(el('div', 'err-box', '监控列表读取失败：' + e.message));
    });
  }

  /* 变更流水的翻页与筛选状态。

     cItems 是已经加载到手的流水（累积），cKind 是类型筛选。
     轮询刷新时按同样的条数重拉，否则一次 60 秒的自动刷新就把用户
     刚展开的内容和刚选的筛选一起收回去。

     两件必须同时成立的事：
     1. **首屏不能一次拉满**。这个面板在页面中段，底下还有推送通道；
        一上来就铺 200 行，等于把后面的内容推到很远，滚动条长得离谱。
     2. **翻页不能撞服务端上限**。/api/v1/changes 的 limit 上限是 200，
        超了如实返回 400（静默改小会让调用方以为后面没数据了）。
        原来这里是 `cWant += 50` 无条件往上堆：点四次就是 limit=210 →
        400 → 整个列表被换成一个错误框，而底下明明还有几百条翻不到。

     所以：每页固定 CHG_PAGE 条，往后翻用 offset，翻到多少条都不会让
     limit 变大。CHG_MAX 只是「别自己越过服务端上限」的守门人。 */
  var CHG_FIRST = 10;
  var CHG_PAGE = 50;
  var CHG_MAX = 200;
  var cItems = [], cKind = '', cHasMore = false, cLoading = false;

  var chgWord = {
    changed: '变更', revoked: '注销', registered: '新增',
    upstream_error: '上游异常', recovered: '已恢复'
  };

  /* 同一条事件的身份：域 + 时间 + 类型。
     at 是秒级时间戳，同一秒里同一域名理论上只会有一条事件；
     加 kind 是为了让「上游异常」和「已恢复」同时刻出现时不被去重掉。 */
  function chgKey(c) { return c.domain + '|' + c.at + '|' + c.kind; }

  /* mergeChanges 把两批记录合成一批：新的在前，重复的按 key 剔掉。
     翻页时用（新一页接在已加载的后面），轮询刷新时也用——
     既让刚发生的变更浮现出来，又不把用户已经展开的旧记录抹掉。 */
  function mergeChanges(fresh, older, prepend) {
    var seen = {}, out = [];
    var add = function (list) {
      list.forEach(function (c) {
        var k = chgKey(c);
        if (seen[k]) return;
        seen[k] = 1;
        out.push(c);
      });
    };
    if (prepend) { add(fresh); add(older); } else { add(older); add(fresh); }
    return out;
  }

  /* loadChanges(extend)：extend 为真时往后翻一页，否则重取开头那一段。 */
  function loadChanges(extend) {
    if (!cList || cLoading) return;
    cLoading = true;
    var offset = extend ? cItems.length : 0;
    // 刷新时按「已加载多少条」重取，让视图保持在同一个位置；
    // 超过上限就只刷新前 CHG_MAX 条，剩下的靠 merge 原样留住。
    var limit = extend ? CHG_PAGE
      : (cItems.length > CHG_FIRST ? Math.min(cItems.length, CHG_MAX) : CHG_FIRST);
    var q = '/api/v1/changes?limit=' + limit;
    if (offset > 0) q += '&offset=' + offset;
    if (cKind) q += '&kind=' + encodeURIComponent(cKind);
    api(q).then(function (d) {
      var page = d.items || [];
      if (extend) {
        cItems = mergeChanges(page, cItems, false);
      } else if (offset === 0 && limit < cItems.length) {
        // 只刷新了最前面一段：后面的旧记录原样保留。
        cItems = mergeChanges(page, cItems, true);
      } else {
        cItems = page;
      }
      cHasMore = !!d.has_more;
      renderChanges();
    }).catch(function (e) {
      cList.innerHTML = '';
      cList.appendChild(el('div', 'err-box', '变更记录读取失败：' + e.message));
    }).finally(function () { cLoading = false; });
  }

  function renderChanges() {
    // 标题要如实反映「看到的是哪一段」：筛了类型还说「最近 10 条」，
    // 用户会以为这类事件一共就这么多。
    var seg = cKind ? (chgWord[cKind] || cKind) + ' · ' : '';
    var shown = cItems.length + (cHasMore ? '+' : '');
    $('#c-total').textContent = seg + (cItems.length ? shown + ' 条' : '0 条');
    cList.innerHTML = '';
    if (!cItems.length) {
      cList.appendChild(el('div', 'hint small-hint', cKind
        ? '这段时间里没有「' + (chgWord[cKind] || cKind) + '」类事件。换个类型看看，或者等下一次巡检。'
        : '暂无变更记录。加入监控后，只有业务字段真的变了才会出现在这里。'));
      return;
    }
    cItems.forEach(function (c) {
      var box = el('div', 'chg');
      var head = el('div', 'chg-head');
      var domBtn = el('button', null, c.domain);
      domBtn.type = 'button';
      domBtn.style.cssText = 'padding:0;font-size:14px;font-weight:600;color:var(--fg);background:none;border:0;cursor:pointer;font-family:inherit';
      domBtn.onclick = function () { input.value = c.domain; submit(); };
      head.appendChild(domBtn);
      if (c.kind === 'revoked') head.appendChild(el('span', 'badge err', '注销'));
      else if (c.kind === 'registered') head.appendChild(el('span', 'badge', '新增'));
      else if (c.kind === 'upstream_error') head.appendChild(el('span', 'badge err', '上游异常'));
      else if (c.kind === 'recovered') head.appendChild(el('span', 'badge', '已恢复'));
      if (c.icp_code) head.appendChild(el('span', 'badge', c.icp_code));
      head.appendChild(el('span', 'when', when(c.at)));
      box.appendChild(head);

      var diff = el('div', 'chg-diff');
      if (c.diff && c.diff.length) {
        diff.appendChild(document.createTextNode(c.diff.map(function (x) {
          return x.field + '：' + (x.from || '无') + ' → ' + (x.to || '无');
        }).join('；')));
      } else {
        diff.appendChild(document.createTextNode('备案信息发生变化'));
      }
      box.appendChild(diff);
      cList.appendChild(box);
    });
    if (cHasMore) {
      // 「还有更多」必须写出来：只说「加载更多」，用户不知道点下去还有几次；
      // 而流水本身有保留上限（默认 2000 条），翻到那儿就真的没有了。
      var more = el('button', 'ghost small more', '加载更早的（每次 ' + CHG_PAGE + ' 条，已加载 ' + cItems.length + ' 条）');
      more.type = 'button';
      more.onclick = function () {
        more.disabled = true;
        more.textContent = '加载中…';
        loadChanges(true);
      };
      cList.appendChild(more);
    }
  }

  /* 「N+ 个异常」是可点的：点一下把全部监控项拉下来，数字才是真的。
     不默认全量拉，是因为这个面板每 60 秒轮询一次，域名上千时全量
     反序列化只为显示一个数字不值当；但用户想知道准数时必须有路可走。 */
  var wBad = $('#w-bad');
  if (wBad) {
    wBad.onclick = function () {
      if (!wShowAll) {
        wShowAll = true;
        wWant = 0;
        loadWatch();
      }
    };
  }

  var cKindSel = $('#c-kind');
  if (cKindSel) {
    cKindSel.onchange = function () {
      cKind = cKindSel.value;
      // 换筛选条件等于换了一份数据集，累积的旧记录必须清掉，
      // 否则「只看注销」的列表里会残留着上一种筛选拉回来的变更。
      cItems = [];
      loadChanges(false);
    };
  }

  var runBtn = $('#w-run');
  if (runBtn) {
    runBtn.onclick = function () {
      runBtn.disabled = true;
      runBtn.textContent = '巡检中…';
      api('/api/v1/watch/run', { method: 'POST' })
        .then(function (d) {
          runBtn.textContent = d.count ? '发现 ' + d.count + ' 处变更' : '无变更';
          loadWatch(); loadChanges();
        })
        .catch(function (e) { runBtn.textContent = '失败：' + e.message; })
        .finally(function () {
          // 复位时别把「巡检已关闭」那句改回去：按钮的可用状态是配置的投影，
          // 不是一次点击的临时状态。
          setTimeout(function () {
            runBtn.textContent = '立即巡检';
            syncRunButton(lastStatus);
          }, 2200);
        });
    };
  }
  var cRefresh = $('#c-refresh');
  // 手动刷新回到「只加载第一页」：用户点刷新想看的是现在怎么样，
  // 不是接着往下翻（那是「加载更早的」按钮的活）。
  if (cRefresh) cRefresh.onclick = function () { cItems = []; loadChanges(false); };

  /* ---- 数据源面板 ---- */
  var pList = $('#p-list');

  function loadProviders() {
    if (!pList) return;
    api('/api/v1/providers').then(function (d) {
      var list = d.providers || [];
      var on = list.filter(function (p) { return p.enabled; });
      $('#p-total').textContent = on.length + ' / ' + list.length + ' 启用';
      pList.innerHTML = '';
      if (!list.length) {
        pList.appendChild(el('div', 'hint small-hint', '没有配置任何数据源。'));
        return;
      }
      var box = el('div', 'row-list');
      list.forEach(function (p) {
        var row = el('div', 'row');
        var dom = el('div', 'dom');
        dom.appendChild(el('span', null, p.name));
        row.appendChild(dom);

        var sub = el('div', 'sub', '权重 ' + p.weight + ' · 超时 ' + p.timeout + ' 秒');
        row.appendChild(sub);

        if (!p.enabled) {
          row.appendChild(el('span', 'badge gray', '已禁用'));
        } else if (p.name === 'apihz' && !p.custom_credential) {
          // 公共测试凭据被限流时不会报错，只会「偶尔查不到」。
          // 界面上不点名，用户永远想不到是凭据的问题。
          var w = el('span', 'badge warn', '公共凭据');
          w.title = '正在使用公共测试凭据，被限流时只表现为查不到。建议在配置里填自己的 id / key。';
          row.appendChild(w);
        } else if (p.custom_credential) {
          row.appendChild(el('span', 'badge', '自有凭据'));
        } else {
          row.appendChild(el('span', 'badge', '参与竞速'));
        }
        box.appendChild(row);
      });
      pList.appendChild(box);
    }).catch(function (e) {
      pList.innerHTML = '';
      pList.appendChild(el('div', 'err-box', '数据源读取失败：' + e.message));
    });
  }
  var pRefresh = $('#p-refresh');
  if (pRefresh) pRefresh.onclick = loadProviders;

  /* ---- 推送通道 ---- */
  var nList = $('#n-list');
  var kindName = { dingtalk: '钉钉', wecom: '企业微信', feishu: '飞书', slack: 'Slack', generic: 'HTTP' };

  function loadNotify() {
    if (!nList) return;
    api('/api/v1/notify/status').then(function (d) {
      $('#n-total').textContent = d.total ? d.total + ' 个' : '未配置';
      nList.innerHTML = '';
      if (!d.enabled) {
        nList.appendChild(el('div', 'hint small-hint', '还没有配置推送通道。在 config.yaml 里加 notify.channels，或设 BEIAN2API_WEBHOOK。'));
        return;
      }
      var box = el('div', 'row-list');
      d.channels.forEach(function (c) {
        var row = el('div', 'row');
        var dom = el('div', 'dom');
        dom.appendChild(el('span', null, c.name));
        row.appendChild(dom);

        var sub = el('div', 'sub', (kindName[c.kind] || c.kind));
        if (c.events && c.events.length) sub.textContent += ' · 只推 ' + c.events.length + ' 类事件';
        row.appendChild(sub);

        if (!c.last_at) {
          row.appendChild(el('span', 'badge gray', '未推送过'));
        } else if (c.last_ok) {
          var ok = el('span', 'badge', '推送正常');
          ok.title = '成功 ' + c.ok + ' 条';
          row.appendChild(ok);
        } else {
          var bad = el('span', 'badge err', '推送失败');
          bad.title = c.last_error || '';
          row.appendChild(bad);
        }
        var grow = el('div', 'grow');
        var t = el('span', 'when', c.last_at ? when(c.last_at) : '');
        grow.appendChild(t);
        row.appendChild(grow);
        box.appendChild(row);

        if (c.last_error) {
          var e = el('div', 'chg-diff err-text', c.last_error);
          e.style.paddingLeft = '2px';
          box.appendChild(e);
        }
      });
      nList.appendChild(box);
    }).catch(function (e) {
      nList.innerHTML = '';
      nList.appendChild(el('div', 'err-box', '推送状态读取失败：' + e.message));
    });
  }

  var nTest = $('#n-test');
  if (nTest) {
    nTest.onclick = function () {
      nTest.disabled = true;
      nTest.textContent = '发送中…';
      api('/api/v1/notify/test', { method: 'POST' })
        .then(function (d) {
          nTest.textContent = d.ok === d.total ? '已送达 ' + d.total + ' 个通道' : '成功 ' + d.ok + ' / ' + d.total;
          loadNotify();
        })
        .catch(function (e) { nTest.textContent = '失败：' + e.message; })
        .finally(function () {
          setTimeout(function () { nTest.disabled = false; nTest.textContent = '发测试消息'; }, 2600);
        });
    };
  }

  // 「上次 / 下次」是相对时间，隔一段时间自己走一走，不用重新拉列表。
  setInterval(function () { rhythm(lastStatus); }, 30000);

  // 面板要自己长出来：页面开着不动，巡检跑完了变更却还停在旧内容，
  // 「改了会通知」这件事在网页上就等于没发生。标签页不可见时不拉，省流量。
  setInterval(function () {
    if (document.hidden) return;
    loadWatch(); loadChanges(); loadNotify(); loadProviders();
  }, 60000);

  refreshRSS();
  loadWatch();
  loadChanges();
  loadNotify();
  loadProviders();

  // 已经存着 Key 但服务端还没要求时，先探一次：这样「Key 失效」也能被及时发现。
  if (apiKey()) {
    api('/api/v1/stats').then(function () { $('#keybox').hidden = true; }).catch(function () {});
  }

  var snippets = {
    curl: 'curl "' + location.origin + '/api/v1/query?domain=example.com"\n\n# 强制回源\ncurl "' + location.origin + '/api/v1/query?domain=example.com&refresh=1"\n\n# 批量\ncurl -X POST "' + location.origin + '/api/v1/batch" \\\n  -H "Content-Type: application/json" \\\n  -d \'{"domains":["qq.com","baidu.com"]}\'',
    js: "const r = await fetch('" + location.origin + "/api/v1/query?domain=example.com');\nconst { data } = await r.json();\nif (data.found) console.log(data.records[0].icp_code);",
    py: 'import requests\n\nr = requests.get("' + location.origin + '/api/v1/query",\n                 params={"domain": "example.com"}, timeout=10)\nprint(r.json()["data"]["records"][0]["icp_code"])',
    go: 'resp, err := http.Get("' + location.origin + '/api/v1/query?domain=example.com")\nif err != nil { log.Fatal(err) }\ndefer resp.Body.Close()\n\nvar out struct {\n\tData struct {\n\t\tFound   bool `json:"found"`\n\t\tRecords []struct {\n\t\t\tICPCode string `json:"icp_code"`\n\t\t} `json:"records"`\n\t} `json:"data"`\n}\njson.NewDecoder(resp.Body).Decode(&out)\nfmt.Println(out.Data.Records[0].ICPCode)',
    badge: '<!-- Markdown -->\n![' + 'example.com 备案](' + location.origin + '/badge/icp.svg?domain=example.com)\n\n<!-- HTML -->\n<img src="' + location.origin + '/badge/icp.svg?domain=example.com" alt="ICP">'
  };
  var tabs = document.querySelectorAll('#tabs button');
  function setSnippet(t) {
    tabs.forEach(function (b) { b.classList.toggle('on', b.dataset.t === t); });
    $('#snippet').textContent = snippets[t];
  }
  tabs.forEach(function (b) { b.addEventListener('click', function () { setSnippet(b.dataset.t); }); });
  setSnippet('curl');
  loadStats();
  setInterval(loadStats, 30000);

  var p = new URLSearchParams(location.search).get('domain');
  if (p) { input.value = p; submit(); }
})();

/* ---- 备案号校验器 ---- */
(function () {
  'use strict';
  var form = document.getElementById('vform');
  var input = document.getElementById('vc');
  var domInput = document.getElementById('vd');
  var box = document.getElementById('vresult');
  if (!form) return;

  // 414px 下完整示例占位文字会被输入框裁掉半截，换一个更短的说法。
  if (window.matchMedia('(max-width: 480px)').matches) {
    [input, domInput].forEach(function (el) {
      if (el && el.dataset.short) el.placeholder = el.dataset.short;
    });
  }

  function kv(list) {
    var dl = document.createElement('dl');
    dl.className = 'kv';
    list.forEach(function (p) {
      if (!p[1]) return;
      var dt = document.createElement('dt'); dt.textContent = p[0];
      var dd = document.createElement('dd'); dd.textContent = p[1];
      dl.appendChild(dt); dl.appendChild(dd);
    });
    return dl;
  }

  function render(d) {
    box.innerHTML = '';
    var card = document.createElement('div');
    card.className = 'res-card fade';
    card.style.padding = '14px 0 0';

    var head = document.createElement('div');
    head.className = 'res-head';
    var h = document.createElement('h2'); h.textContent = d.normalized || d.input;
    head.appendChild(h);
    var badge = document.createElement('span');
    badge.className = 'badge' + (d.valid ? '' : ' err');
    badge.textContent = d.valid ? '结构合法' : '存在问题';
    head.appendChild(badge);
    card.appendChild(head);

    card.appendChild(kv([
      ['省份', d.province ? d.province + '（' + d.region + '）' : ''],
      ['类型', d.kind],
      ['主体序号', d.serial],
      ['站点序号', d.site_seq],
      ['规范化', d.normalized]
    ]));

    if (d.fixed && d.fixed.length) {
      var n = document.createElement('div');
      n.className = 'notice';
      n.textContent = '已自动纠正：' + d.fixed.join('；');
      card.appendChild(n);
    }
    if (d.issues && d.issues.length) {
      var e = document.createElement('div');
      e.className = 'err-box';
      e.textContent = d.issues.join('；');
      card.appendChild(e);
    }
    if (d.cross_check) {
      var c = document.createElement('div');
      c.className = 'notice';
      if (!d.cross_check.checked) {
        c.textContent = '与域名交叉核对未完成：' + (d.cross_check.reason || '');
      } else if (d.cross_check.match) {
        c.textContent = '✓ 与查询到的实际备案号一致';
        c.style.color = 'var(--ok)';
        c.style.background = 'rgba(5,150,105,.1)';
        c.style.borderColor = 'rgba(5,150,105,.24)';
      } else if (d.cross_check.same_subject) {
        // 主体对得上、站点序号对不上：这是伪造备案号最常见的样子
        // （拿一家真实存在的主体号，只改尾数）。只说「不一致」会把
        // 这层信息丢掉——用户看到实际号，才知道差在哪一位。
        //
        // 用红色而不是 notice 的黄色：黄色的语义是「注意」，而这里是一条
        // 否定结论——它和「✓ 一致」必须是两种一眼能分辨的颜色。
        c.className = 'err-box';
        c.textContent = '✗ 主体对得上，但不是同一个网站。该域名实际备案号：'
          + (d.cross_check.actual || '未查到');
      } else {
        c.className = 'err-box';
        c.textContent = '✗ 与该域名的实际备案号完全不一致，实际为：' + (d.cross_check.actual || '未查到');
      }
      card.appendChild(c);
    }
    box.appendChild(card);
  }

  function showFail(msg) {
    box.innerHTML = '';
    var d = document.createElement('div');
    d.className = 'err-box'; d.textContent = msg || '校验失败';
    box.appendChild(d);
  }

  form.addEventListener('submit', function (e) {
    e.preventDefault();
    var v = input.value.trim();
    if (!v) return;
    box.innerHTML = '<div class="hint">校验中…</div>';
    // 顺带把可选的域名带上：只有它能让「结构合法」升级成「真的是这个域名的」。
    var domain = domInput ? domInput.value.trim() : '';
    var url = '/api/v1/verify?code=' + encodeURIComponent(v);
    if (domain) url += '&domain=' + encodeURIComponent(domain);
    // 同步异常（比如拼接 URL 时出错）不能让它停在「校验中…」：
    // 那是界面上最糟的一种状态——既没说成功，也没说失败，用户只能干等。
    var req;
    try {
      req = fetch(withKey(url));
    } catch (err) {
      showFail(err && err.message ? err.message : String(err));
      return;
    }
    req.then(function (r) { return r.json(); })
      .then(function (j) {
        if (!j.ok) throw new Error((j.error && j.error.message) || '校验失败');
        render(j.data);
      })
      .catch(function (err) { showFail(err.message); });
  });
})();
