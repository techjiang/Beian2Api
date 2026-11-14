// Package icpcode 校验并解析 ICP 备案号本身。
//
// 这是本系统区别于「纯转发式备案 API」的地方：即使上游不可用，
// 也能判断一个备案号的结构是否成立、属于哪个省、是备案还是许可证。
package icpcode

import (
	"fmt"
	"strings"
)

// Province 是省级行政区及其简称、行政区划前两位。
type Province struct {
	Name   string `json:"name"`
	Short  string `json:"short"`
	Region string `json:"region"`
}

// Provinces 覆盖全部省级行政区，按简称去重后建立索引。
var Provinces = []Province{
	{"北京市", "京", "11"}, {"天津市", "津", "12"}, {"河北省", "冀", "13"},
	{"山西省", "晋", "14"}, {"内蒙古自治区", "蒙", "15"}, {"辽宁省", "辽", "21"},
	{"吉林省", "吉", "22"}, {"黑龙江省", "黑", "23"}, {"上海市", "沪", "31"},
	{"江苏省", "苏", "32"}, {"浙江省", "浙", "33"}, {"安徽省", "皖", "34"},
	{"福建省", "闽", "35"}, {"江西省", "赣", "36"}, {"山东省", "鲁", "37"},
	{"河南省", "豫", "41"}, {"湖北省", "鄂", "42"}, {"湖南省", "湘", "43"},
	{"广东省", "粤", "44"}, {"广西壮族自治区", "桂", "45"}, {"海南省", "琼", "46"},
	{"重庆市", "渝", "50"}, {"四川省", "川", "51"}, {"四川省", "蜀", "51"},
	{"贵州省", "贵", "52"}, {"贵州省", "黔", "52"}, {"云南省", "云", "53"},
	{"云南省", "滇", "53"}, {"西藏自治区", "藏", "54"}, {"陕西省", "陕", "61"},
	{"陕西省", "秦", "61"}, {"甘肃省", "甘", "62"}, {"甘肃省", "陇", "62"},
	{"青海省", "青", "63"}, {"宁夏回族自治区", "宁", "64"}, {"新疆维吾尔自治区", "新", "65"},
	{"香港特别行政区", "港", "81"}, {"澳门特别行政区", "澳", "82"}, {"台湾省", "台", "71"},
}

var shortIndex = func() map[string]Province {
	m := make(map[string]Province, len(Provinces))
	for _, p := range Provinces {
		if _, ok := m[p.Short]; !ok {
			m[p.Short] = p
		}
	}
	return m
}()

// allDigits 判断字符串是否全为数字。
func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// Info 是备案号的解析结果。
type Info struct {
	Raw        string   `json:"raw"`
	Normalized string   `json:"normalized"`
	Valid      bool     `json:"valid"`
	Province   string   `json:"province,omitempty"`
	Region     string   `json:"region,omitempty"`
	Kind       string   `json:"kind,omitempty"`
	Serial     string   `json:"serial,omitempty"`
	SiteSeq    string   `json:"site_seq,omitempty"`
	Issues     []string `json:"issues,omitempty"`
	Fixed      []string `json:"fixed,omitempty"`
}

// 常见手输/OCR 错字映射：只处理首字（省份简称）与数字段里的字母。
var firstCharFix = map[rune]rune{
	'泸': '沪', '炉': '沪', '沪': '沪',
	'像': '豫', '橡': '豫',
	'矇': '蒙', '濛': '蒙', '朦': '蒙',
	'榆': '渝', '愉': '渝',
	'干': '甘', '柑': '甘',
	'靑': '青', '清': '青',
	'奧': '澳', '奥': '澳',
	'粵': '粤',
	'閩': '闽',
	'贛': '赣',
	'陝': '陕',
	'遼': '辽',
	'寧': '宁',
	'瓊': '琼',
	'貴': '贵',
	'雲': '云',
	'瀋': '辽',
}

// digitFix 把数字段里常见的字母误写纠正回数字。
var digitFix = map[byte]byte{'O': '0', 'o': '0', 'I': '1', 'l': '1', 'i': '1', 'Z': '2', 'z': '2', 'S': '5', 's': '5'}

// fullwidth 把全角字符转半角。
var fullwidth = strings.NewReplacer(
	"０", "0", "１", "1", "２", "2", "３", "3", "４", "4",
	"５", "5", "６", "6", "７", "7", "８", "8", "９", "9",
	"Ｉ", "I", "Ｃ", "C", "Ｐ", "P", "Ｂ", "B", "－", "-", "—", "-",
	"　", "", " ", "", "\t", "", "\u00a0", "", "，", "", ",", "",
)

// Sanitize 清洗输入：去空白、全角转半角、修正常见错字。
func Sanitize(s string) (string, []string) {
	var fixed []string
	s = fullwidth.Replace(strings.TrimSpace(s))
	if s == "" {
		return "", nil
	}
	// 首字纠错（按 rune 处理，避免多字节断裂）
	runes := []rune(s)
	if fix, ok := firstCharFix[runes[0]]; ok && fix != runes[0] {
		fixed = append(fixed, fmt.Sprintf("首字 %q 疑似误写，按 %q 处理", runes[0], fix))
		runes[0] = fix
	}

	// 数字段里的字母纠错：只在"前一个字符是数字"或"后一个字符是数字"时替换，
	// 这样不会破坏 B1/B2 这类业务标识。
	out := make([]rune, 0, len(runes))
	for i, c := range runes {
		if c < 128 {
			prevDigit := i > 0 && runes[i-1] >= '0' && runes[i-1] <= '9'
			nextDigit := i+1 < len(runes) && runes[i+1] >= '0' && runes[i+1] <= '9'
			if fix, ok := digitFix[byte(c)]; ok && (prevDigit || nextDigit) {
				fixed = append(fixed, fmt.Sprintf("数字段中的 %q 已纠正为 %q", c, fix))
				c = rune(fix)
			}
		}
		out = append(out, c)
	}
	return string(out), fixed
}

// Parse 解析并校验一个备案号。
func Parse(raw string) Info {
	info := Info{Raw: strings.TrimSpace(raw)}
	s, fixed := Sanitize(raw)
	info.Fixed = fixed
	if s == "" {
		info.Issues = append(info.Issues, "备案号为空")
		return info
	}
	info.Normalized = s
	info.Valid = parse(s, &info)
	if info.Valid {
		info.Issues = nil
	}
	return info
}

// parse 逐段解析：省份简称 -> 备案类型 -> 主体序号 -> 号 -> 站点序号。
func parse(s string, info *Info) bool {
	runes := []rune(s)
	i := 0
	// 1. 省份简称：1 个汉字
	if len(runes) < 2 {
		info.Issues = append(info.Issues, "长度过短，不是合法的备案号")
		return false
	}
	if runes[0] < 0x4e00 || runes[0] > 0x9fa5 {
		info.Issues = append(info.Issues, "应以省份简称汉字开头，例如「京」「沪」「粤」")
		return false
	}
	short := string(runes[0])
	p, ok := shortIndex[short]
	if !ok {
		info.Issues = append(info.Issues, fmt.Sprintf("省份简称 %q 不属于任何省级行政区", short))
		return false
	}
	info.Province, info.Region = p.Name, p.Region
	i = 1

	// 2. 可选 "ICP"，兼容大小写
	rest := string(runes[i:])
	lower := strings.ToLower(rest)
	hasICP := strings.HasPrefix(lower, "icp")
	if hasICP {
		i += 3
		rest = string(runes[i:])
	}

	// 3. 类型：ICP 后的「备 / 证」，或不带 ICP 的「B1 / B2」业务标识。
	kind := ""
	bizCode := ""
	switch {
	case hasICP && strings.HasPrefix(rest, "备"):
		kind = "非经营性备案"
		i++
	case hasICP && strings.HasPrefix(rest, "证"):
		kind = "经营性许可证"
		i++
	case len(rest) >= 2 && (rest[0] == 'B' || rest[0] == 'b') && rest[1] >= '0' && rest[1] <= '9':
		kind = "经营性备案（B 类业务）"
		bizCode = "B" + string(rest[1])
		i += 2
	default:
		info.Issues = append(info.Issues, "缺少「ICP备 / ICP证 / B 类业务」标识，无法判断备案类型")
		return false
	}
	info.Kind = kind

	// 4. 可选分隔符 '-'
	rest = string(runes[i:])
	rest = strings.TrimPrefix(rest, "-")

	// 5. 主体序号 + 可选「号」+ 可选「-站点序号」
	numEnd := 0
	for numEnd < len(rest) && rest[numEnd] >= '0' && rest[numEnd] <= '9' {
		numEnd++
	}
	num := rest[:numEnd]
	if num == "" {
		info.Issues = append(info.Issues, "缺少主体序号（数字）")
		return false
	}
	if len(num) < 4 {
		info.Issues = append(info.Issues, fmt.Sprintf("主体序号 %q 位数偏少，ICP 备案号至少 4 位", num))
	}
	info.Serial = num
	tail := rest[numEnd:]
	tail = strings.TrimPrefix(tail, "号")

	// 6. 站点序号
	if strings.HasPrefix(tail, "-") {
		seq := tail[1:]
		if allDigits(seq) && len(seq) <= 5 {
			info.SiteSeq = seq
			tail = ""
		}
	}
	if tail != "" {
		info.Issues = append(info.Issues, fmt.Sprintf("末尾存在无法识别的部分 %q", tail))
	}

	// 7. 生成规范化写法
	// 规范化统一成业界通行写法：
	//   非经营性 → 京ICP备12345678号-1
	//   许可证   → 京ICP证030173号-1
	//   B 类业务 → 粤B2-20090059号-5
	var norm string
	switch {
	case bizCode != "":
		norm = short + bizCode + "-" + num + "号"
	case kind == "经营性许可证":
		norm = short + "ICP证" + num + "号"
	default:
		norm = short + "ICP备" + num + "号"
	}
	if info.SiteSeq != "" {
		norm += "-" + info.SiteSeq
	}
	info.Normalized = norm
	return len(info.Issues) == 0
}

// Match 判断两个备案号是否是「同一个备案」——主体相同，且站点序号也对得上。
//
// Same 只比对主体（省份 + 主体序号），它回答的是「是不是同一家公司」。
// 而 /api/v1/verify 的交叉核对问的是另一件事：**这个域名上挂的备案号是不是真的那个**。
// 一个主体下有多个网站时，`-1` 和 `-7` 是同一家公司、但属于不同站点；
// 拿 `-7` 去冒充只备了 `-1` 的域名，正是这套功能该抓出来的伪造。
// 之前核对用的是 Same，于是这种情况一路返回「✓ 与实际备案号一致」——
// 假备案号被自己的校验器盖了章，比没有这个功能更糟。
//
// 站点序号缺失时（只写了主体号，写法本身合法）退回到主体比对：
// 这种情况下无法证明他填错，不该武断判假。
func Match(a, b string) bool {
	ia, ib := Parse(a), Parse(b)
	if !Same(a, b) {
		return false
	}
	if ia.SiteSeq == "" || ib.SiteSeq == "" {
		return true
	}
	return ia.SiteSeq == ib.SiteSeq
}

// Same 判断两个备案号是否属于同一主体（忽略站点序号）。
func Same(a, b string) bool {
	ia, ib := Parse(a), Parse(b)
	return ia.Serial != "" && ib.Serial != "" && ia.Province == ib.Province && ia.Serial == ib.Serial
}

// Serial 是 package 内部使用的轻量提取。
func Serial(s string) string { return Parse(s).Serial }
