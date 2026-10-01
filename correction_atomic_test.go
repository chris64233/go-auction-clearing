package auctionclearing

import (
	"context"
	"testing"
)

// TestApproveCorrection_FailureIsAtomicAcrossMixedItems 一张申请同时含
// 已交割成交（需补偿）与未交割成交（直接改价）：补偿通道失败时，未交割
// 部分也不得提前生效——结算版本不前进，两笔成交都维持原价；重试成功后
// 才整体进入新版本。
func TestApproveCorrection_FailureIsAtomicAcrossMixedItems(t *testing.T) {
	gw := &failingGateway{fail: true}
	svc, a := settledCorrectionFixture(t, gw)
	ctx := context.Background()
	// ORD-A 已全部交割 3@10；ORD-B 未交割。
	if _, err := svc.ConfirmFill(ctx, a.ID, "ORD-A"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.MarkDelivered(ctx, a.ID, "ORD-A", 3); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SubmitCorrection(ctx, a.ID, correctionParams("COR-MIX",
		CorrectionItem{ExternalRef: "ORD-A", TargetUnitPrice: MustParseMoney("9")},
		CorrectionItem{ExternalRef: "ORD-B", TargetUnitPrice: MustParseMoney("7")})); err != nil {
		t.Fatal(err)
	}
	app, _, err := svc.ApproveCorrection(ctx, a.ID, "COR-MIX", ApproveCorrectionParams{})
	if err != nil {
		t.Fatal(err)
	}
	if app.Status != CorrectionPending {
		t.Fatalf("status = %s, want pending", app.Status)
	}
	st, _ := svc.GetSettlement(ctx, a.ID)
	if st.Version != 1 {
		t.Fatalf("version must stay at 1, got %d", st.Version)
	}
	if e := entryByRef(st, "ORD-A"); e.EffectiveUnitPrice.String() != "10" {
		t.Fatalf("delivered price changed on failure: %s", e.EffectiveUnitPrice)
	}
	if e := entryByRef(st, "ORD-B"); e.EffectiveUnitPrice.String() != "8" || e.SettledAmount.String() != "16" {
		t.Fatalf("undelivered price must not early-apply: price=%s amount=%s",
			e.EffectiveUnitPrice, e.SettledAmount)
	}

	// 重试成功：v2 同时承载补偿（A）与直接改价（B）。
	gw.setFail(false)
	app2, err := svc.RetryCompensation(ctx, a.ID, "COR-MIX")
	if err != nil {
		t.Fatal(err)
	}
	if app2.Status != CorrectionCompleted || app2.EffectiveVersion != 2 ||
		len(app2.Compensations) != 1 || app2.Compensations[0].Amount.String() != "3" {
		t.Fatalf("retried correction: %+v", app2)
	}
	st2, _ := svc.GetSettlement(ctx, a.ID)
	if e := entryByRef(st2, "ORD-A"); e.EffectiveUnitPrice.String() != "10" || e.SettledAmount.String() != "30" {
		t.Fatalf("delivered fill after retry: %+v", e)
	}
	if e := entryByRef(st2, "ORD-B"); e.EffectiveUnitPrice.String() != "7" || e.SettledAmount.String() != "14" {
		t.Fatalf("undelivered fill after retry: %+v", e)
	}
	if st2.TotalSettled.String() != "44" { // 30 + 14
		t.Fatalf("total = %s, want 44", st2.TotalSettled)
	}

	// pending 期间可幂等重复批准，不产生副作用。
	app3, replayed, err := svc.ApproveCorrection(ctx, a.ID, "COR-MIX", ApproveCorrectionParams{})
	if err != nil || !replayed || app3.Status != CorrectionCompleted {
		t.Fatalf("approve after completion: %+v replay=%v %v", app3, replayed, err)
	}
}
