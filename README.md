<div align="center">

<img src="docs/logo.svg" width="96" height="96" alt="Beian2API">

# Beian2API

**一个二进制，一个数据文件，把 ICP 备案查询做成你自己的基础设施。**

多源竞速 · 本地快照 · 备案号校验 · 变更巡检

[文档](docs/API.md) · [部署](#部署) · [配置](#配置)

</div>

---

## 这是什么

一个可自托管的 ICP 备案查询 API。给它一个域名，它返回备案号、主办单位、单位性质、审核时间等结构化数据。

市面上"备案 API"大多只是把某家第三方接口换个包装：上游一挂就报错，上游改字段就崩，还要按次付费。这里换了个思路——**把上游当成不可靠的依赖来设计**。

| 能力 | 说明 |
| --- | --- |
| 多源竞速 | 多个数据源并行查询，谁先给出结果用谁的，失败者自动出局 |
| 本地快照 | 上游全挂时回退到最近一次成功的数据，业务不会因为别人的故障中断 |
| 备案号校验 | 不查上游，按编码规则逐段验真，能识别伪造的备案号 |
| 变更巡检 | 监控域名备案变化（含新增备案与备案注销），首页即可增删、看时间线与巡检节奏，改动通过 Webhook 或 RSS 通知你 |
| 单文件部署 | 内嵌前端与图标，`./beian2api` 就能跑，无外部依赖 |

后端 Go（标准库 + bbolt），前端零框架，全部资源编译进二进制。

## 三分钟上手

### 方式一：下载二进制

```bash
# 从 Release 下载对应平台的文件后
chmod +x beian2api_linux_amd64
./beian2api_linux_amd64 serve
```

打开 <http://localhost:8080> 就是查询台。

### 方式二：自己编译

```bash
git clone https://cnb.cool/asoe/TechSauce/Beian2Api.git
cd Beian2Api
make build        # 产物在 bin/beian2api
make test         # 跑一遍单元测试
```

需要 Go 1.23+，无 CGO。

### 方式三：Docker

```bash
docker run -d --name beian2api -p 127.0.0.1:8080:8080 \
  -v $(pwd)/data:/data \
  cnb.cool/asoe/techsauce/beian2api:latest
```

或者用 Compose（已含健康检查与日志轮转）：

```bash
cd deploy && docker compose up -d
```

### 方式四：systemd

```bash
make build
sudo ./scripts/install.sh          # 默认装到 /opt/beian2api
```

脚本会创建独立用户、安装 unit 文件、启动服务。

## 第一次查询

```bash
curl "http://localhost:8080/api/v1/query?domain=example.com"
```

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
    "cost_ms": 312.4
  }
}
```

## 接口

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| GET/POST | `/api/v1/query` | 单域名查询，`refresh=1` 强制回源 |
| GET/POST | `/api/v1/batch` | 批量查询，最多 50 个 |
| GET | `/api/v1/history` | 历史快照与逐次差异 |
| GET | `/api/v1/diff` | 最近两次快照对比 |
| GET | `/api/v1/verify` | 校验备案号是否成立，可带 `domain` 交叉核对 |
| POST | `/api/v1/watch` | 加入巡检列表 |
| DELETE | `/api/v1/watch` | 移出巡检列表 |
| GET | `/api/v1/watch/list` | 巡检列表 |
| GET | `/api/v1/watch/status` | 巡检运行概况（是否在跑 / 上次 / 下次），分页 |
| POST | `/api/v1/watch/run` | 立即执行一轮巡检 |
| GET | `/api/v1/changes` | 变更事件流水（含注销、上游异常、恢复），支持 `kind` 筛选与 `offset` 翻页 |
| GET | `/api/v1/changes.rss` | 变更事件 RSS |
| GET/POST | `/api/v1/admin/backup` | 在线生成一致性快照 |
| GET | `/badge/icp.svg` | SVG 徽章，可直接放进 README |
| GET | `/api/v1/openapi.json` | OpenAPI 3.0 描述 |
| GET | `/healthz` `/readyz` | 健康 / 就绪探针 |

完整参数与响应示例见 [docs/API.md](docs/API.md)，也可以在浏览器打开服务地址访问 `/docs`。

## 后台管理

打开 `/admin` 是一个真正的管理后台，不用登服务器、不用记 curl 参数：

| 页面 | 能做什么 |
| --- | --- |
| 概览 | 数据量、命中率、存储与保留策略、上游权重与凭据、巡检节奏、推送通道最近结果 |
| 缓存 | 按域名/备案号/主办单位搜索、逐条删除、清理过期、全清 |
| 历史快照 | 按域名聚合看到「谁占了多少配额」，单个域名一键清空 |
| 变更流水 | 按类型筛选、翻页、按条删除、清空 |
| 监控 | 批量粘贴导入（最多 500 个，自动归一化去重）、单个移出、立即巡检、清空 |
| 备份 | 列出 `backups/` 里的快照文件、立即生成一份、清空 |

写操作的门槛和在线备份一致：**配了 API Key 就必须带 Key，没配 Key 时只接受本机直连**。
页面本身是公开的（里面没有数据），方便你打开后看到「需要 API Key」的提示。

后台背后的接口在 `docs/API.md` 的「后台管理接口」一节，脚本里可以直接调。

## 备案号校验

这是本项目的差异化能力之一。不依赖任何上游，按备案号的编码结构验真：

```bash
curl "http://localhost:8080/api/v1/verify?code=京ICP备12345678号-1"
```

```json
{
  "data": {
    "valid": true,
    "normalized": "京ICP备12345678号-1",
    "province": "北京市",
    "region": "11",
    "kind": "非经营性备案",
    "serial": "12345678",
    "site_seq": "1"
  }
}
```

它会：
- 校验省份简称是否为合法省级行政区，并给出完整省名与行政区划码
- 区分「ICP备」「ICP证」「B1/B2」三类备案形态
- 自动纠正全角字符、多余空格、常见错字（如「泸」→「沪」）、数字段里的字母误写
- 带上 `domain` 时，还会与该域名的实际备案号交叉核对

交叉核对比的是**整条备案号**，包括 `-N` 站点序号。一个主体下可以有多个网站，
`-1` 和 `-7` 是同一家公司、不同站点；拿真实存在的 `-1` 改个尾数写成 `-7`
去冒充另一个域名，结构完全合法、主体也对得上——这正是伪造备案号最常见的样子。
所以 `cross_check.match` 要求站点序号也一致，另有一个 `same_subject` 字段
告诉你「是不是同一家公司」。界面上也分两种话说：

- `✗ 主体对得上，但不是同一个网站`
- `✗ 与该域名的实际备案号完全不一致`

备案号里根本没写站点序号时（`京ICP备12345678号`，这种写法本身合法），退回主体比对——
无法证明他填错，就不该武断判假。

## 变更巡检

把域名加入监控后，服务会按间隔回源比对，只有内容真的变了才通知：

```bash
curl -X POST localhost:8080/api/v1/watch \
  -H 'Content-Type: application/json' -d '{"domain":"example.com"}'
```

推送支持钉钉、企业微信、飞书、Slack 和任意 HTTP 端点，可以同时配多个：

```yaml
notify:
  retries: 3
  channels:
    - name: 运维群
      kind: dingtalk                      # 留空按 URL 自动识别
      url: "https://oapi.dingtalk.com/robot/send?access_token=xxx"
      secret: ""                          # 机器人开了加签就填这里
      events: [changed, revoked, registered, upstream_error, recovered]   # 省略即全推

    - name: 飞书
      url: "https://open.feishu.cn/open-apis/bot/v2/hook/xxx"
```

容器里一行也能配完：

```bash
BEIAN2API_NOTIFY_CHANNELS="dingtalk:https://oapi.dingtalk.com/robot/send?access_token=xxx"
```

每家 IM 的协议不一样（钉钉要 `markdown.text`、企业微信要 `markdown.content`、
飞书要 `interactive.card`），所以按通道类型分别成型，而不是塞一个「谁都能收」的扁平包。

两个容易被忽略的坑，这里都处理了：

- **HTTP 200 不代表送达**。钉钉和企业微信在 token 失效、关键词不匹配、被限流时照样返回 200，
  真正的错误藏在响应体的 `errcode` 里。只看状态码会把「一条都没发出去」记成成功。
  现在会解析响应体，并且区分「重试有用」（5xx / 限流）和「重试没用」（token 错、关键词不匹配）。
- **推送慢不该拖住巡检**。投递在后台跑，一轮巡检不会因为某个群机器人卡 10 秒而被拖住。

配完想确认通不通，点首页「发测试消息」，或直接打：

```bash
curl -X POST localhost:8080/api/v1/notify/test
```

每个通道最近一次推送成功与否会落盘并显示在首页，失败原因（上游 errcode/errmsg）直接摊开——
深夜定时巡检挂掉这件事，总得有人知道。

不想配推送就用 RSS：`/api/v1/changes.rss`。

会报的事件有五类：

- `changed` 备案信息变了
- `revoked` 之前有备案、现在查不到（多半被注销）
- `registered` 之前没备案、现在有了
- `upstream_error` 连续多轮查不到。这比变更更急——你根本不知道它现在是死是活。
  默认连续 3 轮才报（`watch.fail_threshold`），偶发抖动不会刷屏；报过就不再重复报。
- `recovered` 从故障里恢复。没有它，群里最后一条消息会永远停在「挂了」。

巡检间隔改了或服务重启了都不会重置节奏——首轮时机按「上次巡检时间 + 间隔」推算，
不会因为你重启服务就额外打一轮上游。

首页的监控面板会显示「上次 12 分钟前 · 下次 6 小时后」，也可以直接调 `/api/v1/watch/status`。
这个接口默认分页（每页 20 条），域名多的时候首页显示「展开更多（还有 N 个）」，
不会为了渲染十几行把上千个监控项全量拉一遍。
推送通道的最近状态在同一页的「推送通道」面板里，`/api/v1/notify/status` 是它的数据源。

一个域名有多条备案（主体下多个网站）时，历史快照、巡检比对、SVG 徽章、备案号交叉核对
统一取「站点序号最小」的那条作为代表记录。这件事必须只有一个答案：如果历史记 `-1`、
巡检比 `-3`，一次真实变更会被报成两条不同字段，而上游只是把列出顺序换一下，也会被误报成变更。

## 部署

### 配置

零配置文件也能跑。要定制就把 `config.example.yaml` 复制成 `config.yaml`：

```bash
cp config.example.yaml config.yaml
```

所有配置都能用环境变量覆盖，方便容器部署：

| 环境变量 | 默认 | 说明 |
| --- | --- | --- |
| `BEIAN2API_LISTEN` | `:8080` | 监听地址 |
| `BEIAN2API_DATA_DIR` | `./data` | 数据目录 |
| `BEIAN2API_API_KEYS` | 空 | 启用鉴权的 Key，逗号分隔 |
| `BEIAN2API_PROVIDERS` | `miit:1,apihz:.4` | 数据源与权重（权重高的先起跑，0 即禁用） |
| `BEIAN2API_TTL` | `21600` | 缓存秒数 |
| `BEIAN2API_WEBHOOK` | 空 | 变更推送地址（单通道，等价于一个 generic 通道） |
| `BEIAN2API_NOTIFY_CHANNELS` | 空 | 多通道推送，形如 `dingtalk:<url>,wecom:<url>` |
| `BEIAN2API_WATCH_INTERVAL` | `86400` | 巡检间隔，支持 `30m` / `6h` / 秒数 |
| `BEIAN2API_WATCH_ENABLED` | `true` | 是否启用自动巡检，接受 `1/true/yes/on` 与 `0/false/no/off` |
| `BEIAN2API_WATCH_FAIL_THRESHOLD` | `3` | 连续几轮查不到才推送「上游异常」 |
| `BEIAN2API_TRUST_PROXY` | `false` | 是否采信 `X-Forwarded-For` / `X-Real-IP` |
| `BEIAN2API_RETAIN_CHANGES` | `2000` | 变更流水保留条数，0 表示不裁剪 |
| `BEIAN2API_RETAIN_HISTORY` | `200` | 每个域名保留的历史快照条数，0 表示不裁剪 |
| `BEIAN2API_RETAIN_HISTORY_TOTAL` | `20000` | 历史快照的全局条数上限（跨所有域名） |
| `BEIAN2API_APIHZ_ID` / `BEIAN2API_APIHZ_KEY` | 空 | apihz 的自有凭据，不填则用公共测试凭据 |

### 开启鉴权

```yaml
auth:
  enabled: true
  keys: ["bk_用 keygen 生成"]
```

或一行环境变量：`BEIAN2API_API_KEYS=bk_xxx,bk_yyy`。

`enabled` 和 `keys` 必须**同时**成立，只写一半会直接拒绝启动：

- 只开 `enabled` 而 `keys` 为空 → 没有任何可校验的凭据，接口其实是全裸的；
- 只配 `keys` 而 `enabled: false` → 你以为在保护，其实是公开的。

这种「看着开了其实没开」比不配更危险，所以宁可不启动。

打开后网页端会在 401 时自动露出 Key 输入框，填一次存在你自己的浏览器里，
保存时会立刻验一次并回话（有效 / 无效 / 浏览器不允许保存）。
RSS 阅读器不方便带请求头，订阅地址里带 `?key=` 即可。
徽章 `/badge/icp.svg` 始终公开——README 里的 `<img>` 发不出自定义请求头。

### 反向代理

`deploy/` 里有现成的 `Caddyfile` 和 `nginx.conf`，都已处理好真实 IP 透传。

**但服务端默认不信任这些头**，跑在反代后面必须显式打开：

```yaml
trust_proxy: true
```

原因是 `X-Forwarded-For` 客户端能随便填。无条件采信它，限流的 key 就交给攻击者自己选——
每个请求换个头就换一桶令牌，限流形同虚设。默认关闭时，请求按 TCP 对端地址分桶。

打开之后也只认 **`X-Forwarded-For` 的末段**。这个头是链式的，代理把自己的对端地址
**追加**到末尾（nginx 的 `$proxy_add_x_forwarded_for` 就是 append），所以第一段永远是
客户端自己填的，末段才是最近一跳代理亲手写的。只取首段等于把「自称的 IP」当身份。

`X-Real-IP` 在 `trust_proxy` 打开时会被采信（nginx 的 `proxy_set_header X-Real-IP
$remote_addr` 是覆盖写，不是追加，所以它本来就只有一个值）。

采信之前会先校验它**是不是一个 IP**。这一点看着多余，但少了它前面那些都白做：
末段虽然取对了，内容仍然是客户端写的字符串，每个请求换一个乱码就是一桶新令牌，
而限流器按 key 建表的 map 会跟着无界增长。反代写进来的 `$remote_addr` 永远是 IP，
所以「不是 IP 就不认」不会误伤任何正常部署。`CF-Connecting-IP` 不再单独认——
除非你的 origin 只接受 Cloudflare 的 IP，那时该在 nginx 里 `set_real_ip_from` +
`real_ip_header CF-Connecting-IP`，让 `$remote_addr` 变成真实客户端。

后台接口在未配 API Key 时只认**真正的本机直连**：TCP 对端是回环，**且 Host 头
也是回环**。这一条完全无视转发头，包括开了 `trust_proxy` 之后——它要判断的是
「你是不是从本机连过来的」，而不是「你自称的 IP 是多少」，经过反代的请求本来
就不是本机直连。看 Host 是为了防 DNS 重绑定：攻击者把自己的域名解析到
`127.0.0.1`，网络层看起来完全合法，只有 Host 会露馅。

### 后台的跨站防护

`/admin` 背后是能删缓存、删历史、清监控的接口，所以除去鉴权之外还有一层
跨站判据，它与「有没有 Key」无关：

- 带 `Origin` 且不是本站、也不是 `cors` 里明确列出的域名 → 403 `cross_site_blocked`
- `Sec-Fetch-Site: cross-site` 或 `same-site` → 同上

这层不能省。`text/plain` 的 POST 不触发 CORS 预检，一个普通的 `<form>` 或
一句 `fetch` 就能往 `127.0.0.1:8080` 发 `{"only_expired":false}`，而 JSON
解析不看 `Content-Type`——清空缓存、清空监控都不需要攻击者拿到 Key。他读不到
响应，但也不需要读。`curl` 和脚本不带这些头，因此不受影响。

`cors: ["*"]` **不会**让后台跨域开放。通配符是给只读查询台的（别人家的前端、
README 徽章），后台要跨域请在 `cors` 里写明确域名。

### 数据与备份

**所有数据都在一个文件里**：`data/beian2api.db`。缓存、历史快照、监控列表、变更记录全在里面。

变更流水和历史快照是只增的数据，所以默认带滚动保留，设 0 关闭裁剪：

| 配置 | 默认 | 语义 |
| --- | --- | --- |
| `storage.retain_changes` | 2000 | 变更流水保留条数（全局） |
| `storage.retain_history` | 200 | 每个域名保留的历史快照条数 |
| `storage.retain_history_total` | 20000 | 历史快照的全局条数上限 |

三个上限都是必要的：只按域名算的话，`/api/v1/query` 是公开接口，随便刷一批随机域名、
每个域名一条快照，各自都没超限，合起来却能让数据文件无限长。全局上限在写入时按时间戳
裁最旧的，留 10% 余量（不是每写一条就全表扫一遍）。

缓存条目也会被清理：超出「回退窗口」（`cache.stale_max`，默认 7 天）的过期条目已经
没有任何代码路径会读到，服务每小时清一次。否则「查过的域名」这个集合只增不减。

数据文件的实际大小和全部保留策略都在 `/api/v1/stats` 的 `storage` 字段里，
首页「服务状态」也直接显示。

数据库用排他文件锁，所以**服务运行时外部进程打不开它**。为此提供了在线备份接口：

```bash
# 服务不停机，直接打快照（默认落到 data/backups/）
./beian2api backup -server http://127.0.0.1:8080

# 指定文件名
./beian2api backup -server http://127.0.0.1:8080 -o before-upgrade.db

# 或者 curl
curl -X POST "http://127.0.0.1:8080/api/v1/admin/backup"
```

服务停着的时候就直接本地备份：

```bash
./beian2api backup -o /backup/beian2api-$(date +%F).db
```

跨机器迁移用 JSON，换版本、换存储都干净：

```bash
./beian2api export -o dump.json    # 旧机器
./beian2api import -i dump.json    # 新机器
```

导的是**完整快照**：缓存、历史、监控列表、变更流水、推送通道状态、计数器全在里面，
在一个读事务里取，所以导出的是某一时刻的一致状态。重复导入同一份文件不会翻倍
（缓存/监控/推送状态是覆盖写，日志类靠内容 key 去重）。旧版本导出的文件也能导入，
只是那种文件里原本就没有历史与监控。

也可以直接 `rsync` 整个 `data/` 目录（停止服务后）。

> 备份接口的访问控制：配置了 API Key 时必须带 Key；没配 Key 时只允许本机回环访问，
> 避免把「能写文件」的能力暴露到公网。

### 自检

上线前后跑一次，把配置、目录、存储、上游逐项查一遍：

```bash
./beian2api doctor
```

## 关于上游

系统内置两个数据源适配器：

- **miit**：直连工信部备案查询接口，数据最权威。该接口有 WAF 保护，云服务器 IP 常被拦截，所以它的失败是常态而非异常。
- **apihz**：第三方公开接口，作为后备。默认用的是公共测试凭据，**生产环境建议换成自己的**：

  ```yaml
  providers:
    - name: apihz
      enabled: true
      weight: 40
      id: "你的 id"
      key: "你的 key"
  ```

  或设 `BEIAN2API_APIHZ_ID` / `BEIAN2API_APIHZ_KEY`。公共凭据被限流时不会报错，
  只表现为「偶尔查不到」——所以首页的数据源面板会给它标一个「公共凭据」，
  `doctor` 也会提示。

正因为上游不可靠，才需要多源竞速和本地快照兜底。你可以按自己的情况调整权重，或者写一个新适配器——实现两个方法就够了：

```go
type Provider interface {
    Name() string
    Lookup(ctx context.Context, domain string) ([]model.Record, error)
}
```

想先看效果不连外网，用内置的演示源：

```bash
BEIAN2API_PROVIDERS="mock:1" ./beian2api serve
```

## 性能

- 缓存命中走单文件 KV，命中路径 ~1ms 级别
- 相同域名的并发请求会被合并（singleflight），不会把上游打爆
- 批量查询内部限并发，避免触发上游频控
- 每 IP 令牌桶限流，惰性补充、无后台 goroutine
- 二进制约 8MB，常驻内存十几 MB

## 测试

```bash
make test     # 单元测试
make race     # 竞态检测（需要 CGO）
make lint     # gofmt + vet + test
```

测试覆盖域名归一化、备案号解析、多源竞速语义、缓存与负缓存、陈旧回退、巡检变更检测、鉴权限流、徽章 XSS 转义等关键路径。

## 许可

MIT
