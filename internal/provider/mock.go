package provider

import (
	"context"
	"strings"

	"github.com/asoe/beian2api/internal/model"
)

// Mock 是离线演示数据源：不访问任何网络，按域名确定性地生成一条记录。
//
// 用途有两个：一是小白第一次运行能看到完整效果，二是 CI 里跑端到端测试。
// 默认不启用，配置 providers 里加 {name: mock, enabled: true} 才会参与竞速。
type Mock struct{}

// NewMock 构造演示适配器。
func NewMock(Options) Provider { return &Mock{} }

// Name 返回适配器名。
func (m *Mock) Name() string { return "mock" }

var provinces = []string{"京", "沪", "粤", "浙", "苏", "川", "鄂", "闽"}
var kinds = []string{"企业", "个人", "政府机关", "事业单位"}

// Lookup 按域名哈希生成稳定的演示数据。
func (m *Mock) Lookup(ctx context.Context, domain string) ([]model.Record, error) {
	if strings.HasSuffix(domain, ".test") || strings.HasSuffix(domain, ".local") {
		return nil, ErrNotFound
	}
	var h uint32 = 2166136261
	for i := 0; i < len(domain); i++ {
		h ^= uint32(domain[i])
		h *= 16777619
	}
	prov := provinces[h%uint32(len(provinces))]
	kind := kinds[(h/7)%uint32(len(kinds))]
	num := 1000000 + h%8000000
	rec := model.Record{
		Domain:    domain,
		UnitName:  strings.Split(domain, ".")[0] + "（演示数据）",
		UnitType:  kind,
		SiteName:  strings.Split(domain, ".")[0] + " 站点",
		ICPCode:   prov + "ICP备" + itoa(int(num)) + "号-1",
		Nature:    kind,
		AuditTime: "2024-06-01",
		Source:    "mock",
		FetchedAt: model.Now(),
	}
	rec.Normalize()
	return []model.Record{rec}, nil
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [12]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
