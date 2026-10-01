package auctionclearing

import (
	"context"
	"sync"
	"testing"
)

func TestApproveCorrection_UpwardPriceNegativeCompensation(t *testing.T) {
	svc, a := settledCorrectionFixture(t, nil)
	ctx := context.Background()
	// 已交割后把目标价上调（10 → 11）：补偿差额为负，表示需向成交方补收/补差。
	if _, err := svc.ConfirmFill(ctx, a.ID, "ORD-A"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.MarkDelivered(ctx, a.ID, "ORD-A", 3); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SubmitCorrection(ctx, a.ID, correctionParams("COR-UP",
		CorrectionItem{ExternalRef: "ORD-A", TargetUnitPrice: MustParseMoney("11")})); err != nil {
		t.Fatal(err)
	}
	app, _, err := svc.ApproveCorrection(ctx, a.ID, "COR-UP", ApproveCorrectionParams{})
	if err != nil {
		t.Fatal(err)
	}
	if len(app.Compensations) != 1 || app.Compensations[0].Amount.String() != "-3" {
		t.Fatalf("upward compensation: %+v", app.Compensations)
	}
}

// TestChainedCorrections_PreservesEveryVersion 验证连续两次更正：
// 版本链完整、旧版本 superseded 可查、旧更正的基准版本不被改写。
func TestChainedCorrections_PreservesEveryVersion(t *testing.T) {
	svc, a := settledCorrectionFixture(t, nil)
	ctx := context.Background()

	submitAndApprove(t, svc, a.ID, "COR-1",
		CorrectionItem{ExternalRef: "ORD-A", TargetUnitPrice: MustParseMoney("9")})
	submitAndApprove(t, svc, a.ID, "COR-2",
		CorrectionItem{ExternalRef: "ORD-A", TargetUnitPrice: MustParseMoney("8")})

	agg, err := svc.repo.Load(ctx, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(agg.Settlements) != 3 {
		t.Fatalf("want 3 settlement versions, got %d", len(agg.Settlements))
	}
	if !agg.Settlements[0].Superseded || !agg.Settlements[1].Superseded || agg.Settlements[2].Superseded {
		t.Fatal("only the last version must be active")
	}
	// 首版原始信息保留；v2 价 9；当前 v3 价 8。
	if agg.Settlements[0].Entries[0].EffectiveUnitPrice.String() != "10" ||
		agg.Settlements[1].Entries[0].EffectiveUnitPrice.String() != "9" ||
		agg.Settlements[2].Entries[0].EffectiveUnitPrice.String() != "8" {
		t.Fatal("version chain prices altered")
	}
	c1, _ := svc.GetCorrection(ctx, a.ID, "COR-1")
	c2, _ := svc.GetCorrection(ctx, a.ID, "COR-2")
	if c1.BaseSettleVersion != 1 || c1.EffectiveVersion != 2 ||
		c2.BaseSettleVersion != 2 || c2.EffectiveVersion != 3 {
		t.Fatalf("chained base/effective versions: c1=%d/%d c2=%d/%d",
			c1.BaseSettleVersion, c1.EffectiveVersion, c2.BaseSettleVersion, c2.EffectiveVersion)
	}
	// 旧版本仍然可以作为更正的最终结算追溯到。
	tr1 := mustTrace(t, svc, a.ID, "COR-1")
	if tr1.Items[0].FinalSettlement.EffectiveUnitPrice.String() != "9" {
		t.Fatalf("old correction final settlement overwritten: %+v", tr1.Items[0].FinalSettlement)
	}
	if tr1.Items[0].CurrentSettlement == nil ||
		tr1.Items[0].CurrentSettlement.EffectiveUnitPrice.String() != "8" {
		t.Fatalf("old trace must point at current v3: %+v", tr1.Items[0].CurrentSettlement)
	}
}

// TestCorrectionRace_ConcurrentApproveAndConfirm 在 -race 下验证
// 批准与成交确认并发时不产生数据竞争，且每个订单只有一套最终结果。
func TestCorrectionRace_ConcurrentApproveAndConfirm(t *testing.T) {
	for trial := 0; trial < 8; trial++ {
		svc, a := settledCorrectionFixture(t, nil)
		ctx := context.Background()
		if _, err := svc.SubmitCorrection(ctx, a.ID, correctionParams("RACE-1",
			CorrectionItem{ExternalRef: "ORD-A", TargetUnitPrice: MustParseMoney("9")})); err != nil {
			t.Fatal(err)
		}
		const n = 12
		var wg sync.WaitGroup
		wg.Add(n * 2)
		for i := 0; i < n; i++ {
			go func() { defer wg.Done(); _, _ = svc.ConfirmFill(ctx, a.ID, "ORD-A") }()
			go func() {
				defer wg.Done()
				_, _, _ = svc.ApproveCorrection(ctx, a.ID, "RACE-1", ApproveCorrectionParams{})
			}()
		}
		wg.Wait()
		st, err := svc.GetSettlement(ctx, a.ID)
		if err != nil {
			t.Fatal(err)
		}
		if st.Superseded {
			t.Fatalf("trial %d: multiple effective versions", trial)
		}
		app, _ := svc.GetCorrection(ctx, a.ID, "RACE-1")
		if app.Status != CorrectionCompleted {
			t.Fatalf("trial %d: status %s", trial, app.Status)
		}
	}
}
