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

func mustAsset(t *testing.T, name string) []byte {
	t.Helper()
	b, err := assets.ReadFile("assets/" + name)
	if err != nil {
		t.Fatalf("读取内嵌资源 %s: %v", name, err)
	}
	return b
}
