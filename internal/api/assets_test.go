package api

import (
	"regexp"
	"strings"
	"testing"
)

// 前端是手写的纯 JS，没有构建链、没有类型检查，Go 的编译器也管不到它。
// 于是「引用了一个从没声明过的名字」这类错误只能靠人眼看——这轮就栽在这里：
// 保存 API Key 的代码引用了 KEY_STORE，而它只存在于另一个闭包里叫 STORE，
// 赋值语句抛 ReferenceError，恰好被外层 try/catch 吞掉。
// 表现是：开鉴权的部署，网页上填 Key 永远存不进去、刷新后全站 401，
// 界面上一点错都不报，只有开发者工具的控制台里有一行。
//
// 这两个检查用几十行 Go 换掉一整类「静默失效」，值得。

var (
	// 全大写标识符（常量风格）如果是被读取的变量，就必须自己有声明。
	upperIdentRe = regexp.MustCompile(`\b[A-Z][A-Z0-9_]{2,}\b`)
	declRe       = regexp.MustCompile(`\b(?:var|let|const)\s+([A-Z][A-Z0-9_]{2,})\b`)
)

// knownGlobals 是浏览器与标准库里本来就存在的全大写名字。
var knownGlobals = map[string]struct{}{
	"JSON": {}, "HTTP": {}, "HTTPS": {}, "URL": {}, "URI": {}, "ID": {}, "API": {},
	"CSS": {}, "SVG": {}, "DOM": {}, "UTF": {}, "PNG": {}, "RSS": {}, "XSS": {},
	"OK": {}, "GET": {}, "POST": {}, "PUT": {}, "DELETE": {}, "HEAD": {}, "OPTIONS": {},
	"NaN": {}, "Infinity": {},
}

func TestFrontendHasNoUndeclaredUpperCaseIdentifiers(t *testing.T) {
	// 先剥掉注释和字符串字面量：里面出现 ICP、HTML、BEIAN2API_WEBHOOK
	// 之类的词是正常的文案，不是变量引用。
	src := stripCommentsAndStrings(string(mustAsset(t, "app.js")))
	declared := map[string]struct{}{}
	for _, m := range declRe.FindAllStringSubmatch(src, -1) {
		declared[m[1]] = struct{}{}
	}
	for _, m := range upperIdentRe.FindAllString(src, -1) {
		if _, ok := knownGlobals[m]; ok {
			continue
		}
		if _, ok := declared[m]; ok {
			continue
		}
		t.Errorf("app.js 引用了未声明的全大写标识符 %q：这类错误在浏览器里是静默的（ReferenceError 可能被 try/catch 吞掉）", m)
	}
}

// stripCommentsAndStrings 粗粒度地去掉 //、/* */ 注释与 ' " ` 字符串内容。
//
// 不需要成为完整的 JS 词法分析器：目标只是让「被引用的大写标识符」
// 出现在干净代码里，而不是出现在一句给人看的注释里。
func stripCommentsAndStrings(src string) string {
	var b strings.Builder
	for i := 0; i < len(src); i++ {
		c := src[i]
		switch {
		case c == '/' && i+1 < len(src) && src[i+1] == '/':
			for i < len(src) && src[i] != '\n' {
				i++
			}
			b.WriteByte('\n')
		case c == '/' && i+1 < len(src) && src[i+1] == '*':
			i += 2
			for i+1 < len(src) && !(src[i] == '*' && src[i+1] == '/') {
				i++
			}
			i++
			b.WriteByte(' ')
		case c == '\'' || c == '"' || c == '`':
			q := c
			i++
			for i < len(src) && src[i] != q {
				if src[i] == '\\' {
					i++
				}
				i++
			}
			b.WriteString(" \"\" ")
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// 保存 Key 的两条路必须都落到同一个 storage key 上。
// 之前一个走常量、一个走闭包里的局部变量，名字还不一样，于是一个能存一个不能。
func TestFrontendKeyStorageIsWired(t *testing.T) {
	src := string(mustAsset(t, "app.js"))
	if !strings.Contains(src, "var KEY_STORE") {
		t.Fatal("app.js 必须显式声明 KEY_STORE（保存与读取共用一个 storage key）")
	}
	if n := strings.Count(src, "localStorage.setItem(KEY_STORE"); n == 0 {
		t.Fatal("保存 Key 必须写入 KEY_STORE，否则刷新后读到的是空")
	}
	if n := strings.Count(src, "localStorage.getItem(KEY_STORE"); n == 0 {
		t.Fatal("读取 Key 必须从 KEY_STORE 读，否则和写入对不上")
	}
}

// 同一个闭包里不能重复声明同名函数/变量。
//
// 上一版 app.js 里 watchRow 和 wWant 各写了两遍（分页那次改动粘重了一段）。
// 后一个声明静默覆盖前一个，两份代码还不一样，读代码的人会以为改的是生效的那份——
// 这不会报错、不会崩，只会让人对着「明明改了却没生效」发呆。
func TestFrontendHasNoDuplicateDeclarations(t *testing.T) {
	src := string(mustAsset(t, "app.js"))
	// 按闭包切块：每个 IIFE 内部是独立的词法作用域，跨块同名是合法的。
	blocks := strings.Split(src, "(function () {\n")
	declLine := regexp.MustCompile(`^  (?:function\s+([A-Za-z_$][\w$]*)\s*\(|(?:var|let|const)\s+([A-Za-z_$][\w$]*))`)
	for bi, block := range blocks {
		seen := map[string]int{}
		for i, line := range strings.Split(block, "\n") {
			m := declLine.FindStringSubmatch(line)
			if m == nil {
				continue
			}
			name := m[1]
			if name == "" {
				name = m[2]
			}
			if prev, ok := seen[name]; ok {
				t.Errorf("第 %d 个闭包内 %q 被声明了两次（第 %d 行与第 %d 行），后者会静默覆盖前者",
					bi, name, prev, i+1)
			}
			seen[name] = i + 1
		}
	}
}

// stripLineComments 只去掉整行注释，保留字符串字面量与缩进结构。
func stripLineComments(src string) string {
	var b strings.Builder
	for _, line := range strings.Split(src, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "//") {
			continue
		}
		b.WriteString(line)
		b.WriteString("\n")
	}
	return b.String()
}

func mustAsset(t *testing.T, name string) []byte {
	t.Helper()
	b, err := assets.ReadFile("assets/" + name)
	if err != nil {
		t.Fatalf("读取内嵌资源 %s: %v", name, err)
	}
	return b
}

// 复制必须走一个带降级的统一入口。
//
// navigator.clipboard 只在安全上下文（https / localhost）里存在。用局域网 IP
// 打开自己部署的服务时它是 undefined，直接调 .writeText 抛 TypeError——
// 按钮点下去毫无反应，也不报错（异常发生在 Promise 之外）。
// 徽章那段更糟：调用完就无条件写「已复制 ✓」，粘贴出来是空的。
func TestFrontendCopyHasFallback(t *testing.T) {
	src := string(mustAsset(t, "app.js"))
	if !strings.Contains(src, "function copyText(") {
		t.Fatal("app.js 要有统一的 copyText 入口")
	}
	if !strings.Contains(src, "navigator.clipboard && navigator.clipboard.writeText") {
		t.Fatal("copyText 必须先判断 clipboard 是否存在，不能直接调")
	}
	if !strings.Contains(src, "document.execCommand('copy')") {
		t.Fatal("不安全上下文里要降级到 execCommand")
	}
	// 除了 copyText 内部，其它地方不许再直接碰 navigator.clipboard。
	code := stripCommentsAndStrings(src)
	start := strings.Index(code, "function copyText(")
	end := strings.Index(code[start:], "\nfunction withKey(")
	if start < 0 || end < 0 {
		t.Fatal("找不到 copyText 的函数体边界")
	}
	inside := code[start : start+end]
	if strings.Count(code, "navigator.clipboard") != strings.Count(inside, "navigator.clipboard") {
		t.Fatal("navigator.clipboard 只允许出现在 copyText 里")
	}
	// 徽章的「已复制 ✓」只能在复制成功之后出现。
	i := strings.Index(src, "copyText('!['")
	if i < 0 {
		t.Fatal("徽章代码必须用 copyText 复制")
	}
	if j := strings.Index(src[i:], "已复制"); j < 0 {
		t.Fatal("徽章仍要给出复制成功提示")
	}
	if strings.Contains(src[i:i+400], "已复制 ✓');\n        setTimeout") {
		t.Fatal("徽章不能无条件写「已复制」")
	}
}

// 保存 Key 后不能立刻收框。
//
// 「✓ Key 有效」这句提示写在 keybox 里。之前提交处理的第一行就是
// `$('#keybox').hidden = !!v`——填了 Key 就收框，提示写进一个已经不可见的
// 元素里。填对和填错在界面上完全一样（都只剩一个不动的搜索框），用户只能靠
// 「怎么还是没数据」自己猜。框必须留到验证结果回来之后才收。
func TestFrontendKeyBoxStaysUntilVerified(t *testing.T) {
	src := string(mustAsset(t, "app.js"))
	// 这里不能用 stripCommentsAndStrings：选择器 `'#keybox'` 本身就是字符串字面量，
	// 剥掉之后一个字都匹配不上。改为剥掉注释、保留字符串。
	code := stripLineComments(src)
	start := strings.Index(code, "var kform")
	if start < 0 {
		t.Fatal("找不到保存 Key 的提交处理")
	}
	end := strings.Index(code[start:], "function refreshRSS(")
	if end < 0 {
		t.Fatal("找不到提交处理的结束位置")
	}
	body := code[start : start+end]
	verify := strings.Index(body, "api('/api/v1/stats')")
	if verify < 0 {
		t.Fatal("保存后必须立刻验一次 Key")
	}
	// 校验之前不许动 hidden。
	before := body[:verify]
	if strings.Contains(before, "#keybox") && strings.Contains(before, "hidden") {
		t.Fatal("校验结果回来之前不能收起 keybox，否则「✓ 有效」这句谁也看不到")
	}
	after := body[verify:]
	if !strings.Contains(after, "#keybox") || !strings.Contains(after, "hidden = true") {
		t.Fatal("验证通过之后才收起 keybox")
	}
}

// 校验器必须能带上域名做交叉核对。
//
// 「这个备案号结构合法」和「这个号真是这个域名的」是两件事，后者才是
// 识别伪造备案号的关键（拿一家真实主体号改个尾数，结构完全合法）。
// 后端 /api/v1/verify 早就支持 domain 参数，但界面上没有入口，
// 这个能力只能靠 curl 手动带参数用——功能在、用户摸不到，等于没有。
func TestFrontendVerifySendsDomain(t *testing.T) {
	src := string(mustAsset(t, "app.js"))
	if !strings.Contains(src, "getElementById('vd')") {
		t.Fatal("校验器要读域名输入框 #vd")
	}
	// 这里不能用 stripCommentsAndStrings：'&domain=' 本身就在字符串字面量里。
	// 校验器那一段里必须同时出现域名输入框的读取与参数拼接。
	i := strings.Index(src, "var domInput")
	if i < 0 {
		t.Fatal("找不到校验器里读域名输入框的代码")
	}
	// 拼参数那一步必须出现在校验器里（读输入框之后、IIFE 结束之前）。
	rest := src[i:]
	if strings.Contains(rest, "\n})();") {
		rest = rest[:strings.Index(rest, "\n})();")]
	}
	if !strings.Contains(rest, "'&domain='") {
		t.Fatal("校验请求要带上 domain 参数，否则交叉核对永远用不上")
	}
	html := string(mustAsset(t, "index.html"))
	if !strings.Contains(html, `id="vd"`) {
		t.Fatal("index.html 里要有域名输入框 #vd")
	}
	// 可选字段不能 required，否则只查备案号结构也得先填个域名。
	hi := strings.Index(html, `id="vd"`)
	if strings.Contains(html[hi-200:hi], "required") {
		t.Fatal("域名是可选项，输入框不能 required")
	}
}
