package main

import (
	"flag"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ---------------------------------------------------------------- YAML 解析器

func TestParseYAMLBasic(t *testing.T) {
	in := `
# 顶部注释
api_base: "https://example.com/api"   # 行尾注释
token: 'abc # 不是注释'
ipv4:
  enabled: true
  source: auto
skip_test:
  - "fc"
  - fd
  - fe80
`
	root, err := parseYAMLSubset([]byte(in))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if got := root.get("api_base").str(); got != "https://example.com/api" {
		t.Errorf("api_base = %q（行尾注释应被去掉）", got)
	}
	if got := root.get("token").str(); got != "abc # 不是注释" {
		t.Errorf("token = %q（单引号内的 # 不是注释）", got)
	}
	if got := root.get("ipv4").get("enabled").str(); got != "true" {
		t.Errorf("ipv4.enabled = %q", got)
	}
	list := root.get("skip_test").Strings()
	if len(list) != 3 || list[0] != "fc" || list[2] != "fe80" {
		t.Errorf("列表解析错误: %#v", list)
	}
	// map 版本按 key 取
	if got := root.StringsOf("skip_test"); len(got) != 3 {
		t.Errorf("StringsOf 取到 %v", got)
	}
	if got := root.StringsOf("不存在"); got != nil {
		t.Errorf("缺失的 key 应返回 nil，实际 %v", got)
	}
}

func TestParseYAMLNestedSeq(t *testing.T) {
	// 覆盖 "- key: value" 之后跟同级兄弟键的写法（本程序模板的核心结构）
	in := `
domains:
  example.com:
    zone: example.com
    records:
      - name: pc
        type: AAAA
        ttl: 1
        proxy: false
      - name: nas
        type: both
`
	root, err := parseYAMLSubset([]byte(in))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	recs := root.get("domains").get("example.com").get("records")
	if recs == nil || !recs.isSeq {
		t.Fatalf("records 应为列表")
	}
	if len(recs.Seq) != 2 {
		t.Fatalf("应有 2 条记录，实际 %d", len(recs.Seq))
	}
	if got := recs.Seq[0].get("name").str(); got != "pc" {
		t.Errorf("第 1 条 name = %q", got)
	}
	if got := recs.Seq[0].get("type").str(); got != "AAAA" {
		t.Errorf("第 1 条 type = %q", got)
	}
	if got := recs.Seq[0].get("ttl").str(); got != "1" {
		t.Errorf("第 1 条 ttl = %q", got)
	}
	// 关键点：第二个键不能丢失（曾经会在这里丢字段）
	if got := recs.Seq[0].get("proxy").str(); got != "false" {
		t.Errorf("第 1 条 proxy = %q (兄弟键丢失)", got)
	}
	if got := recs.Seq[1].get("name").str(); got != "nas" {
		t.Errorf("第 2 条 name = %q", got)
	}
}

func TestParseYAMLErrors(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"制表符缩进", "a:\n\tb: 1\n", "制表符"},
		{"制表符缩进2", "a:\n  b: 1\n\tc: 2\n", "制表符"},
		{"空文件", "\n# 只有注释\n", "空"},
		{"键重复", "a: 1\na: 2\n", "重复"},
		{"缺少冒号", "a\n", "key: value"},
		{"缩进不一致", "a:\n    b: 1\n  c: 2\n", "缩进"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseYAMLSubset([]byte(tc.in))
			if err == nil {
				t.Fatalf("期望报错，但解析成功了")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("错误信息 %q 未包含 %q", err.Error(), tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------- 模板与脱字符

func TestDefaultTemplateIsComplete(t *testing.T) {
	text := renderConfigTemplate()
	if !strings.HasPrefix(text, "# cfddns 配置") {
		t.Errorf("模板首行异常: %q", strings.SplitN(text, "\n", 2)[0])
	}
	root, err := parseYAMLSubset([]byte(text))
	if err != nil {
		t.Fatalf("生成的模板必须能被自己的解析器读取，但报错: %v", err)
	}
	if strings.Contains(root.get("token").str(), "cfat_") {
		t.Errorf("模板里的 token 不应是真实令牌")
	}
	// 模板不能泄露任何具体域名，必须是通用占位符
	if root.get("zone").str() != "example.com" {
		t.Errorf("zone 占位符应为 example.com，实际 %q", root.get("zone").str())
	}
	// 模板里不得出现任何具体域名：凡「含点且含字母」的取值都算泄漏
	// （IPv4 前缀如 192.168. 含点但不含字母，会被排除）。
	// 这里刻意不写死真实域名 —— 否则测试源码本身就泄露了。
	for _, s := range scalarTexts(root) {
		if !strings.Contains(s, ".") || !strings.ContainsAny(s, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ") {
			continue
		}
		if s == "example.com" || strings.Contains(s, "://") || strings.Contains(s, "dash.cloudflare.com") {
			continue
		}
		t.Errorf("模板里出现了具体域名 %q", s)
	}

	// 程序支持的所有顶层配置项都应在模板里出现
	for _, k := range []string{
		"token", "zone", "dns", "ipv4", "ipv6",
	} {
		if root.get(k) == nil {
			t.Errorf("模板缺少顶层键 %q", k)
		}
	}
	// 探测块的每一项
	for _, fam := range []string{"ipv4", "ipv6"} {
		n := root.get(fam)
		if n == nil || !n.isMap {
			t.Fatalf("%s 段缺失或不是映射", fam)
		}
		for _, k := range []string{"enabled", "source", "url", "match_regex", "skip_prefix"} {
			if n.get(k) == nil {
				t.Errorf("%s 段缺少 %q", fam, k)
			}
		}
	}

	// 默认 dns 必须是「v4+v6 都绑」的默认写法
	dns := root.get("dns")
	if dns == nil || !dns.isSeq || len(dns.Seq) == 0 {
		t.Fatalf("dns 段缺失或为空: %v", dns)
	}
	rs, err := parseRecords(dns, "", "dns")
	if err != nil {
		t.Fatalf("模板里的 dns 解析失败: %v", err)
	}
	if len(rs) != 1 {
		t.Fatalf("模板默认应只启用 1 条记录，实际 %d 条: %+v", len(rs), rs)
	}
	if rs[0].Name != "pc" || rs[0].Type != "BOTH" {
		t.Errorf("默认记录应为 pc/BOTH，实际 %s/%s", rs[0].Name, rs[0].Type)
	}

	// 模板必须真的能跑通校验（占位 token 除外）
	cfg, err := loadConfig(writeTempConfig(t, strings.Replace(text,
		"token: 在这里填你的 Cloudflare API 令牌", "token: tok_test", 1)))
	if err != nil {
		t.Fatalf("模板装载失败: %v", err)
	}
	if err := cfg.validate(); err != nil {
		t.Fatalf("模板（填上 token 后）应通过校验: %v", err)
	}
	if len(cfg.Domains) != 1 || len(cfg.Domains[0].Records) != 1 {
		t.Errorf("模板解析出的域名/记录数不对: %+v", cfg.Domains)
	}
	// 被注释掉的自定义示例不能生效
	if cfg.Domains[0].Records[0].Type != "BOTH" {
		t.Errorf("注释里的记录被误解析了: %+v", cfg.Domains[0].Records)
	}
}

// skip_prefix 要按协议给出有意义的默认值：
// fc/fd/fe80 是 IPv6 前缀，填在 ipv4 下永远匹配不到任何地址。
func TestTemplateSkipPrefixPerFamily(t *testing.T) {
	root, err := parseYAMLSubset([]byte(renderConfigTemplate()))
	if err != nil {
		t.Fatal(err)
	}
	v6 := root.get("ipv6").StringsOf("skip_prefix")
	if strings.Join(v6, ",") != "fc,fd,fe80" {
		t.Errorf("ipv6.skip_prefix = %v", v6)
	}
	for _, bad := range []string{"fc", "fd", "fe80"} {
		for _, p := range root.get("ipv4").StringsOf("skip_prefix") {
			if strings.HasPrefix(bad, p) || p == bad {
				t.Errorf("ipv4.skip_prefix 不应含 IPv6 前缀 %q", p)
			}
		}
	}
	if len(root.get("ipv4").StringsOf("skip_prefix")) == 0 {
		t.Errorf("ipv4.skip_prefix 应给出私有网段示例")
	}
}

func TestConfigPath(t *testing.T) {
	got, err := configPath("/tmp/x.yaml")
	if err != nil || !strings.HasSuffix(got, "x.yaml") {
		t.Fatalf("configPath 覆盖失败: %q %v", got, err)
	}
	home, _ := os.UserHomeDir()
	got, err = configPath("")
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(home, ".cfddns", "config.yaml"); got != want {
		t.Errorf("默认配置路径 = %q，期望 %q", got, want)
	}
	if sp := statePathFor(got); sp != filepath.Join(home, ".cfddns", "state.json") {
		t.Errorf("状态路径 = %q", sp)
	}
}

// ---------------------------------------------------------------- 子命令解析

// 覆盖一个真实踩到的 bug：Go 的 flag 在第一个非标志参数处停止，
// 于是 `cfddns --config x status` 里的 status 被忽略，命令退化成了执行一轮同步。
// parseCommand 返回 (子命令, 交给 flag 解析的参数, 错误)。
func TestParseCommand(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"无参数默认 run", nil, "run"},
		{"命令在前", []string{"status"}, "status"},
		{"命令在前带选项", []string{"status", "--config", "/tmp/x.yaml"}, "status"},
		{"命令在后", []string{"--config", "/tmp/x.yaml", "status"}, "status"},
		{"命令在多个选项后", []string{"--json", "--config", "/tmp/x.yaml", "paths"}, "paths"},
		{"gen-config 带 --force", []string{"gen-config", "--force"}, "gen-config"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cmd, rest, err := parseCommand(tc.args)
			if err != nil {
				t.Fatalf("应解析成功，却报错: %v", err)
			}
			if cmd != tc.want {
				t.Errorf("命令 = %q，期望 %q", cmd, tc.want)
			}
			// 剩余参数里不能再出现子命令，否则它会再次被当成位置参数
			for _, a := range rest {
				if knownCommand(a) {
					t.Errorf("剩余参数里仍有子命令 %q: %v", a, rest)
				}
			}
		})
	}
}

// 标志的取值不能被误当成子命令
func TestParseCommandKeepsFlagValues(t *testing.T) {
	cmd, rest, err := parseCommand([]string{"--config", "/tmp/x.yaml", "status"})
	if err != nil {
		t.Fatal(err)
	}
	if cmd != "status" {
		t.Errorf("命令 = %q", cmd)
	}
	// flag 包还需要收到 --config 及其取值
	want := []string{"--config", "/tmp/x.yaml"}
	if strings.Join(rest, " ") != strings.Join(want, " ") {
		t.Errorf("剩余参数 = %v，期望 %v", rest, want)
	}

	// 用等号形式
	_, rest2, err := parseCommand([]string{"--config=/tmp/x.yaml", "paths"})
	if err != nil {
		t.Fatal(err)
	}
	if len(rest2) != 1 || rest2[0] != "--config=/tmp/x.yaml" {
		t.Errorf("等号形式解析错误: %v", rest2)
	}
}

func TestParseCommandErrors(t *testing.T) {
	// 未知命令要报错，不能静默按 run 执行（那会真的写 DNS）
	if _, _, err := parseCommand([]string{"stats"}); err == nil {
		t.Errorf("拼错的命令应报错")
	}
	// 未知命令在选项之后也是错误
	if _, _, err := parseCommand([]string{"--config", "/tmp/x", "stats"}); err == nil {
		t.Errorf("选项后的未知命令应报错")
	}
	// 多余参数要报错
	if _, _, err := parseCommand([]string{"status", "extra"}); err == nil {
		t.Errorf("多余参数应报错")
	}
}

// scalarTexts 收集节点树里所有标量文本，用于「不得泄露具体内容」这类断言。
func scalarTexts(n *CfNode) []string {
	if n == nil {
		return nil
	}
	if n.Scalar != nil {
		return []string{n.Scalar.Text}
	}
	var out []string
	for _, v := range n.Map {
		out = append(out, scalarTexts(v)...)
	}
	for _, v := range n.Seq {
		out = append(out, scalarTexts(v)...)
	}
	return out
}

// gen-config 的契约：只输出一行结果说明，不打印 YAML 内容（用户明确要求）。
// 配置内容写到文件里，需要看内容就读文件或 cfddns status。
func TestGenConfigOutputsOneLine(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "sub", "config.yaml")

	capture := func(fn func() int) (string, int) {
		old := os.Stdout
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		os.Stdout = w
		done := make(chan string, 1)
		go func() {
			b, _ := io.ReadAll(r)
			done <- string(b)
		}()
		code := fn()
		w.Close()
		os.Stdout = old
		return <-done, code
	}

	// 输出里不能出现配置项本身
	assertNoYAML := func(what, out string) {
		t.Helper()
		for _, bad := range []string{"token:", "zone:", "dns:", "ipv4:", "ipv6:", "source:", "skip_prefix"} {
			if strings.Contains(out, bad) {
				t.Errorf("%s 的 stdout 不应含配置内容 %q:\n%s", what, bad, out)
			}
		}
		// 提到配置文件路径是允许且必要的
		if !strings.Contains(out, target) {
			t.Errorf("%s 的 stdout 应给出配置路径:\n%s", what, out)
		}
	}

	// ① 首次生成
	out1, code := capture(func() int { return cmdGenConfig(target, false) })
	if code != 0 {
		t.Fatalf("首次生成退出码 = %d", code)
	}
	assertNoYAML("首次生成", out1)
	if n := len(strings.Split(strings.TrimRight(out1, "\n"), "\n")); n != 1 {
		t.Errorf("首次生成应只输出 1 行，实际 %d 行:\n%s", n, out1)
	}
	// 文件必须真的写入了完整配置
	onDisk, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("文件未写入: %v", err)
	}
	full := renderConfigTemplate()
	if string(onDisk) != full {
		t.Errorf("落盘内容与模板不一致（长度 %d vs %d）", len(onDisk), len(full))
	}

	// ② 已存在且不带 --force：不覆盖，输出仍只是一行（外加一句提示）
	if err := os.WriteFile(target, []byte("token: 我改过的\ndns:\n  - keep\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out2, code := capture(func() int { return cmdGenConfig(target, false) })
	if code != 0 {
		t.Fatalf("退出码 = %d", code)
	}
	assertNoYAML("不带 --force", out2)
	kept, _ := os.ReadFile(target)
	if !strings.Contains(string(kept), "我改过的") {
		t.Errorf("不带 --force 不该覆盖已有文件，实际内容: %s", kept)
	}

	// ③ 带 --force：覆盖 + 仍然只输出一行
	out3, code := capture(func() int { return cmdGenConfig(target, true) })
	if code != 0 {
		t.Fatalf("退出码 = %d", code)
	}
	assertNoYAML("--force", out3)
	if n := len(strings.Split(strings.TrimRight(out3, "\n"), "\n")); n != 1 {
		t.Errorf("--force 应只输出 1 行，实际 %d 行:\n%s", n, out3)
	}
	if !strings.Contains(out3, "已覆盖配置") {
		t.Errorf("--force 的提示语不对: %q", out3)
	}
	forced, _ := os.ReadFile(target)
	if strings.Contains(string(forced), "我改过的") {
		t.Errorf("--force 应覆盖旧内容")
	}
	if string(forced) != full {
		t.Errorf("--force 后落盘内容应为完整模板")
	}
}

// gen-config 带 --force 时，标志必须真的被解析到（曾经因为 flag 在
// 第一个非标志参数处停止而被丢掉，导致 --force 静默失效）。
func TestGenConfigForceFlagParsed(t *testing.T) {
	cmd, flagArgs, err := parseCommand([]string{"gen-config", "--force"})
	if err != nil {
		t.Fatal(err)
	}
	if cmd != "gen-config" {
		t.Errorf("命令 = %q", cmd)
	}
	if len(flagArgs) != 1 || flagArgs[0] != "--force" {
		t.Errorf("--force 没有传给 flag 解析: %v", flagArgs)
	}
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	f := fs.Bool("force", false, "")
	if err := fs.Parse(flagArgs); err != nil {
		t.Fatal(err)
	}
	if !*f {
		t.Errorf("--force 未被解析为 true")
	}
}

// ---------------------------------------------------------------- 极简配置格式

func TestLoadConfigMinimal(t *testing.T) {
	// 这是生成的默认配置去掉注释后的等价形式：用户只需改 token/zone/dns
	p := writeTempConfig(t, `
token: tok_abc
zone: example.com
dns:
  - pc
`)
	cfg, err := loadConfig(p)
	if err != nil {
		t.Fatalf("极简配置装载失败: %v", err)
	}
	if err := cfg.validate(); err != nil {
		t.Fatalf("极简配置应通过校验: %v", err)
	}
	if len(cfg.Domains) != 1 {
		t.Fatalf("应得到 1 个域名，实际 %d", len(cfg.Domains))
	}
	d := cfg.Domains[0]
	if d.Zone != "example.com" {
		t.Errorf("zone = %q", d.Zone)
	}
	if len(d.Records) != 1 || d.Records[0].Name != "pc" {
		t.Fatalf("records 解析错误: %+v", d.Records)
	}
	// 没写 type 时默认 both
	if d.Records[0].Type != "BOTH" {
		t.Errorf("默认类型应为 BOTH，实际 %q", d.Records[0].Type)
	}
	if d.Records[0].TTL != 1 {
		t.Errorf("默认 ttl 应为 1，实际 %d", d.Records[0].TTL)
	}
	if !enabledFor(cfg, "ipv4") || !enabledFor(cfg, "ipv6") {
		t.Errorf("未配置探测时应两个协议都启用")
	}
}

func TestRecordShorthand(t *testing.T) {
	p := writeTempConfig(t, `
token: tok
zone: a.com
dns:
  - pc: AAAA
  - nas
  - x: ipv4
  - "*": both
  - name: www
    type: A
    ttl: 300
    proxy: true
`)
	cfg, err := loadConfig(p)
	if err != nil {
		t.Fatalf("装载失败: %v", err)
	}
	rs := cfg.Domains[0].Records
	if len(rs) != 5 {
		t.Fatalf("应有 5 条，实际 %d: %+v", len(rs), rs)
	}
	want := []struct{ name, typ string }{
		{"pc", "AAAA"}, {"nas", "BOTH"}, {"x", "A"}, {"*", "BOTH"}, {"www", "A"},
	}
	for i, w := range want {
		if rs[i].Name != w.name || rs[i].Type != w.typ {
			t.Errorf("第 %d 条 = (%q,%q)，期望 (%q,%q)", i+1, rs[i].Name, rs[i].Type, w.name, w.typ)
		}
	}
	if rs[4].TTL != 300 || rs[4].Proxy == nil || !*rs[4].Proxy {
		t.Errorf("完整写法的高级字段丢失: %+v", rs[4])
	}
}

func TestGlobalTypeDefault(t *testing.T) {
	p := writeTempConfig(t, `
token: tok
zone: a.com
type: AAAA
dns:
  - pc
  - nas: A
`)
	cfg, err := loadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	rs := cfg.Domains[0].Records
	if rs[0].Type != "AAAA" {
		t.Errorf("顶层 type 未生效: %q", rs[0].Type)
	}
	if rs[1].Type != "A" {
		t.Errorf("记录级 type 应覆盖顶层: %q", rs[1].Type)
	}
}

func TestInlineListAndMultipleZones(t *testing.T) {
	p := writeTempConfig(t, `
token: tok
zone: a.com
dns: [pc, nas: AAAA]
`)
	cfg, err := loadConfig(p)
	if err != nil {
		t.Fatalf("行内列表装载失败: %v", err)
	}
	rs := cfg.Domains[0].Records
	if len(rs) != 2 || rs[0].Name != "pc" || rs[1].Type != "AAAA" {
		t.Errorf("行内列表解析错误: %+v", rs)
	}
	if rs[0].Type != "BOTH" {
		t.Errorf("无类型条目应默认 BOTH: %q", rs[0].Type)
	}

	// zone + domains 混用：各自独立，互不影响
	p2 := writeTempConfig(t, `
token: tok
zone: a.com
dns:
  - pc
domains:
  b.com:
    records:
      - name: nas
        type: A
`)
	cfg2, err := loadConfig(p2)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg2.Domains) != 2 {
		t.Fatalf("应有 2 个域名，实际 %d", len(cfg2.Domains))
	}
	// domains 按字典序先加入，zone 的合并进已有项或追加
	byZone := map[string]Domain{}
	for _, d := range cfg2.Domains {
		byZone[d.Zone] = d
	}
	if len(byZone["a.com"].Records) != 1 || len(byZone["b.com"].Records) != 1 {
		t.Errorf("两个域名各自记录数不对: %+v", byZone)
	}

	// 同一个 zone 同时用两种写法 → 记录合并
	p3 := writeTempConfig(t, `
token: tok
zone: a.com
dns:
  - pc
domains:
  a.com:
    records:
      - name: nas
        type: A
`)
	cfg3, err := loadConfig(p3)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg3.Domains) != 1 || len(cfg3.Domains[0].Records) != 2 {
		t.Errorf("同 zone 应合并记录，实际 %+v", cfg3.Domains)
	}
}

func TestTypeAliasesAndErrors(t *testing.T) {
	// 大小写与中文别名
	for _, tc := range []struct{ in, want string }{
		{"aaaa", "AAAA"}, {"IPv6", "AAAA"}, {"6", "AAAA"},
		{"A", "A"}, {"ipv4", "A"}, {"4", "A"},
		{"both", "BOTH"}, {"双栈", "BOTH"}, {"all", "BOTH"},
	} {
		got, err := normalizeType(tc.in)
		if err != nil || got != tc.want {
			t.Errorf("normalizeType(%q) = (%q,%v)，期望 %q", tc.in, got, err, tc.want)
		}
	}
	for _, bad := range []string{"CNAME", "mx", "txt"} {
		if _, err := normalizeType(bad); err == nil {
			t.Errorf("normalizeType(%q) 应报错", bad)
		}
	}

	// 非法类型要报错而不是静默当名字处理
	p := writeTempConfig(t, "token: t\nzone: a.com\ndns:\n  - pc: CNAME\n")
	if _, err := loadConfig(p); err == nil {
		t.Errorf("非法类型应报错")
	}
	// 写了 dns 却没写 zone
	p2 := writeTempConfig(t, "token: t\ndns:\n  - pc\n")
	if _, err := loadConfig(p2); err == nil || !strings.Contains(err.Error(), "zone") {
		t.Errorf("缺 zone 应报错，实际 %v", err)
	}
	// dns 写成映射
	p3 := writeTempConfig(t, "token: t\nzone: a.com\ndns:\n  pc: both\n")
	if _, err := loadConfig(p3); err == nil {
		t.Errorf("dns 写成映射应报错")
	}
}

// 旧版配置必须继续可用，否则升级会打断已在运行的部署
func TestLegacyConfigStillWorks(t *testing.T) {
	p := writeTempConfig(t, `
token: tok_old
domains:
  example.com:
    records:
      - name: pc
        type: both
        ttl: 1
        proxy: false
`)
	cfg, err := loadConfig(p)
	if err != nil {
		t.Fatalf("旧配置应继续可用: %v", err)
	}
	if err := cfg.validate(); err != nil {
		t.Fatalf("旧配置应通过校验: %v", err)
	}
	if len(cfg.Domains) != 1 || cfg.Domains[0].Zone != "example.com" {
		t.Fatalf("旧格式 zone 解析错误: %+v", cfg.Domains)
	}
	r := cfg.Domains[0].Records[0]
	if r.Name != "pc" || r.Type != "BOTH" || r.Proxy == nil || *r.Proxy {
		t.Errorf("旧格式记录解析错误: %+v", r)
	}
}

// ---------------------------------------------------------------- 配置装载

func writeTempConfig(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadConfigFull(t *testing.T) {
	p := writeTempConfig(t, `
token: "tok_123"
api_base: "https://cf.example/api"
ipv4:
  enabled: true
  source: auto
ipv6:
  enabled: false
  source: iface
domains:
  example.com:
    records:
      - name: pc
        type: AAAA
        proxy: true
      - name: "@"
        type: A
        ttl: 300
      - name: wild
        type: both
        ipv4: false
        ipv6: true
  other.com:
    zone: zzz-other.com
    records:
      - name: a
        type: both
`)
	cfg, err := loadConfig(p)
	if err != nil {
		t.Fatalf("装载失败: %v", err)
	}
	if cfg.Token != "tok_123" || cfg.APIBase != "https://cf.example/api" {
		t.Errorf("顶层字段错误: %+v", cfg)
	}
	if !enabledFor(cfg, "ipv4") || cfg.IPv4.Source != "auto" {
		t.Errorf("ipv4 解析错误: %+v", cfg.IPv4)
	}
	if enabledFor(cfg, "ipv6") || cfg.IPv6.Source != "iface" {
		t.Errorf("ipv6 解析错误: %+v", cfg.IPv6)
	}
	if len(cfg.Domains) != 2 {
		t.Fatalf("应有 2 个 zone，实际 %d", len(cfg.Domains))
	}
	// 按 key 取用而不是依赖切片顺序：zone 的排序是内部实现细节，
	// 测试不该因为换了示例域名就失败。
	byKey := map[string]Domain{}
	for _, d := range cfg.Domains {
		byKey[d.Zone] = d
	}
	if got := byKey["zzz-other.com"]; got.Zone != "zzz-other.com" {
		t.Errorf("domains.other.com 的 zone 覆盖失败: %+v", got)
	}
	tg, ok := byKey["example.com"]
	if !ok || len(tg.Records) != 3 {
		t.Fatalf("example.com 解析错误: %+v", cfg.Domains)
	}
	if tg.Records[0].Type != "AAAA" || tg.Records[0].Proxy == nil || !*tg.Records[0].Proxy {
		t.Errorf("proxy=true 未生效: %+v", tg.Records[0])
	}
	if tg.Records[1].TTL != 300 {
		t.Errorf("ttl=300 未生效: %d", tg.Records[1].TTL)
	}
	r2 := tg.Records[2]
	if r2.IPv4 == nil || *r2.IPv4 || r2.IPv6 == nil || !*r2.IPv6 {
		t.Errorf("记录级协议开关未生效: %+v", r2)
	}
	// type=both 且 ipv4=false → 只应产生 AAAA
	fams := recordFams(&r2)
	if len(fams) != 2 {
		t.Errorf("recordFams(both) 应返回两个协议: %v", fams)
	}
}

func TestLoadConfigDefaults(t *testing.T) {
	p := writeTempConfig(t, `
token: "t"
domains:
  a.com:
    records:
      - name: x
`)
	cfg, err := loadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.APIBase != defaultAPIBase {
		t.Errorf("api_base 默认值错误: %q", cfg.APIBase)
	}
	if !enabledFor(cfg, "ipv4") || !enabledFor(cfg, "ipv6") {
		t.Errorf("未写 ipv4/ipv6 时应默认启用")
	}
	r := cfg.Domains[0].Records[0]
	if r.Type != "BOTH" || r.TTL != 1 || r.Proxy != nil {
		t.Errorf("默认值错误: %+v", r)
	}
	// applyDefaults 不应把未配置的协议改成禁用
	raw := &Config{}
	raw.applyDefaults()
	if !enabledFor(raw, "ipv4") || !enabledFor(raw, "ipv6") {
		t.Errorf("applyDefaults 不应禁用协议")
	}
}

func TestLoadConfigErrors(t *testing.T) {
	cases := []struct{ name, body, want string }{
		{"ttl 越界", "token: t\ndomains:\n  a.com:\n    records:\n      - name: x\n        ttl: 30\n", "ttl"},
		{"type 非法", "token: t\ndomains:\n  a.com:\n    records:\n      - name: x\n        type: CNAME\n", "type"},
		{"records 不是列表", "token: t\ndomains:\n  a.com:\n    records:\n      name: x\n", "列表"},
		{"布尔值非法", "token: t\nipv4:\n  enabled: maybe\n", "布尔"},
		{"整数非法", "token: t\ndomains:\n  a.com:\n    records:\n      - name: x\n        ttl: abc\n", "整数"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadConfig(writeTempConfig(t, tc.body))
			if err == nil {
				t.Fatalf("期望报错")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("错误 %q 未包含 %q", err.Error(), tc.want)
			}
		})
	}
}

func TestValidate(t *testing.T) {
	yes := true
	base := &Config{
		Token:   "tok",
		IPv4:    Detection{Enabled: &yes},
		Domains: []Domain{{Zone: "a.com", Records: []Record{{Name: "x", Type: "both", TTL: 1}}}},
	}
	if err := base.validate(); err != nil {
		t.Errorf("合法配置不应报错: %v", err)
	}
	c1 := *base
	c1.Token = "在这里填写你的 Cloudflare API 令牌"
	if err := c1.validate(); err == nil || !strings.Contains(err.Error(), "token") {
		t.Errorf("占位令牌应被识别: %v", err)
	}
	c2 := *base
	c2.Domains = nil
	if err := c2.validate(); err == nil {
		t.Errorf("空 domains 应报错")
	}
	no := false
	c3 := *base
	c3.IPv4.Enabled, c3.IPv6.Enabled = &no, &no
	if err := c3.validate(); err == nil || !strings.Contains(err.Error(), "ipv4") {
		t.Errorf("两个协议都关闭应报错: %v", err)
	}
}

// ---------------------------------------------------------------- 名称/键/状态

func TestFQDN(t *testing.T) {
	cases := []struct{ name, zone, want string }{
		{"pc", "example.com", "pc.example.com"},
		{"@", "example.com", "example.com"},
		{"", "example.com", "example.com"},
		{"pc.example.com", "example.com", "pc.example.com"},
		{"a.b", "example.com", "a.b.example.com"},
		{"example.com", "example.com", "example.com"},
		{"*", "example.com", "*.example.com"},
	}
	for _, tc := range cases {
		if got := fqdn(tc.name, tc.zone); got != tc.want {
			t.Errorf("fqdn(%q,%q) = %q，期望 %q", tc.name, tc.zone, got, tc.want)
		}
	}
}

func TestStateKeyAndRoundTrip(t *testing.T) {
	// 同一记录的不同大小写写法必须映射到同一个状态键
	k1 := stateKey("Example.COM", "PC.Example.Com", "aaaa")
	k2 := stateKey("example.com", "pc.example.com", "AAAA")
	if k1 != k2 {
		t.Errorf("状态键应大小写无关: %q vs %q", k1, k2)
	}
	st := State{Records: map[string]string{k1: "2001:db8::1"}, Zones: map[string]string{"example.com": "zid"}}
	if v, ok := st.getRecord(k2); !ok || v != "2001:db8::1" {
		t.Errorf("getRecord 失败")
	}
	if v, ok := st.getZone("EXAMPLE.COM"); !ok || v != "zid" {
		t.Errorf("getZone 应大小写无关: %v %v", v, ok)
	}

	dir := t.TempDir()
	p := filepath.Join(dir, "state.json")
	if err := saveState(p, &st); err != nil {
		t.Fatal(err)
	}
	got := loadState(p)
	if got.Records[k1] != "2001:db8::1" || got.Zones["example.com"] != "zid" {
		t.Errorf("状态往返失败: %+v", got)
	}
	// 文件不存在时应返回可用的空状态而不是 panic
	empty := loadState(filepath.Join(dir, "nope.json"))
	if empty.Records == nil || empty.Zones == nil {
		t.Errorf("空状态必须已初始化，否则写入会 panic")
	}
	empty.setRecord("a", "b")
	empty.setZone("z", "i")
}

func TestRecordMatches(t *testing.T) {
	// 用文档保留地址 2001:db8::/32，并保留大小写混合以真正验证「忽略大小写」
	// （十六进制里的 a-f 必须大小写都有，否则这条断言没有意义）
	r := &cfRecord{Content: "2001:0DB8:1A2B:3C4D:D2D9:7363:B187:7201"}
	if !r.matches("2001:0db8:1a2b:3c4d:d2d9:7363:b187:7201") {
		t.Errorf("IPv6 比较应忽略大小写")
	}
	if !r.matches(" 2001:0db8:1a2b:3c4d:d2d9:7363:b187:7201 ") {
		t.Errorf("比较应忽略首尾空白")
	}
	if r.matches("2001:0db8:1a2b:3c4d:d2d9:7363:b187:7202") {
		t.Errorf("不同地址不应相等")
	}
}

// ---------------------------------------------------------------- 时间与显示宽度

func TestDurationParse(t *testing.T) {
	good := map[string]int64{
		"30s": 30, "5m": 300, "1h": 3600, "2d": 172800,
		"90分钟": 5400, "1天": 86400,
	}
	for in, sec := range good {
		d, err := durationParse(in)
		if err != nil {
			t.Errorf("durationParse(%q) 报错: %v", in, err)
			continue
		}
		if int64(d.Seconds()) != sec {
			t.Errorf("durationParse(%q) = %v，期望 %ds", in, d, sec)
		}
	}
	for _, bad := range []string{"", "90", "5x", "abc", "m5"} {
		if _, err := durationParse(bad); err == nil {
			t.Errorf("durationParse(%q) 应报错", bad)
		}
	}
}

func TestDisplayWidth(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"", 0},
		{"abc", 3},
		{"待更新", 6},
		{"已更新", 6},
		{"2001:db8:1a2b:3c4d:d2d9:7363:b187:7201", 38},
		{"去重", 4},
		{"a→b", 4}, // 箭头按双宽计
	}
	for _, tc := range cases {
		if got := displayWidth(tc.in); got != tc.want {
			t.Errorf("displayWidth(%q) = %d，期望 %d", tc.in, got, tc.want)
		}
	}
	if got := padRight("待更新", 8); displayWidth(got) != 8 {
		t.Errorf("padRight 后宽度 = %d，期望 8", displayWidth(got))
	}
	if got := padRight("abcdef", 3); got != "abcdef" {
		t.Errorf("超长不应截断: %q", got)
	}
	if got := truncDisplay("abcdef", 4); got != "abc…" {
		t.Errorf("truncDisplay = %q", got)
	}
	if got := truncDisplay("中文测试", 5); displayWidth(got) > 5 {
		t.Errorf("truncDisplay 超宽: %q (%d)", got, displayWidth(got))
	}
	if got := truncDisplay("短", 10); got != "短" {
		t.Errorf("未超宽不应截断: %q", got)
	}
}

func TestRedact(t *testing.T) {
	// 用明显是假的令牌，避免任何真实凭据进入仓库。
	// 刻意用与真实 cfat_ 令牌不同的长度，这样仓库里的凭据扫描规则
	// （cfat_[A-Za-z0-9]{20,}）可以保持严格而不必为测试开例外。
	tok := "cfat_FAKE_AbCdEfGhIjKlMn"
	got := redact(tok)
	if strings.Contains(got, "AbCdEfGh") {
		t.Errorf("脱敏后仍暴露中段: %q", got)
	}
	if !strings.HasPrefix(got, "cfat_FAKE") || !strings.HasSuffix(got, "lMn") {
		t.Errorf("脱敏格式异常: %q", got)
	}
	if strings.Contains(got, tok) {
		t.Errorf("脱敏后不应保留完整令牌")
	}
	if redact("") != "(空)" {
		t.Errorf("空令牌提示异常")
	}
}

// ---------------------------------------------------------------- 探测源解析

func TestMatchText(t *testing.T) {
	text := `
2: eth1    inet6 2001:db8:1a2b:3c4d:807b:2a6:2d93:75d1/64 scope global
3: eth1    inet6 fd00:1234:5678::1/128 scope global
    inet6 fe80::1/64 scope link
`
	// 默认应跳过 ULA 与链接本地，取到全局单播
	got, err := matchText(text, "ipv6", "", nil)
	if err != nil {
		t.Fatalf("matchText 报错: %v", err)
	}
	if got != "2001:db8:1a2b:3c4d:807b:2a6:2d93:75d1" {
		t.Errorf("取到 %q", got)
	}

	// 显式排除该网段后应失败
	if _, err := matchText(text, "ipv6", "", []string{"2001:db8:1a2b"}); err == nil {
		t.Errorf("skip_prefix 未生效")
	}

	// 正则优先
	got, err = matchText("ip: 203.0.113.45\n", "ipv4", `ip:\s*(\S+)`, nil)
	if err != nil || got != "203.0.113.45" {
		t.Errorf("正则提取失败: %q %v", got, err)
	}

	if _, err := matchText("no address here", "ipv6", "", nil); err == nil {
		t.Errorf("无地址时应报错")
	}
}

func TestRecordFams(t *testing.T) {
	cases := []struct {
		typ  string
		want []string
	}{
		{"A", []string{"ipv4"}},
		{"AAAA", []string{"ipv6"}},
		{"both", []string{"ipv4", "ipv6"}},
		{"", []string{"ipv4", "ipv6"}},
	}
	for _, tc := range cases {
		got := recordFams(&Record{Type: tc.typ})
		if strings.Join(got, ",") != strings.Join(tc.want, ",") {
			t.Errorf("recordFams(%q) = %v，期望 %v", tc.typ, got, tc.want)
		}
	}
}

func TestEnabledFor(t *testing.T) {
	yes, no := true, false
	c := &Config{IPv4: Detection{Enabled: &yes}, IPv6: Detection{Enabled: &no}}
	if !enabledFor(c, "ipv4") {
		t.Errorf("ipv4 应启用")
	}
	if enabledFor(c, "ipv6") {
		t.Errorf("ipv6 应禁用")
	}
	// 未配置的协议默认启用；显式 enabled: false 必须生效
	n, err := parseYAMLSubset([]byte("ipv6:\n  enabled: false\nipv4:\n  enabled: true\n"))
	if err != nil {
		t.Fatal(err)
	}
	var d Detection
	if err := parseDetection(n.get("ipv6"), &d, "ipv6"); err != nil {
		t.Fatal(err)
	}
	if enabledFor(&Config{IPv6: d}, "ipv6") {
		t.Errorf("enabled: false 未生效")
	}
	var d2 Detection
	if err := parseDetection(nil, &d2, "ipv6"); err != nil {
		t.Fatal(err)
	}
	if !enabledFor(&Config{IPv6: d2}, "ipv6") {
		t.Errorf("未配置的协议应默认启用")
	}
}
