package xhttp

import (
	"container/heap"
	"io"
	"sync"
)

// Packet 是上行队列中的一个数据单元：要么是 packet-up 的完整
// 请求体（Payload != nil），要么是 stream-up 的整个连接（Reader != nil）。
type Packet struct {
	Reader  *Conn
	Payload []byte
	Seq     uint64
}

// uploadQueue 复刻 Xray-core splithttp/upload_queue.go 的语义：
//   - stream-up（Reader 包）只允许一个，独占后所有读请求直接透传给它；
//   - packet-up（Payload 包）按 Seq 升序重排输出，早于 nextSeq 的包被丢弃；
//   - 重排缓冲超过 maxPackets 时报错（对应 Xray "packet queue is too large"）；
//   - Close 之后 Read 永远返回 io.EOF（用于触发会话结束）。
type uploadQueue struct {
	pushed     chan Packet
	heap       uploadHeap
	nextSeq    uint64
	maxPackets int

	mu     sync.Mutex
	reader *Conn
	closed bool
}

func newUploadQueue(maxPackets int) *uploadQueue {
	return &uploadQueue{
		pushed:     make(chan Packet, maxPackets),
		maxPackets: maxPackets,
	}
}

func (q *uploadQueue) Close() {
	q.mu.Lock()
	if q.closed {
		q.mu.Unlock()
		return
	}
	q.closed = true
	r := q.reader
	q.mu.Unlock()
	close(q.pushed)
	if r != nil {
		r.Close()
	}
}

// push 投递一个包。stream-up 的 Reader 包具有独占性：已有 reader 时
// 再次 push reader 会失败（对应 Xray Push 返回 "h.reader already exists"）。
func (q *uploadQueue) push(p Packet) error {
	q.mu.Lock()
	if q.closed {
		q.mu.Unlock()
		return io.ErrClosedPipe
	}
	if p.Reader != nil {
		if q.reader != nil {
			q.mu.Unlock()
			return errReaderTaken
		}
		q.reader = p.Reader
	}
	q.mu.Unlock()
	q.pushed <- p
	return nil
}

var errReaderTaken = errorString("xhttp: stream-up reader already taken")

func errorString(s string) error { return &errStr{s} }

type errStr struct{ s string }

func (e *errStr) Error() string { return e.s }

// Read 按 seq 顺序产出上行数据；实现 io.Reader。
func (q *uploadQueue) Read(b []byte) (int, error) {
	if q.closed {
		return 0, io.EOF
	}

	q.mu.Lock()
	r := q.reader
	q.mu.Unlock()
	if r != nil {
		// stream-up：直接从 POST 连接体读取。
		return r.Read(b)
	}

	if len(q.heap) == 0 {
		p, ok := <-q.pushed
		if !ok {
			return 0, io.EOF
		}
		if p.Reader != nil {
			// 并发下 stream-up 包刚到达：切换为透传模式。
			return p.Reader.Read(b)
		}
		heap.Push(&q.heap, p)
	}

	for len(q.heap) > 0 {
		packet := heap.Pop(&q.heap).(Packet)

		if packet.Seq == q.nextSeq {
			n := copy(b, packet.Payload)
			if n < len(packet.Payload) {
				// 半包读：剩余部分留回堆里继续输出。
				packet.Payload = packet.Payload[n:]
				heap.Push(&q.heap, packet)
			} else {
				q.nextSeq = packet.Seq + 1
			}
			return n, nil
		}

		if packet.Seq > q.nextSeq {
			// 乱序：等待后续 seq 的包（客户端会按序号补发）。
			if len(q.heap) > q.maxPackets {
				return 0, errorString("xhttp: packet queue is too large")
			}
			heap.Push(&q.heap, packet)
			p, ok := <-q.pushed
			if !ok {
				return 0, io.EOF
			}
			if p.Reader != nil {
				return p.Reader.Read(b)
			}
			heap.Push(&q.heap, p)
		}
		// packet.Seq < nextSeq：重复或过期包，直接丢弃。
	}

	return 0, nil
}

// heap 实现按 Seq 的最小堆（container/heap）。
type uploadHeap []Packet

func (h uploadHeap) Len() int           { return len(h) }
func (h uploadHeap) Less(i, j int) bool { return h[i].Seq < h[j].Seq }
func (h uploadHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }

func (h *uploadHeap) Push(x any) { *h = append(*h, x.(Packet)) }

func (h *uploadHeap) Pop() any {
	old := *h
	n := len(old)
	x := old[n-1]
	*h = old[:n-1]
	return x
}
