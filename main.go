// xhttp-go 是 Xray-core 的 VLESS+XHTTP 协议的最小化零依赖重写（仅服务端）。
//
// 架构：
//
//	HTTP 服务 → xhttp.Handler（XHTTP 传输层，重组上行/提供下行）
//	          → xhttpConn（把 HTTP 会话适配成 net.Conn）
//	          → vless.ParseRequest（VLESS 协议头）
//	          → freedom（直连目标：TCP 或 UDP）
package main

import (
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"xhttp-go/internal/page"
	"xhttp-go/internal/uuid"
	"xhttp-go/internal/vless"
	"xhttp-go/internal/xhttp"
)

var (
	listenAddr = flag.String("host", envOr("XHOST", "127.0.0.1"), "HTTP 监听地址（直连公网设 0.0.0.0；IPv6-only 服务器设 ::）")
	port       = flag.Int("port", envIntOr("PORT", 6027), "HTTP 监听端口")
	pathFlag   = flag.String("path", envOr("XPATH", "/xtp"), "XHTTP 路径")
	uuidFlag   = flag.String("uuid", envOr("UUID", "b64c9a01-3f09-4dea-a0f1-dc85e5a3ac19"), "VLESS 用户 UUID")
	camoFlag   = flag.Bool("camo", envBoolOr("HEALTHY_PAGE", true), "启用伪装页（根路径返回 index.html/Hello world）")
)

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envIntOr(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil {
			return n
		}
	}
	return def
}

func envBoolOr(key string, def bool) bool {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return def
	}
	return b
}

// freedom 直连出站：TCP 直接拨号；UDP 用 NAT 表维护会话（5 秒空闲回收）。
type freedom struct{}

func (f *freedom) dial(req *vless.Request) (net.Conn, error) {
	addr := req.Address()
	if req.Command == vless.CommandTCP {
		return net.DialTimeout("tcp", addr, 15*time.Second)
	}
	// UDP：与 Xray freedom 的 PacketConnWrapper 语义一致——
	// WriteTo 固定目标，ReadFrom 忽略来源地址（保持 cone NAT 行为）。
	udpAddr, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		return nil, err
	}
	remote, err := net.DialUDP("udp", nil, udpAddr)
	if err != nil {
		return nil, err
	}
	return &udpConn{UDPConn: remote}, nil
}

// udpConn 把 connected UDP socket 包装成面向目标的 net.Conn。
type udpConn struct {
	*net.UDPConn
}

// handleConn 是 VLESS 层入口：在 xhttpConn 上解析 VLESS 头，
// 校验 UUID，然后桥接到 freedom 出站。
// TCP：双向 io.Copy。UDP：两个方向都使用 2 字节大端长度分帧
// （对应 Xray LengthPacketReader/Writer）。
func handleConn(nc net.Conn) {
	defer nc.Close()

	req, err := vless.ParseRequest(nc)
	if err != nil {
		log.Printf("[vless] parse request: %v", err)
		return
	}
	if !validUUID(req.UUID) {
		log.Printf("[vless] invalid user: %s", req.UUID)
		return
	}

	// 响应头（2 字节），写入后下行流即为纯数据。
	if err := vless.WriteResponse(nc); err != nil {
		return
	}

	dst, err := out.dial(req)
	if err != nil {
		log.Printf("[vless] dial %s: %v", req.Destination(), err)
		return
	}
	defer dst.Close()

	log.Printf("[vless] -> %s", req.Destination())

	if req.Command == vless.CommandTCP {
		go func() {
			io.Copy(dst, nc)
			// 上行结束：半关目标连接的写侧，让目标回传剩余数据。
			if rw, ok := dst.(interface{ CloseWrite() error }); ok {
				rw.CloseWrite()
			}
		}()
		io.Copy(nc, dst)
		return
	}

	// UDP over VLESS：2 字节 BE 长度分帧。
	go copyFramed(dst, nc) // 上行：VLESS 流 → 分帧 → UDP
	uc, ok := dst.(*udpConn)
	if !ok {
		return
	}
	copyUDPToFrame(nc, uc) // 回程：UDP 整包 → 加帧 → VLESS 流
}

// copyUDPToFrame 从 UDP socket 读整包（ReadFrom 保证不截断），
// 加 2 字节大端长度帧头后写入 VLESS 下行流。
func copyUDPToFrame(dst io.Writer, src *udpConn) {
	pkt := make([]byte, 65535)
	for {
		n, err := src.UDPConn.Read(pkt)
		if err != nil {
			return
		}
		hdr := []byte{byte(n >> 8), byte(n)}
		if _, err := dst.Write(append(hdr, pkt[:n]...)); err != nil {
			return
		}
	}
}

// copyFramed 在读写之间复制 2 字节大端长度分帧的 UDP 包。
func copyFramed(dst io.WriteCloser, src io.Reader) {
	defer func() {
		if c, ok := dst.(interface{ Close() error }); ok {
			c.Close()
		}
	}()
	var hdr [2]byte
	var pkt [65535]byte
	for {
		if _, err := io.ReadFull(src, hdr[:]); err != nil {
			return
		}
		n := int(hdr[0])<<8 | int(hdr[1])
		if _, err := io.ReadFull(src, pkt[:n]); err != nil {
			return
		}
		if _, err := dst.Write(pkt[:n]); err != nil {
			return
		}
	}
}

var (
	out       *freedom
	validUUID func(uuid.UUID) bool
)

func main() {
	flag.Parse()

	u, err := uuid.Parse(*uuidFlag)
	if err != nil {
		fmt.Fprintln(os.Stderr, "invalid -uuid:", err)
		os.Exit(2)
	}
	validUUID = func(x uuid.UUID) bool { return x == u }
	out = &freedom{}

	path := *pathFlag
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}

	xh := xhttp.NewHandler(path, func(c net.Conn) {
		go handleConn(c)
	})

	addr := fmt.Sprintf("%s:%d", *listenAddr, *port)
	srv := &http.Server{
		Addr: addr,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// 伪装页：HEALTHY_PAGE=true（或 -camo）时根路径返回
			// 编译时嵌入的 index.html（无则 "Hello world"）。
			if r.URL.Path == "/" {
				if !*camoFlag {
					w.Header().Set("Content-Type", "text/plain; charset=utf-8")
					fmt.Fprintln(w, "OK")
					return
				}
				if isHTML(page.Index) {
					w.Header().Set("Content-Type", "text/html; charset=utf-8")
				} else {
					// 纯文本 index.html（默认 "Hello world"）。
					w.Header().Set("Content-Type", "text/plain; charset=utf-8")
				}
				w.Write(page.Index)
				return
			}
			xh.ServeHTTP(w, r)
		}),
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
		<-sig
		srv.Close()
	}()

	fmt.Printf("xhttp-go listening on %s\n", addr)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}

// isHTML 判断嵌入的 index.html 是否为 HTML 文档（而非纯文本）。
func isHTML(b []byte) bool {
	s := strings.ToLower(string(b))
	return strings.Contains(s, "<!doctype html") || strings.Contains(s, "<html")
}
