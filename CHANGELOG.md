# 更新日志

格式参考 [Keep a Changelog](https://keepachangelog.com/zh-CN/1.1.0/)。

## [1.0.0] - 2026-11-14

首个可用版本。

### 新增

- 单域名 / 批量备案查询，支持强制回源
- 多数据源并行竞速，先成功者胜出，失败自动降级
- 本地快照缓存：上游全挂时回退最近一次成功数据，并明确标记 `stale`
- 负缓存：明确"未备案"的结论短暂缓存，减少无谓回源
- 同域名并发请求合并（singleflight），避免打爆上游
- 备案号结构校验：省份、类型、主体序号逐段验真，支持与域名交叉核对
- 备案号自动纠错：全角字符、空格、常见错字、数字段字母误写
- 历史快照与逐次差异对比
- 变更巡检：定时回源比对，支持 Webhook（钉钉/企业微信/飞书/任意 HTTP）与 RSS
- SVG 徽章，可直接嵌入 README
- 内置 Web 查询台与文档页，深浅色自适应，移动端适配
- 静态资源内嵌并带指纹版本，升级二进制后自动失效旧缓存
- 单文件存储（bbolt），备份 = 复制一个文件
- CLI：`serve` / `config` / `backup` / `export` / `import` / `keygen` / `doctor`
- 可选 API Key 鉴权、按 IP 令牌桶限流、CORS 白名单
- 健康 / 就绪探针、OpenAPI 3.0 描述
- 交叉编译脚本、Dockerfile、docker-compose、systemd unit、Nginx / Caddy 示例、一键安装脚本
- 离线演示数据源，无需外网即可体验完整链路
