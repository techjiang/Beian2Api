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

// SameDomain 判断两个域名是不是「同一个网站」——忽略前导 www.。
//
// 备案是按主域名登记的，`www.qq.com` 和 `qq.com` 是同一条备案。上游返回的
// domain 字段通常不带 www.，所以「按域名精确比对结果」的适配器（miit 就是）
// 必须用这个函数，否则查 `www.qq.com` 时会把唯一命中的记录丢掉，
// 一个已经备案的域名被判成「未备案」——比报错更糟，因为它看起来像结论。
func SameDomain(a, b string) bool {
	return trimWWW(NormalizeDomain(a)) == trimWWW(NormalizeDomain(b))
}

// trimWWW 去掉前导 www.，但保留 `www.com` 这种本身就是注册域名的情况。
func trimWWW(d string) string {
	rest := strings.TrimPrefix(d, "www.")
	if rest == d || !strings.Contains(rest, ".") {
		return d
	}
	return rest
}

// CanonicalDomain 返回「同一个网站」的统一写法：归一化后去掉前导 www.。
//
// 为什么需要一个统一的 key：备案是按主域名登记的，`www.example.com` 和
// `example.com` 是同一份备案。但缓存、历史、监控三处都以域名字符串做 key，
// 于是查一次 www 版本就多出一条缓存、加一次监控就多出一行——同一个备案
// 被监控两遍，改一次会推两条重复通知，任何一条移出后另一条还在报。
// 文档里写着「两者共用同一份缓存、同一行监控」，实现必须对得上。
//
// 注意这不是「猜主域名」：只剥 www.，不动 api. / m. 这类真正的子域名——
// 它们可能是独立备案的站点，贸然合并会把不同网站的数据混在一起。
func CanonicalDomain(raw string) string {
	d := NormalizeDomain(raw)
	if d == "" {
		return ""
	}
	return trimWWW(d)
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

// Primary 从同一域名的多条备案记录里挑出「代表记录」。
//
// 一个主体下可以有多个网站，上游会一次返回多条。缓存哪种顺序、巡检比对
// 哪一条、徽章显示哪一条，必须是同一个答案，否则：
//   - 历史快照按「站点序号最小」记，巡检却拿 Records[0] 比对，
//     真正发生变更时 diff 会凭空多出一行「备案号 -1 → -3」；
//   - 上游返回顺序一变（同一域名两次请求列出顺序不同），巡检就会
//     报一条根本不存在的变更。
//
// 所以这里把选择规则收在一处：站点序号最小的那条优先，序号不可比较时
// 保持原顺序（上游给的顺序），保证结果稳定、可复现。
func Primary(recs []Record) (Record, bool) {
	if len(recs) == 0 {
		return Record{}, false
	}
	best := recs[0]
	for _, r := range recs[1:] {
		if siteSeqLess(r.ICPCode, best.ICPCode) {
			best = r
		}
	}
	return best, true
}

// siteSeqLess 判断 a 的站点序号是否小于 b；任一无法解析时返回 false（不交换）。
func siteSeqLess(a, b string) bool {
	sa, sb := SiteSeq(a), SiteSeq(b)
	if sa == "" || sb == "" {
		return false
	}
	if len(sa) != len(sb) {
		return len(sa) < len(sb)
	}
	return sa < sb
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
