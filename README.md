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
| 变更巡检 | 监控域名备案变化，改动通过 Webhook 或 RSS 通知你 |
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
| POST | `/api/v1/watch/run` | 立即执行一轮巡检 |
| GET | `/api/v1/changes` | 变更事件流水 |
| GET | `/api/v1/changes.rss` | 变更事件 RSS |
| GET/POST | `/api/v1/admin/backup` | 在线生成一致性快照 |
| GET | `/badge/icp.svg` | SVG 徽章，可直接放进 README |
| GET | `/api/v1/openapi.json` | OpenAPI 3.0 描述 |
| GET | `/healthz` `/readyz` | 健康 / 就绪探针 |

完整参数与响应示例见 [docs/API.md](docs/API.md)，也可以在浏览器打开服务地址访问 `/docs`。

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
- 带上 `domain` 时，还会与该域名的实际备案号交叉核对，判断是否张冠李戴

## 变更巡检

把域名加入监控后，服务会按间隔回源比对，只有内容真的变了才通知：

```bash
curl -X POST localhost:8080/api/v1/watch \
  -H 'Content-Type: application/json' -d '{"domain":"example.com"}'
```

推送地址支持钉钉、企业微信、飞书以及任意 HTTP 端点：

```yaml
watch:
  interval: 86400          # 秒
  webhook: "https://oapi.dingtalk.com/robot/send?access_token=xxx"
```

不想配 Webhook 就用 RSS：`/api/v1/changes.rss`。

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
| `BEIAN2API_PROVIDERS` | `miit:1,apihz:.4` | 数据源与权重，0 即禁用 |
| `BEIAN2API_TTL` | `21600` | 缓存秒数 |
| `BEIAN2API_WEBHOOK` | 空 | 变更推送地址 |

### 反向代理

`deploy/` 里有现成的 `Caddyfile` 和 `nginx.conf`，都已处理好真实 IP 透传。

### 数据与备份

**所有数据都在一个文件里**：`data/beian2api.db`。缓存、历史快照、监控列表、变更记录全在里面。

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
- **apihz**：第三方公开接口，作为后备。默认用的是公共测试凭据，**生产环境建议换成自己的**。

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
