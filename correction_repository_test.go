package auctionclearing

import (
	"context"
	"testing"
	"time"
)

func TestFileRepository_CorrectionPersistAndReload(t *testing.T) {
	dir := t.TempDir()
	clock := newFakeClock()
	repo, err := NewFileRepository(dir)
	if err != nil {
		t.Fatal(err)
	}
	svc := NewService(repo, clock)
	a, err := svc.CreateAuction(context.Background(), CreateAuctionParams{
		AvailableQty: 10, Deadline: clock.Now().Add(time.Hour), ReservePrice: MustParseMoney("5"),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := svc.PlaceBid(ctx, a.ID, bidParams("ORD-A", 3, "10")); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.PlaceBid(ctx, a.ID, bidParams("ORD-B", 2, "8")); err != nil {
		t.Fatal(err)
	}
	clock.Advance(2 * time.Hour)
	if _, err := svc.ClearAuction(ctx, a.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SettleAuction(ctx, a.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ConfirmFill(ctx, a.ID, "ORD-A"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.MarkDelivered(ctx, a.ID, "ORD-A", 3); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SubmitCorrection(ctx, a.ID, correctionParams("COR-1",
		CorrectionItem{ExternalRef: "ORD-A", TargetUnitPrice: MustParseMoney("9")},
		CorrectionItem{ExternalRef: "ORD-B", TargetUnitPrice: MustParseMoney("7")})); err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.ApproveCorrection(ctx, a.ID, "COR-1", ApproveCorrectionParams{}); err != nil {
		t.Fatal(err)
	}

	// 重启恢复：结算版本链、更正申请、补偿单与审计事件必须完整。
	reloaded, err := LoadFileRepository(dir)
	if err != nil {
		t.Fatal(err)
	}
	svc2 := NewService(reloaded, clock)
	st, err := svc2.GetSettlement(ctx, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if st.Version != 2 || st.Superseded {
		t.Fatalf("reloaded current settlement: %+v", st)
	}
	app, err := svc2.GetCorrection(ctx, a.ID, "COR-1")
	if err != nil {
		t.Fatal(err)
	}
	if app.Status != CorrectionCompleted || app.BaseSettleVersion != 1 || app.EffectiveVersion != 2 {
		t.Fatalf("reloaded correction: %+v", app)
	}
	if len(app.Snapshots) != 2 || len(app.Compensations) != 1 || len(app.Events) != 1 {
		t.Fatalf("reloaded correction parts: snaps=%d comps=%d events=%d",
			len(app.Snapshots), len(app.Compensations), len(app.Events))
	}
	if app.Compensations[0].Amount.String() != "3" || app.Compensations[0].SettledAt == nil {
		t.Fatalf("reloaded compensation: %+v", app.Compensations[0])
	}
	// 原成交（清算结果）仍按原版本保留。
	res, _ := svc2.GetResult(ctx, a.ID)
	if res.Fills[0].UnitPrice.String() != "10" {
		t.Fatal("reloaded clearing result altered")
	}
	// 链路追溯在重启后仍可组装。
	trace, err := svc2.GetCorrectionTrace(ctx, a.ID, "COR-1")
	if err != nil {
		t.Fatal(err)
	}
	if trace.Items[0].Scenario != ScenarioDelivered || trace.Items[1].Scenario != ScenarioUndelivered {
		t.Fatalf("reloaded trace scenarios: %s / %s",
			trace.Items[0].Scenario, trace.Items[1].Scenario)
	}

	// 恢复后重复批准同一更正是幂等的，不会再产生新版本。
	app2, replayed, err := svc2.ApproveCorrection(ctx, a.ID, "COR-1", ApproveCorrectionParams{})
	if err != nil || !replayed || app2.EffectiveVersion != 2 {
		t.Fatalf("post-reload approve: %+v replay=%v %v", app2, replayed, err)
	}
}

// TestFileRepository_PendingCompensationPersists 验证补偿失败停在
// pending 的状态也被完整持久化，重启后可继续重试。
func TestFileRepository_PendingCompensationPersists(t *testing.T) {
	dir := t.TempDir()
	clock := newFakeClock()
	gw := &failingGateway{fail: true}
	repo, err := NewFileRepository(dir)
	if err != nil {
		t.Fatal(err)
	}
	svc := NewService(repo, clock, WithCompensationGateway(gw))
	a, err := svc.CreateAuction(context.Background(), CreateAuctionParams{
		AvailableQty: 5, Deadline: clock.Now().Add(time.Hour), ReservePrice: MustParseMoney("1"),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := svc.PlaceBid(ctx, a.ID, bidParams("ORD-A", 3, "10")); err != nil {
		t.Fatal(err)
	}
	clock.Advance(2 * time.Hour)
	if _, err := svc.ClearAuction(ctx, a.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SettleAuction(ctx, a.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.MarkDelivered(ctx, a.ID, "ORD-A", 3); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SubmitCorrection(ctx, a.ID, correctionParams("COR-F",
		CorrectionItem{ExternalRef: "ORD-A", TargetUnitPrice: MustParseMoney("9")})); err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.ApproveCorrection(ctx, a.ID, "COR-F", ApproveCorrectionParams{}); err != nil {
		t.Fatal(err)
	}

	reloaded, err := LoadFileRepository(dir)
	if err != nil {
		t.Fatal(err)
	}
	gw.setFail(false)
	svc2 := NewService(reloaded, clock, WithCompensationGateway(gw))
	app, err := svc2.GetCorrection(ctx, a.ID, "COR-F")
	if err != nil {
		t.Fatal(err)
	}
	if app.Status != CorrectionPending || app.Compensations[0].Status != CompensationPending {
		t.Fatalf("pending state not persisted: %+v", app)
	}
	app2, err := svc2.RetryCompensation(ctx, a.ID, "COR-F")
	if err != nil || app2.Status != CorrectionCompleted {
		t.Fatalf("retry after reload: %+v %v", app2, err)
	}
}
