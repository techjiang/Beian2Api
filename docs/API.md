# Beian2API 接口文档

Base URL：`http://你的域名:8080`

所有响应统一为以下外壳，成功时 `ok=true` 且带 `data`，失败时 `ok=false` 且带 `error`：

```json
{ "ok": true, "data": { } }
{ "ok": false, "error": { "code": "invalid_domain", "message": "域名格式不正确，示例：example.com" } }
```

HTTP 状态码同时反映结果：`200` 成功，`400` 参数错误，`401` 缺少 API Key，`405` 方法不允许，`429` 触发限流，`502` 上游不可用且无本地快照。

`/api/` 下的未知路径也走同一外壳（`404 not_found`），不会漏出一行纯文本
`404 page not found`：调用方按 `ok==false` 判错，拿到 HTML 文本只会报
「Unexpected token <」，而真正的原因只是把路径写错了。

`POST` 请求体上限 1 MB。超限返回 `400 bad_request`，`message` 会写明是超过上限
而不是 JSON 语法错——两者都是 400，但提示含混会让人反复检查一份没写错的 JSON。

---

## GET /api/v1/query

查询单个域名的备案信息。也支持 POST + JSON body。

**参数**

| 名称 | 位置 | 必填 | 说明 |
| --- | --- | --- | --- |
| domain | query / body | 是 | 域名。可带协议、路径、端口、`www.`，服务端会归一化 |
| refresh | query / body | 否 | `1`/`true` 时跳过缓存强制回源 |

**示例**

```bash
curl "http://localhost:8080/api/v1/query?domain=example.com"
curl -X POST "http://localhost:8080/api/v1/query" \
  -H "Content-Type: application/json" \
  -d '{"domain":"example.com","refresh":true}'
```

**响应**

```json
{
  "ok": true,
  "data": {
    "domain": "example.com",
    "found": true,
    "records": [{
      "domain": "example.com",
      "unit_name": "示例科技有限公司",
      "unit_type": "企业",
      "site_name": "示例站点",
      "icp_code": "京ICP备12345678号-1",
      "province": "京",
      "nature": "企业",
      "audit_time": "2024-01-01",
      "source": "miit",
      "fetched_at": 1735689600
    }],
    "cached": false,
    "stale": false,
    "source": "miit",
    "cost_ms": 312.4,
    "fetched_at": 1735689600,
    "notice": ""
  }
}
```

**字段**

| 字段 | 说明 |
| --- | --- |
| found | 公开备案库中是否存在该域名 |
| cached | 本次结果是否来自本地缓存 |
| stale | 上游全部不可用，返回的是本地最后快照 |
| notice | `stale=true` 时的说明文字 |
| source | 实际提供数据的数据源名 |
| records[].domain | 始终是你查询时用的那个域名（不是上游回显的）。历史快照按它做 key，两边不一致会让「查看历史」永远是空的 |
| records[].nature | 备案性质，已从上游代码翻译为中文 |
| records[].province | 从备案号解析出的省份简称 |

---

## GET / POST /api/v1/batch

批量查询，单次最多 50 个，自动去重。

```bash
# GET：逗号分隔
curl "http://localhost:8080/api/v1/batch?domains=qq.com,baidu.com"

# POST：JSON 数组
curl -X POST "http://localhost:8080/api/v1/batch" \
  -H "Content-Type: application/json" \
  -d '{"domains":["qq.com","baidu.com"],"refresh":false}'
```

```json
{
  "ok": true,
  "data": {
    "total": 2,
    "items": [
      { "domain": "qq.com", "result": { "...": "同 query 的 data" } },
      { "domain": "bad", "error": "域名格式不正确" }
    ]
  }
}
```

单项失败不影响其他项，错误落在该条的 `error` 字段里。

---

## GET /api/v1/history

某域名的历史快照，按时间升序。相邻两次之间的差异会预先算好。

| 参数 | 必填 | 说明 |
| --- | --- | --- |
| domain | 是 | 域名 |
| limit | 否 | 只返回最近 N 条 |

```json
{
  "ok": true,
  "data": {
    "domain": "example.com",
    "total": 2,
    "history": [
      { "record": { "...": "快照" } },
      { "record": { "...": "快照" }, "diff": [{ "field": "备案号", "from": "京ICP备1号", "to": "京ICP备2号" }] }
    ]
  }
}
```

每次内容变化都会留一条快照，同一时刻重复写入则覆盖。按天合并会让「一天内改了两次」
只剩最后一版，中间那次变更连同它的 diff 就永久丢了。

---

## GET /api/v1/diff

直接对比最近两次快照，适合做"备案信息被悄悄改了"的检查。

```json
{
  "ok": true,
  "data": {
    "domain": "example.com",
    "changed": true,
    "previous": { "...": "旧快照" },
    "current": { "...": "新快照" },
    "diff": [{ "field": "主办单位", "from": "A 公司", "to": "B 公司" }]
  }
}
```

历史不足两条时返回 `changed=false` 和说明文字。

---

## GET /api/v1/verify

**校验备案号本身，不访问任何上游。** 可同时传 `domain` 做交叉核对。

| 参数 | 必填 | 说明 |
| --- | --- | --- |
| code | 是 | 备案号，也接受 `icp` 作为参数名 |
| domain | 否 | 传入后会与该域名的实际备案号比对（首页校验框的第二个输入框就是它） |

```bash
curl "http://localhost:8080/api/v1/verify?code=京ICP备12345678号-1"
curl "http://localhost:8080/api/v1/verify?code=京ICP备12345678号-1&domain=example.com"
```

```json
{
  "ok": true,
  "data": {
    "input": "京ICP备12345678号-1",
    "valid": true,
    "normalized": "京ICP备12345678号-1",
    "province": "北京市",
    "region": "11",
    "kind": "非经营性备案",
    "serial": "12345678",
    "site_seq": "1",
    "issues": null,
    "fixed": null,
    "cross_check": {
      "checked": true,
      "match": true,
      "same_subject": true,
      "actual": "京ICP备12345678号-1",
      "verified": true
    }
  }
}
```

**校验内容**

- 省份简称必须是合法省级行政区，返回完整省名与行政区划前两位
- 区分「ICP备」（非经营性）、「ICP证」（经营性许可证）、「B1/B2」（经营性业务）
- 主体序号至少 4 位
- 自动纠正：全角字符、空格、常见错字（泸→沪、像→豫、榆→渝等）、数字段里的字母误写（O→0、I→1）
- 纠正过程会在 `fixed` 数组里逐条说明

`valid=false` 时 `issues` 会列出所有不成立的地方。

**`cross_check` 的三个布尔值语义不同，别混用**

| 字段 | 含义 |
| --- | --- |
| `checked` | 是否真的查了域名。为 `false` 时看 `reason`（域名不合法、上游不可用等） |
| `match` | **整条备案号**是否一致，含 `-N` 站点序号 |
| `same_subject` | 是否同一主体（省份 + 主体序号相同，忽略站点序号） |

`www.` 前缀不影响查询：备案按主域名登记，`www.example.com` 与 `example.com`
是同一条备案，两者共用同一份缓存、同一行监控。适配器按域名精确比对上游结果时
用的是「忽略 www.」的语义，否则查 `www.example.com` 会把唯一命中的记录丢掉，
一个已备案的域名被判成「未备案」——还带着 10 分钟的负缓存。

`match` 要求站点序号也一致，因为 `/verify` 回答的是「这个域名上挂的备案号是不是真的那一个」。
一个主体下可以有多个网站，`-1` 和 `-7` 是同一家公司、不同站点——拿真实存在的 `-1`
改个尾数写成 `-7` 去冒充另一个域名，结构合法、主体也对得上，而这正是伪造备案号
最常见的样子。只比对主体会给这种伪造盖章「一致」。

备案号里没有站点序号时（`京ICP备12345678号`，写法本身合法），`match` 退回主体比对。

---

## POST /api/v1/watch

把域名加入巡检列表。加入时先抓一次建立基线，之后只在内容真的变化时才产生事件。

```bash
curl -X POST "http://localhost:8080/api/v1/watch" \
  -H "Content-Type: application/json" -d '{"domain":"example.com"}'
```

```json
{ "ok": true, "data": { "domain": "example.com", "watching": true, "baseline_icp": "京ICP备12345678号-1" } }
```

## DELETE /api/v1/watch?domain=example.com

移出巡检列表。

## GET /api/v1/watch/status

巡检运行概况：是否正在跑、上一轮何时结束、下一轮何时开始。首页那颗「上次/下次」标签就取自这里。

**分页**：默认只返回第一页（`limit=20`，上限 200）。首页每 60 秒轮询一次，
域名上千时全量返回是纯浪费——屏幕上就那十几行。`total` 是监控总数，
不是本页条数，所以翻页时它不会变。

| 参数 | 说明 |
| --- | --- |
| limit | 本页条数，默认 20，上限 200 |
| offset | 跳过前多少条，默认 0 |

```json
{
  "ok": true,
  "data": {
    "enabled": true,
    "interval": 86400,
    "total": 137,
    "offset": 0,
    "limit": 20,
    "has_more": true,
    "running": false,
    "last_run_at": 1735776000,
    "last_count": 1,
    "next_run_at": 1735862400,
    "items": [{ "...": "同 watch/list 的 items" }]
  }
}
```

`offset` 不是非负整数时返回 400（`invalid_offset`）。不能静默当成 0：
翻页客户端会把第一页当成第 N 页，然后无限循环。

巡检未启用（`watch.enabled=false`）时该接口依然可用：`enabled=false`、`interval=0`、
`running=false`，但 **`items` / `total` 照常返回库里真实存在的监控项**。
「有没有在自动巡检」和「监控着哪些域名」是两件事，不能因为前者关掉就把后者答成空——
那样首页会显示「还没有监控域名」，而库里明明躺着数据，用户只会以为自己加丢了。

## GET /api/v1/watch/list

同样支持 `limit` / `offset`（`limit` 留空表示全量，上限 5000），`total` 始终是监控总数。

```json
{
  "ok": true,
  "data": {
    "total": 1,
    "interval": 86400,
    "items": [{
      "domain": "example.com",
      "added_at": 1735689600,
      "checked_at": 1735776000,
      "last_fp": "dd7de4d5e0ddbed4d790474841a524bb",
      "last_icp": "京ICP备12345678号-1",
      "changes": 1,
      "fail_streak": 0,
      "last_ok_at": 1735776000
    }]
  }
}
```

## POST /api/v1/watch/run

立即执行一轮巡检，返回本次发现的变更。适合配合外部定时任务使用。

巡检串行执行，同一时刻只有一轮在跑；客户端断开连接不会中断正在进行的巡检。

执行窗口按「域名数 ÷ worker 数 × 单域名超时」估算，最少 2 分钟、最多 30 分钟，
并同步放宽这个请求的 HTTP 写超时——否则一轮跑十几分钟的巡检会在 30 秒处被掐断，
巡检其实跑完了，浏览器看到的却是「连接被重置」。

```json
{
  "ok": true,
  "data": {
    "count": 1,
    "changes": [{
      "domain": "example.com",
      "at": 1735776000,
      "kind": "changed",
      "icp_code": "京ICP备87654321号-1",
      "diff": [{ "field": "备案号", "from": "京ICP备12345678号-1", "to": "京ICP备87654321号-1" }]
    }]
  }
}
```

---

## GET /api/v1/changes

最近记录的变更事件，新的在前。每条都带 `kind` 字段：

- `changed` — 备案信息本身变了
- `revoked` — 之前有备案，现在查不到了（域名可能被注销）
- `registered` — 之前没有备案，现在有了
- `upstream_error` — 连续多轮回源失败（默认 3 轮，见 `watch.fail_threshold`）。
  这比备案变更更急：你不知道这个域名现在是死是活。同一次故障只报一次。
- `recovered` — 从上一类故障中恢复。没有它，通知里最后一条会永远停在「挂了」。

`upstream_error` 的 `diff` 形如：

```json
[{ "field": "查询状态", "from": "正常", "to": "连续 3 轮失败" },
 { "field": "失败原因", "from": "", "to": "上游数据源均不可用（miit: 401）" }]
```

| 参数 | 说明 |
| --- | --- |
| limit | 返回条数，默认 50，上限 200。超上限返回 400 而不是静默改小——静默降级会让调用方的分页逻辑以为「后面没数据了」 |
| since | Unix 秒，只返回该时间之后的变更。写成非数字或负数返回 400（`invalid_since`）：静默当成 0 会让增量同步的调用方把历史事件重复处理一遍。遍历是新的在前，读到第一条更旧的记录就停——增量拉取不用扫完整条流水 |
| offset | 跳过前 N 条（分页）。配合 `limit` 往前翻历史，首页「加载更早的」就用它 |
| kind | 只看某一类事件，取值同上五个。写错返回 400（`invalid_kind`）并列出合法值——静默当成「不过滤」会让「只看注销」的页面显示成全部 |

响应里除 `total`（本页条数）和 `items`，还有 `offset`、`limit`、`has_more`。
`has_more` 是「后面还有没有」的准确答案，客户端不用自己拿 `total` 去猜：

```json
{ "ok": true, "data": { "total": 2, "items": [], "offset": 0, "limit": 50, "has_more": true } }
```

## GET /api/v1/changes.rss

同样的变更事件，输出 RSS 2.0，可直接丢进 RSS 阅读器或 IM 订阅机器人。

启用 API Key 后，这个接口同样需要鉴权——变更流水等于「你在监控哪些域名」的清单，
不该对全网公开。RSS 阅读器不方便加请求头，所以订阅地址里带上 `?key=` 即可：

```
http://localhost:8080/api/v1/changes.rss?key=bk_xxx
```

---

## GET /api/v1/notify/status

推送通道与最近一次投递状态。不回显通道 URL 与密钥——机器人地址本身就是凭据。

```json
{
  "ok": true,
  "data": {
    "enabled": true,
    "total": 2,
    "channels": [{
      "name": "运维群",
      "kind": "dingtalk",
      "events": ["changed", "revoked", "registered", "upstream_error", "recovered"],
      "last_at": 1735776000,
      "last_ok": true,
      "ok": 12,
      "failed": 0,
      "attempts": 1
    }]
  }
}
```

未配置任何通道时返回 `enabled=false`，不会 404。

## POST /api/v1/notify/test

立刻往全部通道发一条测试消息，同步返回每个通道的真实投递结果：

```json
{
  "ok": true,
  "data": {
    "total": 2,
    "ok": 1,
    "results": [
      { "channel": "运维群", "kind": "dingtalk", "ok": true, "attempts": 1 },
      { "channel": "飞书", "kind": "feishu", "ok": false, "attempts": 1,
        "error": "接口返回错误 code=19001: param invalid" }
    ]
  }
}
```

这是验证配置的唯一可靠办法：钉钉、企业微信在 token 失效或关键词不匹配时
同样返回 HTTP 200，只看状态码看不出问题。`error` 里会带上游原始错误码与说明。

---

## GET /badge/icp.svg

SVG 徽章，可直接嵌进 README。

| 参数 | 说明 |
| --- | --- |
| domain | 域名 |
| label | 左侧标签文字，默认 `ICP` |

```markdown
![备案](http://localhost:8080/badge/icp.svg?domain=example.com)
```

已备案为绿色并显示备案号，未备案为橙色，查询出错为红色。响应带 10 分钟缓存头。

---

## GET / POST /api/v1/admin/backup

在服务运行中生成一致性数据快照到 `data/backups/`。

数据库用 bbolt 的排他文件锁，外部进程在服务运行时无法打开，因此备份必须由服务自身完成。

| 参数 | 说明 |
| --- | --- |
| name | 文件名，只能是纯文件名（默认 `beian2api-<时间>.db`） |

```bash
curl -X POST "http://127.0.0.1:8080/api/v1/admin/backup"
curl -X POST "http://127.0.0.1:8080/api/v1/admin/backup?name=before-upgrade.db"
```

```json
{
  "ok": true,
  "data": {
    "path": "/opt/beian2api/data/backups/before-upgrade.db",
    "size": 24576,
    "created_at": 1735689600
  }
}
```

**访问控制**：配置了 API Key 时必须携带 Key（走标准鉴权）；未配置 Key 时只允许本机回环访问，
其他来源返回 403。带 `X-Forwarded-For` 头（说明经过反代）也不视为本机。

---

## GET /api/v1/stats

```json
{
  "ok": true,
  "data": {
    "version": "1.0.0",
    "uptime_sec": 3600,
    "providers": ["miit", "apihz"],
    "counts": { "cache": 128, "history": 96, "watch": 3, "keys": 0, "changes": 2 },
    "requests": {
      "cache_hits": 512,
      "cache_misses": 96,
      "stale_fallbacks": 4,
      "hit_rate": "84.2%"
    },
    "data_file": "/opt/beian2api/data/beian2api.db",
    "storage": {
      "size_bytes": 32768,
      "retain_changes": 2000,
      "retain_history": 200,
      "retain_history_total": 20000,
      "stale_window": 604800
    }
  }
}
```

`storage` 是数据文件的实际大小与滚动保留策略（上限为 `0` 表示不裁剪）：
`retain_changes` 是变更流水条数，`retain_history` 是每个域名的历史快照条数，
`retain_history_total` 是历史快照的全局条数上限，`stale_window` 是「过期缓存还能
被回退使用」的窗口（秒），超出这个窗口的缓存条目会被定期清理。
计数器（命中 / 回源 / 快照回退）在内存里累加、每 15 秒刷盘一次，
所以这里的数字最多滞后十几秒——用这点新鲜度换掉每次请求一次 fsync，很划算。

## GET /api/v1/providers

各数据源的启用状态、权重、超时，以及是否配了自有凭据。

```json
{
  "ok": true,
  "data": {
    "providers": [
      { "name": "miit", "enabled": true, "weight": 100, "timeout": 8, "custom_credential": false },
      { "name": "apihz", "enabled": true, "weight": 40, "timeout": 8, "custom_credential": false }
    ]
  }
}
```

`custom_credential` 目前只对 apihz 有意义：它为 `false` 时用的就是公共测试凭据，
被限流时不会报错，只表现为「偶尔查不到」。

权重会实际影响竞速：权重最高的源立刻出发，其余按权重差按比例延后起跑
（最多 200ms）。这是对冲而不是排队——高权重源慢了或者挂了，低权重源照样接手，
只是让它先跑一小会儿。权重全相等时延迟为 0，回到「同时竞速、谁快谁赢」。

## GET /healthz / GET /readyz

健康探针。`healthz` 始终快速返回进程存活，`readyz` 会检查存储是否可读。

---

## 访问日志

`log_level: info`（默认）下，每个 `/api/*` 请求会输出一行访问日志：

```
level=INFO msg=http method=GET path=/api/v1/query status=200 ms=12.4 ip=203.0.113.7
```

`debug` 下所有请求（含静态资源与探针）都会记录。之前这行日志在默认配置下从不出现——
条件判断写了「info 级别下的 /api 请求」，输出却用的 `Debug`，被 handler 直接丢掉了。

## 鉴权与限流

配置了 `auth.keys` 或环境变量 `BEIAN2API_API_KEYS` 后（两者都会自动打开 `auth.enabled`，
或在 yaml 里显式写 `enabled: true`），所有 `/api` 接口都需要 Key：

```bash
curl -H "X-API-Key: bk_xxx" "http://localhost:8080/api/v1/query?domain=example.com"
curl -H "Authorization: Bearer bk_xxx" "..."
curl "...&key=bk_xxx"
```

生成 Key：

```bash
./beian2api keygen -n 3
```

开了鉴权之后网页仍然可以正常用：首页依旧是公开的，检测到 401 时会露出一个
API Key 输入框，填一次就存在浏览器本地，后续请求自动带上。保存时会立刻发一次请求
验证，界面直接告诉你「Key 有效」还是「无效」——填错了跟没填一样，若只提示
「已保存」，你要等到下一次刷新才发现还是 401。

`auth.enabled` 与 `auth.keys` 必须同时成立，只写一半会**拒绝启动**：
只开开关而没 Key，等于没有任何可校验的凭据、接口全裸；只配 Key 而忘了开开关，
你以为在保护其实对外公开。这两种状态都比「没配鉴权」更危险，所以宁可起不来。

限流按客户端 IP 做令牌桶，默认 20 rps / burst 60。被限时返回 429 与 `Retry-After`。
健康检查与静态页面不受限流影响；`/api` 与 `/badge` 都算。

限流按客户端 IP 分桶。`trust_proxy: false`（默认）时用 TCP 对端地址；
打开后取 `X-Forwarded-For` 的**末段**——这个头是链式的，代理把自己的对端地址追加到末尾，
首段永远是客户端自己填的，只取首段等于把「自称的 IP」当身份。

## CORS

默认 `Access-Control-Allow-Origin: *`。生产环境建议在 `config.yaml` 里把 `cors` 改成你的前端域名列表。

## OpenAPI

`GET /api/v1/openapi.json` 返回 OpenAPI 3.0 描述，可直接喂给任何语言的代码生成器。
