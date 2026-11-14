// Package model 定义系统中流转的核心数据结构和它们的行为。
package model

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strings"
	"time"
)

// Record 是一条归一化后的备案记录。所有上游适配器都必须产出这个结构，
// 让下游的缓存、聚合、变更检测只面对一种形状的数据。
type Record struct {
	Domain    string `json:"domain"`     // 域名（小写，无协议）
	UnitName  string `json:"unit_name"`  // 主办单位名称
	UnitType  string `json:"unit_type"`  // 主办单位性质：企业/个人/政府...
	SiteName  string `json:"site_name"`  // 网站名称
	ICPCode   string `json:"icp_code"`   // 备案号全文
	Province  string `json:"province"`   // 从备案号解析出的省份
	Nature    string `json:"nature"`     // 备案性质
	AuditTime string `json:"audit_time"` // 审核通过日期
	Source    string `json:"source"`     // 数据来源适配器名
	FetchedAt int64  `json:"fetched_at"` // 抓取时间（Unix 秒）
}

// Fingerprint 用于变更检测：只关心业务字段，不含抓取时间和来源。
func (r Record) Fingerprint() string {
	h := sha256.New()
	for _, s := range []string{r.Domain, r.UnitName, r.UnitType, r.SiteName, r.ICPCode, r.Province, r.Nature, r.AuditTime} {
		h.Write([]byte(strings.TrimSpace(s)))
		h.Write([]byte{0x1f})
	}
	return hex.EncodeToString(h.Sum(nil))[:32]
}

var (
	icpCleaner = regexp.MustCompile(`\s+`)
	// 备案号形如「京ICP备1234号-1」「粤B2-20090059-5」「京ICP证030173号-1」，
	// 省份是开头 1~2 个汉字，不能只认 "ICP备"，否则会漏掉一大类。
	provinceRe = regexp.MustCompile(`^([\x{4e00}-\x{9fa5}]{1,2})`)
	serialRe   = regexp.MustCompile(`(?:备|证|B)\s*(?:\d+-)?(\d+)`)
)

// natureNames 把上游的编号映射成人能读懂的性质名。
var natureNames = map[string]string{
	"1-1": "企业", "1-2": "企业", "1-3": "企业",
	"2-1": "个人", "2-2": "个人", "2-3": "个人",
	"3-1": "事业单位", "3-2": "事业单位",
	"4-1": "社会团体", "5-1": "政府机关", "6-1": "其他",
}

// NatureName 把 "1-2" 这样的代码翻译成中文；已经是文字的则原样返回。
func NatureName(code string) string {
	c := strings.TrimSpace(code)
	if n, ok := natureNames[c]; ok {
		return n
	}
	if c == "" {
		return ""
	}
	if c[0] >= '0' && c[0] <= '9' {
		if i := strings.IndexByte(c, '-'); i > 0 {
			switch c[0] {
			case '1':
				return "企业"
			case '2':
				return "个人"
			case '3':
				return "事业单位"
			case '4':
				return "社会团体"
			case '5':
				return "政府机关"
			}
		}
		return ""
	}
	return c
}

// Normalize 就地清洗字段。
func (r *Record) Normalize() {
	r.Domain = NormalizeDomain(r.Domain)
	r.UnitName = strings.TrimSpace(r.UnitName)
	r.UnitType = strings.TrimSpace(r.UnitType)
	r.SiteName = strings.TrimSpace(r.SiteName)
	r.Nature = NatureName(r.Nature)
	r.ICPCode = icpCleaner.ReplaceAllString(strings.TrimSpace(r.ICPCode), "")
	r.AuditTime = strings.TrimSpace(r.AuditTime)
	if r.Province == "" {
		r.Province = ProvinceFromICP(r.ICPCode)
	}
}

// ProvinceFromICP 从备案号中解析省份简称。
func ProvinceFromICP(code string) string {
	m := provinceRe.FindStringSubmatch(code)
	if len(m) < 2 {
		return ""
	}
	return m[1]
}

// SerialFromICP 返回备案号里的主体序号。
func SerialFromICP(code string) string {
	m := serialRe.FindStringSubmatch(code)
	if len(m) < 2 {
		return ""
	}
	return m[1]
}

// SiteSeq 返回备案号的站点序号，如「京ICP备1号-3」中的 "3"。
func SiteSeq(code string) string {
	i := strings.LastIndexByte(code, '-')
	if i < 0 || i+1 >= len(code) {
		return ""
	}
	s := code[i+1:]
	for j := 0; j < len(s); j++ {
		if s[j] < '0' || s[j] > '9' {
			return ""
		}
	}
	return s
}

// NormalizeDomain 去掉协议、路径、端口、userinfo，转小写。
func NormalizeDomain(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ""
	}
	lower := strings.ToLower(s)
	if i := strings.Index(lower, "://"); i >= 0 {
		s = s[i+3:]
	} else if strings.HasPrefix(lower, "//") {
		s = s[2:]
	}
	if i := strings.IndexAny(s, "/?#"); i >= 0 {
		s = s[:i]
	}
	if i := strings.IndexByte(s, '@'); i >= 0 {
		s = s[i+1:]
	}
	if i := strings.LastIndexByte(s, ':'); i >= 0 && !strings.Contains(s, "]") {
		s = s[:i]
	}
	s = strings.TrimSuffix(strings.ToLower(s), ".")
	return strings.TrimSpace(s)
}

// IsValidDomain 做语法校验。
func IsValidDomain(d string) bool {
	if len(d) < 4 || len(d) > 253 {
		return false
	}
	labels := strings.Split(d, ".")
	if len(labels) < 2 {
		return false
	}
	for i, l := range labels {
		if l == "" || len(l) > 63 {
			return false
		}
		for j := 0; j < len(l); j++ {
			c := l[j]
			if !((c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-') {
				return false
			}
		}
		if strings.HasPrefix(l, "-") || strings.HasSuffix(l, "-") {
			return false
		}
		if i == len(labels)-1 && strings.Trim(l, "0123456789") == "" {
			return false
		}
	}
	return true
}

// Result 是一次查询的完整产出。
type Result struct {
	Domain    string   `json:"domain"`
	Found     bool     `json:"found"`
	Records   []Record `json:"records"`
	Cached    bool     `json:"cached"`
	Stale     bool     `json:"stale"`
	Source    string   `json:"source"`
	CostMS    float64  `json:"cost_ms"`
	FetchedAt int64    `json:"fetched_at"`
	Notice    string   `json:"notice,omitempty"`
}

// Now 统一时间来源，方便测试替换。
var Now = func() int64 { return time.Now().Unix() }
