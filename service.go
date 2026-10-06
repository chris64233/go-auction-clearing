package auctionclearing

import (
	"fmt"
	"time"
)

// Service 在 Store 之上提供清算、更正与交割批次操作。
// 所有变更操作都在 Store 的同一把互斥锁内完成校验、状态迁移与落盘，
// 因此交割确认、成交更正与批次关闭并发时，按锁内观察到的结算版本裁决。
type Service struct {
	store *Store
}

func NewService(store *Store) *Service {
	return &Service{store: store}
}

// CreateAuction 登记一场新竞价，初始状态为 OPEN，结算版本为 0。
func (s *Service) CreateAuction(id string) (*Auction, error) {
	if id == "" {
		return nil, fmt.Errorf("%w: auction id required", ErrValidation)
	}
	st := s.store
	st.mu.Lock()
	defer st.mu.Unlock()
	if _, ok := st.Auctions[id]; ok {
		return nil, fmt.Errorf("%w: auction %s already exists", ErrInvalidState, id)
	}
	a := &Auction{ID: id, Status: AuctionOpen}
	st.Auctions[id] = a
	if err := st.saveLocked(); err != nil {
		return nil, err
	}
	return a, nil
}

// RecordTrade 在竞价未清算前登记一笔成交（清算撮合的产物）。
func (s *Service) RecordTrade(auctionID, buyerID, sellerID string, quantity, priceCents int64) (*Trade, error) {
	if quantity <= 0 || priceCents < 0 {
		return nil, fmt.Errorf("%w: quantity must be positive and price non-negative", ErrValidation)
	}
	st := s.store
	st.mu.Lock()
	defer st.mu.Unlock()
	a, ok := st.Auctions[auctionID]
	if !ok {
		return nil, fmt.Errorf("%w: auction %s", ErrNotFound, auctionID)
	}
	if a.Status != AuctionOpen {
		return nil, fmt.Errorf("%w: auction %s already cleared", ErrInvalidState, auctionID)
	}
	t := &Trade{
		ID:             st.nextID("trade"),
		AuctionID:      auctionID,
		BuyerID:        buyerID,
		SellerID:       sellerID,
		Quantity:       quantity,
		PriceCents:     priceCents,
		Status:         TradeActive,
		CreatedVersion: a.SettlementVersion,
	}
	st.Trades[t.ID] = t
	if err := st.saveLocked(); err != nil {
		return nil, err
	}
	return t, nil
}

// ClearAuction 标记清算完成，之后才能创建交割批次。
func (s *Service) ClearAuction(auctionID string) (*Auction, error) {
	st := s.store
	st.mu.Lock()
	defer st.mu.Unlock()
	a, ok := st.Auctions[auctionID]
	if !ok {
		return nil, fmt.Errorf("%w: auction %s", ErrNotFound, auctionID)
	}
	if a.Status != AuctionOpen {
		return nil, fmt.Errorf("%w: auction %s not open", ErrInvalidState, auctionID)
	}
	a.Status = AuctionCleared
	if err := st.saveLocked(); err != nil {
		return nil, err
	}
	return a, nil
}

// CorrectTrade 更正一笔成交的数量与价格，生效后拍卖的结算版本递增，
// 此前冻结的交割批次随即过期，不得继续确认。
func (s *Service) CorrectTrade(tradeID string, newQuantity, newPriceCents int64, reason string) (*Correction, error) {
	if newQuantity <= 0 || newPriceCents < 0 {
		return nil, fmt.Errorf("%w: quantity must be positive and price non-negative", ErrValidation)
	}
	st := s.store
	st.mu.Lock()
	defer st.mu.Unlock()
	t, ok := st.Trades[tradeID]
	if !ok {
		return nil, fmt.Errorf("%w: trade %s", ErrNotFound, tradeID)
	}
	if t.Status != TradeActive {
		return nil, fmt.Errorf("%w: trade %s not active", ErrInvalidState, tradeID)
	}
	if newQuantity < t.DeliveredQuantity {
		return nil, fmt.Errorf("%w: new quantity %d below already delivered %d", ErrValidation, newQuantity, t.DeliveredQuantity)
	}
	a := st.Auctions[t.AuctionID]
	a.SettlementVersion++
	c := &Correction{
		ID:            st.nextID("corr"),
		TradeID:       tradeID,
		Version:       a.SettlementVersion,
		OldQuantity:   t.Quantity,
		NewQuantity:   newQuantity,
		OldPriceCents: t.PriceCents,
		NewPriceCents: newPriceCents,
		Reason:        reason,
		CreatedAt:     time.Now(),
	}
	t.Quantity = newQuantity
	t.PriceCents = newPriceCents
	st.Corrections[tradeID] = append(st.Corrections[tradeID], c)
	if err := st.saveLocked(); err != nil {
		return nil, err
	}
	return c, nil
}

// CreateBatch 在清算完成后创建交割批次，冻结成交清单、剩余交割数量、
// 价格与当前结算版本。一笔成交只能属于一个未完成批次，
// 已全部交割的成交不能再次加入新批次。
func (s *Service) CreateBatch(auctionID string, tradeIDs []string, deadline time.Time) (*DeliveryBatch, error) {
	if len(tradeIDs) == 0 {
		return nil, fmt.Errorf("%w: batch requires at least one trade", ErrValidation)
	}
	if deadline.IsZero() {
		return nil, fmt.Errorf("%w: deadline required", ErrValidation)
	}
	st := s.store
	st.mu.Lock()
	defer st.mu.Unlock()
	a, ok := st.Auctions[auctionID]
	if !ok {
		return nil, fmt.Errorf("%w: auction %s", ErrNotFound, auctionID)
	}
	if a.Status != AuctionCleared {
		return nil, fmt.Errorf("%w: auction %s not cleared", ErrInvalidState, auctionID)
	}
	seen := map[string]bool{}
	items := make([]*BatchItem, 0, len(tradeIDs))
	for _, tid := range tradeIDs {
		if seen[tid] {
			return nil, fmt.Errorf("%w: duplicate trade %s", ErrValidation, tid)
		}
		seen[tid] = true
		t, ok := st.Trades[tid]
		if !ok || t.AuctionID != auctionID {
			return nil, fmt.Errorf("%w: trade %s in auction %s", ErrNotFound, tid, auctionID)
		}
		if t.Status != TradeActive {
			return nil, fmt.Errorf("%w: trade %s not active", ErrInvalidState, tid)
		}
		remaining := t.Quantity - t.DeliveredQuantity
		if remaining <= 0 {
			return nil, fmt.Errorf("%w: trade %s fully delivered", ErrInvalidState, tid)
		}
		if b := st.openBatchOfLocked(tid); b != nil {
			return nil, fmt.Errorf("%w: trade %s already in open batch %s", ErrInvalidState, tid, b.ID)
		}
		items = append(items, &BatchItem{
			TradeID:         tid,
			PlannedQuantity: remaining,
			PriceCents:      t.PriceCents,
			Status:          ItemPending,
		})
	}
	b := &DeliveryBatch{
		ID:        st.nextID("batch"),
		AuctionID: auctionID,
		Version:   a.SettlementVersion,
		Deadline:  deadline,
		Status:    BatchOpen,
		Items:     items,
		CreatedAt: time.Now(),
	}
	st.Batches[b.ID] = b
	if err := st.saveLocked(); err != nil {
		return nil, err
	}
	return b, nil
}

// ConfirmDelivery 按成交逐笔登记实际交割数量与保证金结果。
// OpID 是外部操作号：同号同内容重放返回原结果，内容变化返回冲突。
// 部分交割只减少剩余数量；批次被更正淘汰或已关闭时返回明确冲突。
func (s *Service) ConfirmDelivery(opID, batchID, tradeID string, quantity int64, margin MarginResult) (*DeliveryConfirmation, *MarginRecord, error) {
	if opID == "" {
		return nil, nil, fmt.Errorf("%w: op id required", ErrValidation)
	}
	if quantity <= 0 {
		return nil, nil, fmt.Errorf("%w: quantity must be positive", ErrValidation)
	}
	if err := validateMargin(margin); err != nil {
		return nil, nil, err
	}
	st := s.store
	st.mu.Lock()
	defer st.mu.Unlock()

	if prev, ok := st.Confirmations[opID]; ok {
		if prev.BatchID == batchID && prev.TradeID == tradeID && prev.Quantity == quantity && prev.Margin == margin {
			return prev, st.marginRecordOfLocked(opID), nil
		}
		return nil, nil, fmt.Errorf("%w: op %s replayed with different content", ErrIdempotencyConflict, opID)
	}
	if _, ok := st.Cancellations[opID]; ok {
		return nil, nil, fmt.Errorf("%w: op %s already used for a cancellation", ErrIdempotencyConflict, opID)
	}

	b, item, a, err := st.confirmTargetLocked(batchID, tradeID)
	if err != nil {
		return nil, nil, err
	}
	if b.Version != a.SettlementVersion {
		return nil, nil, fmt.Errorf("%w: batch %s frozen at version %d, current %d", ErrVersionConflict, batchID, b.Version, a.SettlementVersion)
	}
	remaining := item.PlannedQuantity - item.DeliveredQuantity
	if quantity > remaining {
		return nil, nil, fmt.Errorf("%w: quantity %d exceeds remaining %d", ErrValidation, quantity, remaining)
	}

	item.DeliveredQuantity += quantity
	if item.DeliveredQuantity == item.PlannedQuantity {
		item.Status = ItemCompleted
	} else {
		item.Status = ItemPartial
	}
	t := st.Trades[tradeID]
	t.DeliveredQuantity += quantity

	conf := &DeliveryConfirmation{
		OpID:      opID,
		BatchID:   batchID,
		TradeID:   tradeID,
		Quantity:  quantity,
		Margin:    margin,
		Version:   b.Version,
		CreatedAt: time.Now(),
	}
	rec := &MarginRecord{
		ID:        st.nextID("margin"),
		OpID:      opID,
		BatchID:   batchID,
		TradeID:   tradeID,
		Version:   b.Version,
		Result:    margin,
		CreatedAt: conf.CreatedAt,
	}
	st.Confirmations[opID] = conf
	st.MarginRecords = append(st.MarginRecords, rec)
	if err := st.saveLocked(); err != nil {
		return nil, nil, err
	}
	return conf, rec, nil
}

// CancelDelivery 明确取消批次内一笔成交的剩余未交割数量，并登记保证金结果。
// 取消是更正淘汰批次后的清理路径，因此不要求批次版本与当前结算版本一致。
func (s *Service) CancelDelivery(opID, batchID, tradeID, reason string, margin MarginResult) (*DeliveryCancellation, *MarginRecord, error) {
	if opID == "" {
		return nil, nil, fmt.Errorf("%w: op id required", ErrValidation)
	}
	if err := validateMargin(margin); err != nil {
		return nil, nil, err
	}
	st := s.store
	st.mu.Lock()
	defer st.mu.Unlock()

	if prev, ok := st.Cancellations[opID]; ok {
		if prev.BatchID == batchID && prev.TradeID == tradeID && prev.Margin == margin && prev.Reason == reason {
			return prev, st.marginRecordOfLocked(opID), nil
		}
		return nil, nil, fmt.Errorf("%w: op %s replayed with different content", ErrIdempotencyConflict, opID)
	}
	if _, ok := st.Confirmations[opID]; ok {
		return nil, nil, fmt.Errorf("%w: op %s already used for a confirmation", ErrIdempotencyConflict, opID)
	}

	b, item, _, err := st.confirmTargetLocked(batchID, tradeID)
	if err != nil {
		return nil, nil, err
	}
	remaining := item.PlannedQuantity - item.DeliveredQuantity
	if remaining <= 0 {
		return nil, nil, fmt.Errorf("%w: trade %s has nothing to cancel", ErrInvalidState, tradeID)
	}
	item.Status = ItemCancelled

	canc := &DeliveryCancellation{
		OpID:              opID,
		BatchID:           batchID,
		TradeID:           tradeID,
		CancelledQuantity: remaining,
		Margin:            margin,
		Version:           b.Version,
		Reason:            reason,
		CreatedAt:         time.Now(),
	}
	rec := &MarginRecord{
		ID:        st.nextID("margin"),
		OpID:      opID,
		BatchID:   batchID,
		TradeID:   tradeID,
		Version:   b.Version,
		Result:    margin,
		CreatedAt: canc.CreatedAt,
	}
	st.Cancellations[opID] = canc
	st.MarginRecords = append(st.MarginRecords, rec)
	if err := st.saveLocked(); err != nil {
		return nil, nil, err
	}
	return canc, rec, nil
}

// CloseBatch 关闭批次：要求批次内所有成交都已交割完成或被明确取消。
// 关闭后迟到的交割确认返回 ErrBatchClosed。
func (s *Service) CloseBatch(batchID string) (*DeliveryBatch, error) {
	st := s.store
	st.mu.Lock()
	defer st.mu.Unlock()
	b, ok := st.Batches[batchID]
	if !ok {
		return nil, fmt.Errorf("%w: batch %s", ErrNotFound, batchID)
	}
	if b.Status != BatchOpen {
		return nil, fmt.Errorf("%w: batch %s", ErrBatchClosed, batchID)
	}
	for _, item := range b.Items {
		if item.Status != ItemCompleted && item.Status != ItemCancelled {
			return nil, fmt.Errorf("%w: trade %s still %s", ErrBatchNotClosable, item.TradeID, item.Status)
		}
	}
	now := time.Now()
	b.Status = BatchClosed
	b.ClosedAt = &now
	if err := st.saveLocked(); err != nil {
		return nil, err
	}
	return b, nil
}

// BatchItemView 是批次内一笔成交的查询视图。
type BatchItemView struct {
	TradeID           string
	PlannedQuantity   int64
	DeliveredQuantity int64
	RemainingQuantity int64
	PriceCents        int64
	Status            ItemStatus
	MarginRecords     []*MarginRecord
}

// BatchView 是批次的查询视图：状态、每笔成交的计划/已交割/剩余数量、
// 保证金处理结果以及关联的成交更正。
type BatchView struct {
	Batch          *DeliveryBatch
	CurrentVersion int64
	Stale          bool
	Items          []*BatchItemView
	Corrections    []*Correction
}

// GetBatchView 组装批次查询视图，返回的是拷贝，可安全在锁外读取。
func (s *Service) GetBatchView(batchID string) (*BatchView, error) {
	st := s.store
	st.mu.Lock()
	defer st.mu.Unlock()
	b, ok := st.Batches[batchID]
	if !ok {
		return nil, fmt.Errorf("%w: batch %s", ErrNotFound, batchID)
	}
	a := st.Auctions[b.AuctionID]
	batchCopy := *b
	view := &BatchView{
		Batch:          &batchCopy,
		CurrentVersion: a.SettlementVersion,
		Stale:          a.SettlementVersion != b.Version,
	}
	for _, item := range b.Items {
		iv := &BatchItemView{
			TradeID:           item.TradeID,
			PlannedQuantity:   item.PlannedQuantity,
			DeliveredQuantity: item.DeliveredQuantity,
			RemainingQuantity: item.PlannedQuantity - item.DeliveredQuantity,
			PriceCents:        item.PriceCents,
			Status:            item.Status,
		}
		for _, rec := range st.MarginRecords {
			if rec.BatchID == batchID && rec.TradeID == item.TradeID {
				recCopy := *rec
				iv.MarginRecords = append(iv.MarginRecords, &recCopy)
			}
		}
		view.Items = append(view.Items, iv)
		view.Corrections = append(view.Corrections, st.Corrections[item.TradeID]...)
	}
	return view, nil
}

// ListBatches 返回一场拍卖的全部批次。
func (s *Service) ListBatches(auctionID string) []*DeliveryBatch {
	st := s.store
	st.mu.Lock()
	defer st.mu.Unlock()
	var out []*DeliveryBatch
	for _, b := range st.Batches {
		if b.AuctionID == auctionID {
			batchCopy := *b
			out = append(out, &batchCopy)
		}
	}
	return out
}

func validateMargin(m MarginResult) error {
	switch m.Action {
	case MarginReleased, MarginForfeited, MarginPartiallyForfeited:
	default:
		return fmt.Errorf("%w: unknown margin action %q", ErrValidation, m.Action)
	}
	if m.AmountCents < 0 {
		return fmt.Errorf("%w: margin amount must be non-negative", ErrValidation)
	}
	return nil
}
