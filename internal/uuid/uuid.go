// Package uuid 提供标准库级别的 UUID 解析（RFC 4122 文本格式）。
// Go 标准库不导出通用 UUID 类型（os.UserConfigDir 那类内置 UUID 位于
// internal 包），因此这里自己实现一个最小的 128-bit 类型。
package uuid

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
)

// Size 是 UUID 的字节长度。
const Size = 16

type UUID [Size]byte

var ErrInvalidFormat = errors.New("uuid: invalid format")

// Parse 解析 36 字符的标准连字符格式：8-4-4-4-12。
func Parse(s string) (UUID, error) {
	var u UUID
	if len(s) != 36 || s[8] != '-' || s[13] != '-' || s[18] != '-' || s[23] != '-' {
		return u, ErrInvalidFormat
	}
	pos := 0
	for i := 0; i < 36; i++ {
		c := s[i]
		if c == '-' {
			if i != 8 && i != 13 && i != 18 && i != 23 {
				return u, ErrInvalidFormat
			}
			continue
		}
		n, ok := hexVal(c)
		if !ok {
			return u, ErrInvalidFormat
		}
		if pos%2 == 0 {
			u[pos/2] = n << 4
		} else {
			u[pos/2] |= n
		}
		pos++
	}
	if pos != 32 {
		return u, ErrInvalidFormat
	}
	return u, nil
}

func hexVal(c byte) (byte, bool) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', true
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, true
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10, true
	}
	return 0, false
}

// Bytes 返回 UUID 的 16 字节原始表示。
func (u UUID) Bytes() []byte { return u[:] }

// String 以 8-4-4-4-12 小写格式返回。
func (u UUID) String() string {
	var buf [36]byte
	hex.Encode(buf[0:8], u[0:4])
	buf[8] = '-'
	hex.Encode(buf[9:13], u[4:6])
	buf[13] = '-'
	hex.Encode(buf[14:18], u[6:8])
	buf[18] = '-'
	hex.Encode(buf[19:23], u[8:10])
	buf[23] = '-'
	hex.Encode(buf[24:36], u[10:16])
	return string(buf[:])
}

// MustNewRandom 生成随机 UUID；rand 失败时 panic（进程级初始化场景）。
func MustNewRandom() UUID {
	u, err := NewRandom()
	if err != nil {
		panic(err)
	}
	return u
}

// NewRandom 生成 RFC 4122 version 4 随机 UUID。
func NewRandom() (UUID, error) {
	var u UUID
	if _, err := rand.Read(u[:]); err != nil {
		return u, err
	}
	u[6] = (u[6] & 0x0f) | 0x40 // version 4
	u[8] = (u[8] & 0x3f) | 0x80 // RFC 4122 variant
	return u, nil
}
