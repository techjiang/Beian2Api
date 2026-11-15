package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/asoe/beian2api/internal/model"
)

// APIHZ 是公开的第三方备案查询接口，作为官方源不可用时的后备。
// 它需要在配置文件里填写自己的 id/key（免费注册），默认值仅用于体验。
type APIHZ struct {
	client *http.Client
	URL    string // 可注入，方便测试
	ID     string
	Key    string
}

const apihzURL = "https://cn.apihz.cn/api/wangzhan/icp.php"

// NewAPIHZ 构造适配器。
func NewAPIHZ(o Options) *APIHZ {
	cl := o.HTTP
	if cl == nil {
		cl = NewHTTPClient(o.Timeout)
	}
	return &APIHZ{client: cl, URL: apihzURL, ID: "88888888", Key: "88888888"}
}

// WithCredential 设置第三方接口凭据。
func (a *APIHZ) WithCredential(id, key string) *APIHZ {
	if id != "" {
		a.ID = id
	}
	if key != "" {
		a.Key = key
	}
	return a
}

// Name 返回适配器名。
func (a *APIHZ) Name() string { return "apihz" }

// redact 把错误文本里的凭据值替换成 ***。
//
// 只抹 id / key 的值而不是整段 URL：剩下的域名部分是有用的，
// 「哪个域名查失败」和「凭据是什么」该保住的是前者。
func (a *APIHZ) redact(s string) string {
	for _, v := range []string{a.ID, a.Key} {
		if len(v) >= 4 {
			s = strings.ReplaceAll(s, v, "***")
		}
	}
	return s
}

// Lookup 查询单个域名。
func (a *APIHZ) Lookup(ctx context.Context, domain string) ([]model.Record, error) {
	u := buildURL(a.URL, map[string]string{
		"id": a.ID, "key": a.Key, "domain": domain,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUpstream, err)
	}
	req.Header.Set("User-Agent", "beian2api/1.0 (+https://cnb.cool/asoe/TechSauce/Beian2Api)")
	req.Header.Set("Accept", "application/json")

	resp, err := a.client.Do(req)
	if err != nil {
		// 凭据在 query 里，而 http.Client 的报错会把整个 URL 打出来：
		// `Get "https://.../icp.php?id=xxx&key=yyy": dial tcp ...`。
		// 这条错误会一路进到 API 的错误响应、巡检的 last_error、
		// 以及推到钉钉/飞书群里的告警正文——等于把你的凭据公开广播。
		return nil, fmt.Errorf("%w: %s", ErrUpstream, a.redact(err.Error()))
	}
	defer func() { io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16)); resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUpstream, err)
	}

	var r struct {
		Code   int    `json:"code"`
		Msg    string `json:"msg"`
		ICP    string `json:"icp"`
		Unit   string `json:"unit"`
		Type   string `json:"type"`
		Domain string `json:"domain"`
		Time   string `json:"time"`
		TD     string `json:"td"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, fmt.Errorf("%w: 响应非 JSON", ErrUpstream)
	}
	// 400 不等于「没有备案」。
	//
	// 这个上游把所有失败都塞进同一个 400，用 msg 区分原因。实测至少三种：
	//
	//	400 查询失败或没有备案。      ← 真的没备案
	//	400 id参数异常，请检查。      ← 凭据写错了
	//	400 通讯秘钥不正确！          ← 凭据写错了
	//	400 失败，请重试！            ← 上游自己抖了
	//
	// 之前一律当 ErrNotFound，后果不是「查不到」而是「查到了，没备案」：
	// 结果会带着 10 分钟的负缓存，而巡检会把它读成「备案被注销了」，
	// 往钉钉/飞书群里推一条 revoked 告警。凭据填错一次，监控里所有域名
	// 会同时被判成注销——一条假警报比十条「查询失败」危险得多。
	if r.Code == 400 {
		if apihzNotFound(r.Msg) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("%w: code=400 %s", ErrUpstream, r.Msg)
	}
	if r.Code != 200 {
		return nil, fmt.Errorf("%w: code=%d %s", ErrUpstream, r.Code, r.Msg)
	}
	// 上游用 "查询失败" 占位表示查不到，必须识别出来，否则会污染缓存。
	icp := cleaner(r.ICP)
	if icp == "" {
		return nil, ErrNotFound
	}

	rec := model.Record{
		// 同 miit：统一落成「查询用的域名」，别信上游回显。
		// 历史快照按 Record.Domain 做 key，和缓存/监控的 key 必须是同一个，
		// 否则「查看历史」永远是空的。
		Domain:    domain,
		UnitName:  cleaner(r.Unit),
		UnitType:  cleaner(r.Type),
		SiteName:  "",
		ICPCode:   icp,
		Nature:    cleaner(r.TD),
		AuditTime: cleaner(r.Time),
		Source:    "apihz",
		FetchedAt: model.Now(),
	}
	rec.Normalize()
	return []model.Record{rec}, nil
}

// apihzNotFound 判断上游的 400 到底是「没有备案」还是「这次请求本身有问题」。
//
// 判据只认上游明确说出「查不到」的那几种措辞。不认识的一律当上游错误：
// 猜错方向的代价不对称——把「凭据错」当成「没备案」会让监控误报注销，
// 而把「没备案」当成上游错误，最多是多回源一次。
func apihzNotFound(msg string) bool {
	// 不把「查询失败」也算进去：上游抖动时回的正是「失败，请重试！」，
	// 而真正的「没备案」那句话里已经带着「没有备案」。
	for _, kw := range []string{"没有备案", "未备案", "无备案", "无数据", "未查询", "没有找到"} {
		if strings.Contains(msg, kw) {
			return true
		}
	}
	return false
}

var _ Provider = (*APIHZ)(nil)
