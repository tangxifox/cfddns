package main

import (
	"errors"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
)

// 这是一个刻意做小的 YAML 读取器，只支持配置文件的常用子集：
//   - 嵌套映射（靠缩进，2 空格一层）
//   - 映射值的块序列（- 开头）
//   - 标量：字符串/整数/浮点/布尔/null/带引号字符串
//   - # 注释（整行或被空白分隔的行尾注释，引号内的 # 不算）
//   - 行内流式标量如 [a, b]
// 不支持：锚点、多文档、块标量(| >)、复杂流式集合。作为交换，程序零第三方依赖。
//
// 选择自写而非 gopkg.in/yaml.v3 的原因：模块依赖会引入下载与供应链面，
// 而配置格式完全由我们自己定义，用不到 YAML 的完整表达力。

// CfScalar 表示一个标量节点，保留其在文件中的原始文本以便给出准确报错。
type CfScalar struct {
	Text string
	line int
}

// CfNode 是解析结果的通用容器：映射或序列。
type CfNode struct {
	Map      map[string]*CfNode
	Seq      []*CfNode
	Scalar   *CfScalar
	line     int
	isMap    bool
	isSeq    bool
	isScalar bool
}

func newScalarNode(s *CfScalar) *CfNode {
	return &CfNode{Scalar: s, line: s.line, isScalar: true}
}

func (n *CfNode) kind() string {
	switch {
	case n == nil:
		return "空"
	case n.isMap:
		return "映射"
	case n.isSeq:
		return "列表"
	default:
		return "标量"
	}
}

// ---------------------------------------------------------------- 词法与结构

type yamlLine struct {
	indent int
	text   string // 已去掉缩进，未去注释
	num    int    // 1-based 行号
}

type yamlParser struct {
	lines []yamlLine
	i     int
}

func parseYAMLSubset(data []byte) (*CfNode, error) {
	// 容忍 Windows 编辑器写入的 UTF-8 BOM
	text := strings.TrimPrefix(string(data), "\ufeff")
	p := &yamlParser{}
	for idx, raw := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
		line := strings.TrimRight(raw, " \t")
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if strings.Contains(line[:len(line)-len(strings.TrimLeft(line, " \t"))], "\t") {
			return nil, fmt.Errorf("第 %d 行使用了制表符缩进，YAML 只允许空格", idx+1)
		}
		indent := len(line) - len(strings.TrimLeft(line, " "))
		p.lines = append(p.lines, yamlLine{indent: indent, text: strings.TrimLeft(line, " "), num: idx + 1})
	}
	if len(p.lines) == 0 {
		return nil, fmt.Errorf("配置文件为空或只有注释")
	}
	n, err := p.parseBlock(0)
	if err != nil {
		return nil, err
	}
	if p.i < len(p.lines) {
		return nil, fmt.Errorf("第 %d 行缩进异常（多余内容: %q）", p.lines[p.i].num, p.lines[p.i].text)
	}
	return n, nil
}

func (p *yamlParser) parseBlock(indent int) (*CfNode, error) {
	if p.i >= len(p.lines) {
		return newScalarNode(&CfScalar{Text: "", line: 0}), nil
	}
	if strings.HasPrefix(strings.TrimSpace(p.lines[p.i].text), "- ") || strings.TrimSpace(p.lines[p.i].text) == "-" {
		return p.parseSeq(indent)
	}
	return p.parseMap(indent)
}

func (p *yamlParser) parseMap(indent int) (*CfNode, error) {
	node := &CfNode{Map: map[string]*CfNode{}, isMap: true, line: p.lines[p.i].num}
	for p.i < len(p.lines) {
		ln := p.lines[p.i]
		if ln.indent < indent {
			break
		}
		if ln.indent > indent {
			return nil, fmt.Errorf("第 %d 行缩进比同级多出 %d 个空格: %q", ln.num, ln.indent-indent, ln.text)
		}
		if strings.HasPrefix(ln.text, "- ") || ln.text == "-" {
			break
		}
		k, v, err := splitKV(ln)
		if err != nil {
			return nil, err
		}
		if _, dup := node.Map[k]; dup {
			return nil, fmt.Errorf("第 %d 行键 %q 重复定义", ln.num, k)
		}
		p.i++
		val, err := p.parseValue(v, ln, indent, false)
		if err != nil {
			return nil, err
		}
		node.Map[k] = val
	}
	return node, nil
}

func (p *yamlParser) parseSeq(indent int) (*CfNode, error) {
	node := &CfNode{Seq: []*CfNode{}, isSeq: true, line: p.lines[p.i].num}
	for p.i < len(p.lines) {
		ln := p.lines[p.i]
		if ln.indent < indent {
			break
		}
		if ln.indent > indent {
			return nil, fmt.Errorf("第 %d 行缩进异常: %q", ln.num, ln.text)
		}
		if ln.text != "-" && !strings.HasPrefix(ln.text, "- ") {
			break
		}
		rest := strings.TrimSpace(strings.TrimPrefix(ln.text, "-"))
		contentIndent := ln.indent + 2

		if rest == "" {
			p.i++
			child, err := p.parseBlock(contentIndent)
			if err != nil {
				return nil, err
			}
			node.Seq = append(node.Seq, child)
			continue
		}

		if key, val, ok := splitKVMaybe(rest); ok {
			// "- name: xxx" 形式的映射项：本行是映射的第一行
			p.i++
			item := &CfNode{Map: map[string]*CfNode{}, isMap: true, line: ln.num}
			// 序列项的值必须留在本行：不能把下一行当成嵌套块，
			// 否则 "- pc: AAAA" 会丢掉 AAAA（那其实是简写类型，不是子映射）。
			child, err := p.parseValue(val, yamlLine{indent: contentIndent, text: val, num: ln.num}, contentIndent, true)
			if err != nil {
				return nil, err
			}
			item.Map[key] = child
			// 同一项后续的兄弟键缩进必须与 key 对齐
			if p.i < len(p.lines) && p.lines[p.i].indent == contentIndent &&
				!strings.HasPrefix(p.lines[p.i].text, "- ") && p.lines[p.i].text != "-" {
				more, err := p.parseMap(contentIndent)
				if err != nil {
					return nil, err
				}
				for k, v := range more.Map {
					if _, dup := item.Map[k]; dup {
						return nil, fmt.Errorf("第 %d 行键 %q 重复定义", ln.num, k)
					}
					item.Map[k] = v
				}
			}
			node.Seq = append(node.Seq, item)
			continue
		}

		// 纯标量项
		p.i++
		sc, err := parseScalar(stripComment(rest), ln.num)
		if err != nil {
			return nil, err
		}
		node.Seq = append(node.Seq, newScalarNode(sc))
	}
	return node, nil
}

// parseValue 处理 "key:" 之后的内容。
//
// inlineOnly=true 时值只能来自本行（用于序列项，如 "- pc: AAAA"）；
// inlineOnly=false 时若本行没有值，则看下一行的缩进块（用于映射，如 "ipv4:\n  enabled: true"）。
func (p *yamlParser) parseValue(v string, ln yamlLine, keyIndent int, inlineOnly bool) (*CfNode, error) {
	stripped := stripComment(v)
	if strings.TrimSpace(stripped) != "" {
		sc, err := parseScalar(stripped, ln.num)
		if err != nil {
			return nil, err
		}
		return newScalarNode(sc), nil
	}
	if !inlineOnly && p.i < len(p.lines) && p.lines[p.i].indent > keyIndent {
		return p.parseBlock(p.lines[p.i].indent)
	}
	sc, _ := parseScalar("", ln.num)
	return newScalarNode(sc), nil
}

// ---------------------------------------------------------------- 标量处理

func splitKV(ln yamlLine) (string, string, error) {
	k, v, ok := splitKVMaybe(ln.text)
	if !ok {
		return "", "", fmt.Errorf("第 %d 行不是合法的 key: value: %q", ln.num, ln.text)
	}
	if k == "" {
		return "", "", fmt.Errorf("第 %d 行键名为空: %q", ln.num, ln.text)
	}
	return k, v, nil
}

// splitKVMaybe 在「不在引号内」的第一个冒号处切分。
func splitKVMaybe(s string) (string, string, bool) {
	inS, inD := false, false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '\'' && !inD:
			inS = !inS
		case c == '"' && !inS:
			inD = !inD
		case c == ':' && !inS && !inD:
			if i+1 < len(s) && s[i+1] != ' ' && s[i+1] != '\t' {
				continue // 例如 12:30 不是键值分隔
			}
			return strings.TrimSpace(s[:i]), strings.TrimSpace(s[i+1:]), true
		}
	}
	return "", "", false
}

// stripComment 去掉行尾注释，但保留引号内的 #。
func stripComment(s string) string {
	inS, inD := false, false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '\'' && !inD:
			inS = !inS
		case c == '"' && !inS:
			inD = !inD
		case c == '#' && !inS && !inD:
			if i == 0 || s[i-1] == ' ' || s[i-1] == '\t' {
				return strings.TrimSpace(s[:i])
			}
		}
	}
	return strings.TrimSpace(s)
}

func parseScalar(s string, line int) (*CfScalar, error) {
	s = strings.TrimSpace(s)
	if len(s) >= 2 {
		if (s[0] == '"' && s[len(s)-1] == '"') || (s[0] == '\'' && s[len(s)-1] == '\'') {
			body := s[1 : len(s)-1]
			if s[0] == '"' {
				if u, err := strconv.Unquote(s); err == nil {
					return &CfScalar{Text: u, line: line}, nil
				}
			}
			return &CfScalar{Text: body, line: line}, nil
		}
	}
	return &CfScalar{Text: s, line: line}, nil
}

// ---------------------------------------------------------------- 取值辅助

func (n *CfNode) get(key string) *CfNode {
	if n == nil || !n.isMap {
		return nil
	}
	return n.Map[key]
}

func (n *CfNode) str() string {
	if n == nil || n.Scalar == nil {
		return ""
	}
	return n.Scalar.Text
}

func (n *CfNode) isNull() bool {
	if n == nil || n.Scalar == nil {
		return true
	}
	switch strings.ToLower(n.Scalar.Text) {
	case "", "~", "null", "none":
		return true
	}
	return false
}

func (n *CfNode) strReq(key string) (string, error) {
	c := n.get(key)
	if c.isNull() {
		return "", fmt.Errorf("缺少必填项 %q", key)
	}
	return c.str(), nil
}

func (n *CfNode) boolDef(key string, def bool) (bool, error) {
	c := n.get(key)
	if c.isNull() {
		return def, nil
	}
	switch strings.ToLower(c.str()) {
	case "true", "yes", "on", "1", "是":
		return true, nil
	case "false", "no", "off", "0", "否":
		return false, nil
	}
	return def, fmt.Errorf("字段 %q 的值 %q 不是布尔（应为 true/false）", key, c.str())
}

func (n *CfNode) boolPtr(key string) (*bool, error) {
	if n.get(key).isNull() {
		return nil, nil
	}
	v, err := n.boolDef(key, false)
	if err != nil {
		return nil, err
	}
	return &v, nil
}

func (n *CfNode) intDef(key string, def int) (int, error) {
	c := n.get(key)
	if c.isNull() {
		return def, nil
	}
	v, err := strconv.Atoi(strings.TrimSpace(c.str()))
	if err != nil {
		return def, fmt.Errorf("字段 %q 的值 %q 不是整数", key, c.str())
	}
	return v, nil
}

// Strings 取出「本身就是列表」的节点内容。
// 与 map 版本的区别是这里不再查 key —— 传 key 给列表节点是最容易犯的错。
func (n *CfNode) Strings() []string {
	if n == nil {
		return nil
	}
	if n.isSeq {
		out := make([]string, 0, len(n.Seq))
		for _, e := range n.Seq {
			if s := e.str(); s != "" {
				out = append(out, s)
			}
		}
		return out
	}
	// 兼容 [a, b] 行内写法
	raw := strings.TrimSpace(n.str())
	raw = strings.TrimPrefix(raw, "[")
	raw = strings.TrimSuffix(raw, "]")
	var out []string
	for _, part := range strings.Split(raw, ",") {
		if s := strings.TrimSpace(part); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// StringsOf 在映射节点上按 key 取列表。
func (n *CfNode) StringsOf(key string) []string {
	return n.get(key).Strings()
}

// ---------------------------------------------------------------- 配置装载

func loadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	root, err := parseYAMLSubset(data)
	if err != nil {
		return nil, err
	}
	if !root.isMap {
		return nil, fmt.Errorf("配置根节点必须是映射（key: value 形式）")
	}
	cfg := &Config{}
	cfg.APIBase = root.get("api_base").str()
	cfg.Token = strings.TrimSpace(root.get("token").str())

	if err := parseDetection(root.get("ipv4"), &cfg.IPv4, "ipv4"); err != nil {
		return nil, err
	}
	if err := parseDetection(root.get("ipv6"), &cfg.IPv6, "ipv6"); err != nil {
		return nil, err
	}

	// 全局 type，可作为每条记录的默认值
	gt, err := normalizeType(root.get("type").str())
	if err != nil {
		return nil, err
	}
	if gt != "" {
		cfg.DefaultType = gt
	}

	// 新格式：zone + dns 两个顶层键
	zone := strings.TrimSpace(firstNonEmpty(root.get("zone"), root.get("domain")).str())
	dnsNode := root.get("dns")
	if dnsNode == nil {
		dnsNode = root.get("records") // 兼容早期写法
	}

	// 旧格式：domains: {zone: {records: [...]}}，与新格式可混用
	domNode := root.get("domains")
	if domNode != nil && domNode.isMap {
		names := make([]string, 0, len(domNode.Map))
		for k := range domNode.Map {
			names = append(names, k)
		}
		sort.Strings(names)
		for _, key := range names {
			dn := domNode.Map[key]
			if !dn.isMap {
				return nil, fmt.Errorf("domains.%s 必须是映射", key)
			}
			d := Domain{Zone: key}
			if z := dn.get("zone").str(); z != "" {
				d.Zone = z
			} else if z := dn.get("domain").str(); z != "" {
				d.Zone = z
			}
			recs := dn.get("records")
			if recs == nil {
				recs = dn.get("dns")
			}
			rs, err := parseRecords(recs, cfg.DefaultType, "domains."+key)
			if err != nil {
				return nil, err
			}
			d.Records = rs
			cfg.Domains = append(cfg.Domains, d)
		}
	}

	// 新格式的 zone/dns 合并进同一个列表项
	if zone != "" || dnsNode != nil {
		if zone == "" {
			return nil, errors.New("写了 dns 但缺少 zone（要绑定哪个域名？）")
		}
		rs, err := parseRecords(dnsNode, cfg.DefaultType, "dns")
		if err != nil {
			return nil, err
		}
		merged := false
		for i := range cfg.Domains {
			if strings.EqualFold(cfg.Domains[i].Zone, zone) {
				cfg.Domains[i].Records = append(cfg.Domains[i].Records, rs...)
				merged = true
				break
			}
		}
		if !merged {
			cfg.Domains = append(cfg.Domains, Domain{Zone: zone, Records: rs})
		}
	}

	cfg.applyDefaults()
	if err := cfg.checkTypes(); err != nil {
		return nil, err
	}
	return cfg, nil
}

func firstNonEmpty(nodes ...*CfNode) *CfNode {
	for _, n := range nodes {
		if n != nil && strings.TrimSpace(n.str()) != "" {
			return n
		}
	}
	return nil
}

// normalizeType 统一记录类型写法，接受大小写与中文别名。
func normalizeType(s string) (string, error) {
	t := strings.ToLower(strings.TrimSpace(s))
	switch t {
	case "":
		return "", nil
	case "both", "all", "双栈", "都":
		return "BOTH", nil
	case "a", "ipv4", "v4", "4":
		return "A", nil
	case "aaaa", "ipv6", "v6", "6":
		return "AAAA", nil
	}
	return "", fmt.Errorf("type 只能是 A / AAAA / both（也接受 ipv4 / ipv6），得到 %q", s)
}

// parseRecords 解析 records/dns，支持四种写法：
//
//	dns:                # ① 一行一条，"名字: 类型"
//	  - pc: both
//	  - nas: AAAA
//
//	dns:                # ② 纯名字列表，类型取全局 type（默认 both）
//	  - pc
//	  - nas
//
//	dns: [pc: A, nas: AAAA]   # ③ 行内写法，适合只有一两条记录时
//
//	dns:                 # ④ 完整写法，需要单独设 ttl/proxy 时再用
//	  - name: pc
//	    type: AAAA
//	    proxy: true
func parseRecords(n *CfNode, defType, where string) ([]Record, error) {
	if n == nil {
		return nil, nil
	}
	if n.isMap {
		return nil, fmt.Errorf("%s 必须是列表（每行以 - 开头），或写成 %s: [pc, nas]", where, where)
	}

	items := n.Seq
	if n.isScalar { // 行内写法 [pc: A, nas: AAAA] 会被当作一个标量
		raw := strings.TrimSpace(n.str())
		if !strings.HasPrefix(raw, "[") {
			return nil, fmt.Errorf("%s 必须是列表", where)
		}
		raw = strings.TrimPrefix(raw, "[")
		raw = strings.TrimSuffix(raw, "]")
		for _, part := range strings.Split(raw, ",") {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			sc, _ := parseScalar(part, n.line)
			items = append(items, newScalarNode(sc))
		}
	}

	out := make([]Record, 0, len(items))
	for i, it := range items {
		r, err := parseOneRecord(it, defType)
		if err != nil {
			// 单条记录里的标量条目也能报出可读位置
			if len(items) == 1 {
				return nil, fmt.Errorf("%s: %w", where, err)
			}
			return nil, fmt.Errorf("%s 第 %d 条: %w", where, i+1, err)
		}
		if r.Type == "" {
			r.Type = orDefault(defType, "both")
		}
		out = append(out, r)
	}
	return out, nil
}

func parseOneRecord(n *CfNode, defType string) (Record, error) {
	r := Record{}
	switch {
	case n.isMap:
		// 简写 "- pc: AAAA" 会被解析成 {pc: AAAA} 这样只有一个键的映射：
		// 键就是记录名，值就是类型。完整的 name/type 写法不会命中这里，
		// 因为它的键必然是 name/type 这些保留字段。
		if name, typ, ok := shorthandFromMap(n); ok {
			t, err := normalizeType(typ)
			if err != nil {
				return r, err
			}
			r.Name, r.Type = name, t
			return r, nil
		}
		r.Name = unquote(firstNonEmpty(n.get("name"), n.get("host")))
		t, err := normalizeType(n.get("type").str())
		if err != nil {
			return r, err
		}
		r.Type = t
		ttl, err := n.intDef("ttl", 1)
		if err != nil {
			return r, err
		}
		if ttl != 1 && (ttl < 60 || ttl > 86400) {
			return r, fmt.Errorf("ttl 只能是 1（自动）或 60~86400，得到 %d", ttl)
		}
		r.TTL = ttl
		if r.Proxy, err = n.boolPtr("proxy"); err != nil {
			return r, err
		}
		if r.IPv4, err = n.boolPtr("ipv4"); err != nil {
			return r, err
		}
		if r.IPv6, err = n.boolPtr("ipv6"); err != nil {
			return r, err
		}
		r.Comment = strings.TrimSpace(n.get("comment").str())
	case n.isScalar:
		// 必须先用 parseScalar 去掉引号：stripComment 只处理注释，
		// 所以 "- \"pc: AAAA\"" 到这里还带着引号，会解析失败。
		sc, err := parseScalar(stripComment(n.str()), n.line)
		if err != nil {
			return r, err
		}
		s := strings.TrimSpace(sc.Text)
		if s == "" {
			return r, fmt.Errorf("空记录")
		}
		// "名字: 类型"
		if name, typ, ok := splitRecordShorthand(s); ok {
			r.Name = name
			t, nerr := normalizeType(typ)
			if nerr != nil {
				return r, nerr
			}
			r.Type = t
			return r, nil
		}
		// 纯名字
		r.Name = s
	default:
		return r, fmt.Errorf("记录格式无法识别（用 名字 或 名字: 类型）")
	}
	if r.Name == "" {
		return r, fmt.Errorf("记录缺少名字")
	}
	return r, nil
}

// shorthandFromMap 识别 {记录名: 类型} 这种单键简写映射。
func shorthandFromMap(n *CfNode) (string, string, bool) {
	if len(n.Map) != 1 {
		return "", "", false
	}
	for k, v := range n.Map {
		// 键和值都可能带引号（例如 "*": "AAAA"），统一去掉
		ks, err1 := parseScalar(stripComment(k), n.line)
		vs, err2 := parseScalar(stripComment(v.str()), n.line)
		if err1 != nil || err2 != nil {
			return "", "", false
		}
		key := strings.TrimSpace(ks.Text)
		if isRecordField(key) || key == "" {
			return "", "", false
		}
		typ, err := normalizeType(vs.Text)
		if err != nil || typ == "" {
			return "", "", false
		}
		return key, vs.Text, true
	}
	return "", "", false
}

// unquote 取出节点的文本并去掉可能的引号。
func unquote(n *CfNode) string {
	if n == nil {
		return ""
	}
	sc, err := parseScalar(stripComment(n.str()), n.line)
	if err != nil {
		return strings.TrimSpace(n.str())
	}
	return strings.TrimSpace(sc.Text)
}

func isRecordField(k string) bool {
	switch strings.ToLower(k) {
	case "name", "host", "type", "ttl", "proxy", "ipv4", "ipv6", "comment":
		return true
	}
	return false
}

// splitRecordShorthand 解析标量形式的 "pc: AAAA"；冒号后必须是合法类型才算简写，
// 避免把 IPv6 地址里的冒号当成分隔符。
func splitRecordShorthand(s string) (string, string, bool) {
	i := strings.LastIndex(s, ":")
	if i <= 0 {
		return "", "", false
	}
	name := strings.TrimSpace(s[:i])
	typ := strings.TrimSpace(s[i+1:])
	if name == "" || typ == "" {
		return "", "", false
	}
	if _, err := normalizeType(typ); err != nil {
		return "", "", false
	}
	return name, typ, true
}

func parseDetection(n *CfNode, det *Detection, fam string) error {
	if n == nil {
		// 未配置时给合理默认：默认启用；探测失败会自动跳过。
		on := true
		det.Enabled = &on
		det.Source = "auto"
		return nil
	}
	if !n.isMap {
		return fmt.Errorf("%s 必须是映射（enabled/source/...）", fam)
	}
	en, err := n.boolDef("enabled", true)
	if err != nil {
		return fmt.Errorf("%s: %w", fam, err)
	}
	det.Enabled = &en
	det.Source = strings.ToLower(strings.TrimSpace(n.get("source").str()))
	if det.Source == "" {
		det.Source = "auto"
	}
	det.URL = strings.TrimSpace(n.get("url").str())
	det.Command = strings.TrimSpace(n.get("command").str())
	det.MatchRegex = strings.TrimSpace(n.get("match_regex").str())
	det.SkipPrefix = n.StringsOf("skip_prefix")
	return nil
}

func detDesc(d *Detection) string {
	s := orDefault(d.Source, "auto")
	switch s {
	case "url":
		if d.URL != "" {
			s += "(" + d.URL + ")"
		}
	case "exec", "command":
		if d.Command != "" {
			s += "(" + d.Command + ")"
		}
	}
	return s
}
