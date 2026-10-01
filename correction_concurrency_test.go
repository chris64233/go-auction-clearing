package auctionclearing

import (
	"sync"
	"testing"
)

// 成交确认/资金冻结/交割与更正批准同时到达时，一个订单只能保留一套有效结果：
// 批准两阶段窗口内到达的流水动作等待其落定，然后作用在新版本上。
func TestConcurrentConfirmFreezeApprove_SingleEffectiveResult(t *testing.T) {
	for trial := 0; trial < 8; trial++ {
		gw := newFailingGateway(false)
		gw.armBlock()
		e, _, id := correctionFixture(t, gw)
		// b1 交割 2 件，批准改价会产生一笔补偿，在锁外通道窗口阻塞；
		// 窗口内对未交割的 b2 发起确认/冻结。
		if _, err := e.ConfirmFill(id, "b1"); err != nil {
			t.Fatal(err)
		}
		if _, err := e.MarkDelivered(id, "b1", 2); err != nil {
			t.Fatal(err)
		}
		submitCorrectionFor(t, e, id, "RACE-1", correctionItem("b1", "6"), correctionItem("b2", "6"))

		var wg sync.WaitGroup
		wg.Add(3)
		go func() {
			defer wg.Done()
			_, _ = e.ApproveCorrection(id, "RACE-1", ApproveCorrectionInput{})
		}()
		// 等批准进入锁外补偿窗口后再发起确认/冻结。
		gw.waitEntered()
		go func() {
			defer wg.Done()
			_, _ = e.ConfirmFill(id, "b1")
		}()
		go func() {
			defer wg.Done()
			// 冻结可能先于确认到达：它等待批准窗口结束，而确认与冻结的
			// 相对顺序仍不保证，因此若返回 ErrNotConfirmed 就补一次确认后再冻结，
			// 最终断言只要求两个动作都恰好生效一次、结果唯一。
			if _, err := e.FreezeFunds(id, "b1"); err != nil {
				_, _ = e.ConfirmFill(id, "b1")
				_, _ = e.FreezeFunds(id, "b1")
			}
		}()
		// 给两个 goroutine 一点时间进入等待。
		// 释放补偿通道，批准落定。
		gw.release()
		wg.Wait()

		rec, err := e.GetSettlementRecord(id)
		if err != nil {
			t.Fatal(err)
		}
		if rec.Superseded || rec.Version != 2 {
			t.Fatalf("trial %d: not exactly one effective version: %+v", trial, rec)
		}
		b1 := entryByExternalID(&rec, "b1")
		if !b1.Confirmed || !b1.FundsFrozen {
			t.Fatalf("trial %d: confirm/freeze lost during approval window: %+v", trial, b1)
		}
		b2 := entryByExternalID(&rec, "b2")
		if b2.EffectiveUnitPrice.String() != "6" || b2.SettledAmount.String() != "30" {
			t.Fatalf("trial %d: b2 correction = %+v", trial, b2)
		}
		app, err := e.GetCorrection(id, "RACE-1")
		if err != nil || app.Status != CorrectionCompleted {
			t.Fatalf("trial %d: correction = %+v %v", trial, app, err)
		}
	}
}

// 交割与批准并发：批准窗口内发起的交割等待新版本生效后，按新单价锁定金额。
func TestConcurrentDeliveryApprove_LocksAtNewPrice(t *testing.T) {
	gw := newFailingGateway(false)
	gw.armBlock()
	e, _, id := correctionFixture(t, gw)
	// b1 先确认并交割 1 件（按 8 锁定）；批准把单价改成 6（已交割 1 件走补偿），
	// 窗口内交割剩余 2 件，落定后必须按新价 6 锁定。
	if _, err := e.ConfirmFill(id, "b1"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.MarkDelivered(id, "b1", 1); err != nil {
		t.Fatal(err)
	}
	submitCorrectionFor(t, e, id, "RACE-2", correctionItem("b1", "6"))

	var wg sync.WaitGroup
	wg.Add(2)
	var delivered SettlementEntry
	go func() {
		defer wg.Done()
		_, _ = e.ApproveCorrection(id, "RACE-2", ApproveCorrectionInput{})
	}()
	gw.waitEntered()
	go func() {
		defer wg.Done()
		delivered, _ = e.MarkDelivered(id, "b1", 2)
	}()
	gw.release()
	wg.Wait()

	if delivered.DeliveredQty != 3 {
		t.Fatalf("delivery lost: %+v", delivered)
	}
	rec, err := e.GetSettlementRecord(id)
	if err != nil {
		t.Fatal(err)
	}
	en := entryByExternalID(&rec, "b1")
	if en.EffectiveUnitPrice.String() != "6" || en.DeliveredAmount.String() != "20" { // 1*8 + 2*6
		t.Fatalf("delivery must lock remainder at new price 6: %+v", en)
	}
}

// 批准进行中提交的新更正等待窗口结束，随后按新版本判定为过期冲突。
func TestConcurrentSubmitDuringApprove_VersionBound(t *testing.T) {
	gw := newFailingGateway(false)
	gw.armBlock()
	e, _, id := correctionFixture(t, gw)
	if _, err := e.ConfirmFill(id, "b1"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.MarkDelivered(id, "b1", 1); err != nil {
		t.Fatal(err)
	}
	submitCorrectionFor(t, e, id, "RACE-3", correctionItem("b1", "6"))

	var wg sync.WaitGroup
	wg.Add(2)
	var submitErr error
	go func() {
		defer wg.Done()
		_, _ = e.ApproveCorrection(id, "RACE-3", ApproveCorrectionInput{})
	}()
	gw.waitEntered()
	go func() {
		defer wg.Done()
		_, submitErr = e.SubmitCorrection(id, correctionInput("RACE-4", correctionItem("b2", "5")))
	}()
	gw.release()
	wg.Wait()

	// RACE-4 在 v2 上登记成功（基准版本为 2），批准它应当产生 v3。
	if submitErr != nil {
		t.Fatalf("submit during window should wait then succeed on new version: %v", submitErr)
	}
	app, err := e.GetCorrection(id, "RACE-4")
	if err != nil || app.BaseSettleVersion != 2 {
		t.Fatalf("RACE-4 base version = %+v %v", app, err)
	}
	approveCorrectionFor(t, e, id, "RACE-4")
	rec, _ := e.GetSettlementRecord(id)
	if rec.Version != 3 {
		t.Fatalf("version = %d, want 3", rec.Version)
	}
}

// 事件溯源：从同一份事件日志构建新引擎，结算/更正/补偿/pending 状态完整恢复。
func TestCorrectionEventSourcing_ReplayRestoresState(t *testing.T) {
	gw := newFailingGateway(true)
	e, _, id := correctionFixture(t, gw)
	if _, err := e.ConfirmFill(id, "b1"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.MarkDelivered(id, "b1", 3); err != nil {
		t.Fatal(err)
	}
	submitCorrectionFor(t, e, id, "COR-REPLAY",
		correctionItem("b1", "6"), correctionItem("b2", "5"))
	if _, err := e.ApproveCorrection(id, "COR-REPLAY", ApproveCorrectionInput{}); err != nil {
		t.Fatal(err)
	}

	store := e.store
	replayed, err := NewEngine(store, SystemClock{}, WithCompensationGateway(newFailingGateway(false)))
	if err != nil {
		t.Fatal(err)
	}
	app, err := replayed.GetCorrection(id, "COR-REPLAY")
	if err != nil {
		t.Fatal(err)
	}
	if app.Status != CorrectionPending || len(app.Compensations) != 1 ||
		app.Compensations[0].Status != CompensationPending {
		t.Fatalf("replayed pending correction: %+v", app)
	}
	rec, err := replayed.GetSettlementRecord(id)
	if err != nil || rec.Version != 1 {
		t.Fatalf("replayed settlement must remain v1: %+v %v", rec, err)
	}
	if en := entryByExternalID(&rec, "b1"); en.DeliveredQty != 3 || en.DeliveredAmount.String() != "24" {
		t.Fatalf("replayed delivery state lost: %+v", en)
	}
	// 恢复后重试补偿成功。
	app2, err := replayed.RetryCompensation(id, "COR-REPLAY")
	if err != nil {
		t.Fatal(err)
	}
	if app2.Status != CorrectionCompleted || app2.EffectiveVersion != 2 {
		t.Fatalf("replayed retry: %+v", app2)
	}

	// 再构建一次引擎，完成态也必须完整恢复。
	replayed2, err := NewEngine(store, SystemClock{}, WithCompensationGateway(newFailingGateway(false)))
	if err != nil {
		t.Fatal(err)
	}
	rec2, err := replayed2.GetSettlementRecord(id)
	if err != nil || rec2.Version != 2 || rec2.Superseded {
		t.Fatalf("final replay: %+v %v", rec2, err)
	}
	if len(rec2.Entries) != 3 {
		t.Fatalf("entries lost on replay: %d", len(rec2.Entries))
	}
}
