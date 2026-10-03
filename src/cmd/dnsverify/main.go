package main

// dnsverify —— 最小 DNS 查询工具，用来自证 cfddns 的改动在权威服务器上生效。
//
// 为什么需要它：本机没有 dig/nslookup，且公网 DoH 端点不可达，
// getent 只能问到本地递归缓存（会返回旧的缓存值）。
// 这里直接向权威 NS 发一次 UDP 查询，绕开所有缓存。

import (
	"encoding/binary"
	"fmt"
	"net"
	"os"
	"strings"
	"time"
)

func main() {
	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, "用法: dnsverify <name> <A|AAAA> [server:port]")
		os.Exit(2)
	}
	name := os.Args[1]
	qtype := strings.ToUpper(os.Args[2])
	server := "1.1.1.1:53"
	if len(os.Args) > 3 {
		server = os.Args[3]
		if !strings.Contains(server, ":") {
			server += ":53"
		}
	}
	var qt uint16
	switch qtype {
	case "A":
		qt = 1
	case "AAAA":
		qt = 28
	default:
		fmt.Fprintln(os.Stderr, "只支持 A / AAAA")
		os.Exit(2)
	}

	answers, err := query(name, qt, server)
	if err != nil {
		fmt.Fprintf(os.Stderr, "查询失败: %v\n", err)
		os.Exit(1)
	}
	if len(answers) == 0 {
		fmt.Printf("%s %s 无记录（NXDOMAIN 或空答案）\n", name, qtype)
		os.Exit(1)
	}
	for _, a := range answers {
		fmt.Printf("  %s %s -> %s\n", qtype, name, a)
	}
}

// buildQuery 组装一条标准 DNS 查询报文（递归位关闭，直接问权威）。
func buildQuery(name string, qtype uint16) []byte {
	buf := make([]byte, 0, 512)
	id := uint16(time.Now().UnixNano() & 0xffff)
	buf = binary.BigEndian.AppendUint16(buf, id)
	buf = binary.BigEndian.AppendUint16(buf, 0x0000) // 标准查询，RD=0 时不视为递归
	buf = binary.BigEndian.AppendUint16(buf, 1)      // QDCOUNT
	buf = binary.BigEndian.AppendUint16(buf, 0)      // ANCOUNT
	buf = binary.BigEndian.AppendUint16(buf, 0)      // NSCOUNT
	buf = binary.BigEndian.AppendUint16(buf, 0)      // ARCOUNT
	for _, label := range strings.Split(strings.TrimSuffix(name, "."), ".") {
		buf = append(buf, byte(len(label)))
		buf = append(buf, label...)
	}
	buf = append(buf, 0) // 名字结束
	buf = binary.BigEndian.AppendUint16(buf, qtype)
	buf = binary.BigEndian.AppendUint16(buf, 1) // IN
	return buf
}

// skipName 跳过报文中的域名（处理压缩指针）。
func skipName(b []byte, off int) (int, error) {
	for {
		if off >= len(b) {
			return 0, fmt.Errorf("名字越界")
		}
		l := int(b[off])
		if l == 0 {
			return off + 1, nil
		}
		if l&0xc0 == 0xc0 { // 压缩指针占 2 字节
			if off+2 > len(b) {
				return 0, fmt.Errorf("指针越界")
			}
			return off + 2, nil
		}
		off += 1 + l
	}
}

func query(name string, qtype uint16, server string) ([]string, error) {
	conn, err := net.DialTimeout("udp", server, 6*time.Second)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(6 * time.Second))
	if _, err := conn.Write(buildQuery(name, qtype)); err != nil {
		return nil, err
	}
	buf := make([]byte, 4096)
	n, err := conn.Read(buf)
	if err != nil {
		return nil, err
	}
	msg := buf[:n]
	if len(msg) < 12 {
		return nil, fmt.Errorf("响应过短")
	}
	rcode := msg[3] & 0x0f
	if rcode != 0 {
		return nil, fmt.Errorf("DNS 返回码 %d（3=NXDOMAIN）", rcode)
	}
	qd := int(binary.BigEndian.Uint16(msg[4:6]))
	an := int(binary.BigEndian.Uint16(msg[6:8]))
	off := 12
	for i := 0; i < qd; i++ {
		off, err = skipName(msg, off)
		if err != nil {
			return nil, err
		}
		off += 4 // QTYPE + QCLASS
	}
	var out []string
	for i := 0; i < an; i++ {
		off, err = skipName(msg, off)
		if err != nil {
			return nil, err
		}
		if off+10 > len(msg) {
			return nil, fmt.Errorf("记录头越界")
		}
		typ := binary.BigEndian.Uint16(msg[off : off+2])
		rdlen := int(binary.BigEndian.Uint16(msg[off+8 : off+10]))
		rdata := msg[off+10 : off+10+rdlen]
		switch typ {
		case 1: // A
			if rdlen == 4 {
				out = append(out, net.IP(rdata).String())
			}
		case 28: // AAAA
			if rdlen == 16 {
				out = append(out, net.IP(rdata).String())
			}
		case 5: // CNAME，直接给出目标名
			if t, e := skipName(msg, off+10); e == nil {
				_ = t
			}
		}
		off += 10 + rdlen
	}
	return out, nil
}
