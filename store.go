package auctionclearing

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"sync"
)

// EventStore 是领域事件的持久化接口。
// 一次 Append 调用即一次原子提交：同一批事件要么全部可见，要么全部不可见。
type EventStore interface {
	// Append 原子追加一批事件并为其分配连续递增的 Seq，返回落库后的事件。
	Append(events []Event) ([]Event, error)
	// Load 按 Seq 顺序读回全部事件（引擎据此重放构建状态）。
	Load() ([]Event, error)
}

// MemoryEventStore 是进程内事件存储，主要用于测试；并发安全，保证原子追加。
type MemoryEventStore struct {
	mu     sync.Mutex
	next   int64
	events []Event
}

// NewMemoryEventStore 构造空的内存事件存储。
func NewMemoryEventStore() *MemoryEventStore {
	return &MemoryEventStore{}
}

// Append 实现 EventStore。
func (s *MemoryEventStore) Append(events []Event) ([]Event, error) {
	if len(events) == 0 {
		return nil, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Event, len(events))
	for i, ev := range events {
		s.next++
		ev.Seq = s.next
		out[i] = ev
	}
	s.events = append(s.events, out...)
	return out, nil
}

// Load 实现 EventStore，返回事件的副本切片。
func (s *MemoryEventStore) Load() ([]Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Event, len(s.events))
	copy(out, s.events)
	return out, nil
}

// FileEventStore 以 JSON Lines（每行一个事件 JSON）追加写文件，
// 每批写入后 fsync，崩溃时不会出现“半批成交”：
// 单行 JSON 要么完整落盘，要么最后一行残缺被加载阶段拒绝。
type FileEventStore struct {
	mu   sync.Mutex
	f    *os.File
	path string
	next int64
}

// NewFileEventStore 打开（不存在则创建）事件日志文件并读回序号水位。
func NewFileEventStore(path string) (*FileEventStore, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open event log: %w", err)
	}
	s := &FileEventStore{f: f, path: path}
	events, err := s.loadLocked()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	if len(events) > 0 {
		s.next = events[len(events)-1].Seq
	}
	return s, nil
}

// Append 实现 EventStore：整批序列化为 JSON 行，一次 Write 后 Fsync。
func (s *FileEventStore) Append(events []Event) ([]Event, error) {
	if len(events) == 0 {
		return nil, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	out := make([]Event, len(events))
	buf := make([]byte, 0, 256*len(events))
	for i, ev := range events {
		s.next++
		ev.Seq = s.next
		line, err := json.Marshal(ev)
		if err != nil {
			return nil, fmt.Errorf("encode event: %w", err)
		}
		buf = append(buf, line...)
		buf = append(buf, '\n')
		out[i] = ev
	}
	// 循环写保证整批一次写全；O_APPEND + 互斥锁保证写入位置始终在文件末尾。
	for written := 0; written < len(buf); {
		n, err := s.f.Write(buf[written:])
		written += n
		if err != nil {
			return nil, fmt.Errorf("write event log: %w", err)
		}
	}
	if err := s.f.Sync(); err != nil {
		return nil, fmt.Errorf("fsync event log: %w", err)
	}
	return out, nil
}

// Load 实现 EventStore。
func (s *FileEventStore) Load() ([]Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loadLocked()
}

func (s *FileEventStore) loadLocked() ([]Event, error) {
	if _, err := s.f.Seek(0, 0); err != nil {
		return nil, fmt.Errorf("seek event log: %w", err)
	}
	defer func() { _, _ = s.f.Seek(0, 2) }()

	var events []Event
	scanner := bufio.NewScanner(s.f)
	// 允许较大的事件行（清算结果可能包含大量成交明细）。
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var ev Event
		if err := json.Unmarshal(line, &ev); err != nil {
			return nil, fmt.Errorf("event log corrupted at line %d: %w", lineNo, err)
		}
		if events != nil && ev.Seq != events[len(events)-1].Seq+1 {
			return nil, fmt.Errorf("event log sequence gap at line %d", lineNo)
		}
		events = append(events, ev)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read event log: %w", err)
	}
	return events, nil
}

// Close 关闭底层文件。
func (s *FileEventStore) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.f.Close()
}
