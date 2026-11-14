package api

import "net/http"

// handleOpenAPI 输出 OpenAPI 3.0 描述，方便任何语言生成客户端。
//
// 这里必须覆盖**全部** /api 路由。之前只写了 8 条（query / batch / history /
// diff / watch / badge / healthz），而实际有 18 个接口：verify、changes、
// changes.rss、notify/*、watch/status、watch/run、watch/list、stats、
// providers、admin/backup 全缺。按这份描述生成的客户端会少一半功能，
// 而这正是它存在的唯一理由——「任何语言都能照着生成」。
// 路由表和这里的一致性由 TestOpenAPICoversEveryRoute 盯着。
func (s *Server) handleOpenAPI(w http.ResponseWriter, r *http.Request) {
	base := s.cfg.BaseURL
	if base == "" {
		scheme := "http"
		if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
			scheme = "https"
		}
		base = scheme + "://" + r.Host
	}
	str := func(desc string) map[string]any {
		return map[string]any{"type": "string", "description": desc}
	}
	boolean := func(desc string) map[string]any {
		return map[string]any{"type": "boolean", "description": desc}
	}
	integer := func(desc string) map[string]any {
		return map[string]any{"type": "integer", "description": desc}
	}
	q := func(name string, schema map[string]any, required bool) map[string]any {
		return map[string]any{"name": name, "in": "query", "required": required, "schema": schema}
	}
	okResp := map[string]any{"200": map[string]any{"description": "成功，结果在 data 里"}}
	errResp := map[string]any{
		"400": map[string]any{"description": "参数错误（ok=false，error.code 说明原因）"},
		"429": map[string]any{"description": "触发限流"},
	}
	withErr := func(extra ...map[string]any) map[string]any {
		out := map[string]any{}
		for k, v := range okResp {
			out[k] = v
		}
		for k, v := range errResp {
			out[k] = v
		}
		for _, e := range extra {
			for k, v := range e {
				out[k] = v
			}
		}
		return out
	}
	domainParam := q("domain", str("域名，可带协议、路径、端口，服务端会归一化"), true)

	doc := map[string]any{
		"openapi": "3.0.3",
		"info": map[string]any{
			"title":       "Beian2API",
			"description": "轻量、高性能、可自托管的 ICP 备案查询 API。多源竞速 + 本地快照 + 变更巡检。所有响应统一为 {ok,data} 或 {ok,error}。",
			"version":     s.version,
		},
		"servers": []map[string]string{{"url": base}},
		"components": map[string]any{
			"securitySchemes": map[string]any{
				"ApiKeyQuery":  map[string]any{"type": "apiKey", "in": "query", "name": "key"},
				"ApiKeyHeader": map[string]any{"type": "apiKey", "in": "header", "name": "X-API-Key"},
			},
		},
		"paths": map[string]any{
			"/api/v1/query": map[string]any{
				"get": map[string]any{
					"summary": "查询单个域名备案信息",
					"tags":    []string{"查询"},
					"parameters": []map[string]any{
						domainParam,
						q("refresh", boolean("为真时跳过缓存强制回源"), false),
					},
					"responses": withErr(map[string]any{"502": map[string]any{"description": "上游全部不可用且无本地快照"}}),
				},
				"post": map[string]any{
					"summary": "查询单个域名备案信息（JSON body）",
					"tags":    []string{"查询"},
					"requestBody": map[string]any{"required": true, "content": map[string]any{"application/json": map[string]any{
						"schema": map[string]any{"type": "object", "required": []string{"domain"}, "properties": map[string]any{
							"domain":  str("域名"),
							"refresh": boolean("强制回源"),
						}},
					}}},
					"responses": okResp,
				},
			},
			"/api/v1/batch": map[string]any{
				"get": map[string]any{
					"summary":    "批量查询（逗号分隔，最多 50 个）",
					"tags":       []string{"查询"},
					"parameters": []map[string]any{q("domains", str("逗号分隔的域名列表"), true), q("refresh", boolean("强制回源"), false)},
					"responses":  withErr(),
				},
				"post": map[string]any{
					"summary": "批量查询（JSON 数组，最多 50 个）",
					"tags":    []string{"查询"},
					"requestBody": map[string]any{"required": true, "content": map[string]any{"application/json": map[string]any{
						"schema": map[string]any{"type": "object", "properties": map[string]any{
							"domains": map[string]any{"type": "array", "items": map[string]string{"type": "string"}},
							"refresh": boolean("强制回源"),
						}},
					}}},
					"responses": withErr(),
				},
			},
			"/api/v1/history": map[string]any{"get": map[string]any{
				"summary":    "备案历史快照与逐次差异",
				"tags":       []string{"查询"},
				"parameters": []map[string]any{domainParam, q("limit", integer("最多返回几条，最大 500"), false)},
				"responses":  withErr(),
			}},
			"/api/v1/diff": map[string]any{"get": map[string]any{
				"summary":    "最近两次快照对比",
				"tags":       []string{"查询"},
				"parameters": []map[string]any{domainParam},
				"responses":  withErr(),
			}},
			"/api/v1/verify": map[string]any{"get": map[string]any{
				"summary": "校验备案号是否成立，可带域名交叉核对",
				"description": "不查上游，按编码规则逐段验真：省份简称、备案类型、主体序号、站点序号。" +
					"带 domain 时与真实备案号比对，match 要求站点序号也一致，same_subject 只比主体。",
				"tags": []string{"校验"},
				"parameters": []map[string]any{
					q("code", str("备案号，例如 京ICP备12345678号-1"), true),
					q("domain", str("可选，用来交叉核对"), false),
				},
				"responses": withErr(),
			}},
			"/api/v1/watch": map[string]any{
				"post": map[string]any{
					"summary": "加入监控",
					"tags":    []string{"巡检"},
					"requestBody": map[string]any{"required": true, "content": map[string]any{"application/json": map[string]any{
						"schema": map[string]any{"type": "object", "required": []string{"domain"}, "properties": map[string]any{"domain": str("域名")}},
					}}},
					"responses": withErr(),
				},
				"delete": map[string]any{
					"summary":    "移出监控",
					"tags":       []string{"巡检"},
					"parameters": []map[string]any{domainParam},
					"responses":  withErr(),
				},
			},
			"/api/v1/watch/list": map[string]any{"get": map[string]any{
				"summary":    "监控域名列表",
				"tags":       []string{"巡检"},
				"parameters": []map[string]any{q("limit", integer("每页条数，最大 5000"), false), q("offset", integer("跳过条数"), false)},
				"responses":  withErr(),
			}},
			"/api/v1/watch/status": map[string]any{"get": map[string]any{
				"summary": "巡检概况：是否在跑、上次/下次何时、监控项分页",
				"description": "巡检关闭（watch.enabled=false）时仍如实返回监控列表，只是 enabled=false。" +
					"首页每 60 秒轮询这个接口，因此默认只给第一页。",
				"tags":       []string{"巡检"},
				"parameters": []map[string]any{q("limit", integer("每页条数，默认 20，最大 200"), false), q("offset", integer("跳过条数"), false)},
				"responses":  withErr(),
			}},
			"/api/v1/watch/run": map[string]any{"post": map[string]any{
				"summary": "立即执行一轮巡检",
				"description": "巡检与请求生命周期脱钩：关掉页面不会让一轮巡检停在半路。" +
					"changes 在没有变更时是空数组而不是 null。",
				"tags":      []string{"巡检"},
				"responses": withErr(map[string]any{"503": map[string]any{"description": "巡检未启用"}}),
			}},
			"/api/v1/changes": map[string]any{"get": map[string]any{
				"summary": "变更事件流水（新的在前）",
				"description": "since 是增量游标：早于游标的事件不会被返回，遍历同时早停。" +
					"offset 是翻页游标，kind 用来只看某一类事件。",
				"tags": []string{"巡检"},
				"parameters": []map[string]any{
					q("limit", integer("每页条数，默认 50，最大 200"), false),
					q("offset", integer("跳过条数"), false),
					q("since", integer("Unix 秒，只取此后的记录"), false),
					q("kind", str("事件类型：changed / revoked / registered / upstream_error / recovered"), false),
				},
				"responses": withErr(),
			}},
			"/api/v1/changes.rss": map[string]any{"get": map[string]any{
				"summary":     "变更事件 RSS",
				"description": "列出的是「你在监控哪些域名」，所以同样需要鉴权；RSS 阅读器带不了请求头，用 ?key= 传。",
				"tags":        []string{"巡检"},
				"responses":   withErr(map[string]any{"401": map[string]any{"description": "缺少或无效的 API Key"}}),
			}},
			"/api/v1/notify/status": map[string]any{"get": map[string]any{
				"summary":     "各推送通道最近一次投递是否成功",
				"description": "不回显 URL 与密钥：机器人地址本身就是凭据。",
				"tags":        []string{"推送"},
				"responses":   withErr(),
			}},
			"/api/v1/notify/test": map[string]any{"post": map[string]any{
				"summary":     "立刻往全部通道发一条测试消息，返回每个通道的真实结果",
				"description": "钉钉/企业微信在 token 失效、关键词不匹配时也返回 HTTP 200，所以这里解析响应体给出真实成败。",
				"tags":        []string{"推送"},
				"responses":   withErr(map[string]any{"503": map[string]any{"description": "未配置任何推送通道"}}),
			}},
			// 后台管理。破坏性操作要求在真实响应里被说清楚：
			// 照这份描述生成的客户端如果以为「删缓存」是幂等 GET，
			// 那就是这份文档的错，不是生成器的错。
			"/api/v1/admin/overview": map[string]any{"get": map[string]any{
				"summary":     "后台概览：数据量、上游、巡检、推送、存储策略",
				"description": "配了 API Key 时必须携带；没配 Key 时只接受本机直连。",
				"tags":        []string{"后台"},
				"responses":   withErr(map[string]any{"403": map[string]any{"description": "未配 Key 且非本机直连"}}),
			}},
			"/api/v1/admin/cache": map[string]any{
				"get": map[string]any{
					"summary":    "列出缓存（可按关键词过滤）",
					"tags":       []string{"后台"},
					"parameters": []map[string]any{q("limit", integer("每页条数，默认 30，最大 200"), false), q("offset", integer("跳过条数"), false), q("q", str("在域名/备案号/主办单位上做子串匹配"), false)},
					"responses":  withErr(),
				},
				"delete": map[string]any{
					"summary":    "删除某个域名的缓存",
					"tags":       []string{"后台"},
					"parameters": []map[string]any{domainParam},
					"responses":  withErr(),
				},
			},
			"/api/v1/admin/cache/clear": map[string]any{"post": map[string]any{
				"summary":     "清理缓存：only_expired=true 只删超出回退窗口的，false 全清",
				"description": "空 body 等价于 only_expired=true：不带参数的请求不该造成不可逆后果。",
				"tags":        []string{"后台"},
				"requestBody": map[string]any{"required": false, "content": map[string]any{"application/json": map[string]any{
					"schema": map[string]any{"type": "object", "properties": map[string]any{
						"only_expired": boolean("只清已过期且超出回退窗口的条目"),
					}},
				}}},
				"responses": withErr(),
			}},
			"/api/v1/admin/history": map[string]any{
				"get": map[string]any{
					"summary":    "按域名聚合历史快照（快照数、最近一次、占用配额）",
					"tags":       []string{"后台"},
					"parameters": []map[string]any{q("limit", integer("每页条数，默认 30，最大 200"), false), q("offset", integer("跳过条数"), false), q("q", str("域名/备案号/主办单位"), false)},
					"responses":  withErr(),
				},
				"delete": map[string]any{
					"summary":    "删除某个域名的全部历史快照",
					"tags":       []string{"后台"},
					"parameters": []map[string]any{domainParam},
					"responses":  withErr(),
				},
			},
			"/api/v1/admin/changes": map[string]any{"delete": map[string]any{
				"summary":    "删除一条变更流水（按 at + domain 定位）",
				"tags":       []string{"后台"},
				"parameters": []map[string]any{q("at", integer("事件时间戳（Unix 秒）"), true), domainParam},
				"responses":  withErr(),
			}},
			"/api/v1/admin/changes/clear": map[string]any{"post": map[string]any{
				"summary":   "清空全部变更流水",
				"tags":      []string{"后台"},
				"responses": withErr(),
			}},
			"/api/v1/admin/watch/bulk": map[string]any{"post": map[string]any{
				"summary": "批量加入或移出监控（单次最多 500 个，自动归一化与去重）",
				"tags":    []string{"后台"},
				"requestBody": map[string]any{"required": true, "content": map[string]any{"application/json": map[string]any{
					"schema": map[string]any{"type": "object", "required": []string{"domains"}, "properties": map[string]any{
						"domains": map[string]any{"type": "array", "items": map[string]string{"type": "string"}},
						"action":  str("add（默认）或 remove"),
					}},
				}}},
				"responses": withErr(),
			}},
			"/api/v1/admin/watch/purge": map[string]any{"post": map[string]any{
				"summary":   "清空监控列表",
				"tags":      []string{"后台"},
				"responses": withErr(),
			}},
			"/api/v1/admin/backups": map[string]any{
				"get": map[string]any{
					"summary":   "列出 backups 目录里的快照文件",
					"tags":      []string{"后台"},
					"responses": withErr(),
				},
				"delete": map[string]any{
					"summary":   "删除 backups 目录里的全部 .db 快照",
					"tags":      []string{"后台"},
					"responses": withErr(),
				},
			},
			"/api/v1/stats": map[string]any{"get": map[string]any{
				"summary":   "运行统计、缓存命中率、存储占用与保留策略",
				"tags":      []string{"运维"},
				"responses": okResp,
			}},
			"/api/v1/providers": map[string]any{"get": map[string]any{
				"summary":   "各数据源的启用状态、权重、超时与是否使用自有凭据",
				"tags":      []string{"运维"},
				"responses": okResp,
			}},
			"/api/v1/admin/backup": map[string]any{
				"get": map[string]any{
					"summary":     "在线生成一致性数据快照",
					"description": "bbolt 是排他文件锁，服务运行时只能由进程自己拷。未配 API Key 时只允许真正的回环直连。",
					"tags":        []string{"运维"},
					"parameters":  []map[string]any{q("name", str("纯文件名，默认带时间戳"), false)},
					"responses":   withErr(map[string]any{"403": map[string]any{"description": "未配 Key 且非本机直连"}}),
				},
				"post": map[string]any{
					"summary":    "在线生成一致性数据快照",
					"tags":       []string{"运维"},
					"parameters": []map[string]any{q("name", str("纯文件名，默认带时间戳"), false)},
					"responses":  withErr(map[string]any{"403": map[string]any{"description": "未配 Key 且非本机直连"}}),
				},
			},
			"/badge/icp.svg": map[string]any{"get": map[string]any{
				"summary":     "SVG 徽章，可直接嵌入 README",
				"description": "始终公开（README 里的 img 带不了请求头），但同样限流。",
				"tags":        []string{"徽章"},
				"parameters":  []map[string]any{domainParam, q("label", str("左侧标签，默认 ICP"), false)},
				"responses":   map[string]any{"200": map[string]any{"description": "image/svg+xml"}},
			}},
			// /badge/icp 是 /badge/icp.svg 的别名（README 里两种写法都有人用）。
			"/badge/icp": map[string]any{"get": map[string]any{
				"summary":    "SVG 徽章（/badge/icp.svg 的别名）",
				"tags":       []string{"徽章"},
				"parameters": []map[string]any{domainParam, q("label", str("左侧标签，默认 ICP"), false)},
				"responses":  map[string]any{"200": map[string]any{"description": "image/svg+xml"}},
			}},
			"/healthz": map[string]any{"get": map[string]any{"summary": "健康检查", "tags": []string{"运维"}, "responses": okResp}},
			"/readyz":  map[string]any{"get": map[string]any{"summary": "就绪检查（存储可用）", "tags": []string{"运维"}, "responses": okResp}},
		},
	}
	writeJSON(w, http.StatusOK, doc)
}
