// Package xhttp 实现最小化的 XHTTP 传输层服务端。
//
// 语义与 Xray-core transport/internet/splithttp 保持一致（默认配置，
// session/seq 均在路径中，uplink 数据在请求体）：
//
//	GET  /{path}/{sid}            stream-down：响应体即下行流
//	POST /{path}/{sid}/{seq}      packet-up：请求体是一个按 seq 重排的上行包
//	POST /{path}/{sid}            stream-up：请求体即整个上行流（独占）
//
// 客户端在会话末尾发一个 seq=0 的空包表示上行结束（Xray 客户端
// splitConn 关闭时的行为，服务端收到后队列闭环，目标连接写侧关闭）。
package xhttp

import (
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	// scMaxEachPostBytes：单个 packet-up 请求体上限（Xray 默认 1MB，超限 413）。
	scMaxEachPostBytes = 1 << 20
	// scMaxBufferedPosts：重排缓冲容量（Xray 默认 30）。
	scMaxBufferedPosts = 30
)

// Handler 处理 XHTTP 请求，并把解析出的双向流交给 VLESS 层。
type Handler struct {
	// Path 是配置的路径（含前导斜杠，如 "/xtp"）。规范化为带尾部斜杠
	// （对应 Xray GetNormalizedPath 在 PlacementPath 下的行为）。
	Path string

	// HandleConn 由上层注入：在 VLESS 头解析完成后调用，传入
	// 上行 reader（来自 uploadQueue）与下行 writer（来自 GET 连接）。
	HandleConn func(c net.Conn)

	// RequirePadding 开启时强制校验每个请求的 x_padding
	// （长度 [100,1000]，Referer 或 URL 查询参数）。关闭时放行。
	RequirePadding bool

	mu       sync.Mutex
	sessions map[string]*session
}

// NewHandler 创建 Handler。requirePadding 对应服务端
// PADDING_REQUIRED 环境变量（默认关闭即放行无 padding 请求）。
func NewHandler(path string, handleConn func(net.Conn), requirePadding bool) *Handler {
	if !strings.HasSuffix(path, "/") {
		path += "/"
	}
	return &Handler{Path: path, HandleConn: handleConn, RequirePadding: requirePadding, sessions: map[string]*session{}}
}

// upsertSession 查找或创建会话。创建后 30 秒内未完成 GET 即回收
// （对应 Xray isFullyConnected 的 TTL 语义）；完成 GET 后由请求生命周期管理。
func (h *Handler) upsertSession(id string) *session {
	h.mu.Lock()
	if s, ok := h.sessions[id]; ok {
		h.mu.Unlock()
		return s
	}
	s := newSession(id)
	h.sessions[id] = s
	h.mu.Unlock()

	go func() {
		select {
		case <-s.connected: // GET 已完成，会话交由请求关闭逻辑回收
		case <-time.After(30 * time.Second):
			h.dropSession(id, s)
		}
	}()
	return s
}

func (h *Handler) dropSession(id string, s *session) {
	h.mu.Lock()
	if cur, ok := h.sessions[id]; ok && cur == s {
		delete(h.sessions, id)
	}
	h.mu.Unlock()
	s.close()
}

// ServeHTTP 实现 http.Handler，路由规则见包注释。
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !strings.HasPrefix(r.URL.Path, h.Path) && r.URL.Path+"/" != h.Path {
		http.NotFound(w, r)
		return
	}

	// CORS（对齐 Xray WriteResponseHeader，浏览器 dialer 需要）。
	h.writeCommonHeaders(w, r)

	// padding 校验：非混淆模式下客户端总是携带 x_padding
	// （URL 查询参数或 Referer 查询参数），缺失或长度出界一律 400。
	if h.RequirePadding && !checkPadding(r) {
		http.Error(w, "invalid padding", http.StatusBadRequest)
		return
	}

	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusOK)
		return
	}

	sid, seqStr := extractMeta(r, h.Path)

	switch {
	case seqStr != "":
		// POST /{path}/{sid}/{seq} —— packet-up
		if r.Method != http.MethodPost && r.Method != http.MethodPut {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		h.servePacketUp(w, r, sid, seqStr)
	case r.Method == http.MethodGet:
		if sid == "" {
			// GET /{path} —— 无会话请求，仅探活。
			w.WriteHeader(http.StatusOK)
			return
		}
		h.serveDown(w, r, sid)
	case r.Method == http.MethodPost || r.Method == http.MethodPut:
		if sid == "" {
			// POST /{path}/ 无 sid —— stream-one（官方 hub.go：sessionId=="" 时
			// 请求直接作为整条连接：body 读入、响应即下行）。
			h.serveStreamOne(w, r)
			return
		}
		// POST /{path}/{sid} —— stream-up
		h.serveStreamUp(w, r, sid)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// writeCommonHeaders 设置 Xray 服务端固定返回的响应头。
func (h *Handler) writeCommonHeaders(w http.ResponseWriter, r *http.Request) {
	hdr := w.Header()
	if origin := r.Header.Get("Origin"); origin == "" {
		hdr.Set("Access-Control-Allow-Origin", "*")
	} else {
		hdr.Set("Access-Control-Allow-Origin", origin)
	}
	if r.Method == http.MethodOptions {
		if m := r.Header.Get("Access-Control-Request-Method"); m != "" {
			hdr.Set("Access-Control-Allow-Methods", m)
		} else {
			hdr.Set("Access-Control-Allow-Methods", "*")
		}
		if hh := r.Header.Get("Access-Control-Request-Headers"); hh != "" {
			hdr.Set("Access-Control-Allow-Headers", hh)
		} else {
			hdr.Set("Access-Control-Allow-Headers", "*")
		}
	}
}

// extractMeta 从路径中取出 sessionId 与 seq（PlacementPath 默认布局）。
// 路径形如 {path}/{sid}[/{seq}]；h.Path 已带尾部斜杠。
func extractMeta(r *http.Request, path string) (sid, seq string) {
	sub := strings.Split(strings.TrimPrefix(r.URL.Path, path), "/")
	if len(sub) > 0 {
		sid = sub[0]
	}
	if len(sub) > 1 {
		seq = sub[1]
	}
	return
}

// checkPadding 对应 Xray ExtractXPaddingFromRequest + IsPaddingValid
// （非混淆模式）：padding 在 Referer 查询参数或 URL 查询参数的
// x_padding 键中，长度必须在 [100,1000]（默认 XPaddingBytes 范围）。
func checkPadding(r *http.Request) bool {
	var pad string
	if ref := r.Header.Get("Referer"); ref != "" {
		if u, err := url.Parse(ref); err == nil {
			pad = u.Query().Get("x_padding")
		}
	} else {
		pad = r.URL.Query().Get("x_padding")
	}
	if pad == "" {
		return false
	}
	return len(pad) >= 100 && len(pad) <= 1000
}
