package model

import "testing"

func TestNormalizeDomain(t *testing.T) {
	cases := []struct{ in, want string }{
		{"example.com", "example.com"},
		{"  EXAMPLE.com  ", "example.com"},
		{"https://www.example.com/a/b?c=1", "www.example.com"},
		{"http://example.com:8080/x", "example.com"},
		{"example.com.", "example.com"},
		{"user:pw@example.com", "example.com"},
		{"//example.com/path", "example.com"},
		{"", ""},
		{"example.com#hash", "example.com"},
	}
	for _, c := range cases {
		if got := NormalizeDomain(c.in); got != c.want {
			t.Errorf("NormalizeDomain(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestIsValidDomain(t *testing.T) {
	valid := []string{"example.com", "a.co", "sub.domain.co.uk", "xn--fiqs8s.cn", "my-site.io"}
	invalid := []string{"", "com", "a", "-bad.com", "bad-.com", "exa mple.com", "exam_ple.com", "123.456", "http://a.com/x"}
	for _, d := range valid {
		if !IsValidDomain(d) {
			t.Errorf("%q 应合法", d)
		}
	}
	for _, d := range invalid {
		if IsValidDomain(d) {
			t.Errorf("%q 应非法", d)
		}
	}
}

func TestProvinceAndSerial(t *testing.T) {
	cases := []struct{ code, prov, serial string }{
		{"粤B2-20090059-5", "粤", "20090059"},
		{"京ICP证030173号-1", "京", "030173"},
		{"沪ICP备13002172号-3", "沪", "13002172"},
		{"", "", ""},
		{"unknown", "", ""},
		{"蒙ICP备1234号-1", "蒙", "1234"},
	}
	for _, c := range cases {
		if got := ProvinceFromICP(c.code); got != c.prov {
			t.Errorf("ProvinceFromICP(%q) = %q, want %q", c.code, got, c.prov)
		}
		if got := SerialFromICP(c.code); got != c.serial {
			t.Errorf("SerialFromICP(%q) = %q, want %q", c.code, got, c.serial)
		}
	}
}

func TestNatureName(t *testing.T) {
	cases := map[string]string{"1-1": "企业", "2-2": "个人", "5-1": "政府机关", "企业": "企业", "": "", "9-9": ""}
	for in, want := range cases {
		if got := NatureName(in); got != want {
			t.Errorf("NatureName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRecordNormalizeFingerprint(t *testing.T) {
	r := Record{Domain: " HTTPS://Example.com/x ", UnitName: " 某公司 ", ICPCode: " 京ICP备 1234 号-1 ", Nature: "1-1"}
	r.Normalize()
	if r.Domain != "example.com" || r.UnitName != "某公司" || r.ICPCode != "京ICP备1234号-1" {
		t.Fatalf("归一化结果不对: %+v", r)
	}
	if r.Province != "京" || r.Nature != "企业" {
		t.Fatalf("派生字段不对: %+v", r)
	}
	fp1 := r.Fingerprint()
	r.Source = "other"
	r.FetchedAt = 123
	if r.Fingerprint() != fp1 {
		t.Fatal("指纹不应受来源与时间影响")
	}
	r.SiteName = "新站名"
	if r.Fingerprint() == fp1 {
		t.Fatal("业务字段变化必须改变指纹")
	}
}

func TestSiteSeq(t *testing.T) {
	cases := map[string]string{
		"京ICP备12345678号-3": "3",
		"粤B2-20090059-5":   "5",
		"京ICP备12345678号":   "",
		"":                 "",
		"京ICP备12号-x":       "",
	}
	for in, want := range cases {
		if got := SiteSeq(in); got != want {
			t.Errorf("SiteSeq(%q) = %q, want %q", in, got, want)
		}
	}
}

// 同一域名的多条备案记录，代表记录的挑选必须唯一且稳定：
// 历史、巡检、徽章、校验走的是同一条，否则 diff 会凭空多出差异。
func TestPrimaryPicksSmallestSiteSeq(t *testing.T) {
	recs := []Record{
		{Domain: "a.com", ICPCode: "粤B2-20090059-5", UnitName: "示例"},
		{Domain: "a.com", ICPCode: "粤ICP备12345678号-1", UnitName: "示例"},
		{Domain: "a.com", ICPCode: "粤ICP备12345678号-3", UnitName: "示例"},
	}
	got, ok := Primary(recs)
	if !ok || got.ICPCode != "粤ICP备12345678号-1" {
		t.Fatalf("应取站点序号最小的那条，实际 %+v", got)
	}
	// 顺序打乱后结果不变：上游列出顺序不稳定时不能报出「假变更」。
	shuffled := []Record{recs[2], recs[1], recs[0]}
	if again, _ := Primary(shuffled); again.ICPCode != got.ICPCode {
		t.Fatalf("结果应稳定，实际 %s", again.ICPCode)
	}
	if _, ok := Primary(nil); ok {
		t.Fatal("空列表不该有代表记录")
	}
}

// 站点序号不可解析时保持原顺序（不因为「没人能比」而随手换一条）。
func TestPrimaryKeepsOrderWhenSeqUnparsable(t *testing.T) {
	recs := []Record{{ICPCode: ""}, {ICPCode: "京ICP备1号-1"}}
	if got, _ := Primary(recs); got.ICPCode != "" {
		t.Fatalf("无法比较时应保持原顺序，实际 %+v", got)
	}
}
