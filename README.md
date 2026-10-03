# cfddns — Cloudflare DDNS 客户端

[![build](https://github.com/tangxifox/cfddns/actions/workflows/build.yml/badge.svg)](https://github.com/tangxifox/cfddns/actions/workflows/build.yml)

把本机当前的公网 IP 自动同步到 Cloudflare 的 A / AAAA 记录上。
单文件静态二进制，**零第三方依赖**（连 YAML 解析都是内置实现）。

## 下载预编译版本

从 [Releases](https://github.com/tangxifox/cfddns/releases/latest) 下载对应平台的文件：

```bash
# Linux x86_64（ARM64 把 amd64 换成 arm64）
curl -fsSL -o cfddns \
  https://github.com/tangxifox/cfddns/releases/latest/download/cfddns-linux-amd64
chmod +x cfddns && sudo install -m 0755 cfddns /usr/local/bin/cfddns

# Windows：下载 cfddns-windows-amd64.exe 后重命名为 cfddns.exe
```

同一 Release 附带 `SHA256SUMS.txt` 可校验完整性。

## 为什么是 Go

- 编译成单个静态 ELF，目标机器不需要装任何运行时；
- 标准库自带完整 HTTP 客户端与并发原语，不需要第三方库；
- 交叉编译一条命令，Linux/Windows 都能出。
- 对 DDNS 这种 I/O 受限的小工具，Rust 的性能优势换不到任何可感知收益，
  而 Go 的编译/迭代速度明显更快。

## 构建

```bash
./build.sh                 # 本机架构 -> dist/cfddns-<goos>-<goarch>
./build.sh all             # 全平台（与 CI 产物命名一致）
./build.sh arm64           # 只出 linux/arm64
./build.sh windows         # 只出 windows/amd64
```

产物命名 `cfddns-<goos>-<goarch>[.exe]`，与 Release 附件同名。

### CI

`.github/workflows/build.yml` 在 push、PR、手动触发时：

1. `go vet` + `go test`；
2. 并行构建 **linux/amd64、linux/arm64、windows/amd64**，每个平台独立上传 artifact
   （含 `SHA256SUMS.txt`），任一平台失败不影响其他平台；
3. push `v*` tag 时额外把三个二进制发布到 GitHub Release。

发布新版本：

```bash
git tag -a v1.0.1 -m "cfddns v1.0.1"
git push origin v1.0.1
```

## 安装与首次使用

```bash
install -m 0755 dist/cfddns-$(uname -s | tr 'A-Z' 'a-z')-$(uname -m | sed 's/x86_64/amd64/; s/aarch64/arm64/') /usr/local/bin/cfddns
cfddns                     # 首次运行会自动生成 ~/.cfddns/config.yaml 并提示填写
```

配置文件默认位置 `$HOME/.cfddns/config.yaml`，权限 `600`；状态文件
`$HOME/.cfddns/state.json`。这两个路径可以用 `--config` 或环境变量
`CFDDNS_CONFIG` 覆盖，用 `cfddns paths` 随时查看。

`cfddns gen-config` 只输出一行结果说明，**不打印 YAML 内容**；配置写进文件，
要看内容直接读文件或运行 `cfddns status`：

| 情况 | 行为 |
|---|---|
| 文件不存在 | 写入，输出 `已覆盖配置: <路径>` |
| 文件已存在，不带 `--force` | **不覆盖**，输出 `配置已存在，未覆盖: <路径>` |
| 文件已存在，带 `--force` | 覆盖，输出 `已覆盖配置: <路径>` |

## 配置

`cfddns gen-config`（或首次运行时自动）会生成**列出全部配置项**的模板，
每项都带注释和默认值，改完直接运行 `cfddns` 生效。下面是它的形状：

```yaml
token: 在这里填你的 Cloudflare API 令牌
zone: example.com         # ← 换成你自己的域名

dns:
  - pc: both              # 默认写法：v4 和 v6 都绑

  # - nas: AAAA            # 自定义：只绑 IPv6
  # - vpn: A               # 自定义：只绑 IPv4
  # - name: blog           # 需要单独设 TTL / 代理时用完整写法
  #   type: A
  #   ttl: 300
  #   proxy: true

ipv4:
  enabled: true
  source: auto
  url: https://api.ipify.org
  match_regex: '(\S+)'
  skip_prefix: [10., 192.168., 172.16., 127.]

ipv6:
  enabled: true
  source: auto
  url: https://api64.ipify.org
  match_regex: '(\S+)'
  skip_prefix: [fc, fd, fe80]
```

只有 `token` 是必须由你填的；其余全部有合理默认值（TTL 自动、不开代理、
两个协议都探测、探不到的自动跳过）。

### 令牌

在 <https://dash.cloudflare.com/profile/api-tokens> 创建，用
**Edit zone DNS** 模板，权限 Zone → DNS → Edit，资源只勾选自己那一个域名
（最小权限）。不要填 Global API Key，那是另一种鉴权方式。

### dns：绑定写法速查

| 写法 | 效果（以 `zone: example.com` 为例） |
|---|---|
| `- pc` 或 `- pc: both` | `pc.example.com` 同时绑 **A + AAAA**（默认） |
| `- pc: A` | 只绑 **IPv4** |
| `- pc: AAAA` | 只绑 **IPv6**（家庭宽带推荐） |
| `- "@"` | 根域名 `example.com` |
| `- "*"` | 泛解析 `*.example.com` |
| `- name: pc` + `type`/`ttl`/`proxy` | 完整写法，需要单独设参数时用 |

两种写法等价，可以混用：

```yaml
dns:
  - pc: AAAA            # 简写
  - name: nas           # 完整写法
    type: A
    ttl: 300
```

> **注意**：把记录从 `both` 改成 `AAAA` 之后，cfddns 不再管理那条 A 记录，
> 但**不会删除它**。想让它消失需要手动删。

### ipv4 / ipv6：公网 IP 探测

| 字段 | 说明 |
|---|---|
| `enabled` | 是否探测该协议，默认 `true`。探不到会自动跳过，不影响另一个协议 |
| `source` | 探测来源，默认 `auto`，见下表 |
| `url` | `source: url` 时读取的地址 |
| `command` | `source: exec` 时执行的本地命令 |
| `match_regex` | 从 url/exec 输出里提取地址的正则，取第一个捕获组 |
| `skip_prefix` | 命中这些前缀的地址被忽略 |

`source` 取值：

| 值 | 行为 |
|---|---|
| `auto` | IPv4 先用 Cloudflare trace，失败退到 ipify；IPv6 先读本机网卡（零网络开销），失败再问云端 |
| `local` | 只读本机网卡（仅 IPv6 可用） |
| `trace` | 只问 Cloudflare trace 端点 |
| `ipify` | 只问 ipify / icanhazip / ident.me（多个端点依次容错） |
| `url` | 只读 `url` 指向的地址 |
| `exec` | 执行 `command` 并从输出里提取 |

**`skip_prefix` 要按协议给**：`fc`/`fd`/`fe80` 是 IPv6 前缀（ULA、链路本地），
填在 `ipv4` 下永远匹配不到任何地址；IPv4 该排除的是私有网段。
程序自身也会过滤掉链路本地、ULA、环回、deprecated 地址，并按 RFC 6724
让临时地址优先于稳定地址。

只想用 ipify 探测 IPv4：

```yaml
ipv4:
  enabled: true
  source: ipify
```

### 只绑一个协议

顶层 `type` 可以给所有记录设默认类型（记录自己写了 `type` 的以记录为准）：

```yaml
type: AAAA        # 下面没写类型的都用它
zone: example.com
dns:
  - pc
  - nas: A        # 这一条单独用 IPv4
  - "*"
```

`dns` 也支持行内写法，适合只有一两条记录：

```yaml
zone: example.com
dns: [pc, nas: AAAA]
```

### 旧格式仍然可用

早期版本的 `domains:` / `records:` / `name:` 写法继续支持，升级不会打断运行：

```yaml
domains:
  example.com:
    records:
      - name: pc
        type: both
```

两种写法可以同时存在：相同 zone 会自动合并记录。

## 命令

```bash
cfddns                        # 执行一轮同步（默认行为）
cfddns --dry-run              # 预演，不写入
cfddns --json                 # JSON 输出，便于脚本消费
cfddns --force                # 忽略本地状态，强制重写
cfddns --interval 5m          # 常驻循环，每 5 分钟一轮
cfddns --interval 1天          # 也支持中文单位
cfddns --concurrency 8        # 并发数（默认 4）
cfddns status                 # 查看配置与状态（令牌脱敏）
cfddns gen-config --force     # 重新生成默认配置（只输出一行结果说明）
cfddns paths                  # 显示配置/状态文件路径
cfddns --version
```

退出码：`0` 成功（含"未变化"），`1` 有记录更新失败，`2` 配置/参数错误。

子命令放在选项前后都可以：`cfddns --config x.yaml status` 与
`cfddns status --config x.yaml` 等价。拼错的命令会明确报错，
不会退回默认的 `run`（那会真的写 DNS）。

`--config` 指向的配置若不在主目录下（例如在项目目录里临时试验），
程序会自动转入**只读运行**，不生成 `state.json`，避免污染别人的工作目录。

## 效率设计

按重要性排序：

1. **本地状态快路径。** 状态文件记录"上次已确认写入的值"。若目标值与上次一致，
   直接判定 `unchanged`，**完全不碰 Cloudflare API**。重复运行的开销只剩一次 IP 探测。
2. **写前二次确认。** 真正写入前先读取该记录，值已正确就不发写请求，
   既省一次配额也降低并发写竞争。
3. **单请求 IP 探测。** IPv4 用 Cloudflare trace 端点的 `ip=` 字段；
   IPv6 优先直接读本机网卡（`/proc/net/if_inet6`，零网络开销），失败才问云端。
4. **协议族锁定拨号。** 这是**正确性**要求而非优化：主机同时有 v4/v6 出口时，
   访问双栈域名会被 happy-eyeballs 选到 IPv6，导致 IPv4 探测拿到错误结果。
   程序自己解析域名并只挑对应协议族的地址拨号（TLS SNI 不受影响）。
5. **并发 + zone_id 缓存。** 多记录并发处理，一轮内 zone_id 只查一次并写回状态文件。

## 配置项参考

| 字段 | 说明 |
|---|---|
| `token` | Cloudflare API 令牌（必填） |
| `zone` | 要绑定的域名（必填） |
| `dns` | 记录列表，`- 名字` 或 `- 名字: 类型` |
| `type` | 顶层默认类型：`A` / `AAAA` / `both`（默认 `both`） |
| `api_base` | 自定义 API 端点（默认 Cloudflare 官方） |
| `ipv4.source` / `ipv6.source` | `auto` / `local` / `trace` / `ipify` / `url` / `exec` |
| `ipv4.url` / `ipv6.url` | `source: url` 时读取的地址 |
| `ipv4.command` / `ipv6.command` | `source: exec` 时执行的命令 |
| `match_regex` | 从 url/exec 输出里提取地址的正则（取第一个捕获组） |
| `skip_prefix` | 命中这些前缀的地址被忽略 |
| `ttl` | `1` = Cloudflare 自动，或 `60`~`86400` 秒 |
| `proxy` | 是否开启 Cloudflare 代理（橙云）。DDNS 通常 `false` |

类型取值同时接受别名：`ipv4`/`v4`/`4` = `A`，`ipv6`/`v6`/`6` = `AAAA`，
`all`/`双栈` = `both`。

IPv6 探测默认按 RFC 6724 排序：临时地址优先于稳定地址，并跳过
deprecated、链接本地、ULA（`fc00::/7`）与环回地址。

## 定时运行

### systemd timer（推荐）

```bash
sudo install -m 0644 systemd/cfddns.service systemd/cfddns.timer /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now cfddns.timer
systemctl list-timers cfddns.timer
```

注意 unit 里的 `User=` / `HOME=` 要改成实际运行者；配置必须位于该用户的
`$HOME/.cfddns/config.yaml`。

### cron

```
*/5 * * * * /usr/local/bin/cfddns --quiet >>/var/log/cfddns.log 2>&1
```

## 测试

```bash
cd src && go test ./...
```

覆盖 YAML 解析器边界（缩进/制表符/重复键/引号内 #/序列项内联值）、
极简与旧版两种配置格式、简写记录、类型别名与非法类型、配置校验、
状态文件往返、中文显示宽度对齐、时长解析、错误分支等。

## 代码结构

| 文件 | 职责 |
|---|---|
| `cfddns.go` | 主流程、Cloudflare API、IP 探测、命令 |
| `yaml.go` | YAML 子集解析器与配置装载 |
| `config.go` | 默认配置模板 |
| `width.go` | 终端显示宽度对齐（中日韩字符按 2 列计算） |
| `cmd/dnsverify/` | 独立的 DNS 查询小工具，用于绕过缓存验证记录 |
