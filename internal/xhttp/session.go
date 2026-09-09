package xhttp

import (
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// session 对应 Xray 的 httpSession：一次 VLESS 代理连接的全部
// HTTP 请求集合。生命周期：
//
//	创建 → packet-up/stream-up 上行就绪 → GET 到达（connected）
//	→ 桥接双向 → 上行 EOF 或下行关闭 → 回收。
type session struct {
	id        string
	queue     *uploadQueue
	connected chan struct{}
	connOnce  sync.Once

	mu     sync.Mutex
	closed bool
	down   *Conn // GET 请求的下行通道
}

var errSessionClosed = errors.New("xhttp: session closed")

func newSession(id string) *session {
	return &session{
		id:        id,
		queue:     newUploadQueue(scMaxBufferedPosts),
		connected: make(chan struct{}),
	}
}

func (s *session) close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	down := s.down
	s.mu.Unlock()
	s.queue.Close()
	if down != nil {
		down.Close()
	}
}

// serveDown 处理 GET /{path}/{sid}：把响应体作为下行通道，
// 并在上行与 VLESS 目标之间完成双向桥接。
func (h *Handler) serveDown(w http.ResponseWriter, r *http.Request, sid string) {
	s := h.upsertSession(sid)
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		h.dropSession(sid, s)
		s = h.upsertSession(sid)
		s.mu.Lock()
	}
	if s.down != nil {
		s.mu.Unlock()
		http.Error(w, "session already has a download stream", http.StatusConflict)
		return
	}
	// Xray 对 stream-down 的固定响应头。
	hdr := w.Header()
	hdr.Set("X-Accel-Buffering", "no")
	hdr.Set("Cache-Control", "no-store")
	hdr.Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	down := newConn(w, r.Body, true)
	s.down = down
	s.mu.Unlock()

	// GET 到达：解除 30s TTL，把会话作为 net.Conn 交给 VLESS 层。
	// up = uploadQueue（VLESS 层从中读上行），down = GET 响应体。
	s.connOnce.Do(func() {
		close(s.connected)
		if h.HandleConn != nil {
			go h.HandleConn(&sessionConn{session: s})
		}
	})

	// 阻塞直到下行关闭或请求取消，然后回收会话。
	select {
	case <-down.done:
	case <-r.Context().Done():
	}
	down.Close()
	h.dropSession(sid, s)
}

// servePacketUp 处理 POST /{path}/{sid}/{seq}：读取完整请求体作为
// 一个上行包入队，按 seq 重排后交给目标连接。
func (h *Handler) servePacketUp(w http.ResponseWriter, r *http.Request, sid, seqStr string) {
	if r.ContentLength > scMaxEachPostBytes {
		http.Error(w, "upload too large", http.StatusRequestEntityTooLarge)
		return
	}
	seq, err := strconv.ParseUint(seqStr, 10, 64)
	if err != nil {
		http.Error(w, "invalid seq", http.StatusInternalServerError)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, scMaxEachPostBytes+1))
	if err != nil {
		http.Error(w, "failed to read body", http.StatusBadRequest)
		return
	}
	if len(body) > scMaxEachPostBytes {
		http.Error(w, "upload too large", http.StatusRequestEntityTooLarge)
		return
	}

	s := h.upsertSession(sid)
	if err := s.queue.push(Packet{Payload: body, Seq: seq}); err != nil {
		http.Error(w, "session closed", http.StatusInternalServerError)
		return
	}
	if len(body) == 0 {
		w.Header().Set("Cache-Control", "no-store")
	}
	w.WriteHeader(http.StatusOK)
}

// serveStreamUp 处理 POST /{path}/{sid}：请求体即整个上行流。
// 一个会话只允许一个 stream-up 读取器（否则 409）。
func (h *Handler) serveStreamUp(w http.ResponseWriter, r *http.Request, sid string) {
	s := h.upsertSession(sid)

	// 先 push 成功再写 200（与官方 hub.go 一致）：push 失败时
	// 响应头尚未发出，可直接写 409 Conflict；反之 200 一旦写出
	// 不可撤销（Go 对二次 WriteHeader 会记 superfluous 警告）。
	conn := newConn(w, r.Body, false)
	if err := s.queue.push(Packet{Reader: conn}); err != nil {
		conn.Close()
		http.Error(w, "stream-up reader already taken", http.StatusConflict)
		return
	}
	hdr := w.Header()
	hdr.Set("X-Accel-Buffering", "no")
	hdr.Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}

	// 等到会话结束（目标连接关闭）再返回，避免响应写侧提前失效。
	select {
	case <-conn.done:
	case <-r.Context().Done():
	}
	conn.Close()
}

// serveStreamOne 处理 GET /{path}（带请求体）：单连接双向流。
// 不经过会话/队列：读 = 请求体，写 = 响应体，直接桥接给 VLESS。
func (h *Handler) serveStreamOne(w http.ResponseWriter, r *http.Request) {
	hdr := w.Header()
	hdr.Set("X-Accel-Buffering", "no")
	hdr.Set("Cache-Control", "no-store")
	hdr.Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}

	conn := newConn(w, r.Body, true)
	if h.HandleConn != nil {
		go h.HandleConn(conn)
	}

	// 等到连接关闭或请求取消，再回收（conn.Close 由 HandleConn/defer 完成）。
	select {
	case <-conn.done:
	case <-r.Context().Done():
	}
	conn.Close()
}

// sessionConn 把会话暴露成 net.Conn：读 = 上行队列，写 = 下行 GET 连接。
type sessionConn struct {
	*session
}

func (c *sessionConn) Read(b []byte) (int, error) { return c.queue.Read(b) }
func (c *sessionConn) Write(b []byte) (int, error) {
	if d := c.getDown(); d != nil {
		return d.Write(b)
	}
	return 0, errSessionClosed
}
func (c *sessionConn) Close() error {
	c.session.close()
	return nil
}
func (c *sessionConn) LocalAddr() net.Addr                { return nil }
func (c *sessionConn) RemoteAddr() net.Addr               { return nil }
func (c *sessionConn) SetDeadline(t time.Time) error      { return nil }
func (c *sessionConn) SetReadDeadline(t time.Time) error  { return nil }
func (c *sessionConn) SetWriteDeadline(t time.Time) error { return nil }

func (s *session) getDown() *Conn {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.down
}
