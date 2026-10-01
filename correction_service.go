package auctionclearing

// CorrectionInput 是登记一张成交更正申请的入参。
type CorrectionInput struct {
	CorrectionID string
	Reason       string
	Basis        string // 更正依据（即便价格不变也必填并留痕）
	Handler      string // 处理人
	Items        []CorrectionItem
}

// SubmitCorrectionOutcome 表示一次更正申请登记的结果。
type SubmitCorrectionOutcome string

const (
	// CorrectionAccepted 表示新更正申请被接受。
	CorrectionAccepted SubmitCorrectionOutcome = "accepted"
	// CorrectionReplayed 表示同一更正号、相同批次/版本/内容的重复请求，返回原申请。
	CorrectionReplayed SubmitCorrectionOutcome = "replayed"
)

// SubmitCorrectionResult 是登记更正申请的返回值。
type SubmitCorrectionResult struct {
	Outcome SubmitCorrectionOutcome
	Data    CorrectionApplication
}

// ApproveCorrectionInput 是批准更正的入参；Handler/Basis 可留空，沿用申请值。
type ApproveCorrectionInput struct {
	Handler string
	Basis   string
}

// ApproveCorrectionResult 是批准更正的返回值；Outcome=replayed 表示重复批准。
type ApproveCorrectionResult struct {
	Outcome SubmitCorrectionOutcome
	Data    CorrectionApplication
}

func (in CorrectionInput) validate() error {
	switch {
	case in.CorrectionID == "":
		return ErrEmptyCorrectionID
	case in.Reason == "":
		return ErrEmptyCorrectionReason
	case in.Basis == "":
		return ErrEmptyCorrectionBasis
	case in.Handler == "":
		return ErrEmptyCorrectionHandler
	case len(in.Items) == 0:
		return ErrEmptyCorrectionItems
	}
	seen := make(map[string]struct{}, len(in.Items))
	for _, it := range in.Items {
		if it.ExternalID == "" {
			return ErrEmptyExternalID
		}
		if _, dup := seen[it.ExternalID]; dup {
			return ErrDuplicateCorrectionItem
		}
		seen[it.ExternalID] = struct{}{}
		if it.TargetUnitPrice.IsNegative() {
			return ErrNegativeTargetPrice
		}
	}
	return nil
}

// SubmitCorrection 登记一张成交更正申请。
//
// 申请保存成交清单（逐笔快照：成交量、已交割量、原价、价格版本、提交时
// 结算状态）、申请原因、更正依据、处理人，以及挂接的价格版本与基准结算版本。
// 更正范围受竞价批次（拍卖）与结算版本双重限制：
//   - 相同更正号、相同批次/版本/内容重复提交：幂等返回原结果（replayed）；
//   - 相同更正号但竞价批次、价格版本、基准结算版本或申请内容不一致：冲突；
//   - 已全额交割的成交申请阶段允许登记，批准时自动转为补偿单，
//     不会尝试改写其原结算。
func (e *Engine) SubmitCorrection(auctionID int64, in CorrectionInput) (SubmitCorrectionResult, error) {
	if err := in.validate(); err != nil {
		return SubmitCorrectionResult{}, err
	}

	e.mu.Lock()
	defer e.mu.Unlock()

	e.waitWhileCorrectingLocked(auctionID)

	if _, ok := e.auctions[auctionID]; !ok {
		return SubmitCorrectionResult{}, ErrAuctionNotFound
	}
	cur := e.currentSettlementLocked(auctionID)
	if cur == nil {
		return SubmitCorrectionResult{}, ErrNoSettlement
	}

	// 同号幂等判定先于一切：批次（拍卖）、价格版本、基准结算版本必须不变，
	// 同版本下再比较申请内容（原因/依据/处理人/逐笔成交号与目标单价）。
	if owner, exists := e.correctionOwner[in.CorrectionID]; exists {
		if owner != auctionID {
			return SubmitCorrectionResult{}, ErrCorrectionConflict
		}
		existing := e.corrections[owner][in.CorrectionID]
		if existing.PriceVersion != cur.PriceVersion ||
			existing.BaseSettleVersion != cur.Version ||
			!correctionRequestEqual(existing, in) {
			return SubmitCorrectionResult{}, ErrCorrectionConflict
		}
		return SubmitCorrectionResult{Outcome: CorrectionReplayed, Data: *existing}, nil
	}

	app := &CorrectionApplication{
		CorrectionID:      in.CorrectionID,
		AuctionID:         auctionID,
		BatchID:           auctionID,
		Reason:            in.Reason,
		Basis:             in.Basis,
		Handler:           in.Handler,
		PriceVersion:      cur.PriceVersion,
		BaseSettleVersion: cur.Version,
		Items:             append([]CorrectionItem(nil), in.Items...),
		Status:            CorrectionSubmitted,
		SubmittedAt:       e.clock.Now(),
		Snapshots:         make([]CorrectionItemSnapshot, 0, len(in.Items)),
	}
	for _, it := range in.Items {
		en := entryByExternalID(cur, it.ExternalID)
		if en == nil {
			return SubmitCorrectionResult{}, ErrFillNotFound
		}
		app.Snapshots = append(app.Snapshots, CorrectionItemSnapshot{
			ExternalID:      en.ExternalID,
			Rank:            en.Rank,
			MatchedQty:      en.MatchedQty,
			DeliveredQty:    en.DeliveredQty,
			OriginalPrice:   en.OriginalUnitPrice,
			PriceVersion:    en.PriceVersion,
			SettlementState: en.Status(),
		})
	}

	ev := Event{
		Type:       EvCorrectionSubmitted,
		Timestamp:  app.SubmittedAt,
		AuctionID:  auctionID,
		ExternalID: in.CorrectionID,
		Correction: app,
	}
	if _, err := e.store.Append([]Event{ev}); err != nil {
		return SubmitCorrectionResult{}, err
	}
	e.indexCorrection(app)
	return SubmitCorrectionResult{Outcome: CorrectionAccepted, Data: *app}, nil
}

// correctionRequestEqual 判定同号更正是否为同一请求的重试。
func correctionRequestEqual(a *CorrectionApplication, in CorrectionInput) bool {
	if a.Reason != in.Reason || a.Basis != in.Basis || a.Handler != in.Handler {
		return false
	}
	if len(a.Items) != len(in.Items) {
		return false
	}
	targets := make(map[string]Money, len(in.Items))
	for _, it := range in.Items {
		targets[it.ExternalID] = it.TargetUnitPrice
	}
	for _, it := range a.Items {
		p, ok := targets[it.ExternalID]
		if !ok || p != it.TargetUnitPrice {
			return false
		}
	}
	return true
}
