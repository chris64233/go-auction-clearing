package auctionclearing

import (
	"fmt"
	"time"
)

// Service 竞价清算与交割结算服务。所有状态变更在同一把锁内完成,
// 交割确认、成交更正、批次关闭并发时按锁内顺序裁决。
type Service struct {
	store *Store
}

func NewService(store *Store) *Service {
	return &Service{store: store}
}

func (s *Service) nextIDLocked(prefix string) string {
	s.store.data.Seq++
	return fmt.Sprintf("%s-%d", prefix, s.store.data.Seq)
}

// RegisterTrade 登记一笔撮合成交(待清算)。
func (s *Service) RegisterTrade(id, auctionID, buyerID, sellerID string, qty, price int64) (*Trade, error) {
	if qty <= 0 || price < 0 {
		return nil, ErrInvalidQuantity
	}
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	now := time.Now()
	t := &Trade{
		ID: id, AuctionID: auctionID, BuyerID: buyerID, SellerID: sellerID,
		Quantity: qty, Price: price, Status: TradeStatusPending,
		SettlementVersion: 1, CreatedAt: now, UpdatedAt: now,
	}
	s.store.data.Trades[id] = t
	return t, s.store.saveLocked()
}

// ClearTrade 清算完成,成交可加入交割批次。
func (s *Service) ClearTrade(tradeID string) error {
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	t, ok := s.store.data.Trades[tradeID]
	if !ok {
		return ErrTradeNotFound
	}
	if t.Status == TradeStatusPending {
		t.Status = TradeStatusCleared
		t.UpdatedAt = time.Now()
	}
	return s.store.saveLocked()
}

// CorrectTrade 更正成交数量/价格,结算版本递增。已冻结该成交的旧批次
// 因版本不一致不得继续确认(ConfirmDelivery 返回 ErrVersionConflict)。
func (s *Service) CorrectTrade(tradeID string, qty, price int64) (*Trade, error) {
	if qty <= 0 || price < 0 {
		return nil, ErrInvalidQuantity
	}
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	t, ok := s.store.data.Trades[tradeID]
	if !ok {
		return nil, ErrTradeNotFound
	}
	t.Quantity = qty
	t.Price = price
	t.SettlementVersion++
	t.UpdatedAt = time.Now()
	return t, s.store.saveLocked()
}

// tradeInOpenBatchLocked 返回成交当前所属的未完成批次 ID。
func (s *Service) tradeInOpenBatchLocked(tradeID string) string {
	for _, b := range s.store.data.Batches {
		if b.Status != BatchStatusOpen {
			continue
		}
		for _, it := range b.Items {
			if it.TradeID == tradeID && it.Status == ItemStatusOpen {
				return b.ID
			}
		}
	}
	return ""
}

// CreateBatch 清算完成后创建交割批次,冻结成交清单、数量、价格版本与截止时间。
// 一笔成交只能属于一个未完成批次;已交割的成交不能再次加入。
func (s *Service) CreateBatch(auctionID string, tradeIDs []string, deadline time.Time) (*DeliveryBatch, error) {
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	items := make([]*BatchItem, 0, len(tradeIDs))
	for _, id := range tradeIDs {
		t, ok := s.store.data.Trades[id]
		if !ok {
			return nil, fmt.Errorf("%w: %s", ErrTradeNotFound, id)
		}
		if open := s.tradeInOpenBatchLocked(id); open != "" {
			return nil, fmt.Errorf("%w: %s in %s", ErrTradeAlreadyInBatch, id, open)
		}
		if t.Status == TradeStatusDelivered {
			return nil, fmt.Errorf("%w: %s", ErrTradeAlreadyDelivered, id)
		}
		if t.Status != TradeStatusCleared {
			return nil, fmt.Errorf("%w: %s", ErrTradeNotCleared, id)
		}
		items = append(items, &BatchItem{
			TradeID: id, PlannedQuantity: t.Quantity, Price: t.Price,
			SettlementVersion: t.SettlementVersion, Status: ItemStatusOpen,
		})
	}
	now := time.Now()
	b := &DeliveryBatch{
		ID: s.nextIDLocked("batch"), AuctionID: auctionID,
		Status: BatchStatusOpen, Items: items, Deadline: deadline, CreatedAt: now,
	}
	s.store.data.Batches[b.ID] = b
	for _, id := range tradeIDs {
		t := s.store.data.Trades[id]
		t.Status = TradeStatusDelivering
		t.UpdatedAt = now
	}
	if err := s.store.saveLocked(); err != nil {
		return nil, err
	}
	return b, nil
}

func confirmFingerprint(batchID, tradeID string, qty int64, action MarginAction, amount int64) string {
	return fmt.Sprintf("%s|%s|%d|%s|%d", batchID, tradeID, qty, action, amount)
}

// ConfirmDelivery 按成交逐笔登记实际交割数量与保证金结果。
// opID 为外部操作号:同号同内容重放返回原结果;内容变化返回冲突。
func (s *Service) ConfirmDelivery(opID, batchID, tradeID string, qty int64, action MarginAction, amount int64) (*DeliveryConfirmation, error) {
	if qty <= 0 {
		return nil, ErrInvalidQuantity
	}
	s.store.mu.Lock()
	defer s.store.mu.Unlock()

	fp := confirmFingerprint(batchID, tradeID, qty, action, amount)
	if ent, ok := s.store.data.Idempotency[opID]; ok {
		if ent.Fingerprint != fp {
			return nil, ErrIdempotencyConflict
		}
		return s.store.data.Confirmations[ent.ConfirmID], nil
	}

	b, ok := s.store.data.Batches[batchID]
	if !ok {
		return nil, ErrBatchNotFound
	}
	if b.Status == BatchStatusClosed {
		return nil, ErrBatchClosed
	}
	var item *BatchItem
	for _, it := range b.Items {
		if it.TradeID == tradeID {
			item = it
			break
		}
	}
	if item == nil {
		return nil, ErrItemNotFound
	}
	if item.Status != ItemStatusOpen {
		return nil, ErrItemFinished
	}
	t := s.store.data.Trades[tradeID]
	if t == nil {
		return nil, ErrTradeNotFound
	}
	// 更正先生效:旧批次冻结的版本已失效,不得继续确认。
	if t.SettlementVersion != item.SettlementVersion {
		return nil, ErrVersionConflict
	}
	// 部分交割只能减少剩余数量,不允许超量。
	if qty > item.RemainingQuantity() {
		return nil, ErrOverDelivery
	}

	now := time.Now()
	c := &DeliveryConfirmation{
		ID: s.nextIDLocked("confirm"), OpID: opID, BatchID: batchID, TradeID: tradeID,
		Quantity: qty, Version: item.SettlementVersion, CreatedAt: now,
		Margin: MarginRecord{
			TradeID: tradeID, BatchID: batchID,
			Action: action, Amount: amount, OpID: opID,
		},
	}
	item.DeliveredQuantity += qty
	if item.RemainingQuantity() == 0 {
		item.Status = ItemStatusCompleted
		t.Status = TradeStatusDelivered
	}
	t.UpdatedAt = now
	s.store.data.Confirmations[c.ID] = c
	s.store.data.Idempotency[opID] = &idempotencyEntry{Fingerprint: fp, ConfirmID: c.ID}
	if err := s.store.saveLocked(); err != nil {
		return nil, err
	}
	return c, nil
}

// CancelBatchItem 明确取消批次内的一笔成交,释放该成交可重新入批。
func (s *Service) CancelBatchItem(batchID, tradeID string) error {
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	b, ok := s.store.data.Batches[batchID]
	if !ok {
		return ErrBatchNotFound
	}
	if b.Status == BatchStatusClosed {
		return ErrBatchClosed
	}
	for _, it := range b.Items {
		if it.TradeID != tradeID {
			continue
		}
		if it.Status != ItemStatusOpen {
			return ErrItemFinished
		}
		it.Status = ItemStatusCancelled
		if t := s.store.data.Trades[tradeID]; t != nil && t.Status == TradeStatusDelivering {
			t.Status = TradeStatusCleared
			t.UpdatedAt = time.Now()
		}
		return s.store.saveLocked()
	}
	return ErrItemNotFound
}

// CloseBatch 所有成交完成或被明确取消后才能关闭;关闭后迟到确认返回冲突。
func (s *Service) CloseBatch(batchID string) error {
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	b, ok := s.store.data.Batches[batchID]
	if !ok {
		return ErrBatchNotFound
	}
	if b.Status == BatchStatusClosed {
		return nil
	}
	for _, it := range b.Items {
		if it.Status == ItemStatusOpen {
			return ErrBatchNotClosable
		}
	}
	b.Status = BatchStatusClosed
	b.ClosedAt = time.Now()
	return s.store.saveLocked()
}

// GetBatchView 查询批次状态、每笔成交计划/已交割/剩余数量、保证金与更正关联。
func (s *Service) GetBatchView(batchID string) (*BatchView, error) {
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	b, ok := s.store.data.Batches[batchID]
	if !ok {
		return nil, ErrBatchNotFound
	}
	view := &BatchView{BatchID: b.ID, AuctionID: b.AuctionID, Status: b.Status, Deadline: b.Deadline}
	for _, it := range b.Items {
		iv := BatchItemView{
			TradeID: it.TradeID, Status: it.Status,
			PlannedQuantity: it.PlannedQuantity, DeliveredQuantity: it.DeliveredQuantity,
			RemainingQuantity: it.RemainingQuantity(), Price: it.Price,
			SettlementVersion: it.SettlementVersion,
		}
		if t := s.store.data.Trades[it.TradeID]; t != nil {
			iv.CurrentVersion = t.SettlementVersion
			iv.CorrectionOf = t.CorrectionOf
		}
		for _, c := range s.store.data.Confirmations {
			if c.BatchID == b.ID && c.TradeID == it.TradeID {
				iv.Margins = append(iv.Margins, c.Margin)
			}
		}
		view.Items = append(view.Items, iv)
	}
	return view, nil
}
