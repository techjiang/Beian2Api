/* Beian2API 前端：原生 JS，无依赖，约 4KB。 */
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
     开了 auth 的部署，所有 /api 接口都要 Key。之前网页上没有任何地方能填，
     首页于是整片报「缺少或无效的 API Key」——配了鉴权就没法用网页了。
     Key 存在 localStorage，随请求带在 ?key= 上；RSS 订阅链接也带上。 */
  var KEY_STORE = 'beian2api.key';
  function apiKey() {
    try { return localStorage.getItem(KEY_STORE) || ''; } catch (e) { return ''; }
  }
  function withKey(path) {
    var k = apiKey();
    if (!k) return path;
    return path + (path.indexOf('?') >= 0 ? '&' : '?') + 'key=' + encodeURIComponent(k);
  }
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
      try {
        if (v) localStorage.setItem(KEY_STORE, v);
        else localStorage.removeItem(KEY_STORE);
      } catch (err) { /* 隐私模式下写不了，就当这次会话有效 */ }
      $('#keybox').hidden = !!v;
      refreshRSS();
      loadStats(); loadWatch(); loadChanges(); loadNotify(); loadProviders();
      if (input.value.trim()) submit();
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
      navigator.clipboard.writeText(JSON.stringify(d, null, 2)).then(function () {
        copy.textContent = '已复制 ✓';
        setTimeout(function () { copy.textContent = '复制 JSON'; }, 1600);
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
        navigator.clipboard.writeText('![' + d.domain + ' 备案](' + src + ')');
        badge.textContent = '已复制 ✓';
        setTimeout(function () { badge.textContent = '徽章代码'; }, 1600);
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

  function loadWatch() {
    if (!wList) return;
    api('/api/v1/watch/status').then(function (d) {
      lastStatus = d;
      $('#w-total').textContent = d.total + ' 个';
      rhythm(d);
      wList.innerHTML = '';
      if (!d.items || !d.items.length) {
        wList.appendChild(el('div', 'hint small-hint', '还没有监控域名。查询到结果后点「加入监控」，备案变更会推送给你。'));
        return;
      }
      var box = el('div', 'row-list');
      d.items.forEach(function (it) {
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
      });
      wList.appendChild(box);
    }).catch(function (e) {
      wList.innerHTML = '';
      wList.appendChild(el('div', 'err-box', '监控列表读取失败：' + e.message));
    });
  }

  function loadChanges() {
    if (!cList) return;
    api('/api/v1/changes?limit=10').then(function (d) {
      $('#c-total').textContent = d.total >= 10 ? '最近 10 条' : d.total + ' 条';
      cList.innerHTML = '';
      if (!d.items || !d.items.length) {
        cList.appendChild(el('div', 'hint small-hint', '暂无变更记录。加入监控后，只有业务字段真的变了才会出现在这里。'));
        return;
      }
      d.items.forEach(function (c) {
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
    }).catch(function (e) {
      cList.innerHTML = '';
      cList.appendChild(el('div', 'err-box', '变更记录读取失败：' + e.message));
    });
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
          setTimeout(function () { runBtn.disabled = false; runBtn.textContent = '立即巡检'; }, 2200);
        });
    };
  }
  var cRefresh = $('#c-refresh');
  if (cRefresh) cRefresh.onclick = loadChanges;

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
  var box = document.getElementById('vresult');
  if (!form) return;

  // 414px 下完整示例占位文字会被输入框裁掉半截，换一个更短的说法。
  if (window.matchMedia('(max-width: 480px)').matches && input.dataset.short) {
    input.placeholder = input.dataset.short;
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
      } else {
        c.textContent = '✗ 与该域名的实际备案号不一致，实际为：' + (d.cross_check.actual || '未查到');
      }
      card.appendChild(c);
    }
    box.appendChild(card);
  }

  form.addEventListener('submit', function (e) {
    e.preventDefault();
    var v = input.value.trim();
    if (!v) return;
    box.innerHTML = '<div class="hint">校验中…</div>';
    fetch(withKey('/api/v1/verify?code=' + encodeURIComponent(v)))
      .then(function (r) { return r.json(); })
      .then(function (j) {
        if (!j.ok) throw new Error((j.error && j.error.message) || '校验失败');
        render(j.data);
      })
      .catch(function (err) {
        box.innerHTML = '';
        var d = document.createElement('div');
        d.className = 'err-box'; d.textContent = err.message;
        box.appendChild(d);
      });
  });
})();
