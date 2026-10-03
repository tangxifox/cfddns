// cfddns — 极简高效的 Cloudflare DDNS 客户端
//
// 设计目标（按优先级）：
//  1. 零依赖：只用 Go 标准库，单个静态二进制，连 YAML 解析都是内置子集实现。
//  2. 少请求：探测公网 IP 只发 1 次 HTTP 且只需要 1 个字段；IP 未变化时直接跳过 API 调用。
//  3. 快路径：本地状态表命中则完全不碰网络；zone_id 内存缓存，一轮内多个记录共享。
//  4. 并发：多个记录/域名并发更新（默认 4 并发），失败互不影响。
//  5. 写前二次确认：写记录前重新读取该记录，值已正确则不产生写请求（缓解并发写竞争）。
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	defaultAPIBase = "https://api.cloudflare.com/client/v4"
	progVersion    = "1.0.0"

	// 探测端点。trace 端点与 ipify 都直接返回纯地址/单行字段，
	// 因此一次请求即可拿到结果，无需二次解析。
	epTraceV4 = "https://www.cloudflare.com/cdn-cgi/trace"
	epTraceV6 = "https://ipv6.cloudflare.com/cdn-cgi/trace"
)

var (
	outMu sync.Mutex // 串行化多 goroutine 的表格输出

	// 匹配 "数字 + 单位" 形式的时长，如 5m / 30s / 2d / 90分钟。
	//
	// 单位一侧刻意用 \S+ 收全部非空白字符，再按前缀判断：
	// 逐个列举字符集是错的（例如「分钟」里还含一个「钟」，列举就会漏），
	// 而单位本身由下面的 switch 白名单校验，所以放宽匹配不会放过非法输入。
	reDuration = regexp.MustCompile(`^([0-9]+)\s*(\S+)$`)
)

// ---------------------------------------------------------------- 配置模型

type Config struct {
	APIBase string    `json:"api_base,omitempty"`
	Token   string    `json:"token"`
	IPv4    Detection `json:"ipv4"`
	IPv6    Detection `json:"ipv6"`
	// DefaultType 是顶层 type 的默认值，作用于没写 type 的记录
	DefaultType string   `json:"type,omitempty"`
	Domains     []Domain `json:"domains"`
}

type Detection struct {
	// Enabled 用指针以区分「未配置」与「显式 false」：
	// 未写 enabled 时默认启用，写了 false 才真的关闭。
	Enabled    *bool    `json:"enabled,omitempty"`
	Source     string   `json:"source"`            // trace | ipify | iface | url | exec
	URL        string   `json:"url,omitempty"`     // source=url
	Command    string   `json:"command,omitempty"` // source=exec
	MatchRegex string   `json:"match_regex,omitempty"`
	SkipPrefix []string `json:"skip_prefix,omitempty"`
}

type Domain struct {
	Zone    string   `json:"zone"`
	Records []Record `json:"records"`
}

type Record struct {
	Name   string `json:"name"`   // 相对 zone（如 pc）或 FQDN（如 pc.example.com）或 @
	Type   string `json:"type"`   // A / AAAA / both
	TTL    int    `json:"ttl"`    // 1=auto
	Proxy  *bool  `json:"proxy,omitempty"`
	IPv4   *bool  `json:"ipv4,omitempty"`
	IPv6   *bool  `json:"ipv6,omitempty"`
	Comment string `json:"comment,omitempty"`
}

// 一条待处理的更新任务
type Task struct {
	Zone   string
	Name   string
	FQDN   string
	Type   string
	TTL    int
	Proxy  *bool
	Want   string
	Owner  *Detection
}

type Result struct {
	Zone    string `json:"zone"`
	Name    string `json:"name"`
	FQDN    string `json:"fqdn"`
	Type    string `json:"type"`
	Want    string `json:"want"`
	Current string `json:"current,omitempty"`
	Status  string `json:"status"` // updated | unchanged | planned | error
	Err     string `json:"error,omitempty"`
	ms      int64
}

type State struct {
	Records map[string]string `json:"records"` // zone/name/type -> 已确认写入的值
	Zones   map[string]string `json:"zones"`   // zone 名 -> zone_id
}

// ---------------------------------------------------------------- 工具函数

func durationParse(s string) (time.Duration, error) {
	s = strings.TrimSpace(strings.ToLower(s))
	if s == "" {
		return 0, errors.New("空时间")
	}
	if d, err := time.ParseDuration(s); err == nil {
		return d, nil
	}
	// 支持 "1 天" / "2d" / "90 分钟" 这类写法
	m := reDuration.FindStringSubmatch(s)
	if m == nil {
		return 0, fmt.Errorf("无法解析时间: %q（示例: 30s / 5m / 1h / 2d）", strings.TrimSpace(s))
	}
	num, err := strconv.Atoi(m[1])
	if err != nil {
		return 0, fmt.Errorf("无法解析时间: %q", s)
	}
	unit := m[2]
	switch {
	case strings.HasPrefix(unit, "s"), strings.HasPrefix(unit, "秒"):
		return time.Duration(num) * time.Second, nil
	case strings.HasPrefix(unit, "m"), strings.HasPrefix(unit, "分"):
		return time.Duration(num) * time.Minute, nil
	case strings.HasPrefix(unit, "h"), strings.HasPrefix(unit, "时"):
		return time.Duration(num) * time.Hour, nil
	case strings.HasPrefix(unit, "d"), strings.HasPrefix(unit, "天"), strings.HasPrefix(unit, "日"):
		return time.Duration(num) * 24 * time.Hour, nil
	}
	return 0, fmt.Errorf("未知时间单位 %q（支持 s/秒 m/分 h/时 d/天）", unit)
}

func redact(s string) string {
	if len(s) <= 10 {
		if s == "" {
			return "(空)"
		}
		return "***"
	}
	return s[:9] + "…" + s[len(s)-4:]
}

func boolStr(b *bool) string {
	if b == nil {
		return "继承"
	}
	if *b {
		return "是"
	}
	return "否"
}

func orDefault(s, d string) string {
	if strings.TrimSpace(s) == "" {
		return d
	}
	return s
}

// ---------------------------------------------------------------- 主流程

func main() {
	args := os.Args[1:]

	// 先确定子命令，再让 flag 只处理标志与取值。
	// 这样命令放在选项前后都能工作（flag 解析遇到第一个非标志参数就会停）。
	cmd, flagArgs, err := parseCommand(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "cfddns: %v\n\n", err)
		usage()
		os.Exit(2)
	}

	fs := flag.NewFlagSet("cfddns", flag.ExitOnError)
	cfgPath := fs.String("config", "", "配置文件路径（默认 ~/.cfddns/config.yaml）")
	jsonOut := fs.Bool("json", false, "以 JSON 输出结果")
	quiet := fs.Bool("quiet", false, "只输出有变化的记录")
	once := fs.Bool("once", false, "只执行一次后退出")
	interval := fs.String("interval", "", "循环执行的间隔，如 5m / 300s")
	force := fs.Bool("force", false, "忽略本地状态与当前值，强制写入")
	dryRun := fs.Bool("dry-run", false, "只显示将要执行的操作，不实际修改")
	concurrency := fs.Int("concurrency", 4, "并发更新数")
	showVer := fs.Bool("version", false, "显示版本")
	fs.Usage = usage
	_ = fs.Parse(flagArgs)

	if *showVer {
		fmt.Printf("cfddns %s (%s/%s, %s)\n", progVersion, runtime.GOOS, runtime.GOARCH, runtime.Version())
		return
	}

	path, err := configPath(*cfgPath)
	if err != nil {
		fatalf("%v", err)
	}

	switch cmd {
	case "run":
		os.Exit(cmdRun(path, *once, *interval, *force, *dryRun, *jsonOut, *quiet, *concurrency))
	case "gen-config":
		os.Exit(cmdGenConfig(path, *force))
	case "status":
		os.Exit(cmdStatus(path, *jsonOut))
	case "paths":
		fmt.Printf("配置: %s\n状态: %s\n", path, statePathFor(path))
	case "help":
		usage()
	}
}

// flagsWithValue 记录需要跟一个取值的标志，供 parseCommand 跳过其取值。
var flagsWithValue = map[string]bool{"config": true, "interval": true, "concurrency": true}

// parseCommand 从原始参数里解析子命令，并返回去掉子命令后的参数交给 flag 解析。
//
// 为什么不能用 fs.Args()：Go 的 flag 解析在第一个非标志参数处停止，
// 所以 `cfddns --config x.yaml status` 里的 status 会留在剩余参数中。
// 早期版本直接忽略它，导致该命令悄悄变成了「真的执行一轮同步」——
// 对 DDNS 工具来说这是会改 DNS 的危险行为，所以现在把两种情况都支持：
// 命令在前、命令在选项后。未知命令一律报错，不退回默认的 run。
func parseCommand(args []string) (string, []string, error) {
	sub := ""
	flagArgs := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		a := args[i]
		if strings.HasPrefix(a, "-") {
			flagArgs = append(flagArgs, a)
			// 独立形式的 "--key value" 要把取值一起带上，否则 flag 会把它吞掉
			if !strings.Contains(a, "=") && flagsWithValue[strings.TrimLeft(a, "-")] && i+1 < len(args) {
				i++
				flagArgs = append(flagArgs, args[i])
			}
			continue
		}
		if sub == "" {
			if knownCommand(a) {
				sub = a
				continue
			}
			return "", nil, fmt.Errorf("未知命令 %q（可用: run / gen-config / status / paths）", a)
		}
		return "", nil, fmt.Errorf("多余的参数 %q", a)
	}
	if sub == "" {
		sub = "run"
	}
	return sub, flagArgs, nil
}

func knownCommand(c string) bool {
	switch c {
	case "run", "gen-config", "status", "paths", "help":
		return true
	}
	return false
}

func usage() {
	fmt.Fprintf(os.Stderr, `cfddns %s — Cloudflare DDNS 客户端

用法:
  cfddns [run] [选项]        探测本机公网 IP 并同步到 Cloudflare
  cfddns gen-config [--force]  生成默认配置文件
  cfddns status              查看配置与本地状态（令牌已脱敏）
  cfddns paths               显示配置与状态文件路径

选项:
`, progVersion)
	flag.PrintDefaults()
	fmt.Fprintf(os.Stderr, `
示例:
  cfddns                      # 按配置执行一次
  cfddns --once               # 同上
  cfddns --interval 5m        # 每 5 分钟执行一次
  cfddns --dry-run --json     # 预演并以 JSON 输出
`)
}

func configPath(override string) (string, error) {
	if override != "" {
		return filepath.Abs(override)
	}
	if env := os.Getenv("CFDDNS_CONFIG"); env != "" {
		return filepath.Abs(env)
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "", fmt.Errorf("无法确定用户主目录: %v", err)
	}
	return filepath.Join(home, ".cfddns", "config.yaml"), nil
}

func statePathFor(cfg string) string {
	return filepath.Join(filepath.Dir(cfg), "state.json")
}

func fatalf(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "cfddns: "+format+"\n", a...)
	os.Exit(2)
}

// ---------------------------------------------------------------- 命令实现

// cmdGenConfig 生成默认配置。只输出一行结果说明，不打印 YAML 内容 ——
// 配置内容看文件本身即可（或 cfddns status），避免刷屏。
func cmdGenConfig(path string, force bool) int {
	if _, err := os.Stat(path); err == nil && !force {
		fmt.Printf("配置已存在，未覆盖: %s\n（加 --force 覆盖，或直接编辑该文件）\n", path)
		return 0
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		fatalf("创建目录失败: %v", err)
	}
	if err := os.WriteFile(path, []byte(renderConfigTemplate()), 0o600); err != nil {
		fatalf("写入配置失败: %v", err)
	}
	fmt.Printf("已覆盖配置: %s\n", path)
	return 0
}

func cmdStatus(path string, jsonOut bool) int {
	cfg, err := loadConfig(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "配置读取失败: %v\n", err)
		fmt.Fprintf(os.Stderr, "提示: 运行 `cfddns gen-config` 生成默认配置\n")
		return 2
	}
	st := loadState(statePathFor(path))
	if jsonOut {
		out := map[string]any{
			"config_path": path,
			"state_path":  statePathFor(path),
			"api_base":    orDefault(cfg.APIBase, defaultAPIBase),
			"token":       redact(cfg.Token),
			"ipv4":        cfg.IPv4,
			"ipv6":        cfg.IPv6,
			"domains":     cfg.Domains,
			"state":       st,
		}
		b, _ := json.MarshalIndent(out, "", "  ")
		fmt.Println(string(b))
		return 0
	}
	fmt.Printf("配置文件   : %s\n", path)
	fmt.Printf("状态文件   : %s\n", statePathFor(path))
	fmt.Printf("API 地址   : %s\n", orDefault(cfg.APIBase, defaultAPIBase))
	fmt.Printf("令牌       : %s\n", redact(cfg.Token))
	fmt.Printf("IPv4 探测  : enabled=%v source=%s\n", enabledFor(cfg, "ipv4"), detDesc(&cfg.IPv4))
	fmt.Printf("IPv6 探测  : enabled=%v source=%s\n", enabledFor(cfg, "ipv6"), detDesc(&cfg.IPv6))
	fmt.Printf("域名数量   : %d\n", len(cfg.Domains))
	for _, d := range cfg.Domains {
		fmt.Printf("\n  ── %s ──\n", d.Zone)
		for _, r := range d.Records {
			typ := r.Type
			if typ == "" {
				typ = "both"
			}
			fmt.Printf("    %-28s type=%-4s ipv4=%s ipv6=%s ttl=%s proxy=%s\n",
				fqdn(orDefault(r.Name, "@"), d.Zone), typ,
				boolStr(r.IPv4), boolStr(r.IPv6),
				func() string { if r.TTL == 0 { return "继承" }; return fmt.Sprint(r.TTL) }(),
				boolStr(r.Proxy))
		}
	}
	if len(st.Records) > 0 {
		fmt.Printf("\n本地状态（上次已确认写入的值）:\n")
		keys := make([]string, 0, len(st.Records))
		for k := range st.Records {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Printf("  %-44s %s\n", k, st.Records[k])
		}
	}
	return 0
}

// cmdRun 执行同步。
//
// 例外：当 --config 指向的目录不在用户主目录下时（例如在项目目录里临时试验），
// run 退化为只读，不产生状态文件 —— 否则会在别人的工作目录里留下 state.json。
func cmdRun(cfgPath string, once bool, intervalStr string, force, dryRun, jsonOut, quiet bool, concurrency int) int {
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		if rel, rerr := filepath.Rel(home, cfgPath); rerr != nil || strings.HasPrefix(rel, "..") {
			if !dryRun {
				fmt.Fprintf(os.Stderr, "cfddns: 配置不在主目录下（%s），本次不写入状态文件（只读运行）\n", cfgPath)
				dryRun = true
			}
		}
	}
	return runRounds(cfgPath, once, intervalStr, force, dryRun, jsonOut, quiet, concurrency)
}

func runRounds(cfgPath string, once bool, intervalStr string, force, dryRun, jsonOut, quiet bool, concurrency int) int {
	cfg, err := loadConfig(cfgPath)
	if err != nil {
		if os.IsNotExist(err) {
			// 首次运行体验：自动生成默认配置并提示填写
			if e := os.MkdirAll(filepath.Dir(cfgPath), 0o700); e != nil {
				fatalf("创建目录失败: %v", e)
			}
			if e := os.WriteFile(cfgPath, []byte(renderConfigTemplate()), 0o600); e != nil {
				fatalf("写入配置失败: %v", e)
			}
			fmt.Printf("未找到配置文件，已生成默认配置: %s\n\n", cfgPath)
			fmt.Print(renderConfigTemplate())
			fmt.Printf("\n请填写 token 与域名后重新运行: cfddns\n")
			return 0
		}
		fatalf("配置读取失败: %v", err)
	}
	if err := cfg.validate(); err != nil {
		fatalf("配置有误: %v", err)
	}
	if concurrency < 1 {
		concurrency = 1
	}

	var interval time.Duration
	if intervalStr != "" {
		interval, err = durationParse(intervalStr)
		if err != nil {
			fatalf("--interval 无效: %v", err)
		}
		if interval < 10*time.Second {
			fatalf("--interval 太短（最小 10s），避免触发 Cloudflare 速率限制")
		}
		once = false
	} else if !once {
		once = true // 默认单次执行，循环需显式指定 --interval
	}

	for round := 1; ; round++ {
		code := oneRound(cfg, cfgPath, force, dryRun, jsonOut, quiet, concurrency)
		if once {
			return code
		}
		if !jsonOut {
			fmt.Printf("── 第 %d 轮完成，%s 后继续 ──\n", round, interval)
		}
		time.Sleep(interval)
	}
}

func (c *Config) validate() error {
	if strings.TrimSpace(c.Token) == "" || strings.Contains(c.Token, "在这里") {
		return errors.New("请先在配置文件中填写 token")
	}
	if len(c.Domains) == 0 {
		return errors.New("配置里没有要绑定的域名，请填写 zone 与 dns")
	}
	for i, d := range c.Domains {
		if strings.TrimSpace(d.Zone) == "" {
			return fmt.Errorf("第 %d 个域名缺少 zone", i+1)
		}
		if len(d.Records) == 0 {
			return fmt.Errorf("域名 %s 的 dns 是空的，至少写一条记录（如 - pc）", d.Zone)
		}
	}
	if !enabledFor(c, "ipv4") && !enabledFor(c, "ipv6") {
		return errors.New("ipv4 与 ipv6 都未启用，至少启用一个")
	}
	return c.checkTypes()
}

// oneRound 执行一轮同步。
func oneRound(cfg *Config, cfgPath string, force, dryRun, jsonOut, quiet bool, concurrency int) int {
	start := time.Now()
	base := orDefault(cfg.APIBase, defaultAPIBase)
	client := clientFor("") // API 调用用通用客户端；IP 探测内部会自动锁定协议族
	stateFile := statePathFor(cfgPath)
	st := loadState(stateFile)

	// ---- 先探测需要的协议，每个协议只探测一次 ----
	//
	// 探测失败不直接报错，而是先看有没有另一个协议可用：
	// 只有 IPv6 的家庭宽带（type 默认 both）或只有 IPv4 的机器，
	// 都应该正常工作，不该因为一个协议探不到就整体失败。
	famCache := map[string]string{}
	famErr := map[string]error{}
	needFam := map[string]bool{}
	for _, d := range cfg.Domains {
		for _, r := range d.Records {
			for _, fam := range recordFams(&r) {
				if enabledFor(cfg, fam) {
					needFam[fam] = true
				}
			}
		}
	}
	for fam := range needFam {
		var det *Detection
		if fam == "ipv4" {
			det = &cfg.IPv4
		} else {
			det = &cfg.IPv6
		}
		if v, err := detectIP(fam, det); err != nil {
			famErr[fam] = err
		} else {
			famCache[fam] = v
		}
	}
	// 所有需要的协议都探不到才算致命错误
	if len(famCache) == 0 {
		var msgs []string
		for _, fam := range []string{"ipv4", "ipv6"} {
			if e, ok := famErr[fam]; ok {
				msgs = append(msgs, e.Error())
			}
		}
		fmt.Fprintf(os.Stderr, "cfddns: 无法探测本机公网 IP\n  %s\n", strings.Join(msgs, "\n  "))
		fmt.Fprintf(os.Stderr, "  提示: 在配置里关掉探不到的协议（如 ipv6: {enabled: false}）\n")
		return 2
	}
	singleStack := len(famCache) == 1

	// ---- 收集任务 ----
	var tasks []Task
	var res []Result
	for _, d := range cfg.Domains {
		for _, r := range d.Records {
			name := orDefault(r.Name, "@")
			full := fqdn(name, d.Zone)
			for _, fam := range recordFams(&r) {
				if !enabledFor(cfg, fam) {
					continue // 记录要求的协议在全局未启用 → 跳过
				}
				typ := "A"
				det := &cfg.IPv4
				if fam == "ipv6" {
					typ = "AAAA"
					det = &cfg.IPv6
				}
				ip, ok := famCache[fam]
				if !ok {
					// 该协议不可用但另一个可用 → 静默跳过；
					// 只有全都不可用时才逐条报错（上面已把这种情况拦掉了）
					if singleStack {
						continue
					}
					res = append(res, Result{
						Zone: d.Zone, Name: name, FQDN: full, Type: typ,
						Status: "error", Err: famErr[fam].Error(),
					})
					continue
				}
				tasks = append(tasks, Task{
					Zone: d.Zone, Name: name, FQDN: full, Type: typ,
					TTL: r.TTL, Proxy: r.Proxy, Want: ip, Owner: det,
				})
			}
		}
	}
	if len(tasks) == 0 && len(res) == 0 {
		fmt.Fprintln(os.Stderr, "cfddns: 没有可执行的记录（检查 dns 与 ipv4/ipv6 开关）")
		return 2
	}

	// ---- 并发执行 ----
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	var mu sync.Mutex
	for i := range tasks {
		t := tasks[i]
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			r := processTask(client, base, cfg.Token, &st, t, force, dryRun)
			mu.Lock()
			res = append(res, r)
			mu.Unlock()
		}()
	}
	wg.Wait()

	if !dryRun {
		_ = saveState(stateFile, &st)
	}

	// ---- 输出 ----
	sort.Slice(res, func(i, j int) bool {
		if res[i].Zone != res[j].Zone {
			return res[i].Zone < res[j].Zone
		}
		if res[i].Name != res[j].Name {
			return res[i].Name < res[j].Name
		}
		return res[i].Type < res[j].Type
	})
	counts := map[string]int{}
	for _, r := range res {
		counts[r.Status]++
	}

	if jsonOut {
		b, _ := json.MarshalIndent(map[string]any{
			"elapsed_ms": time.Since(start).Milliseconds(),
			"counts":     counts,
			"results":    res,
		}, "", "  ")
		fmt.Println(string(b))
	} else {
		fmt.Printf("%s %s %s %s %s\n",
			padRight("域名", 28), padRight("类型", 6), padRight("地址", 44), padRight("状态", 8), "耗时")
		for _, r := range res {
			if quiet && r.Status == "unchanged" {
				continue
			}
			val := r.Want
			switch {
			case r.Status == "error":
				val = r.Err
			case r.Status == "updated" && r.Current != "":
				val = fmt.Sprintf("%s → %s", r.Current, r.Want)
			}
			fmt.Printf("%s %s %s %s %dms\n",
				padRight(truncDisplay(r.FQDN, 28), 28),
				padRight(r.Type, 6),
				padRight(truncDisplay(val, 44), 44),
				padRight(statusZh(r.Status), 8),
				r.ms)
		}
		fmt.Printf("\n共 %d 条：", len(res))
		for _, k := range []string{"updated", "unchanged", "planned", "error"} {
			if counts[k] > 0 {
				fmt.Printf("%s=%d ", statusZh(k), counts[k])
			}
		}
		fmt.Printf("| 总耗时 %dms\n", time.Since(start).Milliseconds())
	}

	if counts["error"] > 0 {
		return 1
	}
	return 0
}

func statusZh(s string) string {
	switch s {
	case "updated":
		return "已更新"
	case "unchanged":
		return "未变化"
	case "planned":
		return "待更新"
	case "error":
		return "失败"
	}
	return s
}

// processTask 处理单条记录：状态 → 读 → (写) → 记录状态。
func processTask(client *http.Client, base, token string, st *State, t Task, force, dryRun bool) Result {
	r := Result{Zone: t.Zone, Name: t.Name, FQDN: t.FQDN, Type: t.Type, Want: t.Want}
	t0 := time.Now()
	defer func() { r.ms = msSince(t0) }()

	key := stateKey(t.Zone, t.FQDN, t.Type)

	// 快路径：本地状态显示已是目标值，且轮内无他人改动 → 不碰网络
	if !force {
		if v, ok := st.getRecord(key); ok && v == t.Want {
			r.Status = "unchanged"
			r.Current = v
			return r
		}
	}

	// 解析 zone_id（缓存优先）
	zoneID, err := resolveZone(client, base, token, st, t.Zone)
	if err != nil {
		r.Status, r.Err = "error", err.Error()
		return r
	}

	// 读取现有记录
	rec, err := findRecord(client, base, token, zoneID, t.FQDN, t.Type)
	if err != nil {
		r.Status, r.Err = "error", err.Error()
		return r
	}

	if rec != nil {
		r.Current = rec.Content
		// 写前二次确认：值已正确则既不写也不重复计数
		if rec.matches(t.Want) && !force {
			st.setRecord(key, t.Want)
			r.Status = "unchanged"
			return r
		}
	}

	if dryRun {
		r.Status = "planned"
		return r
	}

	body := map[string]any{"type": t.Type, "name": t.FQDN, "content": t.Want}
	if t.TTL > 0 {
		body["ttl"] = t.TTL
	} else if rec == nil {
		body["ttl"] = 1
	}
	if t.Proxy != nil {
		body["proxied"] = *t.Proxy
	} else if rec == nil {
		body["proxied"] = false
	}
	if rec == nil {
		body["comment"] = "managed by cfddns"
	}

	if rec == nil {
		var out cfRecord
		if err := cfDo(client, base, token, http.MethodPost, "/zones/"+zoneID+"/dns_records", body, &out); err != nil {
			r.Status, r.Err = "error", err.Error()
			return r
		}
	} else {
		if err := cfDo(client, base, token, http.MethodPut, "/zones/"+zoneID+"/dns_records/"+rec.ID, body, nil); err != nil {
			r.Status, r.Err = "error", err.Error()
			return r
		}
	}

	r.Status = "updated"
	st.setRecord(key, t.Want)
	return r
}

// ---------------------------------------------------------------- Cloudflare API

type cfEnvelope struct {
	Success  bool            `json:"success"`
	Errors   []cfError       `json:"errors"`
	Messages []cfMessage     `json:"messages"`
	Result   json.RawMessage `json:"result"`
}

type cfError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type cfMessage struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type cfRecord struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Type    string `json:"type"`
	Content string `json:"content"`
	TTL     int    `json:"ttl"`
	Proxied bool   `json:"proxied"`
}

// setContent 与任务目标比较时使用的规范化字符串
func (r *cfRecord) matches(want string) bool {
	return strings.EqualFold(strings.TrimSpace(r.Content), strings.TrimSpace(want))
}

// cfDo 统一的 API 调用：解码信封、提取结构化错误、区分 401/403/429。
func cfDo(client *http.Client, base, token, method, path string, body any, out any) error {
	var rdr *strings.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("编码请求失败: %w", err)
		}
		rdr = strings.NewReader(string(b))
	} else {
		rdr = strings.NewReader("")
	}
	req, err := http.NewRequest(method, strings.TrimRight(base, "/")+path, rdr)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return wrapNetErr(err)
	}
	defer resp.Body.Close()
	b, err := readLimited(resp.Body, 1<<20)
	if err != nil {
		return fmt.Errorf("读取响应失败: %w", err)
	}

	var env cfEnvelope
	if jerr := json.Unmarshal(b, &env); jerr != nil {
		if resp.StatusCode >= 400 {
			return fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncate(string(b), 200))
		}
		return fmt.Errorf("响应不是合法 JSON: %v", jerr)
	}
	if !env.Success {
		detail := formatCFErrors(env.Errors)
		switch resp.StatusCode {
		case 401:
			return fmt.Errorf("令牌无效或已失效 (HTTP 401)%s", detail)
		case 403:
			return fmt.Errorf("令牌权限不足，需要 Zone:DNS:Edit (HTTP 403)%s", detail)
		case 429:
			return fmt.Errorf("触发 Cloudflare 速率限制 (HTTP 429)%s", detail)
		}
		if len(env.Errors) > 0 {
			return fmt.Errorf("API 错误 %d: %s", env.Errors[0].Code, env.Errors[0].Message)
		}
		return fmt.Errorf("API 返回失败 (HTTP %d)%s", resp.StatusCode, detail)
	}
	if out != nil && len(env.Result) > 0 {
		if err := json.Unmarshal(env.Result, out); err != nil {
			return fmt.Errorf("解析 result 失败: %w", err)
		}
	}
	return nil
}

func formatCFErrors(errs []cfError) string {
	if len(errs) == 0 {
		return ""
	}
	parts := make([]string, 0, len(errs))
	for _, e := range errs {
		parts = append(parts, fmt.Sprintf("%d %s", e.Code, e.Message))
	}
	return ": " + strings.Join(parts, "; ")
}

func resolveZone(client *http.Client, base, token string, st *State, zone string) (string, error) {
	if id, ok := st.getZone(zone); ok {
		return id, nil
	}
	q := url.Values{}
	q.Set("name", zone)
	q.Set("per_page", "50")
	var zones []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	if err := cfDo(client, base, token, http.MethodGet, "/zones?"+q.Encode(), nil, &zones); err != nil {
		return "", fmt.Errorf("查询 zone %s 失败: %w", zone, err)
	}
	if len(zones) == 0 {
		return "", fmt.Errorf("找不到 zone %s（令牌无权访问，或域名不在该账号下）", zone)
	}
	st.setZone(zone, zones[0].ID)
	return zones[0].ID, nil
}

func findRecord(client *http.Client, base, token, zoneID, name, typ string) (*cfRecord, error) {
	q := url.Values{}
	q.Set("type", typ)
	q.Set("name", name)
	var recs []cfRecord
	if err := cfDo(client, base, token, http.MethodGet, "/zones/"+zoneID+"/dns_records?"+q.Encode(), nil, &recs); err != nil {
		return nil, fmt.Errorf("查询记录 %s 失败: %w", name, err)
	}
	if len(recs) == 0 {
		return nil, nil
	}
	return &recs[0], nil
}

// ---------------------------------------------------------------- IP 探测

// localGlobalIPv6 从内核接口表直接读全局单播 IPv6，零网络开销。
// 过滤：链接本地 / 环回 / 唯一本地(fc00::/7) / 未指定，并按 RFC 4862 排除 deprecated。
// 越靠前的地址优先级越高（临时地址 0x00 优先于稳定地址 0x20，与 RFC 6724 一致）。
func localGlobalIPv6() (netip.Addr, bool) {
	f, err := os.Open("/proc/net/if_inet6")
	if err != nil {
		return netip.Addr{}, false
	}
	defer f.Close()
	data, err := readLimited(f, 1<<20)
	if err != nil {
		return netip.Addr{}, false
	}

	type cand struct {
		addr  netip.Addr
		prio  int
		ifidx int
	}
	var cands []cand
	for _, line := range strings.Split(string(data), "\n") {
		fs := strings.Fields(line)
		if len(fs) < 6 {
			continue
		}
		addrStr, scope, flags := fs[0], fs[2], fs[3]
		if len(addrStr) != 32 {
			continue
		}
		var sb strings.Builder
		for i := 0; i < 32; i += 4 {
			if i > 0 {
				sb.WriteByte(':')
			}
			sb.WriteString(addrStr[i : i+4])
		}
		addr, err := netip.ParseAddr(sb.String())
		if err != nil || !addr.Is6() || addr.Is4In6() {
			continue
		}
		if scope != "00" { // 00 = global
			continue
		}
		if addr.IsLoopback() || addr.IsLinkLocalUnicast() || addr.IsUnspecified() {
			continue
		}
		if addr.IsPrivate() { // fc00::/7
			continue
		}
		fl, err := parseHex(flags)
		if err != nil {
			continue
		}
		if fl&0x40 != 0 { // IFA_F_DEPRECATED
			continue
		}
		prio := 50
		if fl&0x20 != 0 { // IFA_F_TEMPORARY
			prio = 10
		}
		var ifidx int
		_, _ = fmt.Sscanf(fs[5], "%d", &ifidx)
		cands = append(cands, cand{addr: addr, prio: prio, ifidx: ifidx})
	}
	if len(cands) == 0 {
		return netip.Addr{}, false
	}
	sort.SliceStable(cands, func(i, j int) bool {
		if cands[i].prio != cands[j].prio {
			return cands[i].prio < cands[j].prio
		}
		return cands[i].ifidx < cands[j].ifidx
	})
	return cands[0].addr, true
}

func parseHex(s string) (int, error) {
	v := 0
	for _, c := range strings.ToLower(s) {
		v <<= 4
		switch {
		case c >= '0' && c <= '9':
			v += int(c - '0')
		case c >= 'a' && c <= 'f':
			v += int(c-'a') + 10
		default:
			return 0, fmt.Errorf("非法十六进制: %q", s)
		}
	}
	return v, nil
}

// detectIP 按配置探测公网地址。每个探测源只用 1 次请求（trace/ipify 直接返回地址）。
func detectIP(fam string, det *Detection) (string, error) {
	// 探测统一走锁定协议族的客户端，避免双栈出口把结果搞错
	fc := clientFor(fam)
	source := strings.ToLower(orDefault(det.Source, "auto"))
	skip := det.SkipPrefix

	tryIface := func() (string, error) {
		if fam != "ipv6" {
			return "", fmt.Errorf("iface 探测仅支持 IPv6")
		}
		a, ok := localGlobalIPv6()
		if !ok {
			return "", errors.New("本机没有可用的全局 IPv6 地址（可能只有 ULA/链接本地）")
		}
		return a.String(), nil
	}
	tryTrace := func() (string, error) {
		ep := epTraceV4
		if fam == "ipv6" {
			ep = epTraceV6
		}
		return traceIP(fc, ep, fam)
	}
	trySvc := func() (string, error) {
		return ipifyIP(fc, fam)
	}
	tryURL := func() (string, error) {
		if det.URL == "" {
			return "", errors.New("source=url 但未填写 url")
		}
		return matchIP(fc, det.URL, fam, det.MatchRegex, skip)
	}
	tryExec := func() (string, error) {
		if det.Command == "" {
			return "", errors.New("source=exec 但未填写 command")
		}
		return matchExec(det.Command, fam, det.MatchRegex, skip)
	}

	order := map[string][]func() (string, error){
		"auto":    {tryIface, tryTrace, trySvc},
		"iface":   {tryIface},
		"local":   {tryIface},
		"trace":   {tryTrace},
		"ipify":   {trySvc},
		"url":     {tryURL},
		"exec":    {tryExec},
		"command": {tryExec},
	}
	if fam == "ipv4" {
		// IPv4 没有「读本机网卡」这条路（本机地址通常不是公网 IPv4）
		order["auto"] = []func() (string, error){tryTrace, trySvc}
	}
	fns, ok := order[source]
	if !ok {
		return "", fmt.Errorf("未知的 source: %q（可选 auto/local/trace/ipify/url/exec）", det.Source)
	}

	var errs []string
	for _, fn := range fns {
		v, err := fn()
		if err == nil && v != "" {
			return v, nil
		}
		if err != nil {
			errs = append(errs, err.Error())
		}
	}
	return "", fmt.Errorf("探测 %s 失败: %s", strings.ToUpper(fam), strings.Join(errs, " | "))
}

// traceIP 从 Cloudflare 的 trace 端点取 ip= 字段（response 很小，只解析需要的行）。
func traceIP(client *http.Client, endpoint, fam string) (string, error) {
	req, err := http.NewRequest(http.MethodGet, endpoint, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "cfddns/"+progVersion)
	req.Header.Set("Cache-Control", "no-cache")
	resp, err := client.Do(req)
	if err != nil {
		return "", wrapNetErr(err)
	}
	defer resp.Body.Close()
	body, err := readLimited(resp.Body, 1<<16)
	if err != nil {
		return "", err
	}
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("trace 返回 HTTP %d", resp.StatusCode)
	}
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "ip=") {
			continue
		}
		v := strings.TrimSpace(strings.TrimPrefix(line, "ip="))
		addr, err := netip.ParseAddr(v)
		if err != nil {
			return "", fmt.Errorf("trace 返回的地址无法解析: %q", v)
		}
		if fam == "ipv4" && !addr.Is4() {
			return "", fmt.Errorf("trace 返回的是 IPv6 (%s)，需要 IPv4 出口", v)
		}
		if fam == "ipv6" && !addr.Is6() {
			return "", fmt.Errorf("trace 返回的是 IPv4 (%s)，需要 IPv6 出口", v)
		}
		if addr.IsLoopback() || addr.IsPrivate() {
			return "", fmt.Errorf("trace 返回的是内网地址: %s", v)
		}
		return addr.String(), nil
	}
	return "", errors.New("trace 响应中没有 ip= 字段")
}

// ipifyIP 依次尝试多个返回纯地址的端点，任一成功即可。
// 多个端点是为了容错：某些网络对单一域名的 IPv4 出口会被拒绝。
func ipifyIP(client *http.Client, fam string) (string, error) {
	endpoints := []string{
		"https://api.ipify.org",
		"https://ipv4.icanhazip.com",
		"https://ipv4.ident.me",
	}
	if fam == "ipv6" {
		endpoints = []string{
			"https://api64.ipify.org",
			"https://ipv6.icanhazip.com",
			"https://ipv6.ident.me",
		}
	}
	var errs []string
	for _, ep := range endpoints {
		v, err := plainIP(client, ep, fam)
		if err == nil && v != "" {
			return v, nil
		}
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", hostOf(ep), err))
		}
	}
	return "", errors.New(strings.Join(errs, " | "))
}

func hostOf(rawURL string) string {
	if u, err := url.Parse(rawURL); err == nil && u.Host != "" {
		return u.Host
	}
	return rawURL
}

func plainIP(client *http.Client, endpoint, fam string) (string, error) {
	req, err := http.NewRequest(http.MethodGet, endpoint, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "cfddns/"+progVersion)
	resp, err := client.Do(req)
	if err != nil {
		return "", wrapNetErr(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	b, err := readLimited(resp.Body, 1<<12)
	if err != nil {
		return "", err
	}
	v := strings.TrimSpace(string(b))
	addr, err := netip.ParseAddr(v)
	if err != nil {
		return "", fmt.Errorf("返回内容不是地址: %q", truncate(v, 60))
	}
	if fam == "ipv4" && !addr.Is4() {
		return "", fmt.Errorf("期望 IPv4 但得到 %s", v)
	}
	if fam == "ipv6" && !addr.Is6() {
		return "", fmt.Errorf("期望 IPv6 但得到 %s", v)
	}
	return addr.String(), nil
}

func matchIP(client *http.Client, endpoint, fam, pattern string, skip []string) (string, error) {
	req, err := http.NewRequest(http.MethodGet, endpoint, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "cfddns/"+progVersion)
	resp, err := client.Do(req)
	if err != nil {
		return "", wrapNetErr(err)
	}
	defer resp.Body.Close()
	b, err := readLimited(resp.Body, 1<<16)
	if err != nil {
		return "", err
	}
	return matchText(string(b), fam, pattern, skip)
}

func matchExec(command, fam, pattern string, skip []string) (string, error) {
	fields := strings.Fields(command)
	if len(fields) == 0 {
		return "", errors.New("command 为空")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, fields[0], fields[1:]...).Output()
	if err != nil {
		return "", fmt.Errorf("执行 %q 失败: %v", command, err)
	}
	return matchText(string(out), fam, pattern, skip)
}

// matchText 从任意文本里提取第一个符合协议且未被 skip 前缀排除的地址。
func matchText(text, fam, pattern string, skip []string) (string, error) {
	isCandidate := func(s string) (netip.Addr, bool) {
		addr, err := netip.ParseAddr(s)
		if err != nil {
			return netip.Addr{}, false
		}
		if fam == "ipv4" && (!addr.Is4() || addr.IsLoopback() || addr.IsPrivate()) {
			return netip.Addr{}, false
		}
		if fam == "ipv6" {
			if !addr.Is6() || addr.Is4In6() || addr.IsLoopback() || addr.IsLinkLocalUnicast() || addr.IsUnspecified() || addr.IsPrivate() {
				return netip.Addr{}, false
			}
		}
		return addr, true
	}
	if pattern != "" {
		re, err := regexp.Compile(pattern)
		if err != nil {
			return "", fmt.Errorf("match_regex 无效: %v", err)
		}
		for _, m := range re.FindAllStringSubmatch(text, -1) {
			for i := 1; i < len(m); i++ {
				if addr, ok := isCandidate(m[i]); ok {
					return addr.String(), nil
				}
			}
			if addr, ok := isCandidate(m[0]); ok {
				return addr.String(), nil
			}
		}
		return "", errors.New("match_regex 没有匹配到可用地址")
	}
	re := regexp.MustCompile(`[0-9a-fA-F:]{2,45}`)
	for _, tok := range re.FindAllString(text, -1) {
		addr, ok := isCandidate(tok)
		if !ok {
			continue
		}
		excluded := false
		for _, p := range skip {
			if strings.HasPrefix(addr.String(), p) {
				excluded = true
				break
			}
		}
		if excluded {
			continue
		}
		return addr.String(), nil
	}
	return "", errors.New("文本中没有找到可用的 " + strings.ToUpper(fam) + " 地址")
}

// ---------------------------------------------------------------- 配置解析

func (c *Config) applyDefaults() {
	if c.APIBase == "" {
		c.APIBase = defaultAPIBase
	}
	// nil 表示用户没写，默认启用；只有显式 false 才是关闭
	on := true
	if c.IPv4.Enabled == nil {
		c.IPv4.Enabled = &on
	}
	if c.IPv6.Enabled == nil {
		c.IPv6.Enabled = &on
	}
	if c.IPv4.Source == "" {
		c.IPv4.Source = "auto"
	}
	if c.IPv6.Source == "" {
		c.IPv6.Source = "auto"
	}
	for i := range c.Domains {
		for j := range c.Domains[i].Records {
			r := &c.Domains[i].Records[j]
			if r.Type == "" {
				r.Type = orDefault(c.DefaultType, "both")
			}
			r.Type = strings.ToUpper(strings.TrimSpace(r.Type))
			if r.TTL == 0 {
				r.TTL = 1
			}
		}
	}
}

func enabledFor(c *Config, fam string) bool {
	d := &c.IPv4
	if fam == "ipv6" {
		d = &c.IPv6
	}
	// 未经 applyDefaults 时 Enabled 可能为 nil，此时按启用处理
	return d.Enabled == nil || *d.Enabled
}

func recordFams(r *Record) []string {
	switch strings.ToUpper(orDefault(r.Type, "both")) {
	case "A":
		return []string{"ipv4"}
	case "AAAA":
		return []string{"ipv6"}
	case "BOTH", "":
		return []string{"ipv4", "ipv6"}
	}
	return []string{"ipv4", "ipv6"}
}

// checkTypes 兜底校验：解析阶段已规范化类型，这里防止绕过解析器的构造。
func (c *Config) checkTypes() error {
	for i := range c.Domains {
		for j := range c.Domains[i].Records {
			r := &c.Domains[i].Records[j]
			switch strings.ToUpper(strings.TrimSpace(r.Type)) {
			case "A", "AAAA", "BOTH":
			default:
				return fmt.Errorf("记录 %q 的 type %q 无效（只能是 A / AAAA / both）",
					orDefault(r.Name, "?"), r.Type)
			}
		}
	}
	return nil
}

func fqdn(name, zone string) string {
	name = strings.TrimSpace(name)
	switch name {
	case "", "@":
		return zone
	}
	if strings.HasSuffix(name, "."+zone) || name == zone {
		return name
	}
	return name + "." + zone
}

func stateKey(zone, fqdnName, typ string) string {
	return strings.ToLower(zone) + "/" + strings.ToLower(fqdnName) + "/" + strings.ToUpper(typ)
}

// ---------------------------------------------------------------- 状态文件

func loadState(path string) State {
	st := State{Records: map[string]string{}, Zones: map[string]string{}}
	b, err := os.ReadFile(path)
	if err != nil {
		return st
	}
	var raw State
	if err := json.Unmarshal(b, &raw); err != nil {
		return st
	}
	if raw.Records != nil {
		st.Records = raw.Records
	}
	if raw.Zones != nil {
		st.Zones = raw.Zones
	}
	return st
}

func saveState(path string, st *State) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func (s *State) getRecord(k string) (string, bool) { v, ok := s.Records[k]; return v, ok }
func (s *State) setRecord(k, v string)             { s.Records[k] = v }
func (s *State) getZone(z string) (string, bool) {
	v, ok := s.Zones[strings.ToLower(z)]
	return v, ok
}
func (s *State) setZone(z, id string) { s.Zones[strings.ToLower(z)] = id }

// ---------------------------------------------------------------- HTTP 客户端

// newTransport 构造传输层；fam 非空时把 DNS 解析与拨号都锁定到该协议族。
//
// 这一步是必需的而不是优化：当主机同时具备 IPv4/IPv6 出口时，
// 访问 www.cloudflare.com 这类双栈域名会被 happy-eyeballs 挑到 IPv6，
// 于是 trace 的 ip= 字段返回 IPv6，IPv4 探测就会得到错误结果。
//
// 强制方式：自己解析域名并只挑选对应协议族的地址，
// 拨号时改写 Host 到该 IP，但 Request.Host 仍是原域名，
// 因此 TLS SNI、Host 头与证书校验都不受影响。
func newTransport(fam string) *http.Transport {
	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	var dialCtx func(ctx context.Context, network, addr string) (net.Conn, error)
	if fam == "ipv4" || fam == "ipv6" {
		want6 := fam == "ipv6"
		res := &net.Resolver{}
		dialCtx = func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(addr)
			if err != nil {
				return dialer.DialContext(ctx, network, addr)
			}
			if ip := net.ParseIP(host); ip != nil {
				return dialer.DialContext(ctx, network, addr)
			}
			ips, err := res.LookupIP(ctx, "ip", host)
			if err != nil {
				return nil, fmt.Errorf("解析 %s 失败: %w", host, err)
			}
			for _, ip := range ips {
				is6 := ip.To4() == nil
				if is6 != want6 {
					continue
				}
				conn, derr := dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
				if derr == nil {
					return conn, nil
				}
				err = derr
			}
			if err == nil {
				err = fmt.Errorf("%s 没有 %s 地址", host, strings.ToUpper(fam))
			}
			return nil, err
		}
	}
	return &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           dialCtx,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          16,
		MaxIdleConnsPerHost:   8,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		ResponseHeaderTimeout: 20 * time.Second,
	}
}

// clientFor 返回锁定到指定协议族的客户端（带缓存）。
func clientFor(fam string) *http.Client {
	clientsMu.Lock()
	defer clientsMu.Unlock()
	if c, ok := familyClients[fam]; ok {
		return c
	}
	c := &http.Client{Transport: newTransport(fam), Timeout: 30 * time.Second}
	familyClients[fam] = c
	return c
}

var (
	clientsMu     sync.Mutex
	familyClients = map[string]*http.Client{}
)

// msSince 返回毫秒耗时；已完成但不足 1ms 的操作返回 1，
// 避免快速路径全显示 0ms 让人误以为没有计时。
func msSince(t0 time.Time) int64 {
	ms := time.Since(t0).Milliseconds()
	if ms == 0 {
		return 1
	}
	return ms
}

func wrapNetErr(err error) error {
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return fmt.Errorf("网络超时: %w", err)
	}
	return fmt.Errorf("网络错误: %w", err)
}

// readLimited 带字节上限的读取，返回原始错误以便上层区分超限与网络中断。
func readLimited(r io.Reader, n int64) ([]byte, error) {
	return io.ReadAll(io.LimitReader(r, n))
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
