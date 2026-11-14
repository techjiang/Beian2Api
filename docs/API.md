# Beian2API 接口文档

Base URL：`http://你的域名:8080`

所有响应统一为以下外壳，成功时 `ok=true` 且带 `data`，失败时 `ok=false` 且带 `error`：

```json
{ "ok": true, "data": { } }
{ "ok": false, "error": { "code": "invalid_domain", "message": "域名格式不正确，示例：example.com" } }
```

HTTP 状态码同时反映结果：`200` 成功，`400` 参数错误，`401` 缺少 API Key，`405` 方法不允许，`429` 触发限流，`502` 上游不可用且无本地快照。

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

同一天内同一域名只保留最新一条，避免快照无限膨胀。

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
| domain | 否 | 传入后会与该域名的实际备案号比对 |

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

```json
{
  "ok": true,
  "data": {
    "enabled": true,
    "interval": 86400,
    "total": 1,
    "running": false,
    "last_run_at": 1735776000,
    "last_count": 1,
    "next_run_at": 1735862400,
    "items": [{ "...": "同 watch/list 的 items" }]
  }
}
```

巡检未启用时该接口依然可用，返回 `enabled=false`，不会 404。

## GET /api/v1/watch/list

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
      "changes": 1
    }]
  }
}
```

## POST /api/v1/watch/run

立即执行一轮巡检，返回本次发现的变更。适合配合外部定时任务使用。

巡检串行执行，同一时刻只有一轮在跑；客户端断开连接不会中断正在进行的巡检。
服务端固定给这一轮 2 分钟执行窗口。

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

| 参数 | 说明 |
| --- | --- |
| limit | 返回条数，默认 50，上限 200 |
| since | Unix 秒，只返回该时间之后的变更 |

## GET /api/v1/changes.rss

同样的变更事件，输出 RSS 2.0，可直接丢进 RSS 阅读器或 IM 订阅机器人。

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
      "events": ["changed", "revoked", "registered"],
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
    "data_file": "/opt/beian2api/data/beian2api.db"
  }
}
```

## GET /api/v1/providers

各数据源的启用状态、权重与超时配置。

## GET /healthz / GET /readyz

健康探针。`healthz` 始终快速返回进程存活，`readyz` 会检查存储是否可读。

---

## 鉴权与限流

配置了 `auth.keys` 或环境变量 `BEIAN2API_API_KEYS` 后，所有 `/api` 接口都需要 Key：

```bash
curl -H "X-API-Key: bk_xxx" "http://localhost:8080/api/v1/query?domain=example.com"
curl -H "Authorization: Bearer bk_xxx" "..."
curl "...&key=bk_xxx"
```

生成 Key：

```bash
./beian2api keygen -n 3
```

限流按客户端 IP 做令牌桶，默认 20 rps / burst 60。被限时返回 429 与 `Retry-After`。健康检查、静态资源、徽章与 RSS 不受限流影响。

## CORS

默认 `Access-Control-Allow-Origin: *`。生产环境建议在 `config.yaml` 里把 `cors` 改成你的前端域名列表。

## OpenAPI

`GET /api/v1/openapi.json` 返回 OpenAPI 3.0 描述，可直接喂给任何语言的代码生成器。
