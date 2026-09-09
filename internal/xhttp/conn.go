package xhttp

import (
	"io"
	"net"
	"net/http"
	"sync"
	"time"
)

// Conn 把一条 HTTP 请求变成双向连接：
// 读 = 请求体（上行数据），写 = 响应体（下行数据，逐块 Flush）。
// Write 在 ServeHTTP 返回后不可用，因此生命周期由 wait/finish 控制。
type Conn struct {
	mu     sync.Mutex
	w      http.ResponseWriter
	r      io.Reader
	flush  bool
	done   chan struct{}
	once   sync.Once
	closed bool
}

func newConn(w http.ResponseWriter, r io.Reader, flush bool) *Conn {
	return &Conn{w: w, r: r, flush: flush, done: make(chan struct{})}
}

// Read 读取上行数据（请求体）。
func (c *Conn) Read(b []byte) (int, error) {
	return c.r.Read(b)
}

// Write 发送下行数据并立即 Flush（避免中间层缓冲）。
func (c *Conn) Write(b []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return 0, io.ErrClosedPipe
	}
	n, err := c.w.Write(b)
	if err == nil && c.flush {
		if f, ok := c.w.(http.Flusher); ok {
			f.Flush()
		}
	}
	return n, err
}

// Close 幂等地关闭连接。返回 error 以适配 net.Conn 接口
// （stream-one 直接作为连接传给 HandleConn） 。
func (c *Conn) Close() error {
	c.once.Do(func() {
		c.mu.Lock()
		c.closed = true
		c.mu.Unlock()
		close(c.done)
	})
	return nil
}

// wait 阻塞到连接关闭（用于让 ServeHTTP 挂起直到会话结束）。
func (c *Conn) wait() { <-c.done }

// net.Conn 接口补齐：stream-one 把 Conn 直接作为连接交给 VLESS 层。
func (c *Conn) LocalAddr() net.Addr                { return nil }
func (c *Conn) RemoteAddr() net.Addr               { return nil }
func (c *Conn) SetDeadline(t time.Time) error      { return nil }
func (c *Conn) SetReadDeadline(t time.Time) error  { return nil }
func (c *Conn) SetWriteDeadline(t time.Time) error { return nil }
