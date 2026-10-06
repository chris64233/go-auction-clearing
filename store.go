package auctionclearing

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
)

// Store 是清算与交割数据的内存存储，可选地以 JSON 快照持久化到磁盘。
// 所有变更在同一把锁内完成并原子落盘，保证崩溃恢复后不会出现
// 交割记录与保证金记录不一致的中间状态。
type Store struct {
	mu   sync.Mutex
	path string

	Seq           int64
	Auctions      map[string]*Auction
	Trades        map[string]*Trade
	Batches       map[string]*DeliveryBatch
	Confirmations map[string]*DeliveryConfirmation // 按外部操作号索引
	Cancellations map[string]*DeliveryCancellation // 按外部操作号索引
	Corrections   map[string][]*Correction         // 按成交号索引
	MarginRecords []*MarginRecord
}

// NewStore 创建空存储；path 为空表示纯内存模式。
func NewStore(path string) *Store {
	return &Store{
		path:          path,
		Auctions:      map[string]*Auction{},
		Trades:        map[string]*Trade{},
		Batches:       map[string]*DeliveryBatch{},
		Confirmations: map[string]*DeliveryConfirmation{},
		Cancellations: map[string]*DeliveryCancellation{},
		Corrections:   map[string][]*Correction{},
	}
}

// LoadStore 从快照恢复存储；快照不存在时返回空存储。
func LoadStore(path string) (*Store, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return NewStore(path), nil
	}
	if err != nil {
		return nil, err
	}
	s := NewStore(path)
	if err := json.Unmarshal(data, s); err != nil {
		return nil, fmt.Errorf("auctionclearing: load snapshot: %w", err)
	}
	return s, nil
}

func (s *Store) nextID(prefix string) string {
	s.Seq++
	return fmt.Sprintf("%s-%06d", prefix, s.Seq)
}

// saveLocked 必须在持有锁的情况下调用；先写临时文件再 rename，保证原子性。
func (s *Store) saveLocked() error {
	if s.path == "" {
		return nil
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return err
	}
	return nil
}

// Path 返回快照路径，供测试与运维使用。
func (s *Store) Path() string { return s.path }

// openBatchOfLocked 返回包含指定成交的未完成批次，不存在时返回 nil。
func (s *Store) openBatchOfLocked(tradeID string) *DeliveryBatch {
	for _, b := range s.Batches {
		if b.Status != BatchOpen {
			continue
		}
		for _, item := range b.Items {
			if item.TradeID == tradeID {
				return b
			}
		}
	}
	return nil
}

// marginRecordOfLocked 按外部操作号查找保证金台账记录。
func (s *Store) marginRecordOfLocked(opID string) *MarginRecord {
	for _, rec := range s.MarginRecords {
		if rec.OpID == opID {
			return rec
		}
	}
	return nil
}

// confirmTargetLocked 解析交割确认/取消的目标：批次、批次内成交项与拍卖。
// 批次已关闭时优先返回 ErrBatchClosed，保证迟到操作得到明确冲突。
func (s *Store) confirmTargetLocked(batchID, tradeID string) (*DeliveryBatch, *BatchItem, *Auction, error) {
	b, ok := s.Batches[batchID]
	if !ok {
		return nil, nil, nil, fmt.Errorf("%w: batch %s", ErrNotFound, batchID)
	}
	if b.Status != BatchOpen {
		return nil, nil, nil, fmt.Errorf("%w: batch %s", ErrBatchClosed, batchID)
	}
	var item *BatchItem
	for _, it := range b.Items {
		if it.TradeID == tradeID {
			item = it
			break
		}
	}
	if item == nil {
		return nil, nil, nil, fmt.Errorf("%w: trade %s in batch %s", ErrNotFound, tradeID, batchID)
	}
	if item.Status == ItemCompleted || item.Status == ItemCancelled {
		return nil, nil, nil, fmt.Errorf("%w: trade %s already %s", ErrInvalidState, tradeID, item.Status)
	}
	return b, item, s.Auctions[b.AuctionID], nil
}
