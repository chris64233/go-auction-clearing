package auctionclearing

import (
	"errors"
	"testing"
	"time"
)

// —— 首版结算与确认/冻结/交割流水 ——

func TestGenerateSettlement_IdempotentAndRequiresClearing(t *testing.T) {
	e, clk, _ := correctionFixture(t, nil)

	res, err := e.GenerateSettlement(1)
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != SettlementReplayed || res.Data.Version != InitialSettlementVersion {
		t.Fatalf("replay = %+v", res)
	}

	// 未清算的拍卖不能生成结算。
	a, err := e.CreateAuction(1, clk.Now().Add(time.Hour), MustParseMoney("1"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.GenerateSettlement(a.ID); !errors.Is(err, ErrSettlementNotFound) {
		t.Fatalf("err = %v, want ErrSettlementNotFound", err)
	}
}

func TestConfirmFreezeDeliver_LifecycleAndOrdering(t *testing.T) {
	e, clk, id := correctionFixture(t, nil)

	if _, err := e.FreezeFunds(id, "b1"); !errors.Is(err, ErrNotConfirmed) {
		t.Fatalf("freeze before confirm: %v", err)
	}
	en, err := e.ConfirmFill(id, "b1")
	if err != nil || en.Status() != EntryConfirmed {
		t.Fatalf("confirm: %+v %v", en, err)
	}
	if en2, err := e.ConfirmFill(id, "b1"); err != nil || !en2.Confirmed {
		t.Fatalf("confirm idempotent: %+v %v", en2, err)
	}
	en, err = e.FreezeFunds(id, "b1")
	if err != nil || en.Status() != EntryFrozen {
		t.Fatalf("freeze: %+v %v", en, err)
	}
	if en2, err := e.FreezeFunds(id, "b1"); err != nil || !en2.FundsFrozen {
		t.Fatalf("freeze idempotent: %+v %v", en2, err)
	}

	if _, err := e.MarkDelivered(id, "b1", 4); !errors.Is(err, ErrDeliveryExceedsFill) {
		t.Fatalf("overdeliver: %v", err)
	}
	if _, err := e.MarkDelivered(id, "b1", -1); !errors.Is(err, ErrNegativeDeliveryQty) {
		t.Fatalf("negative delivery: %v", err)
	}
	// 增量交割 2 + 1：金额按首版有效单价 8 锁定。
	clk.Advance(time.Minute)
	en, err = e.MarkDelivered(id, "b1", 2)
	if err != nil {
		t.Fatal(err)
	}
	if en.DeliveredQty != 2 || en.DeliveredAmount.String() != "16" {
		t.Fatalf("delivery 2: qty=%d amount=%s", en.DeliveredQty, en.DeliveredAmount)
	}
	en, err = e.MarkDelivered(id, "b1", 1)
	if err != nil {
		t.Fatal(err)
	}
	if en.DeliveredQty != 3 || en.DeliveredAmount.String() != "24" || en.DeliveredAt == nil ||
		en.Status() != EntryDelivered {
		t.Fatalf("delivery 3: %+v", en)
	}

	if _, err := e.ConfirmFill(id, "nope"); !errors.Is(err, ErrFillNotFound) {
		t.Fatalf("unknown fill: %v", err)
	}
}

// —— 更正申请：快照、校验、幂等与冲突 ——

func TestSubmitCorrection_SavesSnapshots(t *testing.T) {
	e, _, id := correctionFixture(t, nil)
	if _, err := e.ConfirmFill(id, "b1"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.FreezeFunds(id, "b1"); err != nil {
		t.Fatal(err)
	}
	app := submitCorrectionFor(t, e, id, "COR-1", correctionItem("b1", "7"), correctionItem("b2", "6"))

	if app.PriceVersion != InitialPriceVersion || app.BaseSettleVersion != InitialSettlementVersion ||
		app.BatchID != id || app.Status != CorrectionSubmitted {
		t.Fatalf("application header: %+v", app)
	}
	if len(app.Snapshots) != 2 {
		t.Fatalf("snapshots = %d, want 2", len(app.Snapshots))
	}
	want := map[string]struct {
		qty, delivered int64
		price, state   string
	}{
		"b1": {3, 0, "8", "frozen"},
		"b2": {5, 0, "8", "pending"},
	}
	for _, snap := range app.Snapshots {
		w := want[snap.ExternalID]
		if snap.MatchedQty != w.qty || snap.DeliveredQty != w.delivered ||
			snap.OriginalPrice.String() != w.price || string(snap.SettlementState) != w.state ||
			snap.PriceVersion != InitialPriceVersion {
			t.Fatalf("snapshot %s = %+v, want %+v", snap.ExternalID, snap, w)
		}
	}
}

func TestSubmitCorrection_Validation(t *testing.T) {
	e, clk, id := correctionFixture(t, nil)
	cases := []struct {
		name string
		mut  func(*CorrectionInput)
		want error
	}{
		{"empty id", func(in *CorrectionInput) { in.CorrectionID = "" }, ErrEmptyCorrectionID},
		{"empty reason", func(in *CorrectionInput) { in.Reason = "" }, ErrEmptyCorrectionReason},
		{"empty basis", func(in *CorrectionInput) { in.Basis = "" }, ErrEmptyCorrectionBasis},
		{"empty handler", func(in *CorrectionInput) { in.Handler = "" }, ErrEmptyCorrectionHandler},
		{"empty items", func(in *CorrectionInput) { in.Items = nil }, ErrEmptyCorrectionItems},
		{"negative target", func(in *CorrectionInput) { in.Items[0].TargetUnitPrice = MustParseMoney("-1") }, ErrNegativeTargetPrice},
		{"duplicate item", func(in *CorrectionInput) {
			in.Items = append(in.Items, correctionItem("b1", "7"))
		}, ErrDuplicateCorrectionItem},
		{"unknown fill", func(in *CorrectionInput) { in.Items[0].ExternalID = "nope" }, ErrFillNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := correctionInput("COR-V", correctionItem("b1", "7"))
			tc.mut(&in)
			if _, err := e.SubmitCorrection(id, in); !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
		})
	}

	// 未生成结算的拍卖不能提交更正。
	a, err := e.CreateAuction(1, clk.Now().Add(time.Hour), MustParseMoney("1"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.SubmitCorrection(a.ID, correctionInput("COR-X", correctionItem("b1", "7"))); !errors.Is(err, ErrNoSettlement) {
		t.Fatalf("err = %v, want ErrNoSettlement", err)
	}
}

func TestSubmitCorrection_IdempotentReplayAndConflict(t *testing.T) {
	e, _, id := correctionFixture(t, nil)
	in := correctionInput("COR-IDEM", correctionItem("b1", "7"))
	if _, err := e.SubmitCorrection(id, in); err != nil {
		t.Fatal(err)
	}
	res, err := e.SubmitCorrection(id, in)
	if err != nil || res.Outcome != CorrectionReplayed {
		t.Fatalf("replay: outcome=%s err=%v", res.Outcome, err)
	}
	for _, mut := range []func(*CorrectionInput){
		func(in *CorrectionInput) { in.Reason = "other" },
		func(in *CorrectionInput) { in.Basis = "other" },
		func(in *CorrectionInput) { in.Handler = "other" },
		func(in *CorrectionInput) { in.Items[0].TargetUnitPrice = MustParseMoney("6") },
	} {
		other := in
		mut(&other)
		if _, err := e.SubmitCorrection(id, other); !errors.Is(err, ErrCorrectionConflict) {
			t.Fatalf("conflict err = %v, want ErrCorrectionConflict", err)
		}
	}
	if _, err := e.GetCorrection(id, "missing"); !errors.Is(err, ErrCorrectionNotFound) {
		t.Fatalf("get missing: %v", err)
	}
}

// TestSubmitCorrection_SameIdDifferentBatchConflicts 相同更正号用于另一个
// 竞价批次时必须报冲突，而不是返回原批次的结果。
func TestSubmitCorrection_SameIdDifferentBatchConflicts(t *testing.T) {
	e, clk, id1 := correctionFixture(t, nil)
	submitCorrectionFor(t, e, id1, "COR-SHARED", correctionItem("b1", "7"))

	t0 := clk.Now()
	a2, err := e.CreateAuction(10, t0.Add(time.Hour), MustParseMoney("5"))
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range []BidInput{
		{AuctionID: a2.ID, ExternalID: "b1", Bidder: "A", Qty: 3, Price: MustParseMoney("10")},
	} {
		clk.Advance(time.Second)
		if _, err := e.SubmitBid(b); err != nil {
			t.Fatal(err)
		}
	}
	clk.Set(t0.Add(2 * time.Hour))
	if _, err := e.SettleAuction(a2.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.GenerateSettlement(a2.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.SubmitCorrection(a2.ID, correctionInput("COR-SHARED", correctionItem("b1", "7"))); !errors.Is(err, ErrCorrectionConflict) {
		t.Fatalf("cross-batch reuse: %v", err)
	}
}
