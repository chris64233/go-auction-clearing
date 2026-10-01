package auctionclearing

import (
	"path/filepath"
	"testing"
	"time"
)

// TestFileEventStore_CorrectionPersistAndReload 验证结算/交割/更正（含
// pending 补偿中间态）随事件日志完整落盘，重启重放后状态一致、可继续重试。
func TestFileEventStore_CorrectionPersistAndReload(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")

	gw := newFailingGateway(true)
	store, err := NewFileEventStore(path)
	if err != nil {
		t.Fatal(err)
	}
	clk := NewControlledClock(time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC))
	e, err := NewEngine(store, clk, WithCompensationGateway(gw))
	if err != nil {
		t.Fatal(err)
	}
	a, err := e.CreateAuction(10, clk.Now().Add(time.Hour), MustParseMoney("5"))
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range []BidInput{
		{AuctionID: a.ID, ExternalID: "b1", Bidder: "A", Qty: 3, Price: MustParseMoney("10")},
		{AuctionID: a.ID, ExternalID: "b2", Bidder: "B", Qty: 5, Price: MustParseMoney("9")},
		{AuctionID: a.ID, ExternalID: "b3", Bidder: "C", Qty: 4, Price: MustParseMoney("8")},
	} {
		clk.Advance(time.Second)
		if _, err := e.SubmitBid(b); err != nil {
			t.Fatal(err)
		}
	}
	clk.Advance(2 * time.Hour)
	if _, err := e.SettleAuction(a.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.GenerateSettlement(a.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.ConfirmFill(a.ID, "b1"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.MarkDelivered(a.ID, "b1", 2); err != nil {
		t.Fatal(err)
	}
	res, err := e.SubmitCorrection(a.ID, correctionInput("COR-FILE",
		correctionItem("b1", "6"), correctionItem("b2", "5")))
	if err != nil || res.Outcome != CorrectionAccepted {
		t.Fatalf("submit: %+v %v", res, err)
	}
	appr, err := e.ApproveCorrection(a.ID, "COR-FILE", ApproveCorrectionInput{})
	if err != nil || appr.Data.Status != CorrectionPending {
		t.Fatalf("approve: %+v %v", appr, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	// 重启：从 JSONL 重放，pending 中间态必须完整恢复。
	store2, err := NewFileEventStore(path)
	if err != nil {
		t.Fatal(err)
	}
	gw.setFail(false)
	e2, err := NewEngine(store2, clk, WithCompensationGateway(gw))
	if err != nil {
		t.Fatal(err)
	}
	app, err := e2.GetCorrection(a.ID, "COR-FILE")
	if err != nil {
		t.Fatal(err)
	}
	if app.Status != CorrectionPending || len(app.Compensations) != 1 ||
		app.Compensations[0].Status != CompensationPending {
		t.Fatalf("reloaded pending correction: %+v", app)
	}
	rec, err := e2.GetSettlementRecord(a.ID)
	if err != nil || rec.Version != 1 {
		t.Fatalf("reloaded settlement: %+v %v", rec, err)
	}
	// 恢复后重试成功，版本前进到 v2。
	app2, err := e2.RetryCompensation(a.ID, "COR-FILE")
	if err != nil || app2.Status != CorrectionCompleted || app2.EffectiveVersion != 2 {
		t.Fatalf("retry after reload: %+v %v", app2, err)
	}
	trace, err := e2.GetCorrectionTrace(a.ID, "COR-FILE")
	if err != nil {
		t.Fatal(err)
	}
	if trace.Items[0].Scenario != ScenarioPartial || trace.Items[1].Scenario != ScenarioUndelivered {
		t.Fatalf("trace scenarios after reload: %s %s",
			trace.Items[0].Scenario, trace.Items[1].Scenario)
	}
	if err := store2.Close(); err != nil {
		t.Fatal(err)
	}
}
