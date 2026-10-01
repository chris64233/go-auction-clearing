package auctionclearing

import "time"

// 结算流水与成交更常用例。所有写操作都在 Engine.mu 上串行化：
// 成交确认、资金冻结、交割与更正批准共用同一把批次锁，
// 同时到达时严格串行，一个订单任一时刻只保留一套有效成交结果。

// GenerateSettlementOutcome 表示生成首版结算请求的结果。
type GenerateSettlementOutcome string

const (
	// SettlementGenerated 表示本次请求实际生成了首版结算。
	SettlementGenerated GenerateSettlementOutcome = "generated"
	// SettlementReplayed 表示首版结算已存在，返回原结果。
	SettlementReplayed GenerateSettlementOutcome = "replayed"
)

// GenerateSettlementResult 是生成首版结算的返回值。
type GenerateSettlementResult struct {
	Outcome GenerateSettlementOutcome
	Data    SettlementRecord
}

// GenerateSettlement 在拍卖已清算后生成首版结算（成交确认/冻结/交割/更正的前提）。
// 清算与结算是两件事：清算给出不可变的撮合结果，结算承载成交后的处理流水。
// 重复调用幂等，返回同一份首版版本。
func (e *Engine) GenerateSettlement(auctionID int64) (GenerateSettlementResult, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	if chain := e.settlementChains[auctionID]; len(chain) > 0 {
		return GenerateSettlementResult{Outcome: SettlementReplayed, Data: *chain[0]}, nil
	}
	cl, ok := e.clearings[auctionID]
	if !ok {
		if _, exists := e.auctions[auctionID]; !exists {
			return GenerateSettlementResult{}, ErrAuctionNotFound
		}
		return GenerateSettlementResult{}, ErrSettlementNotFound
	}
	now := e.clock.Now()
	rec := newSettlementRecordFromClearing(*cl, now)
	ev := Event{
		Type:             EvSettlementGenerated,
		Timestamp:        now,
		AuctionID:        auctionID,
		SettlementRecord: rec,
	}
	if _, err := e.store.Append([]Event{ev}); err != nil {
		return GenerateSettlementResult{}, err
	}
	e.settlementChains[auctionID] = append(e.settlementChains[auctionID], rec)
	return GenerateSettlementResult{Outcome: SettlementGenerated, Data: *rec}, nil
}

// currentSettlementLocked 返回当前有效结算版本（调用方须持锁）。
func (e *Engine) currentSettlementLocked(auctionID int64) *SettlementRecord {
	chain := e.settlementChains[auctionID]
	for i := len(chain) - 1; i >= 0; i-- {
		if !chain[i].Superseded {
			return chain[i]
		}
	}
	return nil
}

// settlementVersionLocked 按版本号取结算版本（调用方须持锁）；
// 版本号为零的旧事件回退到当前有效版本。
func (e *Engine) settlementVersionLocked(auctionID int64, version SettlementVersion) *SettlementRecord {
	if version == 0 {
		return e.currentSettlementLocked(auctionID)
	}
	for _, rec := range e.settlementChains[auctionID] {
		if rec.Version == version {
			return rec
		}
	}
	return nil
}

// waitWhileCorrectingLocked 等待更正批准的两阶段窗口结束（调用方须持锁）。
func (e *Engine) waitWhileCorrectingLocked(auctionID int64) {
	for {
		if _, busy := e.correcting[auctionID]; !busy {
			return
		}
		e.cond.Wait()
	}
}

func entryByExternalID(rec *SettlementRecord, externalID string) *SettlementEntry {
	if rec == nil {
		return nil
	}
	for i := range rec.Entries {
		if rec.Entries[i].ExternalID == externalID {
			return &rec.Entries[i]
		}
	}
	return nil
}

// mutateCurrentEntry 在当前有效结算版本上对单笔成交推进一个处理步骤。
// 与更正批准共用同一把批次锁：撞上批准两阶段窗口时等待其落定，
// 随后在唯一有效的新版本上执行，绝不作用在一个即将被取代的版本上。
func (e *Engine) mutateCurrentEntry(auctionID int64, externalID string, apply func(en *SettlementEntry) error, buildEvent func(now time.Time, en *SettlementEntry) Event) (SettlementEntry, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	e.waitWhileCorrectingLocked(auctionID)

	rec := e.currentSettlementLocked(auctionID)
	if rec == nil {
		if _, ok := e.auctions[auctionID]; !ok {
			return SettlementEntry{}, ErrAuctionNotFound
		}
		return SettlementEntry{}, ErrNoSettlement
	}
	idx := -1
	for i := range rec.Entries {
		if rec.Entries[i].ExternalID == externalID {
			idx = i
			break
		}
	}
	if idx < 0 {
		return SettlementEntry{}, ErrFillNotFound
	}
	// 在副本上校验并构造事件；落盘成功后才写回内存，
	// 保证存储失败时内存状态（及重放结果）不留半步推进。
	work := rec.Entries[idx]
	if err := apply(&work); err != nil {
		return SettlementEntry{}, err
	}
	now := e.clock.Now()
	ev := buildEvent(now, &work)
	ev.SettlementVersion = rec.Version
	if _, err := e.store.Append([]Event{ev}); err != nil {
		return SettlementEntry{}, err
	}
	rec.Entries[idx] = work
	return work, nil
}

// ConfirmFill 完成一笔成交的成交确认。幂等：已确认返回现状。
func (e *Engine) ConfirmFill(auctionID int64, externalID string) (SettlementEntry, error) {
	return e.mutateCurrentEntry(auctionID, externalID,
		func(en *SettlementEntry) error {
			en.Confirmed = true
			return nil
		},
		func(now time.Time, _ *SettlementEntry) Event {
			return Event{Type: EvFillConfirmed, Timestamp: now, AuctionID: auctionID, ExternalID: externalID}
		},
	)
}

// FreezeFunds 对一笔已确认成交冻结资金。未确认先冻结属于状态冲突。
// 幂等：已冻结返回现状。
func (e *Engine) FreezeFunds(auctionID int64, externalID string) (SettlementEntry, error) {
	return e.mutateCurrentEntry(auctionID, externalID,
		func(en *SettlementEntry) error {
			if !en.Confirmed {
				return ErrNotConfirmed
			}
			en.FundsFrozen = true
			return nil
		},
		func(now time.Time, _ *SettlementEntry) Event {
			return Event{Type: EvFundsFrozen, Timestamp: now, AuctionID: auctionID, ExternalID: externalID}
		},
	)
}

// MarkDelivered 记录一笔成交的增量交割数量（不得为负、不得超过未交割余量）。
// 交割金额按交割时刻有效结算版本的单价锁定；已交割事实随后续更正原样继承。
func (e *Engine) MarkDelivered(auctionID int64, externalID string, qty int64) (SettlementEntry, error) {
	if qty < 0 {
		return SettlementEntry{}, ErrNegativeDeliveryQty
	}
	var lockedAmount Money
	return e.mutateCurrentEntry(auctionID, externalID,
		func(en *SettlementEntry) error {
			if qty > en.UndeliveredQty() {
				return ErrDeliveryExceedsFill
			}
			amount, err := en.EffectiveUnitPrice.Mul(qty)
			if err != nil {
				return err
			}
			lockedAmount = amount
			en.DeliveredQty += qty
			total, err := en.DeliveredAmount.Add(amount)
			if err != nil {
				return err
			}
			en.DeliveredAmount = total
			if en.DeliveredQty >= en.MatchedQty {
				now := e.clock.Now()
				en.DeliveredAt = &now
			}
			return nil
		},
		func(now time.Time, _ *SettlementEntry) Event {
			return Event{
				Type:           EvFillDelivered,
				Timestamp:      now,
				AuctionID:      auctionID,
				ExternalID:     externalID,
				DeliveryQty:    qty,
				DeliveryAmount: lockedAmount,
			}
		},
	)
}

// GetSettlementRecord 查询当前有效结算版本（含每笔成交的确认/冻结/交割状态）。
func (e *Engine) GetSettlementRecord(auctionID int64) (SettlementRecord, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if _, ok := e.auctions[auctionID]; !ok {
		return SettlementRecord{}, ErrAuctionNotFound
	}
	rec := e.currentSettlementLocked(auctionID)
	if rec == nil {
		return SettlementRecord{}, ErrNoSettlement
	}
	return *rec, nil
}

// GetSettlementVersion 查询指定结算版本（旧版本同样可查，供更正追溯）。
func (e *Engine) GetSettlementVersion(auctionID int64, version SettlementVersion) (SettlementRecord, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if _, ok := e.auctions[auctionID]; !ok {
		return SettlementRecord{}, ErrAuctionNotFound
	}
	for _, rec := range e.settlementChains[auctionID] {
		if rec.Version == version {
			return *rec, nil
		}
	}
	return SettlementRecord{}, ErrNoSettlement
}

// GetFillEntry 查询一笔成交在当前有效结算版本上的条目。
func (e *Engine) GetFillEntry(auctionID int64, externalID string) (SettlementEntry, error) {
	rec, err := e.GetSettlementRecord(auctionID)
	if err != nil {
		return SettlementEntry{}, err
	}
	if en := entryByExternalID(&rec, externalID); en != nil {
		return *en, nil
	}
	return SettlementEntry{}, ErrFillNotFound
}

// buildNextSettlement 构造新结算版本：原样继承全部原始撮合信息与交割状态，
// 仅覆盖允许更正的结算字段（结算单价、结算金额）。
// 只有未交割数量允许直接改单价；已全额交割的成交单价保持原价，差额走补偿单。
func buildNextSettlement(cur *SettlementRecord, targets map[string]Money, now time.Time) (*SettlementRecord, error) {
	next := &SettlementRecord{
		AuctionID:    cur.AuctionID,
		PriceVersion: cur.PriceVersion, // 更正不重新撮合定价，价格版本不变
		Version:      cur.Version + 1,
		CreatedAt:    now,
		Entries:      make([]SettlementEntry, len(cur.Entries)),
	}
	copy(next.Entries, cur.Entries)
	for i := range next.Entries {
		en := &next.Entries[i]
		if target, hit := targets[en.ExternalID]; hit && en.UndeliveredQty() > 0 {
			en.EffectiveUnitPrice = target
		}
		undelivered, err := en.EffectiveUnitPrice.Mul(en.UndeliveredQty())
		if err != nil {
			return nil, err
		}
		amount, err := en.DeliveredAmount.Add(undelivered)
		if err != nil {
			return nil, err
		}
		en.SettledAmount = amount
		total, err := next.TotalSettled.Add(amount)
		if err != nil {
			return nil, err
		}
		next.TotalSettled = total
	}
	return next, nil
}

// buildCompensations 为每笔涉及更正且已有交割的成交生成补偿单：
// 差额 = 交割锁定金额 - 目标单价*已交割数量（有符号，精确计算）。
// 完全未交割的成交直接在新版本改价，不产生补偿；差额为 0 同样不产生。
func buildCompensations(items []CorrectionItem, next *SettlementRecord, now time.Time) ([]Compensation, error) {
	comps := make([]Compensation, 0)
	for _, it := range items {
		en := entryByExternalID(next, it.ExternalID)
		if en == nil || en.DeliveredQty <= 0 {
			continue
		}
		desired, err := it.TargetUnitPrice.Mul(en.DeliveredQty)
		if err != nil {
			return nil, err
		}
		delta, err := en.DeliveredAmount.Sub(desired)
		if err != nil {
			return nil, err
		}
		if delta == 0 {
			continue
		}
		comps = append(comps, Compensation{
			ExternalID:   en.ExternalID,
			Rank:         en.Rank,
			Quantity:     en.DeliveredQty,
			LockedAmount: en.DeliveredAmount,
			TargetPrice:  it.TargetUnitPrice,
			Amount:       delta,
			Status:       CompensationPending,
			CreatedAt:    now,
		})
	}
	return comps, nil
}

func compensationNote(comps []Compensation, allSettled bool) string {
	switch {
	case len(comps) == 0:
		return "no delivered quantity; price corrected directly on the new settlement version"
	case allSettled:
		return "all compensations settled"
	default:
		pending := 0
		for _, c := range comps {
			if c.Status == CompensationPending {
				pending++
			}
		}
		if pending == len(comps) {
			return "compensation settlement failed; correction held pending"
		}
		return "some compensations remain pending; correction held pending"
	}
}
