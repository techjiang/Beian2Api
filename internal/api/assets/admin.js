/* Beian2API 后台：原生 JS，无依赖。
   和查询台共用 KEY_STORE，但保存逻辑各写各的——后台不关心
   「Key 填错了请求全 401」，它要的是「Key 不对时明确告诉你哪不对」。 */

var KEY_STORE = 'beian2api.key';

function b2aKey() {
  try { return localStorage.getItem(KEY_STORE) || ''; } catch (e) { return ''; }
}

function setKey(v) {
  try {
    if (v) localStorage.setItem(KEY_STORE, v);
    else localStorage.removeItem(KEY_STORE);
    return true;
  } catch (e) {
    return false;
  }
}

function withKey(path) {
  var k = b2aKey();
  if (!k) return path;
  return path + (path.indexOf('?') >= 0 ? '&' : '?') + 'key=' + encodeURIComponent(k);
}

(function () {
  'use strict';

  var $ = function (s, r) { return (r || document).querySelector(s); };

  /* 页面拆成六个 tab，每个 tab 只管自己那块的状态。
     放在一个对象里而不是六个闭包：它们共享 Key、共享 toast、
     共享「出错时把面板换成错误框」这套行为。 */

  function el(tag, cls, text) {
    var n = document.createElement(tag);
    if (cls) n.className = cls;
    if (text != null) n.textContent = text;
    return n;
  }

  function num(n) { return (n == null ? 0 : n).toLocaleString('zh-CN'); }

  function bytes(n) {
    if (!n) return '0 B';
    var u = ['B', 'KB', 'MB', 'GB', 'TB'], i = 0;
    while (n >= 1024 && i < u.length - 1) { n /= 1024; i++; }
    return (i === 0 ? n : n.toFixed(1)) + ' ' + u[i];
  }

  function ago(ts) {
    if (!ts) return '—';
    var d = Math.floor(Date.now() / 1000 - ts);
    if (d < 0) return '即将';
    if (d < 60) return d + ' 秒前';
    if (d < 3600) return Math.floor(d / 60) + ' 分钟前';
    if (d < 86400) return Math.floor(d / 3600) + ' 小时前';
    return Math.floor(d / 86400) + ' 天前';
  }

  /* fmtExpire 专门给「过期时间」这一列用。
     ago() 对未来的时间只能返回「即将」——那是在说「有什么事快要发生」，
    而这一列问的是「还能用多久」。24 行全是「即将」，等于这一列没有信息。 */
  function fmtExpire(ts) {
    if (!ts) return '—';
    var d = ts - Math.floor(Date.now() / 1000);
    if (d <= 0) {
      d = -d;
      if (d < 60) return '刚过期';
      if (d < 3600) return Math.floor(d / 60) + ' 分钟前过期';
      if (d < 86400) return Math.floor(d / 3600) + ' 小时前过期';
      return Math.floor(d / 86400) + ' 天前过期';
    }
    if (d < 60) return '不到 1 分钟';
    if (d < 3600) return Math.floor(d / 60) + ' 分钟后';
    if (d < 86400) return Math.floor(d / 3600) + ' 小时后';
    return Math.floor(d / 86400) + ' 天后';
  }

  function when(ts) {
    if (!ts) return '—';
    var d = new Date(ts * 1000), p = function (n) { return (n < 10 ? '0' : '') + n; };
    return d.getFullYear() + '-' + p(d.getMonth() + 1) + '-' + p(d.getDate()) +
      ' ' + p(d.getHours()) + ':' + p(d.getMinutes());
  }

  function dur(sec) {
    sec = sec || 0;
    if (sec < 60) return sec + ' 秒';
    if (sec < 3600) return Math.floor(sec / 60) + ' 分钟';
    if (sec < 86400) return Math.floor(sec / 3600) + ' 小时';
    return Math.floor(sec / 86400) + ' 天';
  }

  var toastTimer = null;
  function toast(msg, bad) {
    var t = $('#toast');
    if (!t) return;
    t.textContent = msg;
    t.className = 'toast' + (bad ? ' bad' : '');
    t.hidden = false;
    if (toastTimer) clearTimeout(toastTimer);
    // 成功 2.6 秒、失败 6 秒：失败信息里往往带着上游原文，
    // 一眼看不完就消失了，等于没提示。
    toastTimer = setTimeout(function () { t.hidden = true; }, bad ? 6000 : 2600);
  }

  /* api 是唯一的请求出口。
     401 单独处理：开了鉴权的部署里，任何一个面板失败都会走到这里，
     所以「露出 Key 框」这件事必须在这一处做完，而不是每个 tab 各写一遍。 */
  function api(path, opts) {
    opts = opts || {};
    if (opts.json !== undefined) {
      opts.method = opts.method || 'POST';
      opts.headers = { 'Content-Type': 'application/json' };
      opts.body = JSON.stringify(opts.json);
    }
    return fetch(withKey(path), opts).then(function (r) {
      return r.json().catch(function () { return { ok: false, error: { message: 'HTTP ' + r.status } }; })
        .then(function (j) {
          if (r.status === 401) {
            showKey('这个实例开启了鉴权，请先填入 API Key。');
            throw new Error('需要 API Key');
          }
          if (!r.ok || j.ok === false) {
            var m = (j.error && j.error.message) || ('HTTP ' + r.status);
            throw new Error(m);
          }
          return j.data;
        });
    });
  }

  function showKey(tip) {
    var bar = $('#keybar');
    if (!bar) return;
    bar.hidden = false;
    if (tip) $('#keytip').textContent = tip;
    var f = $('#k');
    if (f && !f.value) f.value = b2aKey();
  }

  /* fail 把面板正文换成一句能看懂的错误。
     留空面板是最糟的处理：用户分不清「没有数据」和「接口挂了」。 */
  function fail(host, err) {
    if (!host) return;
    host.innerHTML = '';
    var box = el('div', 'err-box', err && err.message ? err.message : String(err));
    host.appendChild(box);
  }

  function table(cols) {
    var t = el('table', 'tbl');
    var thead = el('thead');
    var tr = el('tr');
    cols.forEach(function (c) {
      if (c) tr.appendChild(el('th', null, c));
    });
    thead.appendChild(tr);
    t.appendChild(thead);
    t.appendChild(el('tbody'));
    return t;
  }

  function tbody(t) { return t.querySelector('tbody'); }

  function cell(tr, text, cls, label) {
    var td = el('td', cls, text);
    if (label) td.setAttribute('data-l', label);
    tr.appendChild(td);
    return td;
  }

  function emptyRow(t, text, span) {
    var tr = el('tr');
    var td = el('td', 'empty', text);
    td.colSpan = span;
    tr.appendChild(td);
    tbody(t).appendChild(tr);
  }

  /* ---- Key ---- */
  var kform = $('#kform');
  if (kform) {
    kform.addEventListener('submit', function (e) {
      e.preventDefault();
      var v = $('#k').value.trim();
      setKey(v);
      // 保存后立刻验一次：只回「已保存」等于没验证过，
      // 填错了要等到下次刷新才发现。
      api('/api/v1/admin/overview').then(function () {
        $('#keytip').textContent = '✓ Key 有效，数据已加载。';
        load('overview');
      }).catch(function (err) {
        $('#keytip').textContent = '✗ ' + (err.message || 'Key 无效');
      });
    });
  }

  /* ---- 趋势图 ----
     原生 SVG，不引图表库。一个概览页的折线图不值得为它背一个 60KB 的依赖，
     而这个项目从第一天起就是「单二进制、零外部资源」——图也算外部资源。 */

  var SVG_NS = 'http://www.w3.org/2000/svg';

  function svg(tag, attrs) {
    var n = document.createElementNS(SVG_NS, tag);
    Object.keys(attrs || {}).forEach(function (k) { n.setAttribute(k, attrs[k]); });
    return n;
  }

  function pad2(n) { return (n < 10 ? '0' : '') + n; }
  function hourLabel(ts) { var d = new Date(ts * 1000); return pad2(d.getHours()) + ':00'; }

  /* 折线路径。values 里小于 0 的项视为「没有数据」，断开而不是画成 0：
     0 和「没请求」在图上必须长得不一样，否则一次停机会被读成命中率归零。 */
  function linePath(values, w, h, max) {
    var n = values.length;
    var step = n > 1 ? w / (n - 1) : w;
    var out = '', pen = false;
    for (var i = 0; i < n; i++) {
      var v = values[i];
      if (v < 0) { pen = false; continue; }
      var x = n > 1 ? i * step : w / 2;
      var y = h - (max > 0 ? (v / max) * h : 0);
      out += (pen ? 'L' : 'M') + x.toFixed(1) + ' ' + y.toFixed(1) + ' ';
      pen = true;
    }
    return out.trim();
  }

  function areaPath(values, w, h, max) {
    var d = linePath(values, w, h, max);
    if (!d) return '';
    var n = values.length;
    var step = n > 1 ? w / (n - 1) : w;
    var first = -1, last = -1;
    for (var i = 0; i < n; i++) {
      if (values[i] >= 0) { if (first < 0) first = i; last = i; }
    }
    var x0 = n > 1 ? first * step : w / 2;
    var x1 = n > 1 ? last * step : w / 2;
    return d + ' L' + x1.toFixed(1) + ' ' + h + ' L' + x0.toFixed(1) + ' ' + h + ' Z';
  }

  function drawChart(points) {
    var host = $('#ov-chart');
    host.innerHTML = '';
    if (!points || !points.length) {
      host.appendChild(el('div', 'hint small-hint', '还没有数据。这个序列按小时聚合，跑满一小时就会出现第一段。'));
      return;
    }
    var W = 720, H = 132, PAD_T = 8, PAD_B = 4;
    var inner = H - PAD_T - PAD_B;

    var hits = [], miss = [], chg = [];
    var totalReq = 0, totalChg = 0;
    points.forEach(function (p) {
      hits.push(p.hits || 0);
      miss.push(p.misses || 0);
      chg.push(p.changes || 0);
      totalReq += (p.hits || 0) + (p.misses || 0);
      totalChg += p.changes || 0;
    });

    // 请求量用两条独立折线而不是堆叠：命中与回源各自一条，交叉点就是
    // 命中率跌破一半的位置，堆叠图反而看不出这一点。
    var reqMax = 1, chgMax = 1;
    points.forEach(function (p) {
      reqMax = Math.max(reqMax, (p.hits || 0) + (p.misses || 0), p.hits || 0);
      chgMax = Math.max(chgMax, p.changes || 0);
    });

    var root = svg('svg', {
      viewBox: '0 0 ' + W + ' ' + H,
      preserveAspectRatio: 'none',
      class: 'chart',
      role: 'img',
      'aria-label': '最近 24 小时的命中、回源与变更趋势'
    });

    // 网格：三条横线，只在有数据时画，免得空图上挂一堆装饰。
    for (var g = 0; g <= 2; g++) {
      var gy = PAD_T + inner * g / 2;
      root.appendChild(svg('line', {
        x1: 0, y1: gy.toFixed(1), x2: W, y2: gy.toFixed(1),
        class: 'grid'
      }));
    }

    var hitsPath = linePath(hits, W, inner, reqMax);
    var missPath = linePath(miss, W, inner, reqMax);

    if (hitsPath) {
      var grp = svg('g', { transform: 'translate(0,' + PAD_T + ')' });
      grp.appendChild(svg('path', { d: areaPath(hits, W, inner, reqMax), class: 'area-hits' }));
      grp.appendChild(svg('path', { d: missPath, class: 'ln miss' }));
      grp.appendChild(svg('path', { d: hitsPath, class: 'ln hits' }));
      root.appendChild(grp);
    }

    // 变更不画折线。
    //
    // 第一版把它也画成折线、按自己的比例铺满图高——结果一条「8 次变更」
    // 的尖峰看着和「270 次查询」一样高，读图的人会以为那小时流量爆了。
    // 换个 Y 轴能让形状好看，但那是骗人：两条曲线一旦不共享刻度，
    // 它们之间的任何高低比较都是假的。
    //
    // 所以变更改成贴底的小柱条，封顶图高的 22%。它回答的是「什么时候
    // 有变动」，不是「变了多少」，柱条够用，也不会被误读成流量。
    var barH = inner * 0.22;
    var n = points.length;
    var bw = Math.max(1.5, Math.min(6, (W / Math.max(n, 1)) * 0.5));
    var bars = svg('g', { transform: 'translate(0,' + (PAD_T + inner - barH) + ')' });
    points.forEach(function (p, i) {
      var v = p.changes || 0;
      if (v <= 0) return;
      var step = n > 1 ? W / (n - 1) : W;
      var x = (n > 1 ? i * step : W / 2) - bw / 2;
      var hh = Math.max(1.5, barH * (v / chgMax));
      bars.appendChild(svg('rect', {
        x: x.toFixed(1), y: (barH - hh).toFixed(1),
        width: bw.toFixed(1), height: hh.toFixed(1),
        rx: Math.min(1.5, bw / 2), class: 'bar-chg'
      }));
    });
    root.appendChild(bars);
    host.appendChild(root);

    // 时间轴：只标首、中、尾，24 个标签在 414px 上会糊成一团。
    var axis = el('div', 'chart-axis');
    var idx = [0, Math.floor((points.length - 1) / 2), points.length - 1];
    idx.forEach(function (i, k) {
      var s = el('span', null, hourLabel(points[i].hour));
      if (k === 1) s.className = 'mid';
      axis.appendChild(s);
    });
    host.appendChild(axis);

    $('#ov-chart-total').textContent = num(totalReq) + ' 次请求';
    var peak = 0, peakAt = 0;
    points.forEach(function (p) {
      var t = (p.hits || 0) + (p.misses || 0);
      if (t > peak) { peak = t; peakAt = p.hour; }
    });
    $('#ov-chart-foot').textContent = totalReq
      ? '峰值 ' + num(peak) + ' 次/时（' + hourLabel(peakAt) + '）· 共 ' + num(totalChg) + ' 次变更'
      : '这 24 小时没有查询请求。';
  }

  /* ---- 概览 ---- */
  function loadOverview() {
    return api('/api/v1/admin/overview').then(function (d) {
      $('#ov-cache').textContent = num(d.counts.cache);
      $('#ov-history').textContent = num(d.counts.history);
      $('#ov-changes').textContent = num(d.counts.changes);
      $('#ov-watch').textContent = num(d.watch.total);
      $('#ov-size').textContent = bytes(d.storage.size_bytes);
      $('#ov-hitrate').textContent = '命中率 ' + d.requests.hit_rate + '%';
      $('#ov-stale').textContent = '回退 ' + num(d.requests.stale_fallbacks) + ' 次';
      $('#ov-uptime').textContent = '运行 ' + dur(d.uptime_sec);
      drawChart(d.series);

      var rh = $('#ov-rhythm');
      if (!d.watch.enabled) {
        rh.textContent = '自动巡检已关闭';
        rh.style.color = 'var(--warn)';
      } else {
        rh.style.color = '';
        rh.textContent = d.watch.running ? '巡检中…'
          : '上次 ' + ago(d.watch.last_run_at) + ' · 每 ' + dur(d.watch.interval);
      }

      // 上游面板
      var host = $('#ov-providers');
      host.innerHTML = '';
      var t = table(['数据源', '状态', '权重', '超时', '凭据']);
      d.providers.forEach(function (p) {
        var tr = el('tr');
        cell(tr, p.name, 'mono', '数据源');
        var st = cell(tr, '', null, '状态');
        var b = el('span', 'badge' + (p.enabled ? '' : ' gray'), p.enabled ? '启用' : '未启用');
        st.appendChild(b);
        cell(tr, String(p.weight), 'num', '权重');
        cell(tr, p.timeout + 's', 'num', '超时');
        var cred = cell(tr, '', null, '凭据');
        if (p.name === 'apihz' && !p.custom_credential) {
          // 公共凭据被限流时不报错，只表现为「偶尔查不到」——
          // 后台是唯一能把这件事说出来给人看的地方。
          cred.appendChild(el('span', 'badge warn', '公共凭据'));
        } else {
          cred.appendChild(el('span', 'badge' + (p.custom_credential ? '' : ' gray'),
            p.custom_credential ? '自有' : '—'));
        }
        tbody(t).appendChild(tr);
      });
      host.appendChild(t);
      $('#ov-prov-count').textContent = d.providers.length + ' 个';

      // 推送面板
      var nh = $('#ov-notify');
      nh.innerHTML = '';
      $('#ov-n-count').textContent = d.notify.length + ' 个';
      if (!d.notify.length) {
        nh.appendChild(el('div', 'hint small-hint', '还没有配置推送通道。在 config.yaml 里加 notify.channels。'));
      } else {
        var nt = table(['通道', '类型', '最近投递', '时间']);
        d.notify.forEach(function (c) {
          var tr = el('tr');
          cell(tr, c.name, null, '通道');
          cell(tr, c.kind || 'generic', 'mono', '类型');
          var st = cell(tr, '', null, '最近投递');
          if (!c.last_at) {
            st.appendChild(el('span', 'badge gray', '未投递'));
          } else if (c.last_ok) {
            st.appendChild(el('span', 'badge', '成功'));
          } else {
            st.appendChild(el('span', 'badge err', '失败'));
            if (c.last_error) {
              var e = el('div', 'err-text', c.last_error);
              st.appendChild(e);
            }
          }
          cell(tr, ago(c.last_at), null, '时间');
          tbody(nt).appendChild(tr);
        });
        nh.appendChild(nt);
      }

      // 存储面板
      var sh = $('#ov-storage');
      sh.innerHTML = '';
      var kv = el('div', 'kv-list');
      [
        ['数据文件', d.storage.path],
        ['文件大小', bytes(d.storage.size_bytes)],
        ['历史快照总数', num(d.storage.history_total) + ' / ' + (d.storage.retain_history_total || '不限')],
        ['历史保留（每域名）', d.storage.retain_history ? num(d.storage.retain_history) + ' 条' : '不裁剪'],
        ['变更流水保留', d.storage.retain_changes ? num(d.storage.retain_changes) + ' 条' : '不裁剪'],
        ['回退窗口', dur(d.storage.stale_window)],
        ['缓存命中', num(d.requests.cache_hits) + ' / ' + num(d.requests.cache_hits + d.requests.cache_misses)],
        ['鉴权', d.auth.enabled ? '已开启' : '未开启（仅本机可管理）']
      ].forEach(function (p) {
        var row = el('div', 'kv-row');
        row.appendChild(el('span', null, p[0]));
        row.appendChild(el('span', null, p[1]));
        kv.appendChild(row);
      });
      sh.appendChild(kv);
    }).catch(function (err) { fail($('#ov-providers'), err); });
  }

  /* ---- 缓存 ---- */
  var cacheState = { offset: 0, items: [], q: '', total: 0 };
  var PAGE = 30;

  function loadCache(reset) {
    if (reset) { cacheState.offset = 0; cacheState.items = []; }
    var path = '/api/v1/admin/cache?limit=' + PAGE + '&offset=' + cacheState.offset +
      (cacheState.q ? '&q=' + encodeURIComponent(cacheState.q) : '');
    return api(path).then(function (d) {
      cacheState.items = cacheState.items.concat(d.items);
      cacheState.total = d.total;
      cacheState.offset = cacheState.items.length;
      renderCache(d.has_more);
    }).catch(function (err) { fail($('#c-list'), err); });
  }

  function renderCache(hasMore) {
    var host = $('#c-list');
    host.innerHTML = '';
    var t = table(['域名', '状态', '备案号', '主办单位', '来源', '过期', '']);
    if (!cacheState.items.length) {
      emptyRow(t, cacheState.q ? '没有匹配「' + cacheState.q + '」的缓存' : '还没有缓存记录', 7);
    }
    cacheState.items.forEach(function (r) {
      var tr = el('tr');
      var d = cell(tr, '', 'mono', '域名');
      var btn = el('button', 'rowlink', r.domain);
      btn.type = 'button';
      btn.addEventListener('click', function () {
        location.href = '/?domain=' + encodeURIComponent(r.domain);
      });
      d.appendChild(btn);
      var st = cell(tr, '', null, '状态');
      var expired = r.expire_at && r.expire_at * 1000 < Date.now();
      st.appendChild(el('span', 'badge ' + (!r.found ? 'gray' : (expired ? 'warn' : '')),
        !r.found ? '无备案' : (expired ? '已过期' : '有效')));
      cell(tr, r.icp_code || '—', 'mono', '备案号');
      cell(tr, r.unit_name || '—', 'wrap', '主办单位');
      cell(tr, r.source || '—', 'mono', '来源');
      cell(tr, fmtExpire(r.expire_at), null, '过期');
      var act = cell(tr, '', 'act');
      var del = el('button', 'mini danger', '删除');
      del.type = 'button';
      del.addEventListener('click', function () {
        if (!confirm('删除 ' + r.domain + ' 的缓存？下次查询会重新回源。')) return;
        api('/api/v1/admin/cache?domain=' + encodeURIComponent(r.domain), { method: 'DELETE' })
          .then(function () { toast('已删除 ' + r.domain); loadCache(true); })
          .catch(function (err) { toast(err.message, true); });
      });
      act.appendChild(del);
      tbody(t).appendChild(tr);
    });
    host.appendChild(t);
    var more = $('#c-more');
    more.hidden = !hasMore;
    more.textContent = '加载更多（共 ' + num(cacheState.total) + ' 条）';
  }

  /* ---- 历史快照 ---- */
  var histState = { offset: 0, items: [], q: '' };

  function loadHistory(reset) {
    if (reset) { histState.offset = 0; histState.items = []; }
    var path = '/api/v1/admin/history?limit=' + PAGE + '&offset=' + histState.offset +
      (histState.q ? '&q=' + encodeURIComponent(histState.q) : '');
    return api(path).then(function (d) {
      histState.items = histState.items.concat(d.items);
      histState.offset = histState.items.length;
      renderHistory(d.total, d.has_more);
    }).catch(function (err) { fail($('#h-list'), err); });
  }

  function renderHistory(total, hasMore) {
    var host = $('#h-list');
    host.innerHTML = '';
    var t = table(['域名', '快照数 / 配额', '最近备案号', '最近主办单位', '最后一次', '']);
    if (!histState.items.length) {
      emptyRow(t, histState.q ? '没有匹配「' + histState.q + '」的历史' : '还没有历史快照', 6);
    }
    histState.items.forEach(function (r) {
      var tr = el('tr');
      var d = cell(tr, '', 'mono', '域名');
      var btn = el('button', 'rowlink', r.domain);
      btn.type = 'button';
      btn.addEventListener('click', function () {
        location.href = '/?domain=' + encodeURIComponent(r.domain);
      });
      d.appendChild(btn);

      // 「6 / 200」而不是单独一个 6：单看数字看不出「这算多还是少」，
      // 而配额就是这个数字唯一有意义的参照系。
      var c = cell(tr, '', null, '快照数');
      c.appendChild(el('span', null, r.kept_limit ? num(r.count) + ' / ' + num(r.kept_limit) : num(r.count)));
      if (r.kept_limit) {
        // 进度条只在「快用完」时才值得看，所以它放在数字下面、很细，
        // 满 80% 变橙——它是一条提醒，不是一列常驻指标。
        var pct = Math.min(100, Math.round(r.count * 100 / r.kept_limit));
        var bar = el('div', 'bar' + (pct >= 80 ? ' warn' : ''));
        var fill = el('i');
        fill.style.width = pct + '%';
        bar.appendChild(fill);
        c.appendChild(bar);
      }
      cell(tr, r.last_icp || '—', 'mono', '最近备案号');
      cell(tr, r.last_unit || '—', 'wrap', '最近主办单位');
      cell(tr, when(r.last_at), null, '最后一次');
      var act = cell(tr, '', 'act');
      var del = el('button', 'mini danger', '清空');
      del.type = 'button';
      del.addEventListener('click', function () {
        if (!confirm('删除 ' + r.domain + ' 的全部 ' + r.count + ' 条历史快照？不可撤销。')) return;
        api('/api/v1/admin/history?domain=' + encodeURIComponent(r.domain), { method: 'DELETE' })
          .then(function (res) { toast('已删除 ' + res.removed + ' 条'); loadHistory(true); })
          .catch(function (err) { toast(err.message, true); });
      });
      act.appendChild(del);
      tbody(t).appendChild(tr);
    });
    host.appendChild(t);
    var more = $('#h-more');
    more.hidden = !hasMore;
    more.textContent = '加载更多（共 ' + num(total) + ' 个域名）';
  }

  /* ---- 变更流水 ---- */
  /* 翻页用 next_cursor 而不是 offset：offset 是「走 N 步再开始收」，
     服务端每翻一页都要把前面所有记录空走一遍并逐条反序列化，流水一多
     就越点越慢。cursor 是上一页最后一条的 key，Seek 直接跳过去。 */
  var chState = { cursor: '', items: [], kind: '' };

  var KIND_WORD = {
    changed: ['变更', ''], revoked: ['注销', 'err'], registered: ['新增', ''],
    upstream_error: ['上游异常', 'warn'], recovered: ['已恢复', 'gray']
  };

  function loadChanges(reset) {
    if (reset) { chState.cursor = ''; chState.items = []; }
    var path = '/api/v1/changes?limit=' + PAGE +
      (chState.cursor ? '&cursor=' + encodeURIComponent(chState.cursor) : '') +
      (chState.kind ? '&kind=' + encodeURIComponent(chState.kind) : '');
    return api(path).then(function (d) {
      chState.items = chState.items.concat(d.items || []);
      chState.cursor = d.next_cursor || '';
      renderChanges(d.has_more);
    }).catch(function (err) { fail($('#ch-list'), err); });
  }

  function renderChanges(hasMore) {
    var host = $('#ch-list');
    host.innerHTML = '';
    var t = table(['域名', '类型', '变化', '时间', '']);
    if (!chState.items.length) {
      emptyRow(t, '暂无变更记录', 5);
    }
    chState.items.forEach(function (c) {
      var tr = el('tr');
      cell(tr, c.domain, 'mono', '域名');
      var k = cell(tr, '', null, '类型');
      var w = KIND_WORD[c.kind] || ['变更', ''];
      k.appendChild(el('span', 'badge ' + w[1], w[0]));
      var diff = cell(tr, '', 'wrap', '变化');
      if (c.diff && c.diff.length) {
        c.diff.forEach(function (f) {
          var line = el('div', null, f.field + '：' + (f.from || '—') + ' → ' + (f.to || '—'));
          diff.appendChild(line);
        });
      } else {
        diff.textContent = c.icp_code || '—';
      }
      cell(tr, when(c.at), null, '时间');
      var act = cell(tr, '', 'act');
      var del = el('button', 'mini danger', '删除');
      del.type = 'button';
      del.addEventListener('click', function () {
        if (!confirm('删除 ' + c.domain + ' 在 ' + when(c.at) + ' 的这条流水？')) return;
        api('/api/v1/admin/changes?at=' + c.at + '&domain=' + encodeURIComponent(c.domain),
          { method: 'DELETE' })
          .then(function () { toast('已删除'); loadChanges(true); })
          .catch(function (err) { toast(err.message, true); });
      });
      act.appendChild(del);
      tbody(t).appendChild(tr);
    });
    host.appendChild(t);
    var more = $('#ch-more');
    more.hidden = !hasMore;
    more.textContent = '加载更早的';
  }

  /* ---- 监控 ---- */
  var wState = { offset: 0, items: [], total: 0 };

  function loadWatch(reset) {
    if (reset) { wState.offset = 0; wState.items = []; }
    return api('/api/v1/watch/status?limit=' + PAGE + '&offset=' + wState.offset)
      .then(function (d) {
        wState.items = wState.items.concat(d.items || []);
        wState.offset = wState.items.length;
        wState.total = d.total;
        $('#w-total').textContent = num(d.total) + ' 个';
        renderWatch(d.has_more, d);
      }).catch(function (err) { fail($('#w-list'), err); });
  }

  function renderWatch(hasMore, st) {
    var host = $('#w-list');
    host.innerHTML = '';
    var t = table(['域名', '备案号', '变更', '状态', '最近检查', '']);
    if (!wState.items.length) {
      emptyRow(t, '还没有监控域名', 6);
    }
    wState.items.forEach(function (w) {
      var tr = el('tr');
      var d = cell(tr, '', 'mono', '域名');
      var btn = el('button', 'rowlink', w.domain);
      btn.type = 'button';
      btn.addEventListener('click', function () {
        location.href = '/?domain=' + encodeURIComponent(w.domain);
      });
      d.appendChild(btn);
      cell(tr, w.last_icp || (w.last_fp ? '无备案' : '—'), 'mono', '备案号');
      cell(tr, String(w.changes || 0), 'num', '变更');
      var stc = cell(tr, '', null, '状态');
      if (w.fail_streak > 0) {
        var b = el('span', 'badge err', '连续 ' + w.fail_streak + ' 轮失败');
        if (w.last_error) b.title = w.last_error;
        stc.appendChild(b);
      } else if (w.last_fp === 'absent') {
        stc.appendChild(el('span', 'badge warn', '无备案'));
      } else {
        stc.appendChild(el('span', 'badge', '正常'));
      }
      cell(tr, ago(w.checked_at), null, '最近检查');
      var act = cell(tr, '', 'act');
      var del = el('button', 'mini danger', '移出');
      del.type = 'button';
      del.addEventListener('click', function () {
        api('/api/v1/watch?domain=' + encodeURIComponent(w.domain), { method: 'DELETE' })
          .then(function () { toast('已移出 ' + w.domain); loadWatch(true); })
          .catch(function (err) { toast(err.message, true); });
      });
      act.appendChild(del);
      tbody(t).appendChild(tr);
    });
    host.appendChild(t);
    var more = $('#w-more');
    more.hidden = !hasMore;
    more.textContent = '加载更多（共 ' + num(wState.total) + ' 个）';
    // 巡检关掉时「立即巡检」只能拿 503，按钮该提前变灰并说明原因。
    var run = $('#w-run');
    if (run && st) {
      run.disabled = !st.enabled;
      run.textContent = st.enabled ? (st.running ? '巡检中…' : '立即巡检') : '巡检已关闭';
      run.title = st.enabled ? '' : 'watch.enabled 为 false';
    }
  }

  /* ---- 备份 ---- */
  function loadBackups() {
    return api('/api/v1/admin/backups').then(function (d) {
      $('#b-total').textContent = num(d.total) + ' 个';
      $('#b-size').textContent = bytes(d.size_bytes);
      var host = $('#b-list');
      host.innerHTML = '';
      var t = table(['文件名', '大小', '生成时间', '']);
      if (!d.items.length) {
        emptyRow(t, '还没有备份。点右上角「立即备份」生成一份一致性快照（服务不停机）。', 4);
      }
      d.items.forEach(function (b) {
        var tr = el('tr');
        cell(tr, b.name, 'mono', '文件名');
        cell(tr, bytes(b.size), 'num', '大小');
        cell(tr, when(b.created_at) + '（' + ago(b.created_at) + '）', null, '生成时间');
        var act = cell(tr, '', 'act');
        // 备份文件在服务端磁盘上，浏览器没法直接下载：
        // 这里给出完整路径，让人用 scp / rsync 拿走，比假装能点更强。
        var cp = el('button', 'mini', '复制路径');
        cp.type = 'button';
        cp.addEventListener('click', function () {
          var p = d.dir + '/' + b.name;
          if (navigator.clipboard && navigator.clipboard.writeText) {
            navigator.clipboard.writeText(p).then(function () { toast('已复制：' + p); })
              .catch(function () { toast(p); });
          } else {
            toast(p);
          }
        });
        act.appendChild(cp);
        tbody(t).appendChild(tr);
      });
      host.appendChild(t);
    }).catch(function (err) { fail($('#b-list'), err); });
  }

  /* ---- tab 切换 ---- */
  var loaded = {};

  function load(name) {
    switch (name) {
      case 'overview': return loadOverview();
      case 'cache': return loadCache(!loaded.cache);
      case 'history': return loadHistory(!loaded.history);
      case 'changes': return loadChanges(!loaded.changes);
      case 'watch': return loadWatch(!loaded.watch);
      case 'backups': return loadBackups();
    }
  }

  function show(name) {
    // 先切面板再加载：反过来的话，接口慢的时候人点了 tab 却停在旧页面上，
    // 会以为点击没生效，然后再点一次。
    document.querySelectorAll('.page').forEach(function (p) { p.classList.remove('on'); });
    var page = $('#p-' + name);
    if (page) page.classList.add('on');
    document.querySelectorAll('.side a').forEach(function (a) {
      a.classList.toggle('on', a.getAttribute('data-p') === name);
    });
    loaded[name] = true;
    if (location.hash !== '#' + name) {
      try { history.replaceState(null, '', '#' + name); } catch (e) { /* file:// 下不支持 */ }
    }
    load(name);
  }

  document.querySelectorAll('.side a').forEach(function (a) {
    a.addEventListener('click', function (e) {
      e.preventDefault();
      show(a.getAttribute('data-p'));
    });
  });

  /* 地址栏里的 hash 也要管。
     后台每块内容都有自己的地址（/admin#cache），人会收藏、会从聊天记录
     里点开，也会用前进/后退。只处理侧栏点击的话，直接用 #cache 打开
     页面会停在概览上——地址写着缓存，屏幕显示概览，这比什么都更容易
     让人以为程序坏了。 */
  window.addEventListener('hashchange', function () {
    var name = (location.hash || '#overview').slice(1);
    var page = $('#p-' + name);
    if (!page) return;
    var isOn = page.classList.contains('on');
    if (isOn) return;
    show(name);
  });

  /* ---- 各面板的按钮 ---- */
  function bind(id, fn) {
    var n = $(id);
    if (n) n.addEventListener('click', fn);
  }

  bind('#ov-refresh', function () { loadOverview().then(function () { toast('已刷新'); }); });

  bind('#c-search', function () {
    cacheState.q = $('#c-q').value.trim();
    loadCache(true);
  });
  bind('#c-more', function () { loadCache(false); });
  bind('#c-clear-expired', function () {
    if (!confirm('清理「已过期且超出回退窗口」的缓存？这些条目已经没有任何代码路径能读到，删掉不会影响查询。')) return;
    api('/api/v1/admin/cache/clear', { json: { only_expired: true } })
      .then(function (d) { toast('已清理 ' + num(d.removed) + ' 条过期缓存'); loadCache(true); })
      .catch(function (err) { toast(err.message, true); });
  });
  bind('#c-clear-all', function () {
    // 全清是不可逆的：回退快照一起没了，上游一挂就没有兜底数据。
    // 所以二次确认里把后果写清楚，而不是只说「确定吗」。
    if (!confirm('清空全部缓存？\n\n这会删掉所有本地快照，包括上游不可用时的回退数据。之后每个域名第一次查询都会重新回源。')) return;
    api('/api/v1/admin/cache/clear', { json: { only_expired: false } })
      .then(function (d) { toast('已清空 ' + num(d.removed) + ' 条缓存'); loadCache(true); })
      .catch(function (err) { toast(err.message, true); });
  });

  bind('#h-search', function () {
    histState.q = $('#h-q').value.trim();
    loadHistory(true);
  });
  bind('#h-more', function () { loadHistory(false); });

  bind('#ch-refresh', function () { loadChanges(true); });
  bind('#ch-more', function () { loadChanges(false); });
  bind('#ch-kind', function () {
    chState.kind = $('#ch-kind').value;
    loadChanges(true);
  });
  bind('#ch-clear', function () {
    if (!confirm('清空全部变更流水？\n\n流水是巡检的历史记录，清掉之后 RSS 与「最近变更」都会变空，且不可恢复。')) return;
    api('/api/v1/admin/changes/clear', { json: {} })
      .then(function (d) { toast('已清空 ' + num(d.removed) + ' 条流水'); loadChanges(true); })
      .catch(function (err) { toast(err.message, true); });
  });

  bind('#w-run', function () {
    var b = $('#w-run');
    b.disabled = true;
    b.textContent = '巡检中…';
    api('/api/v1/watch/run', { json: {} })
      .then(function (d) {
        toast('巡检完成，发现 ' + num(d.count) + ' 处变更');
        loadWatch(true);
        loadChanges(true);
      })
      .catch(function (err) { toast(err.message, true); loadWatch(true); });
  });
  bind('#w-purge', function () {
    if (!confirm('清空全部监控域名？\n\n这不会删掉缓存和历史，但巡检从此不再跟踪它们。')) return;
    api('/api/v1/admin/watch/purge', { json: {} })
      .then(function (d) { toast('已清空 ' + num(d.removed) + ' 个监控'); loadWatch(true); })
      .catch(function (err) { toast(err.message, true); });
  });
  bind('#w-more', function () { loadWatch(false); });

  function bulk(action) {
    var raw = $('#w-bulk').value || '';
    var list = raw.split(/[\s,;、]+/).filter(function (s) { return s.trim() !== ''; });
    if (!list.length) { toast('先贴几个域名进来', true); return; }
    api('/api/v1/admin/watch/bulk', { json: { domains: list, action: action } })
      .then(function (d) {
        var msg = (action === 'add' ? '加入 ' : '移出 ') + num(d.added) + ' 个';
        if (d.duplicated) msg += '，重复跳过 ' + num(d.duplicated) + ' 个';
        if (d.failed) msg += '，失败 ' + num(d.failed) + ' 个：' + d.invalid.join('、');
        toast(msg, d.failed > 0);
        $('#w-bulk-tip').textContent = msg;
        if (!d.failed) $('#w-bulk').value = '';
        loadWatch(true);
      })
      .catch(function (err) { toast(err.message, true); });
  }
  bind('#w-add', function () { bulk('add'); });
  bind('#w-remove', function () { bulk('remove'); });

  bind('#b-make', function () {
    var b = $('#b-make');
    b.disabled = true;
    b.textContent = '备份中…';
    api('/api/v1/admin/backup', { method: 'POST' })
      .then(function (d) { toast('已生成 ' + d.path.split('/').pop() + '（' + bytes(d.size) + '）'); loadBackups(); })
      .catch(function (err) { toast(err.message, true); })
      .then(function () { b.disabled = false; b.textContent = '立即备份'; });
  });
  bind('#b-purge', function () {
    if (!confirm('删除 backups 目录里的全部快照文件？\n\n这是真的删文件，不是清缓存。建议先确认最新一份已经拷到机器外。')) return;
    api('/api/v1/admin/backups', { method: 'DELETE' })
      .then(function (d) { toast('已删除 ' + num(d.removed) + ' 个备份文件'); loadBackups(); })
      .catch(function (err) { toast(err.message, true); });
  });

  /* ---- 首屏 ----
     没配 Key 的实例（本机直连）应该直接进后台，不要先让人看一个空框。
     所以先打一次 overview：成功就不显示 Key 框，401 才显示。 */
  var start = (location.hash || '#overview').slice(1);
  if (!/^[a-z]+$/.test(start) || !$('#p-' + start)) start = 'overview';

  api('/api/v1/admin/overview').then(function () {
    show(start);
  }).catch(function (err) {
    if (err.message === '需要 API Key') {
      show(start);
      return;
    }
    // 其它错误（比如非本机访问返回 403）也把页面显示出来，
    // 但要说清楚为什么连不上——空白页面最容易让人以为是程序坏了。
    show(start);
    toast(err.message, true);
  });
})();
