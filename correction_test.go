package auctionclearing

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// failingGateway 是可切换成败的补偿结算通道，用于模拟补偿结算失败。
type failingGateway struct {
	mu    sync.Mutex
	fail  bool
	calls int
}

func (g *failingGateway) SettleCompensation(context.Context, string, string, Money) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.calls++
	if g.fail {
		return errors.New("compensation channel unavailable")
	}
	return nil
}

func (g *failingGateway) setFail(v bool) {
	g.mu.Lock()
	g.fail = v
	g.mu.Unlock()
}

// settledCorrectionFixture 构造一场已清算并已生成结算的拍卖，
// 含两笔成交：ORD-A（3 @ 10）与 ORD-B（2 @ 8）。
func settledCorrectionFixture(t *testing.T, gateway CompensationGateway) (*Service, *Auction) {
	t.Helper()
	clock := newFakeClock()
	var opts []ServiceOption
	if gateway != nil {
		opts = append(opts, WithCompensationGateway(gateway))
	}
	svc := NewService(NewMemoryRepository(), clock, opts...)
	a := createTestAuctionDeadline(t, svc, time.Hour)
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
	return svc, a
}

func correctionParams(id string, items ...CorrectionItem) SubmitCorrectionParams {
	return SubmitCorrectionParams{
		CorrectionID: id,
		Reason:       "price feed published an erroneous fill price",
		Basis:        "feed-v2-reconciliation-2026-09-30",
		Handler:      "risk-officer-1",
		Items:        items,
	}
}

func TestSettleAuction_IdempotentAndRequiresClearing(t *testing.T) {
	svc := NewService(NewMemoryRepository(), newFakeClock())
	a := createTestAuction(t, svc)
	ctx := context.Background()

	if _, err := svc.SettleAuction(ctx, a.ID); ErrorKindOf(err) != KindInvalidState {
		t.Fatalf("settle before clear: kind=%v want invalid_state", ErrorKindOf(err))
	}

	clock := svc.clock.(*fakeClock)
	clock.Advance(2 * time.Hour)
	if _, err := svc.ClearAuction(ctx, a.ID); err != nil {
		t.Fatal(err)
	}
	out1, err := svc.SettleAuction(ctx, a.ID)
	if err != nil || out1.Replayed {
		t.Fatalf("first settle: %+v %v", out1, err)
	}
	if out1.Settlement.Version != InitialSettlementVersion || out1.Settlement.PriceVersion != InitialPriceVersion {
		t.Fatalf("initial version: settle=%d price=%d", out1.Settlement.Version, out1.Settlement.PriceVersion)
	}
	out2, err := svc.SettleAuction(ctx, a.ID)
	if err != nil || !out2.Replayed {
		t.Fatalf("second settle must replay same result: %+v %v", out2, err)
	}
	if out2.Settlement.Version != out1.Settlement.Version ||
		out2.Settlement.TotalSettled.Cmp(out1.Settlement.TotalSettled) != 0 {
		t.Fatalf("replayed settlement content differs: %+v vs %+v", out1.Settlement, out2.Settlement)
	}
}

func TestConfirmFreezeDelivered_LifecycleAndOrdering(t *testing.T) {
	svc, a := settledCorrectionFixture(t, nil)
	ctx := context.Background()

	// 未确认先冻结：状态错误。
	if _, err := svc.FreezeFunds(ctx, a.ID, "ORD-A"); ErrorKindOf(err) != KindInvalidState {
		t.Fatalf("freeze before confirm: kind=%v", ErrorKindOf(err))
	}
	e, err := svc.ConfirmFill(ctx, a.ID, "ORD-A")
	if err != nil || !e.Confirmed || e.Status() != EntryConfirmed {
		t.Fatalf("confirm: %+v %v", e, err)
	}
	// 重复确认幂等。
	if e2, err := svc.ConfirmFill(ctx, a.ID, "ORD-A"); err != nil || !e2.Confirmed {
		t.Fatalf("re-confirm: %+v %v", e2, err)
	}
	e, err = svc.FreezeFunds(ctx, a.ID, "ORD-A")
	if err != nil || !e.FundsFrozen || e.Status() != EntryFrozen {
		t.Fatalf("freeze: %+v %v", e, err)
	}
	// 超量交割被拒。
	if _, err := svc.MarkDelivered(ctx, a.ID, "ORD-A", 99); ErrorKindOf(err) != KindInvalidState {
		t.Fatalf("over-deliver: kind=%v", ErrorKindOf(err))
	}
	// 部分交割 1 件 @10，锁定金额 10；尚未全量交割。
	if e, err = svc.MarkDelivered(ctx, a.ID, "ORD-A", 1); err != nil {
		t.Fatal(err)
	}
	if e.DeliveredQty != 1 || e.DeliveredAmount.String() != "10" || e.Status() == EntryDelivered {
		t.Fatalf("partial delivery: qty=%d amount=%s status=%s", e.DeliveredQty, e.DeliveredAmount, e.Status())
	}
	// 剩余 2 件交割后进入 delivered。
	if e, err = svc.MarkDelivered(ctx, a.ID, "ORD-A", 2); err != nil {
		t.Fatal(err)
	}
	if e.Status() != EntryDelivered || e.DeliveredAt == nil {
		t.Fatalf("full delivery status: %s deliveredAt=%v", e.Status(), e.DeliveredAt)
	}
}

func TestSubmitCorrection_SavesSnapshotsAndRestrictsVersion(t *testing.T) {
	svc, a := settledCorrectionFixture(t, nil)
	ctx := context.Background()

	out, err := svc.SubmitCorrection(ctx, a.ID, correctionParams("COR-1",
		CorrectionItem{ExternalRef: "ORD-A", TargetUnitPrice: MustParseMoney("9")}))
	if err != nil || out.Replayed {
		t.Fatalf("submit: %+v %v", out, err)
	}
	app := out.Application
	if app.BatchID != a.ID || app.PriceVersion != InitialPriceVersion ||
		app.BaseSettleVersion != InitialSettlementVersion || app.Status != CorrectionSubmitted {
		t.Fatalf("application header: %+v", app)
	}
	if len(app.Snapshots) != 1 {
		t.Fatalf("want 1 snapshot, got %d", len(app.Snapshots))
	}
	snap := app.Snapshots[0]
	if snap.ExternalRef != "ORD-A" || snap.MatchedQty != 3 || snap.DeliveredQty != 0 ||
		snap.SettlementState != EntryPending || snap.OriginalPrice.String() != "10" {
		t.Fatalf("snapshot mismatch: %+v", snap)
	}

	// 依据是强制项（价格不变时也不允许为空）。
	bad := correctionParams("COR-X", CorrectionItem{ExternalRef: "ORD-A", TargetUnitPrice: MustParseMoney("9")})
	bad.Basis = ""
	if _, err := svc.SubmitCorrection(ctx, a.ID, bad); ErrorKindOf(err) != KindInvalidArgument {
		t.Fatalf("empty basis: kind=%v", ErrorKindOf(err))
	}
	// 引用不存在的成交：not_found。
	bad = correctionParams("COR-Y", CorrectionItem{ExternalRef: "GHOST", TargetUnitPrice: MustParseMoney("9")})
	if _, err := svc.SubmitCorrection(ctx, a.ID, bad); ErrorKindOf(err) != KindNotFound {
		t.Fatalf("missing fill: kind=%v", ErrorKindOf(err))
	}
	// 同一申请内重复成交号：invalid_argument。
	bad = correctionParams("COR-Z",
		CorrectionItem{ExternalRef: "ORD-A", TargetUnitPrice: MustParseMoney("9")},
		CorrectionItem{ExternalRef: "ORD-A", TargetUnitPrice: MustParseMoney("8")})
	if _, err := svc.SubmitCorrection(ctx, a.ID, bad); ErrorKindOf(err) != KindInvalidArgument {
		t.Fatalf("duplicate item: kind=%v", ErrorKindOf(err))
	}
}

func TestSubmitCorrection_IdempotentReplayAndConflict(t *testing.T) {
	svc, a := settledCorrectionFixture(t, nil)
	ctx := context.Background()
	p := correctionParams("COR-1", CorrectionItem{ExternalRef: "ORD-A", TargetUnitPrice: MustParseMoney("9")})

	out1, err := svc.SubmitCorrection(ctx, a.ID, p)
	if err != nil {
		t.Fatal(err)
	}
	firstAt := out1.Application.SubmittedAt

	svc.clock.(*fakeClock).Advance(time.Minute)
	out2, err := svc.SubmitCorrection(ctx, a.ID, p)
	if err != nil || !out2.Replayed || !out2.Application.SubmittedAt.Equal(firstAt) {
		t.Fatalf("replay: %+v %v", out2, err)
	}

	// 同号但目标价变化：冲突。
	p2 := p
	p2.Items = []CorrectionItem{{ExternalRef: "ORD-A", TargetUnitPrice: MustParseMoney("8")}}
	if _, err := svc.SubmitCorrection(ctx, a.ID, p2); ErrorKindOf(err) != KindConflict {
		t.Fatalf("changed price: kind=%v want conflict", ErrorKindOf(err))
	}
	// 同号但原因变化：冲突。
	p3 := p
	p3.Reason = "another reason"
	if _, err := svc.SubmitCorrection(ctx, a.ID, p3); ErrorKindOf(err) != KindConflict {
		t.Fatalf("changed reason: kind=%v want conflict", ErrorKindOf(err))
	}
	// 冲突提交不得污染原申请。
	got, _ := svc.GetCorrection(ctx, a.ID, "COR-1")
	if got.Items[0].TargetUnitPrice.String() != "9" {
		t.Fatal("original application altered by conflicting submit")
	}

	// 先批准 COR-1 使结算版本前进；同号申请挂接的基准版本已变，必须冲突。
	if _, _, err := svc.ApproveCorrection(ctx, a.ID, "COR-1", ApproveCorrectionParams{}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SubmitCorrection(ctx, a.ID, p); ErrorKindOf(err) != KindConflict {
		t.Fatalf("same id after version advanced: kind=%v want conflict", ErrorKindOf(err))
	}
}

func TestApproveCorrection_UndeliveredCorrectsOnlySettlementFields(t *testing.T) {
	svc, a := settledCorrectionFixture(t, nil)
	ctx := context.Background()
	p := correctionParams("COR-1",
		CorrectionItem{ExternalRef: "ORD-A", TargetUnitPrice: MustParseMoney("9")})
	if _, err := svc.SubmitCorrection(ctx, a.ID, p); err != nil {
		t.Fatal(err)
	}
	app, replayed, err := svc.ApproveCorrection(ctx, a.ID, "COR-1", ApproveCorrectionParams{})
	if err != nil || replayed || app.Status != CorrectionCompleted {
		t.Fatalf("approve: %+v replayed=%v %v", app, replayed, err)
	}
	if app.EffectiveVersion != 2 || len(app.Compensations) != 0 {
		t.Fatalf("version/comp: version=%d comps=%d", app.EffectiveVersion, len(app.Compensations))
	}

	st, err := svc.GetSettlement(ctx, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if st.Version != 2 || st.Superseded {
		t.Fatalf("current settlement must be v2 and active: %+v", st)
	}
	eA := entryByRef(st, "ORD-A")
	if eA.EffectiveUnitPrice.String() != "9" || eA.SettledAmount.String() != "27" {
		t.Fatalf("corrected entry: price=%s amount=%s", eA.EffectiveUnitPrice, eA.SettledAmount)
	}
	// 原始撮合信息必须按原版本保留。
	if eA.OriginalUnitPrice.String() != "10" || eA.MatchedQty != 3 || eA.PriceVersion != InitialPriceVersion {
		t.Fatalf("original match info altered: %+v", eA)
	}
	// 未被更正的成交保持不变。
	eB := entryByRef(st, "ORD-B")
	if eB.EffectiveUnitPrice.String() != "8" || eB.SettledAmount.String() != "16" {
		t.Fatalf("untouched fill changed: price=%s amount=%s", eB.EffectiveUnitPrice, eB.SettledAmount)
	}
	if st.TotalSettled.String() != "43" { // 27 + 16
		t.Fatalf("total settled = %s, want 43", st.TotalSettled)
	}

	// 不可变清算结果仍是原价。
	res, _ := svc.GetResult(ctx, a.ID)
	if res.Fills[0].UnitPrice.String() != "10" {
		t.Fatal("clearing result must remain immutable")
	}

	// 重复批准幂等返回原结果，不再产生新版本。
	app2, replay2, err := svc.ApproveCorrection(ctx, a.ID, "COR-1", ApproveCorrectionParams{})
	if err != nil || !replay2 || app2.EffectiveVersion != 2 {
		t.Fatalf("re-approve: %+v replay=%v %v", app2, replay2, err)
	}
}

func TestApproveCorrection_DeliveredCreatesCompensationOnly(t *testing.T) {
	svc, a := settledCorrectionFixture(t, nil)
	ctx := context.Background()
	// 成交确认 → 冻结 → 全量交割 ORD-A 3@10。
	if _, err := svc.ConfirmFill(ctx, a.ID, "ORD-A"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.FreezeFunds(ctx, a.ID, "ORD-A"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.MarkDelivered(ctx, a.ID, "ORD-A", 3); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SubmitCorrection(ctx, a.ID, correctionParams("COR-D",
		CorrectionItem{ExternalRef: "ORD-A", TargetUnitPrice: MustParseMoney("9")})); err != nil {
		t.Fatal(err)
	}
	app, _, err := svc.ApproveCorrection(ctx, a.ID, "COR-D", ApproveCorrectionParams{})
	if err != nil {
		t.Fatal(err)
	}
	if app.Status != CorrectionCompleted || len(app.Compensations) != 1 {
		t.Fatalf("status=%s comps=%d", app.Status, len(app.Compensations))
	}
	c := app.Compensations[0]
	// 原结算锁定 30 - 新价 27 = 补偿 3。
	if c.Quantity != 3 || c.LockedAmount.String() != "30" ||
		c.TargetPrice.String() != "9" || c.Amount.String() != "3" || c.Status != CompensationSettled {
		t.Fatalf("compensation mismatch: %+v", c)
	}

	st, _ := svc.GetSettlement(ctx, a.ID)
	e := entryByRef(st, "ORD-A")
	// 已交割原成交的结算单价与金额不变，交割事实保留。
	if e.EffectiveUnitPrice.String() != "10" || e.SettledAmount.String() != "30" ||
		e.DeliveredQty != 3 || e.Status() != EntryDelivered {
		t.Fatalf("delivered fill must stay intact: %+v", e)
	}
	// 版本仍前进（记录更正生效），但旧版本可查。
	if st.Version != 2 || st.Superseded {
		t.Fatalf("current version = %d superseded=%v", st.Version, st.Superseded)
	}
}

func TestApproveCorrection_PartialCompensation(t *testing.T) {
	svc, a := settledCorrectionFixture(t, nil)
	ctx := context.Background()
	// 先确认/冻结并交割 2 件（锁定 20），剩余 1 件未交割。
	if _, err := svc.ConfirmFill(ctx, a.ID, "ORD-A"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.FreezeFunds(ctx, a.ID, "ORD-A"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.MarkDelivered(ctx, a.ID, "ORD-A", 2); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SubmitCorrection(ctx, a.ID, correctionParams("COR-P",
		CorrectionItem{ExternalRef: "ORD-A", TargetUnitPrice: MustParseMoney("9")})); err != nil {
		t.Fatal(err)
	}
	app, _, err := svc.ApproveCorrection(ctx, a.ID, "COR-P", ApproveCorrectionParams{})
	if err != nil {
		t.Fatal(err)
	}
	if app.Status != CorrectionCompleted {
		t.Fatalf("status=%s", app.Status)
	}
	// 补偿只覆盖已交割的 2 件：20 - 2*9 = 2。
	if len(app.Compensations) != 1 {
		t.Fatalf("comps=%d", len(app.Compensations))
	}
	c := app.Compensations[0]
	if c.Quantity != 2 || c.Amount.String() != "2" {
		t.Fatalf("partial compensation: qty=%d amount=%s", c.Quantity, c.Amount)
	}
	st, _ := svc.GetSettlement(ctx, a.ID)
	e := entryByRef(st, "ORD-A")
	// 最终结算：已交割 20 + 未交割 1*9 = 29；交割数量仍为 2。
	if e.EffectiveUnitPrice.String() != "9" || e.SettledAmount.String() != "29" ||
		e.DeliveredQty != 2 || e.DeliveredAmount.String() != "20" {
		t.Fatalf("partial entry: %+v", e)
	}
	if e.Status() == EntryDelivered {
		t.Fatal("partially delivered fill must not flip to delivered")
	}
}

func TestApproveCorrection_NoPriceChangeStillRecorded(t *testing.T) {
	svc, a := settledCorrectionFixture(t, nil)
	ctx := context.Background()
	// 目标价与当前有效单价完全相同。
	if _, err := svc.SubmitCorrection(ctx, a.ID, correctionParams("COR-N",
		CorrectionItem{ExternalRef: "ORD-A", TargetUnitPrice: MustParseMoney("10")})); err != nil {
		t.Fatal(err)
	}
	app, _, err := svc.ApproveCorrection(ctx, a.ID, "COR-N", ApproveCorrectionParams{})
	if err != nil {
		t.Fatal(err)
	}
	if app.Status != CorrectionCompleted {
		t.Fatalf("no-change correction must be completed, got %s", app.Status)
	}
	// 不产生新版本，但必须留下依据与处理人的审计痕迹。
	if app.EffectiveVersion != InitialSettlementVersion {
		t.Fatalf("no-change must not bump version, got %d", app.EffectiveVersion)
	}
	if len(app.Events) != 1 {
		t.Fatalf("want 1 audit event, got %d", len(app.Events))
	}
	ev := app.Events[0]
	if ev.Changed || ev.Handler != "risk-officer-1" || ev.Basis == "" ||
		ev.FromVersion != ev.ToVersion {
		t.Fatalf("no-change audit event mismatch: %+v", ev)
	}
	// 原结算继续可用且仍是唯一版本。
	st, _ := svc.GetSettlement(ctx, a.ID)
	if st.Version != 1 || st.Superseded {
		t.Fatalf("settlement must stay at v1: version=%d", st.Version)
	}

	// 批准人/依据也可在批准时显式提供并记录。
	if _, err := svc.SubmitCorrection(ctx, a.ID, correctionParams("COR-N2",
		CorrectionItem{ExternalRef: "ORD-B", TargetUnitPrice: MustParseMoney("8")})); err != nil {
		t.Fatal(err)
	}
	app2, _, err := svc.ApproveCorrection(ctx, a.ID, "COR-N2",
		ApproveCorrectionParams{Handler: "manager-7", Basis: "manual-review-42"})
	if err != nil {
		t.Fatal(err)
	}
	if app2.Events[0].Handler != "manager-7" || app2.Events[0].Basis != "manual-review-42" {
		t.Fatalf("approve-time handler/basis not recorded: %+v", app2.Events[0])
	}
}

func TestApproveCorrection_StaleVersionConflicts(t *testing.T) {
	svc, a := settledCorrectionFixture(t, nil)
	ctx := context.Background()
	// COR-1 先申请（挂在 v1），COR-2 先批准使结算前进到 v2。
	if _, err := svc.SubmitCorrection(ctx, a.ID, correctionParams("COR-1",
		CorrectionItem{ExternalRef: "ORD-A", TargetUnitPrice: MustParseMoney("9")})); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SubmitCorrection(ctx, a.ID, correctionParams("COR-2",
		CorrectionItem{ExternalRef: "ORD-B", TargetUnitPrice: MustParseMoney("7")})); err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.ApproveCorrection(ctx, a.ID, "COR-2", ApproveCorrectionParams{}); err != nil {
		t.Fatal(err)
	}
	// COR-1 的基准版本已被取代：冲突，且申请仍停留在 submitted 可重新申请。
	if _, _, err := svc.ApproveCorrection(ctx, a.ID, "COR-1", ApproveCorrectionParams{}); ErrorKindOf(err) != KindConflict {
		t.Fatalf("stale approve: kind=%v want conflict", ErrorKindOf(err))
	}
	got, _ := svc.GetCorrection(ctx, a.ID, "COR-1")
	if got.Status != CorrectionSubmitted {
		t.Fatalf("stale application must remain submitted, got %s", got.Status)
	}
}

func TestApproveCorrection_CompensationFailureStaysPending(t *testing.T) {
	gw := &failingGateway{fail: true}
	svc, a := settledCorrectionFixture(t, gw)
	ctx := context.Background()
	if _, err := svc.ConfirmFill(ctx, a.ID, "ORD-A"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.FreezeFunds(ctx, a.ID, "ORD-A"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.MarkDelivered(ctx, a.ID, "ORD-A", 3); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SubmitCorrection(ctx, a.ID, correctionParams("COR-F",
		CorrectionItem{ExternalRef: "ORD-A", TargetUnitPrice: MustParseMoney("9")})); err != nil {
		t.Fatal(err)
	}
	app, _, err := svc.ApproveCorrection(ctx, a.ID, "COR-F", ApproveCorrectionParams{})
	if err != nil {
		t.Fatalf("approve must not hard-fail when compensation channel fails: %v", err)
	}
	// 更正停在待处理，补偿单 pending 且带失败原因。
	if app.Status != CorrectionPending || len(app.Compensations) != 1 {
		t.Fatalf("status=%s comps=%d", app.Status, len(app.Compensations))
	}
	if app.Compensations[0].Status != CompensationPending || app.Compensations[0].FailureNote == "" {
		t.Fatalf("compensation must be pending with failure note: %+v", app.Compensations[0])
	}
	// 原成交仍可查询且原结算内容完好。
	st, err := svc.GetSettlement(ctx, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if st.Version != 1 {
		t.Fatalf("compensation failure must not switch settlement version, got %d", st.Version)
	}
	e := entryByRef(st, "ORD-A")
	if e.EffectiveUnitPrice.String() != "10" || e.SettledAmount.String() != "30" || e.DeliveredQty != 3 {
		t.Fatalf("original fill must remain queryable and intact: %+v", e)
	}
	res, _ := svc.GetResult(ctx, a.ID)
	if res.TotalProceeds.String() != "46" { // 30 + 16
		t.Fatalf("clearing proceeds changed: %s", res.TotalProceeds)
	}
	// 链路追溯同样不能伪装完成：状态 pending、最终结算仍挂在 v1 原价。
	trace, err := svc.GetCorrectionTrace(ctx, a.ID, "COR-F")
	if err != nil {
		t.Fatal(err)
	}
	if trace.Status != CorrectionPending || trace.EffectiveVersion != 0 || trace.CurrentVersion != 1 {
		t.Fatalf("pending trace header: status=%s effective=%d current=%d",
			trace.Status, trace.EffectiveVersion, trace.CurrentVersion)
	}
	if trace.Items[0].Scenario != ScenarioDelivered ||
		trace.Items[0].Compensation == nil ||
		trace.Items[0].Compensation.Status != CompensationPending ||
		trace.Items[0].FinalSettlement.EffectiveUnitPrice.String() != "10" {
		t.Fatalf("pending trace item must expose pending compensation: %+v", trace.Items[0])
	}

	// 通道恢复后重试：补偿结清，更正转为 completed。
	gw.setFail(false)
	app2, err := svc.RetryCompensation(ctx, a.ID, "COR-F")
	if err != nil {
		t.Fatal(err)
	}
	if app2.Status != CorrectionCompleted || app2.Compensations[0].Status != CompensationSettled ||
		app2.Compensations[0].SettledAt == nil {
		t.Fatalf("after retry: %+v", app2)
	}
	st2, _ := svc.GetSettlement(ctx, a.ID)
	if st2.Version != 2 {
		t.Fatalf("successful retry must switch to v2, got %d", st2.Version)
	}

	// 通道仍失败时对 completed 更正再重试属于状态错误。
	if _, err := svc.RetryCompensation(ctx, a.ID, "COR-F"); ErrorKindOf(err) != KindInvalidState {
		t.Fatalf("retry completed: kind=%v", ErrorKindOf(err))
	}
}

func TestCorrectionTrace_ThreeScenariosDiffer(t *testing.T) {
	svc, a := settledCorrectionFixture(t, nil)
	ctx := context.Background()

	// 未交割：ORD-B 全部未交割。
	submitAndApprove(t, svc, a.ID, "TRACE-U",
		CorrectionItem{ExternalRef: "ORD-B", TargetUnitPrice: MustParseMoney("7")})
	// 已交割：ORD-A 先全量交割再更正。
	if _, err := svc.ConfirmFill(ctx, a.ID, "ORD-A"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.MarkDelivered(ctx, a.ID, "ORD-A", 3); err != nil {
		t.Fatal(err)
	}
	submitAndApprove(t, svc, a.ID, "TRACE-D",
		CorrectionItem{ExternalRef: "ORD-A", TargetUnitPrice: MustParseMoney("9")})

	traceU := mustTrace(t, svc, a.ID, "TRACE-U")
	if traceU.Status != CorrectionCompleted || traceU.Reason == "" || traceU.Basis == "" || traceU.Handler == "" {
		t.Fatalf("trace header: %+v", traceU)
	}
	u := traceU.Items[0]
	if u.Scenario != ScenarioUndelivered || u.Compensation != nil {
		t.Fatalf("undelivered scenario: %+v", u)
	}
	if u.Original.UnitPrice.String() != "8" || u.Original.Quantity != 2 {
		t.Fatalf("original fill view: %+v", u.Original)
	}
	if u.SubmitSnapshot.SettlementState != EntryPending {
		t.Fatalf("snapshot state: %s", u.SubmitSnapshot.SettlementState)
	}
	if u.FinalSettlement.EffectiveUnitPrice.String() != "7" || u.FinalSettlement.SettledAmount.String() != "14" {
		t.Fatalf("undelivered final settlement: %+v", u.FinalSettlement)
	}

	traceD := mustTrace(t, svc, a.ID, "TRACE-D")
	d := traceD.Items[0]
	if d.Scenario != ScenarioDelivered || d.Compensation == nil {
		t.Fatalf("delivered scenario must carry compensation: %+v", d)
	}
	if d.Compensation.Amount.String() != "3" || d.Compensation.Status != CompensationSettled {
		t.Fatalf("delivered compensation: %+v", d.Compensation)
	}
	if d.FinalSettlement.EffectiveUnitPrice.String() != "10" || d.FinalSettlement.DeliveredQty != 3 {
		t.Fatalf("delivered final settlement keeps original: %+v", d.FinalSettlement)
	}

	// 部分补偿：需要一笔 3 件成交中只交割 1 件。新建一场拍卖构造。
	svc2, a2 := settledCorrectionFixture(t, nil)
	ctx2 := context.Background()
	if _, err := svc2.MarkDelivered(ctx2, a2.ID, "ORD-A", 1); err != nil {
		t.Fatal(err)
	}
	submitAndApprove(t, svc2, a2.ID, "TRACE-P",
		CorrectionItem{ExternalRef: "ORD-A", TargetUnitPrice: MustParseMoney("9")})
	traceP := mustTrace(t, svc2, a2.ID, "TRACE-P")
	p := traceP.Items[0]
	if p.Scenario != ScenarioPartial {
		t.Fatalf("want partial_compensation, got %s", p.Scenario)
	}
	if p.Compensation == nil || p.Compensation.Quantity != 1 || p.Compensation.Amount.String() != "1" {
		t.Fatalf("partial compensation view: %+v", p.Compensation)
	}
	if p.FinalSettlement.DeliveredQty != 1 || p.FinalSettlement.SettledAmount.String() != "28" {
		t.Fatalf("partial final settlement (10+2*9=28): %+v", p.FinalSettlement)
	}
}

func submitAndApprove(t *testing.T, svc *Service, auctionID, corrID string, items ...CorrectionItem) {
	t.Helper()
	p := correctionParams(corrID, items...)
	if _, err := svc.SubmitCorrection(context.Background(), auctionID, p); err != nil {
		t.Fatalf("submit %s: %v", corrID, err)
	}
	app, _, err := svc.ApproveCorrection(context.Background(), auctionID, corrID, ApproveCorrectionParams{})
	if err != nil {
		t.Fatalf("approve %s: %v", corrID, err)
	}
	if app.Status != CorrectionCompleted {
		t.Fatalf("approve %s status = %s", corrID, app.Status)
	}
}

func mustTrace(t *testing.T, svc *Service, auctionID, corrID string) *CorrectionTrace {
	t.Helper()
	trace, err := svc.GetCorrectionTrace(context.Background(), auctionID, corrID)
	if err != nil {
		t.Fatalf("trace %s: %v", corrID, err)
	}
	return trace
}

// TestConcurrentConfirmFreezeApprove_SingleEffectiveResult 验证成交确认、
// 资金冻结与更正批准同时到达时，同一订单经过批次锁串行化后只保留一套
// 自洽的有效成交结果，且任何中间状态都不会被读到。
func TestConcurrentConfirmFreezeApprove_SingleEffectiveResult(t *testing.T) {
	for trial := 0; trial < 20; trial++ {
		svc, a := settledCorrectionFixture(t, nil)
		ctx := context.Background()
		if _, err := svc.SubmitCorrection(ctx, a.ID, correctionParams("COR-C",
			CorrectionItem{ExternalRef: "ORD-A", TargetUnitPrice: MustParseMoney("9")})); err != nil {
			t.Fatal(err)
		}

		var wg sync.WaitGroup
		errs := make(chan error, 3)
		wg.Add(3)
		go func() { defer wg.Done(); _, err := svc.ConfirmFill(ctx, a.ID, "ORD-A"); errs <- err }()
		go func() {
			defer wg.Done()
			// 冻结可能因确认尚未提交而失败；失败是合法结局，不算错误。
			_, err := svc.FreezeFunds(ctx, a.ID, "ORD-A")
			if err != nil && ErrorKindOf(err) != KindInvalidState {
				errs <- err
				return
			}
			errs <- nil
		}()
		go func() {
			defer wg.Done()
			_, _, err := svc.ApproveCorrection(ctx, a.ID, "COR-C", ApproveCorrectionParams{})
			errs <- err
		}()
		wg.Wait()
		close(errs)
		for err := range errs {
			if err != nil {
				t.Fatalf("trial %d concurrent op: %v", trial, err)
			}
		}

		// 收敛后：只有一个当前有效结算版本；条目要么在 v1 要么在 v2，
		// 但同一订单不会同时存在两套可写结果。
		st, err := svc.GetSettlement(ctx, a.ID)
		if err != nil {
			t.Fatal(err)
		}
		if st.Superseded {
			t.Fatalf("trial %d: current settlement flagged superseded", trial)
		}
		app, _ := svc.GetCorrection(ctx, a.ID, "COR-C")
		if app.Status != CorrectionCompleted {
			t.Fatalf("trial %d: correction status = %s", trial, app.Status)
		}
		// 若批准先生效则当前为 v2；确认标志必须体现在当前版本上（不存在
		// “v1 已确认、v2 却未确认”的双结果分裂）。
		if e := entryByRef(st, "ORD-A"); st.Version == 2 && !e.Confirmed {
			// 批准先于确认：确认发生在 v2（当前版本），应已体现。
			// 若确认发生在 v1 而批准在其后，确认会随版本继承——两种顺序
			// 都要求当前版本的确认标志与最终操作一致。
			t.Fatalf("trial %d: confirm lost across versions: %+v", trial, e)
		}
	}
}
