package main

import "strings"

// 默认配置模板。用反引号原样保留缩进，写入前会统一去掉首行换行与公共缩进。
//
// 模板列出程序支持的全部配置项，每项都带注释与默认值，用户既可以照抄最小的
// 三行（token/zone/dns），也可以在原地改任意一项。改完直接运行 cfddns 生效。
const defaultConfigTemplate = `
# cfddns 配置 —— 改完直接运行 cfddns 生效

# Cloudflare API 令牌。创建: https://dash.cloudflare.com/profile/api-tokens
# 用 "Edit zone DNS" 模板，权限 Zone → DNS → Edit；不要填 Global API Key。
token: 在这里填你的 Cloudflare API 令牌

# 要绑定的域名（zone_id 由程序自动查询，不用填）
zone: example.com

# 要更新的记录，一行一条：名字 或 名字: 类型
#   名字  pc → pc.example.com   @ → 根域名   * → 泛解析
#   类型  A=只绑 IPv4  AAAA=只绑 IPv6  both=两个都绑（默认）
dns:

  # 默认写法：v4 和 v6 都绑
  - pc: both

  # 自定义示例：只绑 IPv6（家庭宽带通常只有公网 IPv6，推荐）
  # - nas: AAAA

  # 自定义示例：只绑 IPv4
  # - vpn: A

  # 需要单独设 TTL 或开 Cloudflare 代理时用完整写法
  # - name: blog
  #   type: A
  #   ttl: 300          # 1=自动（默认），或 60~86400 秒
  #   proxy: true       # 默认 false。开橙云后 DNS 返回 Cloudflare 的 IP，不是你的真实 IP

# ── 公网 IP 探测（通常不用改）─────────────────────────────────────────
# enabled  是否探测该协议。默认都 true；某个协议探不到会自动跳过，不影响另一个。
# source   探测来源，默认 auto：
#            auto   自动选。IPv4 用 Cloudflare trace，失败退到 ipify；
#                   IPv6 先读本机网卡（零网络开销），失败再问云端。
#            local  只读本机网卡（仅 IPv6 可用）
#            trace  只问 Cloudflare trace 端点
#            ipify  只问 ipify / icanhazip / ident.me
#            url    只读下面 url 指向的地址
#            exec   执行下面 command 指定的本地命令
# url          source: url 时读取的地址（默认指向 ipify，仅 source 为 url 时生效）
# match_regex  从 url/exec 的输出里提取地址的正则，取第一个捕获组
# skip_prefix  命中这些前缀的地址会被忽略

ipv4:
  enabled: true
  source: auto
  # 默认走 Cloudflare trace；想固定用 ipify 时把这行改成 ipify
  url: https://api.ipify.org
  match_regex: '(\S+)'
  # 排除私有网段。IPv4 的 skip_prefix 只对私有地址有意义
  # （fc/fd/fe80 是 IPv6 前缀，填在这里不会生效）
  skip_prefix: [10., 192.168., 172.16., 127.]

ipv6:
  enabled: true
  source: auto
  url: https://api64.ipify.org
  match_regex: '(\S+)'
  # fc00::/7 是唯一本地地址(ULA)，fe80 是链路本地，都不是公网 IPv6
  skip_prefix: [fc, fd, fe80]
`

// renderConfigTemplate 返回可写入磁盘的默认配置文本（已规范化缩进）。
func renderConfigTemplate() string {
	return dedent(defaultConfigTemplate)
}

// dedent 去掉首尾空行并移除所有行的公共缩进。
func dedent(s string) string {
	lines := strings.Split(s, "\n")
	for len(lines) > 0 && strings.TrimSpace(lines[0]) == "" {
		lines = lines[1:]
	}
	for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}
	min := -1
	for _, l := range lines {
		if strings.TrimSpace(l) == "" {
			continue
		}
		n := len(l) - len(strings.TrimLeft(l, " "))
		if min < 0 || n < min {
			min = n
		}
	}
	if min <= 0 {
		return strings.Join(lines, "\n") + "\n"
	}
	for i, l := range lines {
		if len(l) >= min {
			lines[i] = l[min:]
		} else {
			lines[i] = strings.TrimLeft(l, " ")
		}
	}
	return strings.Join(lines, "\n") + "\n"
}
