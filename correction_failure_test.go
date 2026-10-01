package auctionclearing

import (
	"errors"
	"testing"
)

// 补偿结算失败：补偿单保持 pending、更正停在 pending、版本不切换、
// 原成交仍可查询；混合条目时未交割部分也不得提前生效；重试成功后整体完成。
func TestApproveCorrection_CompensationFailureStaysPending(t *testing.T) {
	gw := newFailingGateway(true)
	e, _, id := correctionFixture(t, gw)
	if _, err := e.ConfirmFill(id, "b1"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.MarkDelivered(id, "b1", 3); err != nil {
		t.Fatal(err)
	}
	submitCorrectionFor(t, e, id, "COR-FAIL",
		correctionItem("b1", "6"), correctionItem("b2", "5"))
	res, err := e.ApproveCorrection(id, "COR-FAIL", ApproveCorrectionInput{})
	if err != nil {
		t.Fatal(err)
	}
	app := res.Data
	if app.Status != CorrectionPending {
		t.Fatalf("status = %s, want pending", app.Status)
	}
	if len(app.Compensations) != 1 || app.Compensations[0].Status != CompensationPending ||
		app.Compensations[0].FailureNote == "" {
		t.Fatalf("pending compensation = %+v", app.Compensations)
	}
	rec, err := e.GetSettlementRecord(id)
	if err != nil || rec.Version != 1 {
		t.Fatalf("version must stay 1: %+v %v", rec, err)
	}
	if en := entryByExternalID(&rec, "b1"); en.EffectiveUnitPrice.String() != "8" {
		t.Fatalf("delivered price changed: %s", en.EffectiveUnitPrice)
	}
	if en := entryByExternalID(&rec, "b2"); en.EffectiveUnitPrice.String() != "8" || en.SettledAmount.String() != "40" {
		t.Fatalf("undelivered price early-applied: %+v", en)
	}
	if _, err := e.GetFillEntry(id, "b1"); err != nil {
		t.Fatalf("original fill vanished: %v", err)
	}
	re, err := e.ApproveCorrection(id, "COR-FAIL", ApproveCorrectionInput{})
	if err != nil || re.Outcome != CorrectionReplayed || re.Data.Status != CorrectionPending {
		t.Fatalf("re-approve while pending: %+v %v", re, err)
	}
	if _, err := e.RetryCompensation(id, "COR-NOTHING"); !errors.Is(err, ErrCorrectionNotFound) {
		t.Fatalf("retry missing: %v", err)
	}

	gw.setFail(false)
	app2, err := e.RetryCompensation(id, "COR-FAIL")
	if err != nil {
		t.Fatal(err)
	}
	if app2.Status != CorrectionCompleted || app2.EffectiveVersion != 2 ||
		len(app2.Compensations) != 1 || app2.Compensations[0].Status != CompensationSettled ||
		app2.Compensations[0].Amount.String() != "6" {
		t.Fatalf("retried app = %+v", app2)
	}
	rec2, err := e.GetSettlementRecord(id)
	if err != nil {
		t.Fatal(err)
	}
	b1 := entryByExternalID(&rec2, "b1")
	b2 := entryByExternalID(&rec2, "b2")
	if b1.EffectiveUnitPrice.String() != "8" || b1.SettledAmount.String() != "24" {
		t.Fatalf("b1 after retry: %+v", b1)
	}
	if b2.EffectiveUnitPrice.String() != "5" || b2.SettledAmount.String() != "25" {
		t.Fatalf("b2 after retry: %+v", b2)
	}
	if rec2.TotalSettled.String() != "65" { // 24(b1) + 25(b2) + 16(b3 未在更正清单)
		t.Fatalf("total = %s, want 65", rec2.TotalSettled)
	}
}

// 连续两次更正：版本链完整、旧版本 superseded 可查、旧更正基准版本不被改写。
func TestChainedCorrections_PreservesEveryVersion(t *testing.T) {
	e, _, id := correctionFixture(t, nil)
	submitCorrectionFor(t, e, id, "COR-1", correctionItem("b1", "7"))
	approveCorrectionFor(t, e, id, "COR-1")
	submitCorrectionFor(t, e, id, "COR-2", correctionItem("b1", "6"))
	approveCorrectionFor(t, e, id, "COR-2")

	c1, err := e.GetCorrection(id, "COR-1")
	if err != nil {
		t.Fatal(err)
	}
	c2, _ := e.GetCorrection(id, "COR-2")
	if c1.BaseSettleVersion != 1 || c1.EffectiveVersion != 2 ||
		c2.BaseSettleVersion != 2 || c2.EffectiveVersion != 3 {
		t.Fatalf("versions c1=%d/%d c2=%d/%d",
			c1.BaseSettleVersion, c1.EffectiveVersion, c2.BaseSettleVersion, c2.EffectiveVersion)
	}
	v1, err := e.GetSettlementVersion(id, 1)
	if err != nil {
		t.Fatal(err)
	}
	v2, _ := e.GetSettlementVersion(id, 2)
	v3, _ := e.GetSettlementVersion(id, 3)
	if !v1.Superseded || !v2.Superseded || v3.Superseded {
		t.Fatal("only the last version must be active")
	}
	if v1.Entries[0].EffectiveUnitPrice.String() != "8" ||
		v2.Entries[0].EffectiveUnitPrice.String() != "7" ||
		v3.Entries[0].EffectiveUnitPrice.String() != "6" {
		t.Fatal("version chain prices altered")
	}
	// 旧更正的追溯仍指向它自己的生效版本，并附带当前版本指针。
	tr1, err := e.GetCorrectionTrace(id, "COR-1")
	if err != nil {
		t.Fatal(err)
	}
	if tr1.Items[0].FinalSettlement.EffectiveUnitPrice.String() != "7" {
		t.Fatalf("old final settlement overwritten: %+v", tr1.Items[0].FinalSettlement)
	}
	if tr1.Items[0].CurrentSettlement == nil ||
		tr1.Items[0].CurrentSettlement.EffectiveUnitPrice.String() != "6" {
		t.Fatalf("old trace must point at current v3: %+v", tr1.Items[0].CurrentSettlement)
	}
}

// 三种情形的追溯结果必须结构不同。
func TestCorrectionTrace_ThreeScenariosDiffer(t *testing.T) {
	e, _, id := correctionFixture(t, nil)

	// b1：全额交割（3/3）；b2：部分交割（2/5）；b3：未交割（0/2）。
	for _, ext := range []string{"b1", "b2"} {
		if _, err := e.ConfirmFill(id, ext); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := e.MarkDelivered(id, "b1", 3); err != nil {
		t.Fatal(err)
	}
	if _, err := e.MarkDelivered(id, "b2", 2); err != nil {
		t.Fatal(err)
	}
	submitCorrectionFor(t, e, id, "COR-TRACE",
		correctionItem("b1", "6"),
		correctionItem("b2", "6"),
		correctionItem("b3", "6"),
	)
	approveCorrectionFor(t, e, id, "COR-TRACE")
	tr, err := e.GetCorrectionTrace(id, "COR-TRACE")
	if err != nil {
		t.Fatal(err)
	}
	if tr.Reason == "" || tr.Basis == "" || tr.Handler == "" || tr.ApprovedAt == nil {
		t.Fatalf("trace header missing audit info: %+v", tr)
	}
	byExt := map[string]CorrectionTraceItem{}
	for _, it := range tr.Items {
		byExt[it.ExternalID] = it
	}

	delivered := byExt["b1"]
	if delivered.Scenario != ScenarioDelivered || delivered.Compensation == nil ||
		delivered.Compensation.Quantity != 3 || delivered.Compensation.Amount.String() != "6" {
		t.Fatalf("delivered item = %+v", delivered)
	}
	if delivered.FinalSettlement.EffectiveUnitPrice.String() != "8" ||
		delivered.FinalSettlement.DeliveredQty != 3 {
		t.Fatalf("delivered final must keep original: %+v", delivered.FinalSettlement)
	}
	if delivered.Original.Price.String() != "8" || delivered.Original.Quantity != 3 ||
		delivered.Original.Amount.String() != "24" {
		t.Fatalf("original fill view = %+v", delivered.Original)
	}

	partial := byExt["b2"]
	if partial.Scenario != ScenarioPartial || partial.Compensation == nil ||
		partial.Compensation.Quantity != 2 || partial.Compensation.Amount.String() != "4" {
		t.Fatalf("partial item = %+v", partial)
	}
	if partial.FinalSettlement.EffectiveUnitPrice.String() != "6" ||
		partial.FinalSettlement.DeliveredQty != 2 ||
		partial.FinalSettlement.SettledAmount.String() != "34" { // 2*8 + 3*6
		t.Fatalf("partial final = %+v", partial.FinalSettlement)
	}

	undelivered := byExt["b3"]
	if undelivered.Scenario != ScenarioUndelivered || undelivered.Compensation != nil {
		t.Fatalf("undelivered item = %+v", undelivered)
	}
	if undelivered.FinalSettlement.EffectiveUnitPrice.String() != "6" ||
		undelivered.FinalSettlement.DeliveredQty != 0 {
		t.Fatalf("undelivered final = %+v", undelivered.FinalSettlement)
	}
}

// 尚未批准的追溯停在申请快照（scenario=submitted），且不泄露补偿单。
func TestCorrectionTrace_SubmittedScenario(t *testing.T) {
	e, _, id := correctionFixture(t, nil)
	submitCorrectionFor(t, e, id, "COR-WAIT", correctionItem("b1", "6"))
	tr, err := e.GetCorrectionTrace(id, "COR-WAIT")
	if err != nil {
		t.Fatal(err)
	}
	if tr.Status != CorrectionSubmitted || tr.EffectiveVersion != 1 ||
		tr.Items[0].Scenario != ScenarioSubmitted || tr.Items[0].Compensation != nil {
		t.Fatalf("submitted trace = %+v", tr)
	}
}
