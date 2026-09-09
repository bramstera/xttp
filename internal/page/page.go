// Package page 提供伪装页：编译时嵌入的 index.html。
//
// 替换本目录下的 index.html 后重新编译即可自定义伪装页；
// 默认内容为 "Hello world"。
package page

import (
	_ "embed"
)

//go:embed index.html
var Index []byte
