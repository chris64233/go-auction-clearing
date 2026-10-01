package auctionclearing

import (
	"errors"
	"testing"
)

func entryOf(t *testing.T, e *Engine, auctionID int64, ext string) SettlementEntry {
	t.Helper()
	rec, err := e.GetSettlementRecord(auctionID)
	if err != nil {
		t.Fatal(err)
	}
	for _, en := range rec.Entries {
		if en.ExternalID == ext {
			return en
		}
	}
	t.Fatalf("entry %s not found", ext)
	return SettlementEntry{}
}

// 未交割成交：直接在新结算版本改价，且只改允许的结算字段；
// 原始撮合信息与确认/冻结状态原样继承。
func TestApproveCorrection_UndeliveredCorrectsOnlySettlementFields(t *testing.T) {
	e, _, id := correctionFixture(t, nil)
	if _, err := e.ConfirmFill(id, "b1"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.FreezeFunds(id, "b1"); err != nil {
		t.Fatal(err)
	}
	submitCorrectionFor(t, e, id, "COR-U", correctionItem("b1", "6"))
	app := approveCorrectionFor(t, e, id, "COR-U")

	if app.Status != CorrectionCompleted || app.EffectiveVersion != 2 {
		t.Fatalf("app = %+v", app)
	}
	rec, err := e.GetSettlementRecord(id)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Version != 2 || rec.Superseded {
		t.Fatalf("current record = %+v", rec)
	}
	en := entryByExternalID(&rec, "b1")
	if en.EffectiveUnitPrice.String() != "6" || en.SettledAmount.String() != "18" {
		t.Fatalf("corrected entry = %+v", en)
	}
	if en.OriginalUnitPrice.String() != "8" || en.MatchedQty != 3 ||
		en.PriceVersion != InitialPriceVersion || en.Rank != 1 ||
		!en.Confirmed || !en.FundsFrozen || en.DeliveredQty != 0 {
		t.Fatalf("preserved fields changed: %+v", en)
	}
	v1, err := e.GetSettlementVersion(id, 1)
	if err != nil || !v1.Superseded || v1.Entries[0].EffectiveUnitPrice.String() != "8" {
		t.Fatalf("v1 = %+v err=%v", v1, err)
	}
	cl, err := e.GetSettlement(id)
	if err != nil || cl.ClearingPrice.String() != "8" {
		t.Fatalf("clearing mutated: %+v %v", cl, err)
	}
	res, err := e.ApproveCorrection(id, "COR-U", ApproveCorrectionInput{})
	if err != nil || res.Outcome != CorrectionReplayed || res.Data.Status != CorrectionCompleted {
		t.Fatalf("re-approve: %+v %v", res, err)
	}
}

// 已全额交割：原成交不动，生成全额差额补偿单。
func TestApproveCorrection_DeliveredCreatesCompensationOnly(t *testing.T) {
	e, _, id := correctionFixture(t, nil)
	if _, err := e.ConfirmFill(id, "b1"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.MarkDelivered(id, "b1", 3); err != nil {
		t.Fatal(err)
	}
	submitCorrectionFor(t, e, id, "COR-D", correctionItem("b1", "6"))
	app := approveCorrectionFor(t, e, id, "COR-D")

	if app.Status != CorrectionCompleted || app.EffectiveVersion != 2 {
		t.Fatalf("app = %+v", app)
	}
	if len(app.Compensations) != 1 {
		t.Fatalf("compensations = %+v", app.Compensations)
	}
	c := app.Compensations[0]
	if c.Quantity != 3 || c.LockedAmount.String() != "24" ||
		c.TargetPrice.String() != "6" || c.Amount.String() != "6" ||
		c.Status != CompensationSettled || c.SettledAt == nil {
		t.Fatalf("compensation = %+v", c)
	}
	en := entryOf(t, e, id, "b1")
	if en.EffectiveUnitPrice.String() != "8" || en.SettledAmount.String() != "24" ||
		en.DeliveredQty != 3 || en.DeliveredAmount.String() != "24" {
		t.Fatalf("delivered entry mutated: %+v", en)
	}
}

// 上调目标价（8 → 10）：补偿差额为负。
func TestApproveCorrection_UpwardPriceNegativeCompensation(t *testing.T) {
	e, _, id := correctionFixture(t, nil)
	if _, err := e.ConfirmFill(id, "b1"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.MarkDelivered(id, "b1", 3); err != nil {
		t.Fatal(err)
	}
	submitCorrectionFor(t, e, id, "COR-UP", correctionItem("b1", "10"))
	app := approveCorrectionFor(t, e, id, "COR-UP")
	if len(app.Compensations) != 1 || app.Compensations[0].Amount.String() != "-6" {
		t.Fatalf("upward compensation: %+v", app.Compensations)
	}
}

// 部分交割：已交割部分走补偿，未交割部分在新版本直接改价，两者并存。
func TestApproveCorrection_PartialCompensation(t *testing.T) {
	e, _, id := correctionFixture(t, nil)
	if _, err := e.ConfirmFill(id, "b1"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.MarkDelivered(id, "b1", 2); err != nil {
		t.Fatal(err)
	}
	submitCorrectionFor(t, e, id, "COR-P", correctionItem("b1", "5"))
	app := approveCorrectionFor(t, e, id, "COR-P")

	if app.Status != CorrectionCompleted || len(app.Compensations) != 1 {
		t.Fatalf("app = %+v", app)
	}
	c := app.Compensations[0]
	if c.Quantity != 2 || c.LockedAmount.String() != "16" || c.Amount.String() != "6" {
		t.Fatalf("partial compensation = %+v", c)
	}
	en := entryOf(t, e, id, "b1")
	if en.EffectiveUnitPrice.String() != "5" || en.SettledAmount.String() != "21" ||
		en.DeliveredQty != 2 || en.DeliveredAmount.String() != "16" {
		t.Fatalf("partial entry = %+v", en)
	}
}

// 价格未变：不产生新版本，但必须留下依据与处理人的审计事件。
func TestApproveCorrection_NoPriceChangeStillRecorded(t *testing.T) {
	e, _, id := correctionFixture(t, nil)
	submitCorrectionFor(t, e, id, "COR-SAME", correctionItem("b1", "8"))
	app := approveCorrectionFor(t, e, id, "COR-SAME")

	if app.Status != CorrectionCompleted || app.EffectiveVersion != 1 {
		t.Fatalf("app = %+v", app)
	}
	rec, err := e.GetSettlementRecord(id)
	if err != nil || rec.Version != 1 {
		t.Fatalf("version must stay 1: %+v %v", rec, err)
	}
	if len(app.Events) != 1 {
		t.Fatalf("events = %+v", app.Events)
	}
	ev := app.Events[0]
	if ev.Changed || ev.Handler != "risk-officer-1" || ev.Basis == "" ||
		ev.FromVersion != 1 || ev.ToVersion != 1 {
		t.Fatalf("audit event = %+v", ev)
	}
	submitCorrectionFor(t, e, id, "COR-SAME-2", correctionItem("b2", "8"))
	res, err := e.ApproveCorrection(id, "COR-SAME-2", ApproveCorrectionInput{
		Handler: "manager-9", Basis: "manual-review-42",
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Data.Events[0].Handler != "manager-9" || res.Data.Events[0].Basis != "manual-review-42" {
		t.Fatalf("override audit = %+v", res.Data.Events[0])
	}
}

// 批准时基准版本已被后续更正取代：冲突，申请保持 submitted，原结算继续可用。
func TestApproveCorrection_StaleVersionConflicts(t *testing.T) {
	e, _, id := correctionFixture(t, nil)
	submitCorrectionFor(t, e, id, "COR-OLD", correctionItem("b1", "7"))
	submitCorrectionFor(t, e, id, "COR-NEW", correctionItem("b1", "6"))
	approveCorrectionFor(t, e, id, "COR-NEW")

	if _, err := e.ApproveCorrection(id, "COR-OLD", ApproveCorrectionInput{}); !errors.Is(err, ErrStaleSettlement) {
		t.Fatalf("stale approve: %v", err)
	}
	old, err := e.GetCorrection(id, "COR-OLD")
	if err != nil || old.Status != CorrectionSubmitted {
		t.Fatalf("old application = %+v %v", old, err)
	}
	if en := entryOf(t, e, id, "b1"); en.EffectiveUnitPrice.String() != "6" {
		t.Fatalf("current settlement = %+v", en)
	}
}
