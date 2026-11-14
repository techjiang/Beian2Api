package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// buildBinary 编译出测试用二进制。
func buildBinary(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "beian2api")
	cmd := exec.Command("go", "build", "-o", bin, ".")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("编译失败: %v\n%s", err, out)
	}
	return bin
}

func runCmd(t *testing.T, bin, dataDir string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Dir = t.TempDir()
	cmd.Env = append(os.Environ(),
		"BEIAN2API_DATA_DIR="+dataDir,
		"BEIAN2API_PROVIDERS=mock:1",
	)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func TestCLIVersion(t *testing.T) {
	bin := buildBinary(t)
	out, err := runCmd(t, bin, t.TempDir(), "version")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "beian2api") {
		t.Fatalf("版本输出异常: %s", out)
	}
}

func TestCLIConfig(t *testing.T) {
	bin := buildBinary(t)
	out, err := runCmd(t, bin, t.TempDir(), "config")
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"listen:", "data_dir:", "providers:"} {
		if !strings.Contains(out, k) {
			t.Fatalf("配置输出缺少 %s:\n%s", k, out)
		}
	}
}

func TestCLIKeygen(t *testing.T) {
	bin := buildBinary(t)
	out, err := runCmd(t, bin, t.TempDir(), "keygen", "-n", "2")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 2 {
		t.Fatalf("应生成 2 个 key，实际 %d 行", len(lines))
	}
	for _, l := range lines {
		if !strings.HasPrefix(l, "bk_") || len(l) < 20 {
			t.Fatalf("key 格式不对: %s", l)
		}
	}
	// 两次生成必须不同
	out2, _ := runCmd(t, bin, t.TempDir(), "keygen")
	if strings.TrimSpace(out2) == lines[0] {
		t.Fatal("随机 key 不应重复")
	}
}

func TestCLIExportImportBackup(t *testing.T) {
	bin := buildBinary(t)
	dataDir := t.TempDir()
	work := t.TempDir()

	// 先启动服务写一点数据？直接跑 export 验证空库也能工作。
	dump := filepath.Join(work, "dump.json")
	cmd := exec.Command(bin, "export", "-o", dump)
	cmd.Dir = work
	cmd.Env = append(os.Environ(), "BEIAN2API_DATA_DIR="+dataDir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("export 失败: %v\n%s", err, out)
	}
	if _, err := os.Stat(dump); err != nil {
		t.Fatal("导出文件不存在")
	}

	// import 回去
	cmd = exec.Command(bin, "import", "-i", dump)
	cmd.Dir = work
	cmd.Env = append(os.Environ(), "BEIAN2API_DATA_DIR="+dataDir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("import 失败: %v\n%s", err, out)
	}

	// backup 生成快照
	bk := filepath.Join(work, "bk.db")
	cmd = exec.Command(bin, "backup", "-o", bk)
	cmd.Dir = work
	cmd.Env = append(os.Environ(), "BEIAN2API_DATA_DIR="+dataDir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("backup 失败: %v\n%s", err, out)
	}
	if info, err := os.Stat(bk); err != nil || info.Size() == 0 {
		t.Fatal("备份文件应有内容")
	}
}

func TestCLIDoctor(t *testing.T) {
	bin := buildBinary(t)
	out, err := runCmd(t, bin, t.TempDir(), "doctor")
	if err != nil {
		t.Fatalf("doctor 失败: %v\n%s", err, out)
	}
	for _, want := range []string{"自检", "数据目录", "上游连通性"} {
		if !strings.Contains(out, want) {
			t.Fatalf("doctor 输出缺少 %q:\n%s", want, out)
		}
	}
}

func TestCLIUnknownCommand(t *testing.T) {
	bin := buildBinary(t)
	if _, err := runCmd(t, bin, t.TempDir(), "nosuchcmd"); err == nil {
		t.Fatal("未知命令应返回错误")
	}
}

func TestCLIHelp(t *testing.T) {
	bin := buildBinary(t)
	out, err := runCmd(t, bin, t.TempDir(), "help")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []string{"serve", "backup", "export", "import", "keygen", "doctor"} {
		if !strings.Contains(out, c) {
			t.Fatalf("帮助信息缺少命令 %s", c)
		}
	}
}

func TestCLIBadConfig(t *testing.T) {
	bin := buildBinary(t)
	dir := t.TempDir()
	bad := filepath.Join(dir, "bad.yaml")
	_ = os.WriteFile(bad, []byte("listen: [unclosed"), 0o600)
	cmd := exec.Command(bin, "config", "-c", bad)
	if out, err := cmd.CombinedOutput(); err == nil {
		t.Fatalf("非法配置应报错，实际输出: %s", out)
	}
}

// -c 是帮助里承诺的通用参数，所有子命令都得认。
// 之前只有 serve 认它，`beian2api doctor -c x.yaml` 会直接报 flag not defined。
func TestCLIAllCommandsAcceptConfigShorthand(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	cfg := "listen: \":18099\"\ndata_dir: \"" + dir + "/data\"\nlog_level: \"error\"\nproviders:\n  - name: mock\n    enabled: true\n    weight: 100\n    timeout: 3\n"
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	bin := buildBinary(t)
	for _, args := range [][]string{
		{"config", "-c", cfgPath},
		{"doctor", "-c", cfgPath},
		{"export", "-c", cfgPath, "-o", filepath.Join(dir, "dump.json")},
	} {
		out, err := runCmd(t, bin, filepath.Join(dir, "data"), args...)
		if err != nil {
			t.Fatalf("%v 应成功: %v\n%s", args, err, out)
		}
	}
}
