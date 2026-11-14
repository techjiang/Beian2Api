package icpcode

import "testing"

func TestParseValid(t *testing.T) {
	cases := []struct{ in, prov, region, kind, serial, seq, norm string }{
		{"京ICP备12345678号-1", "北京市", "11", "非经营性备案", "12345678", "1", "京ICP备12345678号-1"},
		{"沪ICP备13002172号-3", "上海市", "31", "非经营性备案", "13002172", "3", "沪ICP备13002172号-3"},
		{"粤B2-20090059-5", "广东省", "44", "经营性备案（B 类业务）", "20090059", "5", "粤B2-20090059号-5"},
		{"京ICP证030173号-1", "北京市", "11", "经营性许可证", "030173", "1", "京ICP证030173号-1"},
		{"粤ICP备20090059号", "广东省", "44", "非经营性备案", "20090059", "", "粤ICP备20090059号"},
		{"京ICP备 1234 号-2", "北京市", "11", "非经营性备案", "1234", "2", "京ICP备1234号-2"},
	}
	for _, c := range cases {
		got := Parse(c.in)
		if !got.Valid {
			t.Errorf("%q 应合法，问题：%v", c.in, got.Issues)
			continue
		}
		if got.Province != c.prov || got.Region != c.region || got.Kind != c.kind || got.Serial != c.serial || got.SiteSeq != c.seq {
			t.Errorf("%q 解析结果不对: %+v", c.in, got)
		}
		if got.Normalized != c.norm {
			t.Errorf("%q 规范化应为 %q，实际 %q", c.in, c.norm, got.Normalized)
		}
	}
}

func TestParseInvalid(t *testing.T) {
	for _, in := range []string{"", "abc", "ICP备1234号", "京备1234号", "京ICP备abc号", "京ICP备1号", "京ICP备12345678号-1-2"} {
		if got := Parse(in); got.Valid {
			t.Errorf("%q 应判为非法，实际 %+v", in, got)
		}
	}
}

func TestSanitizeFixes(t *testing.T) {
	// 全角、空格、错字、字母误写都应被纠正
	got := Parse("泸ICP备１23456７8号-1")
	if !got.Valid {
		t.Fatalf("应纠正后合法: %+v", got)
	}
	if got.Province != "上海市" {
		t.Fatalf("泸 应纠正为 沪，实际 %s", got.Province)
	}
	if len(got.Fixed) == 0 {
		t.Fatal("应记录纠正内容")
	}
	if got := Parse("京ICP备12O45678号-1"); got.Serial != "12045678" {
		t.Fatalf("字母 O 应纠正为 0，实际 %s", got.Serial)
	}
}

func TestUnknownProvince(t *testing.T) {
	got := Parse("囧ICP备1234号-1")
	if got.Valid {
		t.Fatal("非法省份简称应判无效")
	}
	if len(got.Issues) == 0 || got.Province != "" {
		t.Fatalf("应给出问题说明: %+v", got)
	}
}

func TestSame(t *testing.T) {
	if !Same("粤ICP备20090059号-1", "粤ICP备20090059号-5") {
		t.Fatal("同主体不同站点序号应判为同一主体")
	}
	if Same("粤ICP备20090059号-1", "京ICP备20090059号-1") {
		t.Fatal("不同省份不应判为同一主体")
	}
	if Same("", "") {
		t.Fatal("空值不应相等")
	}
}

func TestAllDigits(t *testing.T) {
	if !allDigits("0123") || allDigits("") || allDigits("12a") {
		t.Fatal("allDigits 判断错误")
	}
}

func TestProvincesCoverage(t *testing.T) {
	if len(shortIndex) < 31 {
		t.Fatalf("省份索引不足 31 个，实际 %d", len(shortIndex))
	}
	for _, p := range Provinces {
		if len([]rune(p.Short)) != 1 {
			t.Errorf("简称 %q 应只有一个字", p.Short)
		}
		if len(p.Region) != 2 {
			t.Errorf("%s 的行政区划码应为两位，实际 %q", p.Name, p.Region)
		}
	}
}

// Match 比 Same 严格：站点序号也要对上。
//
// 一个主体下有多个网站时，`-1` 和 `-7` 是同一家公司、不同站点。
// /api/v1/verify 的交叉核对问的是「这个域名上挂的备案号是不是真的那一个」，
// 拿 `-7` 冒充只备了 `-1` 的域名正是它该抓的伪造。之前用的是 Same，
// 于是假备案号被自己的校验器盖章「一致」——比没有这个功能更糟。
func TestMatch(t *testing.T) {
	if !Match("粤ICP备20090059号-1", "粤ICP备20090059号-1") {
		t.Fatal("完全相同的备案号应判为一致")
	}
	if Match("粤ICP备20090059号-7", "粤ICP备20090059号-1") {
		t.Fatal("同主体不同站点序号不是同一个备案")
	}
	if !Same("粤ICP备20090059号-7", "粤ICP备20090059号-1") {
		t.Fatal("Same 仍应判为同一主体（它的语义没变）")
	}
	// 只写主体号（不写站点序号）时无法证明填错，退回主体比对。
	if !Match("粤ICP备20090059号", "粤ICP备20090059号-1") {
		t.Fatal("缺站点序号时应退回到主体比对")
	}
	if Match("京ICP备20090059号-1", "粤ICP备20090059号-1") {
		t.Fatal("不同省份不应判为一致")
	}
	if Match("", "") {
		t.Fatal("空值不应判为一致")
	}
}
