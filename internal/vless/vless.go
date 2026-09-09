// Package vless 实现最小化的 VLESS 协议头解析与响应编码。
// 线格式与 Xray-core 保持一致：
//
//	版本(1B=0) | UUID(16B) | 附加数据长度(1B) | 附加数据(protobuf) | 指令(1B)
//	| 端口(2B 大端) | 地址类型(1B) | 地址(变长)
//
// 地址类型：1=IPv4(4B) 2=域名(1B长度+内容) 3=IPv6(16B)。
// 响应头固定为两个零字节：版本(0) + 附加数据长度(0)。
package vless

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"

	"xhttp-go/internal/uuid"
)

// 指令常量（对应 Xray protocol.RequestCommand*）。
const (
	CommandTCP byte = 0x01
	CommandUDP byte = 0x02
	CommandMux byte = 0x03 // 本实现不支持 MUX，收到时拒绝该会话
)

// 地址类型常量（对应 Xray protocol.AddressType*）。
const (
	AddrIPv4   byte = 0x01
	AddrDomain byte = 0x02
	AddrIPv6   byte = 0x03
)

// Version 是 VLESS 协议版本号（当前恒为 0）。
const Version byte = 0

var (
	ErrInvalidVersion = errors.New("vless: invalid protocol version")
	ErrInvalidUser    = errors.New("vless: invalid user")
	ErrInvalidCommand = errors.New("vless: invalid command")
	ErrInvalidAddress = errors.New("vless: invalid address")
)

// Request 是解析后的 VLESS 请求头。
type Request struct {
	Version  byte
	UUID     uuid.UUID
	Command  byte
	Port     uint16
	AddrType byte
	Addr     []byte // IPv4 为 4 字节，域名为原始域名，IPv6 为 16 字节
}

// Host 返回目标主机（IP 已还原为文本形式）。
func (r *Request) Host() string {
	switch r.AddrType {
	case AddrIPv4:
		return net.IP(r.Addr).String()
	case AddrIPv6:
		return net.IP(r.Addr).String()
	default:
		return string(r.Addr)
	}
}

// Address 以 host:port 形式返回目标地址（可直接交给 net.Dial）。
// IPv6 由 JoinHostPort 自动加方括号。
func (r *Request) Address() string {
	return net.JoinHostPort(r.Host(), strconv.Itoa(int(r.Port)))
}

// Network 返回 "tcp" 或 "udp"。
func (r *Request) Network() string {
	if r.Command == CommandUDP {
		return "udp"
	}
	return "tcp"
}

// Destination 用于日志输出。
func (r *Request) Destination() string {
	return r.Network() + ":" + r.Address()
}

// ParseRequest 从 r 中读取并解析一个 VLESS 请求头。
// 附加数据（protobuf）仅跳过——本实现不支持 flow/seed 等扩展，
// 客户端不带 flow 时该字段通常为空。
func ParseRequest(r io.Reader) (*Request, error) {
	var hdr [18]byte // 版本(1) + UUID(16) + 附加数据长度(1)
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, fmt.Errorf("vless: read header: %w", err)
	}
	if hdr[0] != Version {
		return nil, ErrInvalidVersion
	}
	req := &Request{Version: hdr[0]}
	copy(req.UUID[:], hdr[1:17])

	if addonsLen := int(hdr[17]); addonsLen > 0 {
		if _, err := io.CopyN(io.Discard, r, int64(addonsLen)); err != nil {
			return nil, fmt.Errorf("vless: read addons: %w", err)
		}
	}

	var rest [4]byte // 指令(1) + 端口(2) + 地址类型(1)
	if _, err := io.ReadFull(r, rest[:]); err != nil {
		return nil, fmt.Errorf("vless: read command/port/addrtype: %w", err)
	}
	req.Command = rest[0]
	req.Port = binary.BigEndian.Uint16(rest[1:3])
	req.AddrType = rest[3]

	switch req.AddrType {
	case AddrIPv4:
		req.Addr = make([]byte, 4)
	case AddrDomain:
		var l [1]byte
		if _, err := io.ReadFull(r, l[:]); err != nil {
			return nil, fmt.Errorf("vless: read domain length: %w", err)
		}
		req.Addr = make([]byte, l[0])
	case AddrIPv6:
		req.Addr = make([]byte, 16)
	default:
		return nil, fmt.Errorf("%w: %d", ErrInvalidAddress, req.AddrType)
	}
	if len(req.Addr) > 0 {
		if _, err := io.ReadFull(r, req.Addr); err != nil {
			return nil, fmt.Errorf("vless: read address: %w", err)
		}
	}
	if req.Command != CommandTCP && req.Command != CommandUDP {
		if req.Command == CommandMux {
			return nil, fmt.Errorf("%w: mux is not supported", ErrInvalidCommand)
		}
		return nil, ErrInvalidCommand
	}
	return req, nil
}

// WriteResponse 写入 VLESS 响应头（版本 + 空附加数据，共 2 字节）。
func WriteResponse(w io.Writer) error {
	_, err := w.Write([]byte{Version, 0})
	return err
}
