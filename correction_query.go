package auctionclearing

import "sort"

// GetCorrection 查询一张更正申请（含成交快照、补偿单与审计事件）。
func (e *Engine) GetCorrection(auctionID int64, correctionID string) (CorrectionApplication, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if _, ok := e.auctions[auctionID]; !ok {
		return CorrectionApplication{}, ErrAuctionNotFound
	}
	app, ok := e.corrections[auctionID][correctionID]
	if !ok {
		return CorrectionApplication{}, ErrCorrectionNotFound
	}
	return *app, nil
}

// ListCorrections 列出一个竞价批次下的全部更正申请（按更正号排序，顺序确定）。
func (e *Engine) ListCorrections(auctionID int64) ([]CorrectionApplication, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if _, ok := e.auctions[auctionID]; !ok {
		return nil, ErrAuctionNotFound
	}
	ids := make([]string, 0, len(e.corrections[auctionID]))
	for id := range e.corrections[auctionID] {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]CorrectionApplication, 0, len(ids))
	for _, id := range ids {
		out = append(out, *e.corrections[auctionID][id])
	}
	return out, nil
}

// GetCorrectionTrace 组装一张更正的完整链路：从更正号追到原成交、
// 申请原因/依据/处理人、补偿单与最终结算。已交割 / 未交割 / 部分补偿
// 三种情形的条目结构不同：
//   - 未交割：Compensation 为空，FinalSettlement 直接体现新单价；
//   - 已交割：Compensation 为全额差额补偿单，FinalSettlement 保留原价与交割状态；
//   - 部分补偿：Compensation 只覆盖已交割数量，FinalSettlement 同时含未交割改价。
//
// 尚未批准（submitted/pending）时链路停在基准版本，scenario 分别为
// submitted（未批准）或按当前交割量派生（pending：补偿失败、版本未切换）。
func (e *Engine) GetCorrectionTrace(auctionID int64, correctionID string) (CorrectionTrace, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	if _, ok := e.auctions[auctionID]; !ok {
		return CorrectionTrace{}, ErrAuctionNotFound
	}
	app, ok := e.corrections[auctionID][correctionID]
	if !ok {
		return CorrectionTrace{}, ErrCorrectionNotFound
	}
	cl, ok := e.clearings[auctionID]
	if !ok {
		return CorrectionTrace{}, ErrSettlementNotFound
	}
	cur := e.currentSettlementLocked(auctionID)
	if cur == nil {
		return CorrectionTrace{}, ErrNoSettlement
	}

	// 生效版本：已批准完成取 EffectiveVersion；尚未批准或停在 pending
	// 时取基准版本（版本未切换）。
	effVersion := app.EffectiveVersion
	if effVersion == 0 {
		effVersion = app.BaseSettleVersion
	}
	var eff *SettlementRecord
	for _, rec := range e.settlementChains[auctionID] {
		if rec.Version == effVersion {
			eff = rec
			break
		}
	}
	if eff == nil {
		eff = cur
	}

	trace := CorrectionTrace{
		CorrectionID:     app.CorrectionID,
		BatchID:          app.BatchID,
		Reason:           app.Reason,
		Basis:            app.Basis,
		Handler:          app.Handler,
		PriceVersion:     app.PriceVersion,
		BaseVersion:      app.BaseSettleVersion,
		EffectiveVersion: eff.Version,
		CurrentVersion:   cur.Version,
		Status:           app.Status,
		SubmittedAt:      app.SubmittedAt,
		Events:           append([]CorrectionEvent(nil), app.Events...),
		Items:            make([]CorrectionTraceItem, 0, len(app.Items)),
	}
	if app.ApprovedAt != nil {
		t := *app.ApprovedAt
		trace.ApprovedAt = &t
	}

	snapByRef := make(map[string]CorrectionItemSnapshot, len(app.Snapshots))
	for _, snap := range app.Snapshots {
		snapByRef[snap.ExternalID] = snap
	}
	compByRef := make(map[string]*Compensation)
	for i := range app.Compensations {
		// 重试会替换补偿单集合；追溯时以最新一次的补偿单为准。
		compByRef[app.Compensations[i].ExternalID] = &app.Compensations[i]
	}

	for _, it := range app.Items {
		item := CorrectionTraceItem{
			ExternalID:      it.ExternalID,
			TargetUnitPrice: it.TargetUnitPrice,
			SubmitSnapshot:  snapByRef[it.ExternalID],
		}
		for _, f := range cl.Fills {
			if f.ExternalID != it.ExternalID {
				continue
			}
			amount, _ := f.Price.Mul(f.FilledQty)
			item.Original = OriginalFillView{
				ExternalID: f.ExternalID,
				Bidder:     f.Bidder,
				Rank:       f.Rank,
				Price:      f.Price,
				Quantity:   f.FilledQty,
				Amount:     amount,
			}
		}
		if en := entryByExternalID(eff, it.ExternalID); en != nil {
			final := *en
			item.FinalSettlement = &final
			switch {
			case app.Status == CorrectionSubmitted:
				item.Scenario = ScenarioSubmitted
			case en.DeliveredQty <= 0:
				item.Scenario = ScenarioUndelivered
			case en.DeliveredQty >= en.MatchedQty:
				item.Scenario = ScenarioDelivered
			default:
				item.Scenario = ScenarioPartial
			}
		}
		if c := compByRef[it.ExternalID]; c != nil {
			cp := *c
			item.Compensation = &cp
		}
		if cur.Version != eff.Version {
			if en := entryByExternalID(cur, it.ExternalID); en != nil {
				cp := *en
				item.CurrentSettlement = &cp
			}
		}
		trace.Items = append(trace.Items, item)
	}
	return trace, nil
}
