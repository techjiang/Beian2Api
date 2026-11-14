package provider

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/asoe/beian2api/internal/model"
)

// MIIT 直连工信部备案查询接口（beian.miit.gov.cn 背后的官方数据源）。
//
// 官方接口有 token 机制：先用 timeStr+authKey 换 token，再带 token 查询。
// 它同时挂了 WAF，机房 IP 常被拦，因此把失败当成普通的上游错误，
// 由上层降级到其他数据源或本地快照——这是系统能长期稳定的关键。
type MIIT struct {
	client   *http.Client
	authURL  string // 可注入，方便测试
	queryURL string

	mu        sync.Mutex
	token     string
	tokenExp  time.Time
	lastError string
}

const (
	miitAuthURL  = "https://hlwicpfwc.miit.gov.cn/icpproject_query/api/auth"
	miitQueryURL = "https://hlwicpfwc.miit.gov.cn/icpproject_query/api/icpAbbreviateInfo/queryByCondition"
	miitSalt     = "testtesttest"
)

// NewMIIT 构造工信部适配器。
func NewMIIT(o Options) *MIIT {
	cl := o.HTTP
	if cl == nil {
		cl = NewHTTPClient(o.Timeout)
	}
	return &MIIT{client: cl, authURL: miitAuthURL, queryURL: miitQueryURL}
}

// Name 返回适配器名。
func (m *MIIT) Name() string { return "miit" }

func (m *MIIT) headers(req *http.Request) {
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36")
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9")
	req.Header.Set("Origin", "https://beian.miit.gov.cn")
	req.Header.Set("Referer", "https://beian.miit.gov.cn/")
	req.Header.Set("Sec-Fetch-Dest", "empty")
	req.Header.Set("Sec-Fetch-Mode", "cors")
	req.Header.Set("Sec-Fetch-Site", "cross-site")
}

// auth 取 token，带 10 分钟本地复用，避免每次请求都握手。
func (m *MIIT) auth(ctx context.Context) (string, error) {
	m.mu.Lock()
	if m.token != "" && time.Now().Before(m.tokenExp) {
		t := m.token
		m.mu.Unlock()
		return t, nil
	}
	m.mu.Unlock()

	ts := strconv.FormatInt(time.Now().UnixMilli(), 10)
	sum := sha256.Sum256([]byte(ts + miitSalt))
	form := "authKey=" + hex.EncodeToString(sum[:]) + "&timeStr=" + ts

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.authURL, strings.NewReader(form))
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrUpstream, err)
	}
	m.headers(req)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=UTF-8")

	resp, err := m.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrUpstream, err)
	}
	defer func() { io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16)); resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrUpstream, err)
	}
	var ar struct {
		Success bool   `json:"success"`
		Msg     string `json:"msg"`
		Params  struct {
			Token string `json:"bussiness"`
		} `json:"params"`
	}
	if err := json.Unmarshal(body, &ar); err != nil {
		// 返回 HTML 说明被 WAF 拦了，这类失败不上报负缓存。
		return "", fmt.Errorf("%w: 认证响应非 JSON（可能被防火墙拦截）", ErrUpstream)
	}
	if !ar.Success || ar.Params.Token == "" {
		return "", fmt.Errorf("%w: 认证失败 %s", ErrUpstream, ar.Msg)
	}
	m.mu.Lock()
	m.token = ar.Params.Token
	m.tokenExp = time.Now().Add(10 * time.Minute)
	m.mu.Unlock()
	return ar.Params.Token, nil
}

// Lookup 查询单个域名。
func (m *MIIT) Lookup(ctx context.Context, domain string) ([]model.Record, error) {
	token, err := m.auth(ctx)
	if err != nil {
		return nil, err
	}
	payload := map[string]any{
		"pageNum":     1,
		"pageSize":    10,
		"unitName":    domain,
		"serviceType": 1,
	}
	buf, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.queryURL, bytes.NewReader(buf))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUpstream, err)
	}
	m.headers(req)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Token", token)

	resp, err := m.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUpstream, err)
	}
	defer func() { io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16)); resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUpstream, err)
	}
	if resp.StatusCode == http.StatusUnauthorized {
		m.mu.Lock()
		m.token = ""
		m.mu.Unlock()
		return nil, fmt.Errorf("%w: token 失效", ErrUpstream)
	}

	var qr struct {
		Success bool   `json:"success"`
		Msg     string `json:"msg"`
		Params  struct {
			List []struct {
				Domain           string `json:"domain"`
				UnitName         string `json:"unitName"`
				UnitType         string `json:"unitType"`
				ServiceName      string `json:"serviceName"`
				MainLicence      string `json:"mainLicence"`
				ServiceLicence   string `json:"serviceLicence"`
				LeaderName       string `json:"leaderName"`
				Nature           string `json:"natureName"`
				UpdateRecordTime string `json:"updateRecordTime"`
			} `json:"list"`
			Total int `json:"total"`
		} `json:"params"`
	}
	if err := json.Unmarshal(body, &qr); err != nil {
		return nil, fmt.Errorf("%w: 查询响应非 JSON", ErrUpstream)
	}
	if !qr.Success {
		if strings.Contains(qr.Msg, "没有") || strings.Contains(qr.Msg, "未查询") {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("%w: %s", ErrUpstream, qr.Msg)
	}
	if len(qr.Params.List) == 0 {
		return nil, ErrNotFound
	}

	now := model.Now()
	out := make([]model.Record, 0, len(qr.Params.List))
	for _, it := range qr.Params.List {
		// unitName 是模糊匹配，只保留真正命中的域名。
		// 比对要忽略 www.：备案按主域名登记，上游回的 domain 一般不带 www.，
		// 而用户很可能查的是 `www.example.com`。用 == 精确比会把它丢掉，
		// 于是这条命中记录被过滤成空列表 → 返回 ErrNotFound → 一个已备案的
		// 域名被判成「未备案」，还带着 10 分钟的负缓存。
		if !model.SameDomain(it.Domain, domain) {
			continue
		}
		r := model.Record{
			// Domain 一律落成「用户查的那个域名」，不用上游回显的那个。
			// 历史快照是按 Record.Domain 做 key 的（AppendHistory），而缓存、
			// 监控、巡检用的都是查询域名；两边不一致时，历史会在另一个 key 下
			// 悄悄写一份，巡检去读自己那个 key 永远读到空——「查看历史」空白，
			// 而磁盘上明明有数据。
			Domain:    domain,
			UnitName:  firstNonEmpty(it.UnitName, it.LeaderName),
			UnitType:  cleaner(it.UnitType),
			SiteName:  firstNonEmpty(it.ServiceName),
			ICPCode:   firstNonEmpty(it.ServiceLicence, it.MainLicence),
			Nature:    cleaner(it.Nature),
			AuditTime: cleaner(it.UpdateRecordTime),
			Source:    "miit",
			FetchedAt: now,
		}
		r.Normalize()
		out = append(out, r)
	}
	if len(out) == 0 {
		return nil, ErrNotFound
	}
	return out, nil
}

var _ Provider = (*MIIT)(nil)
