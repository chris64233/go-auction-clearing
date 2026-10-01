package auctionclearing

import "time"

// ApproveCorrection 批准并执行一张更正申请。
//
// 批准与成交确认、资金冻结、交割共用同一把批次锁，保证一个订单只有一套
// 有效成交结果。批准采用与清算相同的两阶段：锁内认领批次并复制基准结算，
// 锁外执行可能失败的补偿结算，最后回锁原子提交一个携带新结算版本的事件：
//   - 未交割部分：只改结算字段（结算单价/金额），生成新结算版本，
//     原始撮合信息与交割状态按原版本继承；
//   - 已交割部分：原成交不动，生成补偿单，差额走补偿通道；
//   - 价格未变：不改结算，但仍记录更正依据与处理人（审计事件），
//     更正置为 completed，绝不只返回“无变化”；
//   - 补偿结算失败：不切换结算版本（原结算继续可用，未交割部分也不提前
//     生效），补偿单与更正停在 pending 并记录失败原因，等待 RetryCompensation；
//   - 申请挂接的基准版本已被取代：ErrStaleSettlement，申请保持 submitted；
//   - 已落定（completed/pending）的申请重复批准：幂等返回原结果。
func (e *Engine) ApproveCorrection(auctionID int64, correctionID string, in ApproveCorrectionInput) (ApproveCorrectionResult, error) {
	if correctionID == "" {
		return ApproveCorrectionResult{}, ErrEmptyCorrectionID
	}

	// —— 阶段一：锁内认领、校验、固定基准快照与更正草案 ——
	e.mu.Lock()
	for {
		if _, ok := e.auctions[auctionID]; !ok {
			e.mu.Unlock()
			return ApproveCorrectionResult{}, ErrAuctionNotFound
		}
		owner, exists := e.correctionOwner[correctionID]
		if !exists {
			e.mu.Unlock()
			return ApproveCorrectionResult{}, ErrCorrectionNotFound
		}
		if owner != auctionID {
			e.mu.Unlock()
			return ApproveCorrectionResult{}, ErrCorrectionConflict
		}
		app := e.corrections[auctionID][correctionID]
		if app.Status != CorrectionSubmitted {
			// 已落定（completed/pending）的重复批准：原样返回，不重复执行。
			e.mu.Unlock()
			return ApproveCorrectionResult{Outcome: CorrectionReplayed, Data: *app}, nil
		}
		if _, busy := e.correcting[auctionID]; busy {
			// 同一批次另一张更正正在两阶段窗口内：等待后重新判定。
			e.cond.Wait()
			continue
		}
		cur := e.currentSettlementLocked(auctionID)
		if cur == nil {
			e.mu.Unlock()
			return ApproveCorrectionResult{}, ErrNoSettlement
		}
		if app.BaseSettleVersion != cur.Version || app.PriceVersion != cur.PriceVersion {
			e.mu.Unlock()
			return ApproveCorrectionResult{}, ErrStaleSettlement
		}

		handler := in.Handler
		if handler == "" {
			handler = app.Handler
		}
		basis := in.Basis
		if basis == "" {
			basis = app.Basis
		}
		now := e.clock.Now()

		// 仅当某笔目标单价不同于当前有效结算单价时才算改变；
		// 已全额交割的成交，其“改变”体现为补偿单而非新版本单价。
		changed := false
		targets := make(map[string]Money, len(app.Items))
		for _, it := range app.Items {
			en := entryByExternalID(cur, it.ExternalID)
			if en == nil {
				e.mu.Unlock()
				return ApproveCorrectionResult{}, ErrFillNotFound
			}
			targets[it.ExternalID] = it.TargetUnitPrice
			if it.TargetUnitPrice != en.EffectiveUnitPrice {
				changed = true
			}
		}

		approvedAt := now
		app.ApprovedAt = &approvedAt

		if !changed {
			// 无价格变化：不产生新版本，原结算继续可用；
			// 但依据与处理人必须作为审计事件留痕，不能只回“无变化”。
			app.Status = CorrectionCompleted
			app.EffectiveVersion = cur.Version
			app.Events = append(app.Events, CorrectionEvent{
				At:          now,
				Handler:     handler,
				Basis:       basis,
				Changed:     false,
				FromVersion: cur.Version,
				ToVersion:   cur.Version,
				Note:        "target price identical to effective price; no settlement change",
			})
			ev := Event{
				Type:       EvCorrectionApproved,
				Timestamp:  now,
				AuctionID:  auctionID,
				ExternalID: correctionID,
				Correction: app,
			}
			if _, err := e.store.Append([]Event{ev}); err != nil {
				app.Status = CorrectionSubmitted
				app.ApprovedAt = nil
				app.EffectiveVersion = 0
				app.Events = app.Events[:len(app.Events)-1]
				e.mu.Unlock()
				return ApproveCorrectionResult{}, err
			}
			e.mu.Unlock()
			return ApproveCorrectionResult{Outcome: CorrectionAccepted, Data: *app}, nil
		}

		// 有价格变化：锁内构造新版本草案与补偿单草案（纯计算），认领批次后
		// 锁外结算补偿，回锁再决定是否切换版本。
		next, err := buildNextSettlement(cur, targets, now)
		if err != nil {
			e.mu.Unlock()
			return ApproveCorrectionResult{}, err
		}
		comps, err := buildCompensations(app.Items, next, now)
		if err != nil {
			e.mu.Unlock()
			return ApproveCorrectionResult{}, err
		}
		e.correcting[auctionID] = struct{}{}
		e.mu.Unlock()

		settled, comps := e.settleCompensations(auctionID, comps, now)

		// —— 阶段三：回锁原子提交 ——
		e.mu.Lock()
		delete(e.correcting, auctionID)
		e.cond.Broadcast()

		app = e.corrections[auctionID][correctionID]
		cur = e.currentSettlementLocked(auctionID)
		if cur == nil || app.BaseSettleVersion != cur.Version || app.PriceVersion != cur.PriceVersion {
			// 认领期间所有写操作都在等待，基准版本不应变化；
			// 兜底：绝不把不一致状态伪装成完成——把“批准尝试”以 pending
			// 原子落盘（不携带新版本），随后返回冲突，内存与日志保持一致。
			app.Status = CorrectionPending
			app.Compensations = append(append([]Compensation(nil), app.Compensations...), comps...)
			app.Events = append(app.Events, CorrectionEvent{
				At:          now,
				Handler:     handler,
				Basis:       basis,
				Changed:     true,
				FromVersion: app.BaseSettleVersion,
				ToVersion:   app.BaseSettleVersion,
				Note:        compensationNote(comps, settled),
			})
			ev := Event{
				Type:       EvCorrectionApproved,
				Timestamp:  now,
				AuctionID:  auctionID,
				ExternalID: correctionID,
				Correction: app,
			}
			if _, appendErr := e.store.Append([]Event{ev}); appendErr != nil {
				e.mu.Unlock()
				return ApproveCorrectionResult{}, appendErr
			}
			e.mu.Unlock()
			return ApproveCorrectionResult{}, ErrStaleSettlement
		}

		res, err := e.commitCorrection(app, cur, next, comps, settled, handler, basis, now)
		e.mu.Unlock()
		if err != nil {
			return ApproveCorrectionResult{}, err
		}
		return res, nil
	}
}

// settleCompensations 在锁外逐笔结算补偿（外部通道，可能失败）。
// 成功的补偿单标记 settled 并记录时间；失败的保持 pending 并记录原因。
func (e *Engine) settleCompensations(auctionID int64, comps []Compensation, now time.Time) (bool, []Compensation) {
	allSettled := true
	for i := range comps {
		if err := e.compGateway.SettleCompensation(auctionID, comps[i].ExternalID, comps[i].Amount); err != nil {
			comps[i].Status = CompensationPending
			comps[i].FailureNote = err.Error()
			allSettled = false
			continue
		}
		comps[i].Status = CompensationSettled
		settledAt := now
		comps[i].SettledAt = &settledAt
	}
	return allSettled, comps
}

// commitCorrection 在持锁状态下落定一次有价格变化的批准。
//
// 补偿全部成功：旧版本置为 superseded，把“新结算版本 + 完整申请”放进
// 同一个 EvCorrectionApproved 事件原子追加；任一补偿失败：不切换版本，
// 原结算继续可用，补偿单与更正停在 pending。调用方须保证 app/cur 与
// 草案 next 基于同一基准版本。
func (e *Engine) commitCorrection(
	app *CorrectionApplication,
	cur *SettlementRecord,
	next *SettlementRecord,
	comps []Compensation,
	allSettled bool,
	handler, basis string,
	now time.Time,
) (ApproveCorrectionResult, error) {
	ev := Event{
		Type:       EvCorrectionApproved,
		Timestamp:  now,
		AuctionID:  app.AuctionID,
		ExternalID: app.CorrectionID,
	}
	if !allSettled {
		app.Status = CorrectionPending
		app.Compensations = append(append([]Compensation(nil), app.Compensations...), comps...)
		app.Events = append(app.Events, CorrectionEvent{
			At:          now,
			Handler:     handler,
			Basis:       basis,
			Changed:     true,
			FromVersion: cur.Version,
			ToVersion:   cur.Version, // 未切换版本
			Note:        compensationNote(comps, false),
		})
		ev.Correction = app
		if _, err := e.store.Append([]Event{ev}); err != nil {
			return ApproveCorrectionResult{}, err
		}
		return ApproveCorrectionResult{Outcome: CorrectionAccepted, Data: *app}, nil
	}

	app.Status = CorrectionCompleted
	app.Compensations = append(append([]Compensation(nil), app.Compensations...), comps...)
	app.EffectiveVersion = next.Version
	app.Events = append(app.Events, CorrectionEvent{
		At:          now,
		Handler:     handler,
		Basis:       basis,
		Changed:     true,
		FromVersion: cur.Version,
		ToVersion:   next.Version,
		Note:        compensationNote(comps, true),
	})
	ev.Correction = app
	ev.SettlementRecord = next
	if _, err := e.store.Append([]Event{ev}); err != nil {
		return ApproveCorrectionResult{}, err
	}
	cur.Superseded = true
	e.settlementChains[app.AuctionID] = append(e.settlementChains[app.AuctionID], next)
	return ApproveCorrectionResult{Outcome: CorrectionAccepted, Data: *app}, nil
}

// RetryCompensation 重试一张停在待处理的更正：从补偿步骤重新执行整个原子动作。
// 全部补偿结清后才切换结算版本、更正转为 completed；仍有失败则保持 pending。
func (e *Engine) RetryCompensation(auctionID int64, correctionID string) (CorrectionApplication, error) {
	if correctionID == "" {
		return CorrectionApplication{}, ErrEmptyCorrectionID
	}

	e.mu.Lock()
	for {
		if _, ok := e.auctions[auctionID]; !ok {
			e.mu.Unlock()
			return CorrectionApplication{}, ErrAuctionNotFound
		}
		owner, exists := e.correctionOwner[correctionID]
		if !exists {
			e.mu.Unlock()
			return CorrectionApplication{}, ErrCorrectionNotFound
		}
		if owner != auctionID {
			e.mu.Unlock()
			return CorrectionApplication{}, ErrCorrectionConflict
		}
		app := e.corrections[auctionID][correctionID]
		if app.Status != CorrectionPending {
			e.mu.Unlock()
			return CorrectionApplication{}, ErrCorrectionNotPending
		}
		if _, busy := e.correcting[auctionID]; busy {
			e.cond.Wait()
			continue
		}
		cur := e.currentSettlementLocked(auctionID)
		if cur == nil || app.BaseSettleVersion != cur.Version || app.PriceVersion != cur.PriceVersion {
			e.mu.Unlock()
			return CorrectionApplication{}, ErrStaleSettlement
		}

		// 回到“尚未批准”的内存状态：去掉上一次失败留下的补偿单与 pending
		// 审计事件，按当前时间重新构造草案并重走补偿结算；ApprovedAt 保留。
		app.Status = CorrectionSubmitted
		app.Compensations = nil
		if n := len(app.Events); n > 0 && app.Events[n-1].FromVersion == app.Events[n-1].ToVersion {
			app.Events = app.Events[:n-1]
		}
		targets := make(map[string]Money, len(app.Items))
		for _, it := range app.Items {
			targets[it.ExternalID] = it.TargetUnitPrice
		}
		now := e.clock.Now()
		next, err := buildNextSettlement(cur, targets, now)
		if err != nil {
			e.mu.Unlock()
			return CorrectionApplication{}, err
		}
		comps, err := buildCompensations(app.Items, next, now)
		if err != nil {
			e.mu.Unlock()
			return CorrectionApplication{}, err
		}
		e.correcting[auctionID] = struct{}{}
		e.mu.Unlock()

		settled, comps := e.settleCompensations(auctionID, comps, now)

		e.mu.Lock()
		delete(e.correcting, auctionID)
		e.cond.Broadcast()

		app = e.corrections[auctionID][correctionID]
		cur = e.currentSettlementLocked(auctionID)
		if cur == nil || app.BaseSettleVersion != cur.Version || app.PriceVersion != cur.PriceVersion {
			app.Status = CorrectionPending
			app.Compensations = append(append([]Compensation(nil), app.Compensations...), comps...)
			app.Events = append(app.Events, CorrectionEvent{
				At:          now,
				Handler:     app.Handler,
				Basis:       app.Basis,
				Changed:     true,
				FromVersion: app.BaseSettleVersion,
				ToVersion:   app.BaseSettleVersion,
				Note:        compensationNote(comps, settled),
			})
			ev := Event{
				Type:       EvCorrectionApproved,
				Timestamp:  now,
				AuctionID:  auctionID,
				ExternalID: correctionID,
				Correction: app,
			}
			if _, appendErr := e.store.Append([]Event{ev}); appendErr != nil {
				e.mu.Unlock()
				return CorrectionApplication{}, appendErr
			}
			e.mu.Unlock()
			return CorrectionApplication{}, ErrStaleSettlement
		}
		if _, err := e.commitCorrection(app, cur, next, comps, settled, app.Handler, app.Basis, now); err != nil {
			e.mu.Unlock()
			return CorrectionApplication{}, err
		}
		e.mu.Unlock()
		return *e.corrections[auctionID][correctionID], nil
	}
}
