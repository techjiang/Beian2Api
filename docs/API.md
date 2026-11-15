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

每个接口都限定了自己接受的方法，其余返回 `405 method_not_allowed` 并带 `Allow` 头。
只读接口只认 `GET`（`HEAD` 一并放行，方便探针与 CDN 预热）；
`/api/v1/watch` 认 `POST` 与 `DELETE`。不属于某个接口的方法会明确被拒，
不会返回 200 和一份数据——客户端与网关不该看到「删成功」而其实什么都没删。

`POST` 请求体上限 1 MB。超限返回 `400 bad_request`，`message` 会写明是超过上限
而不是 JSON 语法错——两者都是 400，但提示含混会让人反复检查一份没写错的 JSON。

请求体里**只能有一个 JSON 值**。`{"domain":"a.com"}{"domain":"b.com"}` 这种
拼接错误回 `400 bad_request`，不会静默只取第一个——那会让脚本以为自己提交了
两个，而实际只写进去一个（后台批量接口曾把后半句 `{"action":"remove"}` 吞掉，
按默认 `add` 执行完还回了 200）。

---

## GET /api/v1/query

查询单个域名的备案信息。也支持 POST + JSON body。

**参数**

| 名称 | 位置 | 必填 | 说明 |
| --- | --- | --- | --- |
| domain | query / body | 是 | 域名。可带协议、路径、端口、`www.`，服务端会归一化 |
| refresh | query / body | 否 | `1`/`true`/`yes`/`on` 时跳过缓存强制回源；其余取值返回 400 |

**`refresh` 写错会报错，不会静默当成 false。** 空串表示没传（取 false），
除此之外只认真/假两类写法（`1/0`、`true/false`、`yes/no`、`y/n`、`on/off`），
其余返回 `400 invalid_refresh`。之前 `refresh=abc` 被当成「不刷新」：调用方
以为自己在强制回源，实际拿到的是缓存里那份旧数据，而且没有任何迹象说明它
没生效。同一批接口里 `limit=abc`、`since=abc` 都会返回 400，
「刷新开关写错了」没道理是唯一的例外。

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

带 `limit` 取最近 N 条时，**返回的第一条也会带 `diff`**。它会多读一条只为做对照，
不返回给你。之前不是这样：按 `limit` 取完直接相邻相减，于是每页的头一条永远没有
`diff`——哪怕它前面确实还有一次变更，看起来都像「从来没变过」。分页取值不该改变
单条记录的含义。

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
是同一条备案，两者**共用同一份缓存、同一行监控**——域名入库前会统一消掉
`www.` 前缀（`model.CanonicalDomain`），所以结果里的 `domain` 字段是主域名写法，
同一个备案也只占一行监控、只推一条通知。

只剥 `www.`，不动 `api.` / `m.` 这类真正的子域名：它们可能是独立备案的站点，
贸然合并会把两个不同网站的数据混在一起，比不去重更糟。

适配器按域名精确比对上游结果时用的是「忽略 www.」的语义，否则查
`www.example.com` 会把唯一命中的记录丢掉，一个已备案的域名被判成「未备案」
——还带着 10 分钟的负缓存。

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

| 结果 | 含义 |
| --- | --- |
| `200 {"domain":"…","watching":false,"removed":true}` | 真的移出了 |
| `404 not_watching` | 它本来就不在列表里，什么都没发生 |
| `400 invalid_domain` | 域名格式不对（`?domain=<script>` 这种不可能被加入的值） |

不存在的域名不返回 200。移出是「让某个域名不再被巡检」的动作——它本来就不在，
那就什么都没发生，回一句 `watching:false` 只会让人以为已经处理好了。
格式校验同理：一个永远不会在列表里的值，不该拿到「成功」的形状。

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

`enabled` / `interval` 与 `/api/v1/watch/status` **必须说同一个数**。巡检关掉
（`watch.enabled: false`）时两边都是 `enabled: false` + `interval: 0`；
这里曾经回配置里那个 `86400`，于是从 `list` 读到的结论是「每 24 小时巡检一轮」，
实际上一次都不会跑。同一个问题在两个接口上有两种答案，先被读到的那个就成了
新的真相。补 `enabled` 是因为光把 `interval` 改成 0 而不说原因，读的人会以为
「间隔没配」。

```json
{
  "ok": true,
  "data": {
    "total": 1,
    "enabled": true,
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
| cursor | 翻页游标：把上一页响应里的 `next_cursor` 原样传回来即可。服务端直接 Seek 到那个位置，**翻到第几页都是同样的代价**。形状不对（不是「20 位时间戳-域名」）、含控制字符、超过 300 字节都返回 400（`invalid_cursor`） |
| offset | 跳过前 N 条。仍然可用（向后兼容），但它是「走 N 步再开始收」——翻到第 k 页要在游标上空走 N 步并把它们逐条反序列化，翻得越深越慢。与 `cursor` 同时给时以 `cursor` 为准 |
| kind | 只看某一类事件，取值同上五个。写错返回 400（`invalid_kind`）并列出合法值——静默当成「不过滤」会让「只看注销」的页面显示成全部 |

响应里除 `total`（本页条数）和 `items`，还有 `offset`、`limit`、`has_more`、
`next_cursor`。`has_more` 是「后面还有没有」的准确答案，客户端不用自己拿
`total` 去猜：

```json
{ "ok": true, "data": { "total": 2, "items": [], "offset": 0, "limit": 50,
  "has_more": true, "next_cursor": "000001735689601-example.com" } }
```

**游标的形状也会校验，因为垃圾游标会静默翻回第一页。** 乱码在字典序上排在
所有 key 之后，Seek 到末尾再 Prev 一步，落点正是最新那一条——于是
`?cursor=abc` 和不带 `cursor` 返回一模一样的内容，调用方拿着一个已经损坏的
游标继续翻，会以为自己在往后走，其实在原地绕圈。「游标指向的记录被保留上限
裁掉了」是正常情况（会从头开始给），但那和「格式错了」完全是两件事。

`next_cursor` 是本页**最后一条已返回记录**的 key。`has_more` 为假时它是空串——
给一个指向最后一条的游标会让客户端再请求一次空页。翻页就照这个写：

```bash
# 第一页
curl "http://127.0.0.1:8080/api/v1/changes?limit=50"
# 之后把上一步的 next_cursor 原样带回来
curl "http://127.0.0.1:8080/api/v1/changes?limit=50&cursor=000001735689601-example.com"
```

游标比库里所有记录都新时（比如那几条已经被保留上限裁掉了），按「从最新
一条开始」返回，而不是回空页——空页会被读成「流水空了」，而库里明明还有
几千条。反过来，游标比最旧的那条还旧时返回空页，这是对的：绝不能回绕到
最新一条，那会让调用方在同一份数据上无限翻下去。

## GET /api/v1/changes.rss

同样的变更事件，输出 RSS 2.0，可直接丢进 RSS 阅读器或 IM 订阅机器人。

| 参数 | 说明 |
| --- | --- |
| kind | 只看某一类事件，取值与 `/api/v1/changes` 相同（`changed` / `revoked` / `registered` / `upstream_error` / `recovered`）。写错返回 `400 invalid_kind` |
| key | 开了鉴权时传 API Key（阅读器带不了请求头） |

**`kind` 在两个接口上是同一个词，就得是同一个语义。** 它曾经只加在
`/api/v1/changes` 上：RSS 侧完全忽略这个参数，`?kind=revoked` 返回 200 和整条
流水，一条都没筛；取值写错也不报错。订阅「只看注销」的人以为自己盯上了注销，
实际收到的是全部事件——而 RSS 的消费方不会核对内容，它只会照单全收。

`lastBuildDate` 取最后一条事件的时间，不是当前时刻——每次请求都刷新成 now，
等于告诉阅读器「内容刚变过」，一个永远不会变的 feed 也会被反复拉取。
没有任何事件时该字段直接省略（RFC 822 里它是可选项），缺失的正确含义就是
「没有内容更新过」，空 feed 因此是逐字节稳定的。

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

没有可用通道时返回 `enabled=false`，不会 404。这时要多看两个字段：

| 字段 | 说明 |
| --- | --- |
| `configured` | 配置文件里一共写了几个通道（含停用的） |
| `disabled` | 其中被停用的通道名（`disabled: true` 或 `url` 为空） |

```json
{ "enabled": false, "total": 0, "channels": [],
  "configured": 2, "disabled": ["运维群", "值班群"] }
```

**「一个都没配」和「配了但全被关掉」不是一件事。** 之前两种情况都只回
`enabled=false` + 空列表，页面上写着「还没有配置推送通道」——而 config.yaml
里明明有三个通道，只是都设了 `disabled: true`。人只会照着那句提示再去加
第四个，加完还是不生效，因为真正的原因是他自己关掉的。

`POST /api/v1/notify/test` 在无可用通道时返回 `503 notify_disabled`，
`message` 会点明是「一个都没配」还是「写了 N 个但全部停用」。

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
| label | 左侧标签文字，默认 `ICP`；超过 24 个字符按字符截断并加省略号 |

```markdown
![备案](http://localhost:8080/badge/icp.svg?domain=example.com)
```

已备案为绿色并显示备案号，未备案为橙色，查询出错为红色。

`label` 有长度上限：这是公开接口，`label` 由 URL 决定，不封顶时一个 5000 字符的
标签能生成三万像素宽、20KB 的 SVG——一次请求就能让别人家的 README 横向滚到天边，
而图还是从这台服务器吐出去的。徽章本来只放得下一两个词。

**成功态**带 10 分钟缓存头（徽章嵌在每个项目主页里，不缓存等于每次回源）；**出错态
是 `Cache-Control: no-store`**。上游抖一下就把红色的「error」图缓存十分钟，
而且缓存的不是本地浏览器，是别人 CDN 上的——上游早就恢复了，所有嵌着它的
README 还在红色报错，清都清不掉。徽章是给别人看的第一眼，它必须比后端
更早知道「现在已经好了」。

---

## GET / POST /api/v1/admin/backup

在服务运行中生成一致性数据快照到 `data/backups/`。

数据库用 bbolt 的排他文件锁，外部进程在服务运行时无法打开，因此备份必须由服务自身完成。

| 参数 | 说明 |
| --- | --- |
| name | 文件名，只能是纯文件名且必须以 `.db` 结尾。默认 `beian2api-<日期-时间.毫秒>.db`；同毫秒撞名时自动加序号，**不会覆盖**已有快照 |

`name` 必须以 `.db` 结尾：备份列表与「清空备份」都只认这个后缀（它们要保护
目录里用户自己放的东西）。一个不带后缀的快照会写进磁盘、后台看不见、删不掉、
也不会被清理——纯占地方，而且没人知道它在。

| 方法 | 行为 |
| --- | --- |
| `POST` | 真的生成一份快照文件 |
| `GET` | 同 `POST`（生成快照并返回路径） |
| `HEAD` | **405**。它不是只读接口，探一下就落一个文件；而 HEAD 正是探针、健康检查、`curl -I` 最常用的方法 |

```bash
curl -X POST "http://127.0.0.1:8080/api/v1/admin/backup"
curl -X POST "http://127.0.0.1:8080/api/v1/admin/backup?name=before-upgrade.db"
```

显式指定的 `name` 已经存在时返回 409（`backup_exists`），而不是默默覆盖。
「每天一份固定名字、覆盖昨天」是个合理用法，但它必须是**说出来的**决定：
两次不同的备份变成一份，中间那次的内容就永久没了。要覆盖就先删再备，
那是两步、看得见。

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

## 后台管理接口

浏览器打开 `/admin` 就是后台页面。以下接口是它背后的数据源，也可以直接用
curl / 脚本调用。**它们都能改数据**，所以访问控制与只读接口不同：

- 配置了 API Key：必须带 Key（`X-API-Key` 头或 `?key=`）；
- 未配置 Key：只接受真正的本机直连——**TCP 回环 + Host 头也是回环**，
  其余来源一律 403 `forbidden`。看 Host 是为了防 DNS 重绑定：那种攻击下
  网络层看起来完全合法，只有 Host 会暴露出请求其实来自攻击者的域名。

在此之上还有一层**跨站防护**，与有没有 Key 无关：

- 带 `Origin` 且不是本站 / 不是 `cors` 里明确列出的域名 → 403 `cross_site_blocked`；
- `Sec-Fetch-Site: cross-site` 或 `same-site` → 同上。

这层不能省。`text/plain` 的 POST 不触发 CORS 预检，任何网页（甚至一个
`<form>`）都能往 `127.0.0.1:8080` 提交一句 `{"only_expired":false}`，而
JSON 解析不看 `Content-Type`——清空缓存、清空监控都不需要攻击者拿到 Key。
`curl` 和脚本不带这些头，因此不受影响。

**这层防护覆盖的不只是 `/api/v1/admin/*`。** 判据是「会不会改变实例状态」，
所以下面这几个公开接口同样受保护：

| 接口 | 为什么算状态变更 |
| --- | --- |
| `POST /api/v1/watch` | 把任意域名塞进监控清单，等于往「这台机器在盯什么」里写入 |
| `DELETE /api/v1/watch` | 把别人的监控悄悄移出，巡检就此停掉——**没有任何人会收到通知** |
| `POST /api/v1/watch/run` | 每调一次就是一次全量回源，等于拿别人的机器打上游 |
| `POST /api/v1/notify/test` | 往所有推送通道发消息，等于替你往群里发言 |

只读的备案查询（`query` / `batch` / `history` / `verify` / 徽章）不受这层
限制——它们本来就是公开信息，跨站调用是设计目标。

### 通配 CORS 只覆盖公开查询

`cors: ["*"]`（默认值）**只**让公开查询类接口可被任意站点读取。
下面这些回答的是「这台机器自己的状态」，通配符一律不生效，要跨域就必须在
`cors` 里把域名写清楚：

`/api/v1/stats`、`/api/v1/providers`、`/api/v1/watch/list`、
`/api/v1/watch/status`、`/api/v1/changes`、`/api/v1/changes.rss`、
`/api/v1/notify/status`、`/api/v1/admin/*`

理由很直接：查一个域名的备案号是公开信息，而「你在监控哪些域名、什么变了、
数据文件在哪」是一份情报。默认配置下攻击者只要让受害者打开一个页面，
就能把整份监控清单读走——这不该是 `cors` 默认值的副作用。

`/admin` 页面本身是公开的静态页面（里面没有任何数据），方便打开后看到
「需要 API Key」的提示；被守住的是下面每一个接口。

### GET /api/v1/admin/overview

概览页一次取全：数据量、命中率、**最近 24 小时趋势**、存储策略、上游、巡检节奏、
推送状态、备份列表。

```bash
curl "http://127.0.0.1:8080/api/v1/admin/overview"
```

```json
{
  "ok": true,
  "data": {
    "version": "1.10.0",
    "uptime_sec": 3612,
    "counts": { "cache": 1240, "history": 3871, "watch": 22, "changes": 96, "notify": 3, "keys": 0 },
    "requests": { "cache_hits": 18420, "cache_misses": 1203, "stale_fallbacks": 4, "hit_rate": 93.9 },
    "storage": {
      "size_bytes": 4718592, "path": "./data/beian2api.db",
      "retain_changes": 2000, "retain_history": 200,
      "retain_history_total": 20000, "stale_window": 604800, "history_total": 3871
    },
    "providers": [{ "name": "miit", "enabled": true, "weight": 100, "timeout": 8, "custom_credential": false }],
    "watch": { "enabled": true, "interval": 86400, "total": 22, "running": false,
               "last_run_at": 1735689600, "last_count": 1, "next_run_at": 1735776000 },
    "notify": [{ "name": "ops", "kind": "dingtalk", "last_at": 1735689500, "last_ok": true }],
    "backups": [{ "name": "beian2api-20241114-120000.db", "size": 4718592, "created_at": 1735689600 }],
    "auth": { "enabled": false },
    "series": [
      { "hour": 1735682400, "hits": 128, "misses": 12, "stale": 0, "changes": 0, "cache": 1240 },
      { "hour": 1735686000, "hits": 96,  "misses": 9,  "stale": 1, "changes": 2, "cache": 1244 }
    ]
  }
}
```

**`series` 是按小时聚合的最近 24 小时**，缺失的小时补零（不跳过），所以
`series` 的长度恒为 24、相邻两点恒差一小时——前端直接按下标铺时间轴，
不需要自己对时间。字段含义：

| 字段 | 说明 |
| --- | --- |
| `hits` / `misses` / `stale` | 这一小时内的累加值 |
| `changes` | 这一小时内产生的变更事件数 |
| `cache` | 这一小时的**最后观测值**（缓存条目数），不是累加 |

`hits + misses == 0` 的小时代表「没有请求」，不是「命中率为 0」——画图时
这两者必须区分开，否则一次停机会被读成命中率归零。前端用
`hits / (hits + misses)` 自己算，为 0 分母时留空断线。

序列保留 7 天（168 个键，约 20KB），滚动覆盖，不需要配置。它跟计数器
共用同一次落盘（15 秒一拍），不会额外多一次 fsync——丢 15 秒的曲线数据
没人看得出来，而每次请求都同步写一遍才是真的伤性能。

### 缓存

```bash
# 列表：limit 默认 30（上限 200），q 在域名/备案号/主办单位上做子串匹配
curl "http://127.0.0.1:8080/api/v1/admin/cache?q=example&limit=30&offset=0"

# 删一个域名（会做 canonical 归一化，用 www 版本删也能删到主域名那条）
curl -X DELETE "http://127.0.0.1:8080/api/v1/admin/cache?domain=example.com"
```

返回体里的 `deleted` 是**真的删到了没有**，不是「有没有出错」：

```json
{ "ok": true, "data": { "domain": "example.com", "deleted": true } }
```

删一个从没缓存过的域名会得到 `"deleted": false`。这一条不能省：
bbolt 的 Delete 对不存在的 key 返回 nil，照旧回 `true` 的话，后台会弹
「已删除 nope.com」——而那个域名从来没被缓存过，用户只会以为自己记错了域名，
或者怀疑删除按钮根本没生效。

```bash

# 清理：only_expired=true 只删「已过期且超出回退窗口」的，false 全清
curl -X POST "http://127.0.0.1:8080/api/v1/admin/cache/clear" \
  -H 'Content-Type: application/json' -d '{"only_expired":true}'
```

**空 body 等价于 `only_expired:true`**：不带参数的请求不该造成不可逆后果。
全清会连回退快照一起删掉，上游一挂就没有兜底数据了。

### 历史快照

按域名聚合，一个域名几百条快照只占一行。

```bash
curl "http://127.0.0.1:8080/api/v1/admin/history?limit=30&offset=0"
curl -X DELETE "http://127.0.0.1:8080/api/v1/admin/history?domain=example.com"
```

```json
{
  "ok": true,
  "data": {
    "total": 1, "offset": 0, "limit": 30, "has_more": false,
    "items": [{
      "domain": "example.com", "count": 42, "oldest_at": 1730000000,
      "last_at": 1735689600, "last_icp": "京ICP备12345678号-1",
      "last_unit": "示例科技有限公司", "kept_limit": 200
    }]
  }
}
```

`kept_limit` 是单域名保留上限，用来算「这个域名快把配额用满了没有」。

### 变更流水

```bash
# 按条删除，at 是事件时间戳（`at` 与 `domain` 两个参数都必需）
curl -X DELETE "http://127.0.0.1:8080/api/v1/admin/changes?at=1735689600&domain=example.com"

# 清空
curl -X POST "http://127.0.0.1:8080/api/v1/admin/changes/clear"
```

按条删除用的是和巡检相同的 key 规则（`%020d-域名`），所以 `domain`
同样会做 canonical 归一化。响应里的 `deleted` 是**真的删掉了吗**：
目标记录不存在时是 `false`（bbolt 的 Delete 对不存在的 key 返回 nil，
把它当成功会让人以为按钮生效了、或者以为记录本来就在别处）。

### 监控

```bash
# 批量加/移，单次最多 500 个；自动归一化与去重
curl -X POST "http://127.0.0.1:8080/api/v1/admin/watch/bulk" \
  -H 'Content-Type: application/json' \
  -d '{"domains":["example.com","https://www.example.org/x"],"action":"add"}'
```

```json
{
  "ok": true,
  "data": {
    "action": "add", "added": 2, "failed": 1, "duplicated": 1,
    "ok": ["example.com", "example.org"], "invalid": ["bad domain!"]
  }
}
```

`action=remove` 时多一个 `missing` 字段，列出「本来就不在监控列表里」的域名：

```json
{ "action": "remove", "added": 1, "failed": 0, "duplicated": 0,
  "ok": ["real.com"], "invalid": null, "missing": ["ghost1.com", "ghost2.com"] }
```

它们不算 `ok`。移出是「让某个域名不再被巡检」的动作，没在列表里就等于没做成——
把 `removed` 说成成功，界面上会弹一句「移出 2 个」而列表一点没变，用户只能
怀疑自己看错了，或者怀疑按钮坏了。

清单里**一个合法域名都没有**时返回 400（`invalid_domain`），而不是
200 + `ok: null`——后者会让脚本以为这批域名已经加进去了。域名逐个处理
是并发的（并发度 5），响应写超时按域名数放宽，所以一次贴 500 个不会在
30 秒处被掐断；`action=add` 时「全部处理失败」（比如上游全挂）回 502
（`bulk_failed`），不假装成功。

`action=remove` 时不会出现 502：移出不访问上游，它只删本地记录。
「全部域名都不在列表里」不是失败，是 `added:0` + 完整的 `missing` 列表——
让用户去「检查上游连通性」是给了个错的排查方向，而正确答案就在手上。

批量加入的域名会走与单个加入相同的路径——**先抓一次建立基线**，
否则第一轮巡检会把「全部字段」当成一次变更推出去。

```bash
# 清空监控列表（不删缓存和历史）
curl -X POST "http://127.0.0.1:8080/api/v1/admin/watch/purge"
```

### 备份文件

```bash
curl "http://127.0.0.1:8080/api/v1/admin/backups"

# 删一个（只删这个文件）
curl -X DELETE "http://127.0.0.1:8080/api/v1/admin/backups?name=beian2api-20241114-120000.db"

# 清空（不带 name 才是清空）
curl -X DELETE "http://127.0.0.1:8080/api/v1/admin/backups"
```

`name` 必须是 `backups/` 下的纯文件名且以 `.db` 结尾，带路径（`../x.db`、
`sub/x.db`）或其它后缀一律 `400 invalid_name`。

带 `name` 时响应 `{"name":"...","removed":1,"deleted":true}`；同名文件不存在时
回 `removed:0` / `deleted:false`，而不是假装删掉了。

**不带 `name` 才是清空**，且只删 `.db`，同目录里你自己放的东西不会被误伤。

> 这两条语义得说清楚，因为之前的实现里 `name` 根本没被读取：路径、这份文档、
> 后台按钮上都写着「删这一个」，实际执行的却是「清空整个备份目录」。备份是
> 出事时最后一份数据，一次手滑全没了，而响应只回一句 `removed: 3`。

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

三个上限都是 **`0` 表示不裁剪**，包括 `retain_history_total`。这一条值得单独写出来：
它原来有一句「等于 0 就填默认值 20000」，而默认值在加载时就填好了，走到
那一步还是 0 只可能是因为有人明确写了 `0`——于是「不裁剪」这个合法取值
永远配不出来，配置上一个字都没错，历史却被悄悄裁到 2 万条。
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

凭据写错时 apihz 回的是 `code=400 id参数异常` / `通讯秘钥不正确！` 这类**同一个
400 状态码**，并不代表「没有备案」。系统按 `msg` 区分：只有明确说出「没有备案」
的才当成 `found: false`，其余按上游错误处理（查询回 502，巡检记故障而不是
推一条注销告警）。把「凭据错」当成「没备案」会让监控里所有域名同时被判成注销。

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
