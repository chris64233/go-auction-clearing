package auctionclearing

import (
	"encoding/json"
	"os"
	"sync"
)

// snapshot 持久化快照。
type snapshot struct {
	Trades        map[string]*Trade                `json:"trades"`
	Batches       map[string]*DeliveryBatch        `json:"batches"`
	Confirmations map[string]*DeliveryConfirmation `json:"confirmations"`
	Idempotency   map[string]*idempotencyEntry     `json:"idempotency"`
	Seq           int64                            `json:"seq"`
}

// idempotencyEntry 外部操作号 -> 请求指纹与结果。
type idempotencyEntry struct {
	Fingerprint string `json:"fingerprint"`
	ConfirmID   string `json:"confirm_id"`
}

// Store 内存存储 + JSON 快照持久化。所有写操作在 Service 锁内完成,
// 保证金记录与交割确认在同一临界区落库,不会产生孤立记录。
type Store struct {
	mu   sync.Mutex
	path string
	data snapshot
}

func NewStore(path string) *Store {
	return &Store{
		path: path,
		data: snapshot{
			Trades:        map[string]*Trade{},
			Batches:       map[string]*DeliveryBatch{},
			Confirmations: map[string]*DeliveryConfirmation{},
			Idempotency:   map[string]*idempotencyEntry{},
		},
	}
}

// Load 从快照恢复;文件不存在时返回空存储。
func LoadStore(path string) (*Store, error) {
	s := NewStore(path)
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return s, nil
		}
		return nil, err
	}
	if err := json.Unmarshal(raw, &s.data); err != nil {
		return nil, err
	}
	return s, nil
}

// save 落盘(调用方须持有 mu)。先写临时文件再 rename,避免半写。
func (s *Store) saveLocked() error {
	if s.path == "" {
		return nil
	}
	raw, err := json.MarshalIndent(&s.data, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}
