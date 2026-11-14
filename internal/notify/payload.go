package notify

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// payload 按通道类型构造请求体。
//
// 各家协议差异比想象中大：同一个"标题 + 几行字段"的意图，
// 钉钉是 markdown.text、企业微信是 markdown.content、飞书要 interactive.card、
// Slack 用 mrkdwn 文本。统一模板做不到，只能各拼一份。
func payload(ch Channel, ev Event) ([]byte, error) {
	switch ch.Kind {
	case KindDingTalk:
		return json.Marshal(map[string]any{
			"msgtype": "markdown",
			"markdown": map[string]string{
				"title": title(ev),
				"text":  markdownText(ev),
			},
		})
	case KindWeCom:
		return json.Marshal(map[string]any{
			"msgtype":  "markdown",
			"markdown": map[string]string{"content": markdownText(ev)},
		})
	case KindFeishu:
		card := map[string]any{
			"config": map[string]any{"wide_screen_mode": true},
			"header": map[string]any{
				"title":    map[string]string{"tag": "plain_text", "content": title(ev)},
				"template": feishuTemplate(ev.Kind),
			},
			"elements": []any{
				map[string]any{"tag": "div", "text": map[string]string{"tag": "lark_md", "content": feishuBody(ev)}},
			},
		}
		body := map[string]any{"msg_type": "interactive", "card": card}
		if ch.Secret != "" {
			ts, sign := feishuSign(ch.Secret, time.Now())
			body["timestamp"] = ts
			body["sign"] = sign
		}
		return json.Marshal(body)
	case KindSlack:
		return json.Marshal(map[string]any{
			"text": slackText(ev),
		})
	default:
		// generic 保持与旧版一致的扁平结构，已有集成不用改。
		text := plainText(ev)
		return json.Marshal(map[string]any{
			"event":    "icp.changed",
			"kind":     ev.Kind,
			"domain":   ev.Domain,
			"at":       ev.At,
			"icp_code": ev.ICPCode,
			"diff":     ev.Diff,
			"text":     text,
			// 兼容字段：钉钉/企业微信/飞书的文本形态
			"msgtype":  "text",
			"content":  map[string]string{"text": text},
			"markdown": map[string]string{"title": title(ev), "text": text},
			"msg_type": "text",
		})
	}
}

func kindWord(kind string) string {
	switch kind {
	case EventRevoked:
		return "注销"
	case EventRegistered:
		return "新增"
	case EventUpstreamError:
		return "上游异常"
	case EventRecovered:
		return "已恢复"
	case EventTest:
		return "测试"
	default:
		return "变更"
	}
}

func title(ev Event) string {
	if ev.Kind == EventTest {
		return "Beian2API 推送测试"
	}
	return "备案" + kindWord(ev.Kind) + "：" + ev.Domain
}

// lines 生成「字段：旧值 → 新值」的行，空值统一显示为「无」。
func lines(ev Event) []string {
	var out []string
	if ev.ICPCode != "" {
		out = append(out, "备案号："+ev.ICPCode)
	}
	for _, d := range ev.Diff {
		out = append(out, fmt.Sprintf("%s：%s → %s", d.Field, dash(d.From), dash(d.To)))
	}
	if len(ev.Diff) == 0 && ev.ICPCode == "" {
		switch ev.Kind {
		case EventUpstreamError:
			out = append(out, "该域名已连续多轮无法回源查询")
		case EventRecovered:
			out = append(out, "上游已恢复，域名可正常查询")
		default:
			out = append(out, "备案状态发生变化")
		}
	}
	if ev.BaseURL != "" && ev.Domain != "" && ev.Kind != EventTest {
		out = append(out, "详情："+strings.TrimRight(ev.BaseURL, "/")+"/?domain="+ev.Domain)
	}
	return out
}

func plainText(ev Event) string {
	if ev.Kind == EventTest {
		return "Beian2API 推送测试成功。收到这条消息，说明通道配置正确。"
	}
	return title(ev) + "\n" + strings.Join(lines(ev), "\n")
}

// markdownText 用于钉钉与企业微信：钉钉把标题当会话列表摘要，正文用 ### 开头更醒目。
func markdownText(ev Event) string {
	if ev.Kind == EventTest {
		return "### Beian2API 推送测试\n\n收到这条消息，说明通道配置正确。"
	}
	return "### " + title(ev) + "\n\n" + strings.Join(lines(ev), "\n\n")
}

func feishuBody(ev Event) string {
	if ev.Kind == EventTest {
		return "收到这条消息，说明通道配置正确。"
	}
	return strings.Join(lines(ev), "\n")
}

func slackText(ev Event) string {
	if ev.Kind == EventTest {
		return "*Beian2API 推送测试*\n收到这条消息，说明通道配置正确。"
	}
	ls := lines(ev)
	for i, l := range ls {
		ls[i] = "• " + l
	}
	return "*" + title(ev) + "*\n" + strings.Join(ls, "\n")
}

func feishuTemplate(kind string) string {
	switch kind {
	case EventRevoked:
		return "red"
	case EventRegistered, EventRecovered:
		return "green"
	case EventUpstreamError:
		return "red"
	case EventTest:
		return "grey"
	default:
		return "orange"
	}
}

func dash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "无"
	}
	return s
}
