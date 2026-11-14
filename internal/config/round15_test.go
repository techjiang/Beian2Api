package config

import (
	"os"
	"path/filepath"
	"testing"
)

// 这组用例守的是两类「配了等于没配」的坑：环境变量把文件里的细节静默抹掉，
// 以及一个合法取值（0 = 不裁剪）被默认值逻辑改写。

// BEIAN2API_PROVIDERS 只该决定「启用哪些源、权重多少」，不该顺手抹掉
// 配置文件里写的凭据和超时。
//
// README 教人用 `BEIAN2API_PROVIDERS=miit:1,apihz:.4` 配数据源。一旦这么写，
// config.yaml 里 apihz 的自有凭据会被整份替换掉，退回公共测试凭据——
// 而被限流时只会表现为「偶尔查不到」，从任何地方都看不出原因。
func TestProvidersEnvKeepsFileDetails(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	yaml := `providers:
  - name: miit
    enabled: true
    weight: 100
    timeout: 3
  - name: apihz
    enabled: false
    weight: 0
    timeout: 5
    id: my_id
    key: my_key
`
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BEIAN2API_PROVIDERS", "miit:1,apihz:0.4")

	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]Provider{}
	for _, p := range c.Providers {
		byName[p.Name] = p
	}
	miit := byName["miit"]
	if miit.Timeout != 3 {
		t.Fatalf("miit 的 timeout 应保留配置里的 3 秒，实际 %d", miit.Timeout)
	}
	apihz := byName["apihz"]
	if apihz.ID != "my_id" || apihz.Key != "my_key" {
		t.Fatalf("apihz 凭据被环境变量抹掉了：id=%q key=%q", apihz.ID, apihz.Key)
	}
	if apihz.Timeout != 5 {
		t.Fatalf("apihz 的 timeout 应保留 5 秒，实际 %d", apihz.Timeout)
	}
	if !apihz.Enabled || apihz.Weight != 40 {
		t.Fatalf("权重/启用状态应由环境变量决定，实际 enabled=%v weight=%d", apihz.Enabled, apihz.Weight)
	}
	if !apihz.HasCredential {
		t.Fatal("合并后仍应认为凭据已配置")
	}
}

// 环境变量里出现配置文件没有的源时，照常按默认值接进来。
func TestProvidersEnvAddsUnknownProvider(t *testing.T) {
	t.Setenv("BEIAN2API_PROVIDERS", "mock:1")
	c, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Providers) != 1 || c.Providers[0].Name != "mock" {
		t.Fatalf("应只剩环境变量指定的那个源: %+v", c.Providers)
	}
	if c.Providers[0].Timeout <= 0 {
		t.Fatal("新出现的源应补上默认超时")
	}
}

// retain_history_total 显式写 0（不裁剪）不能被改写成 20000。
//
// 默认值在 Default() 里就填好了，走到 Validate 还是 0 只可能是有人
// 明确写了 0。改写回去等于让「不裁剪」这个合法取值永远配不出来，
// 而配置文件上一个字都没错——用着用着历史就被悄悄裁到 2 万条。
func TestRetainHistoryTotalZeroMeansNoTrim(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	yaml := `storage:
  retain_changes: 0
  retain_history: 0
  retain_history_total: 0
`
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.Storage.RetainHistoryTotal != 0 {
		t.Fatalf("显式的 0 应表示不裁剪，实际被改成 %d", c.Storage.RetainHistoryTotal)
	}

	// 没配的时候仍然是默认值，不受影响。
	c2, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if c2.Storage.RetainHistoryTotal != 20000 {
		t.Fatalf("未配置时应取默认 20000，实际 %d", c2.Storage.RetainHistoryTotal)
	}
}
