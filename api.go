package auctionclearing

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"
)

// Server 把 Engine 暴露为 HTTP 服务。路由：
//
//	POST   /v1/auctions                             创建拍卖
//	GET    /v1/auctions/{id}                        查询拍卖
//	POST   /v1/auctions/{id}/bids                   提交报价（幂等）
//	GET    /v1/auctions/{id}/bids                   列出全部报价
//	POST   /v1/auctions/{id}/bids/{ext}/withdraw    撤回报价（幂等）
//	POST   /v1/auctions/{id}/settle                 触发清算（幂等）
//	GET    /v1/auctions/{id}/settlement             查询成交结果
//
// 结算流水与成交更正：
//
//	POST   /v1/auctions/{id}/settlements            清算后生成首版结算（幂等）
//	GET    /v1/auctions/{id}/settlements            查询当前有效结算版本
//	POST   /v1/auctions/{id}/fills/{ext}/confirm    成交确认（幂等）
//	POST   /v1/auctions/{id}/fills/{ext}/freeze     资金冻结（须先确认，幂等）
//	POST   /v1/auctions/{id}/fills/{ext}/deliver    增量交割
//	POST   /v1/auctions/{id}/corrections            提交更正申请（幂等）
//	GET    /v1/auctions/{id}/corrections            列出本批次全部更正
//	GET    /v1/auctions/{id}/corrections/{cid}      查询单张更正
//	GET    /v1/auctions/{id}/corrections/{cid}/trace 更正链路追溯
//	POST   /v1/auctions/{id}/corrections/{cid}/approve           批准更正（幂等）
//	POST   /v1/auctions/{id}/corrections/{cid}/retry-compensation 重试待处理补偿
//
// 所有金额字段均为十进制字符串（如 "10.5000"），时间字段为 RFC3339。
type Server struct {
	engine *Engine
}

// NewServer 构造 HTTP 服务。
func NewServer(engine *Engine) *Server {
	return &Server{engine: engine}
}

// Handler 注册全部路由，返回可挂载的 http.Handler。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/auctions", s.createAuction)
	mux.HandleFunc("GET /v1/auctions/{id}", s.getAuction)
	mux.HandleFunc("POST /v1/auctions/{id}/bids", s.submitBid)
	mux.HandleFunc("GET /v1/auctions/{id}/bids", s.listBids)
	mux.HandleFunc("POST /v1/auctions/{id}/bids/{ext}/withdraw", s.withdrawBid)
	mux.HandleFunc("POST /v1/auctions/{id}/settle", s.settle)
	mux.HandleFunc("GET /v1/auctions/{id}/settlement", s.getSettlement)
	mux.HandleFunc("POST /v1/auctions/{id}/settlements", s.generateSettlement)
	mux.HandleFunc("GET /v1/auctions/{id}/settlements", s.getSettlementRecord)
	mux.HandleFunc("POST /v1/auctions/{id}/fills/{ext}/confirm", s.confirmFill)
	mux.HandleFunc("POST /v1/auctions/{id}/fills/{ext}/freeze", s.freezeFunds)
	mux.HandleFunc("POST /v1/auctions/{id}/fills/{ext}/deliver", s.markDelivered)
	mux.HandleFunc("POST /v1/auctions/{id}/corrections", s.submitCorrection)
	mux.HandleFunc("GET /v1/auctions/{id}/corrections", s.listCorrections)
	mux.HandleFunc("GET /v1/auctions/{id}/corrections/{cid}", s.getCorrection)
	mux.HandleFunc("GET /v1/auctions/{id}/corrections/{cid}/trace", s.getCorrectionTrace)
	mux.HandleFunc("POST /v1/auctions/{id}/corrections/{cid}/approve", s.approveCorrection)
	mux.HandleFunc("POST /v1/auctions/{id}/corrections/{cid}/retry-compensation", s.retryCompensation)
	return mux
}

// —— 请求/响应 DTO ——

type createAuctionRequest struct {
	AvailableQty int64  `json:"available_qty"`
	Deadline     string `json:"deadline"`
	ReservePrice string `json:"reserve_price"`
}

type auctionResponse struct {
	ID           int64  `json:"id"`
	AvailableQty int64  `json:"available_qty"`
	Deadline     string `json:"deadline"`
	ReservePrice string `json:"reserve_price"`
	Status       string `json:"status"`
	CreatedAt    string `json:"created_at"`
}

type submitBidRequest struct {
	ExternalID string `json:"external_id"`
	Bidder     string `json:"bidder"`
	Qty        int64  `json:"qty"`
	Price      string `json:"price"`
}

type bidResponse struct {
	ExternalID  string `json:"external_id"`
	Bidder      string `json:"bidder"`
	Qty         int64  `json:"qty"`
	Price       string `json:"price"`
	SubmittedAt string `json:"submitted_at"`
	UpdatedAt   string `json:"updated_at"`
	Status      string `json:"status"`
}

type submitBidResponse struct {
	Outcome string      `json:"outcome"`
	Bid     bidResponse `json:"bid"`
}

type withdrawResponse struct {
	Outcome string      `json:"outcome"`
	Bid     bidResponse `json:"bid"`
}

type fillResponse struct {
	ExternalID   string `json:"external_id"`
	Bidder       string `json:"bidder"`
	Price        string `json:"price"`
	RequestedQty int64  `json:"requested_qty"`
	FilledQty    int64  `json:"filled_qty"`
	Rank         int    `json:"rank"`
}

type settlementResponse struct {
	ClearingPrice string         `json:"clearing_price"`
	SoldQty       int64          `json:"sold_qty"`
	TotalProceeds string         `json:"total_proceeds"`
	Fills         []fillResponse `json:"fills"`
	SettledAt     string         `json:"settled_at"`
}

type settleResponse struct {
	Outcome string             `json:"outcome"`
	Data    settlementResponse `json:"data"`
}

// —— 结算流水 / 更正 DTO ——

type settlementEntryResponse struct {
	ExternalID         string  `json:"external_id"`
	Bidder             string  `json:"bidder"`
	Rank               int     `json:"rank"`
	MatchedQty         int64   `json:"matched_qty"`
	OriginalUnitPrice  string  `json:"original_unit_price"`
	PriceVersion       int64   `json:"price_version"`
	EffectiveUnitPrice string  `json:"effective_unit_price"`
	SettledAmount      string  `json:"settled_amount"`
	Confirmed          bool    `json:"confirmed"`
	FundsFrozen        bool    `json:"funds_frozen"`
	DeliveredQty       int64   `json:"delivered_qty"`
	DeliveredAmount    string  `json:"delivered_amount"`
	DeliveredAt        *string `json:"delivered_at,omitempty"`
	Status             string  `json:"status"`
}

type settlementRecordResponse struct {
	PriceVersion int64                     `json:"price_version"`
	Version      int64                     `json:"version"`
	CreatedAt    string                    `json:"created_at"`
	Superseded   bool                      `json:"superseded"`
	Entries      []settlementEntryResponse `json:"entries"`
	TotalSettled string                    `json:"total_settled"`
}

type generateSettlementResponse struct {
	Outcome string                   `json:"outcome"`
	Data    settlementRecordResponse `json:"data"`
}

type deliverRequest struct {
	Quantity int64 `json:"quantity"`
}

type correctionItemRequest struct {
	ExternalID      string `json:"external_id"`
	TargetUnitPrice string `json:"target_unit_price"`
}

type submitCorrectionRequest struct {
	CorrectionID string                  `json:"correction_id"`
	Reason       string                  `json:"reason"`
	Basis        string                  `json:"basis"`
	Handler      string                  `json:"handler"`
	Items        []correctionItemRequest `json:"items"`
}

type correctionItemResponse struct {
	ExternalID      string `json:"external_id"`
	TargetUnitPrice string `json:"target_unit_price"`
}

type correctionSnapshotResponse struct {
	ExternalID      string `json:"external_id"`
	Rank            int    `json:"rank"`
	MatchedQty      int64  `json:"matched_qty"`
	DeliveredQty    int64  `json:"delivered_qty"`
	OriginalPrice   string `json:"original_price"`
	PriceVersion    int64  `json:"price_version"`
	SettlementState string `json:"settlement_state"`
}

type compensationResponse struct {
	ExternalID   string  `json:"external_id"`
	Rank         int     `json:"rank"`
	Quantity     int64   `json:"quantity"`
	LockedAmount string  `json:"locked_amount"`
	TargetPrice  string  `json:"target_price"`
	Amount       string  `json:"amount"`
	Status       string  `json:"status"`
	CreatedAt    string  `json:"created_at"`
	SettledAt    *string `json:"settled_at,omitempty"`
	FailureNote  string  `json:"failure_note,omitempty"`
}

type correctionEventResponse struct {
	At          string `json:"at"`
	Handler     string `json:"handler"`
	Basis       string `json:"basis"`
	Changed     bool   `json:"changed"`
	FromVersion int64  `json:"from_version"`
	ToVersion   int64  `json:"to_version"`
	Note        string `json:"note,omitempty"`
}

type correctionResponse struct {
	CorrectionID     string                       `json:"correction_id"`
	BatchID          int64                        `json:"batch_id"`
	Reason           string                       `json:"reason"`
	Basis            string                       `json:"basis"`
	Handler          string                       `json:"handler"`
	PriceVersion     int64                        `json:"price_version"`
	BaseVersion      int64                        `json:"base_settlement_version"`
	Items            []correctionItemResponse     `json:"items"`
	Snapshots        []correctionSnapshotResponse `json:"snapshots"`
	Status           string                       `json:"status"`
	SubmittedAt      string                       `json:"submitted_at"`
	ApprovedAt       *string                      `json:"approved_at,omitempty"`
	EffectiveVersion int64                        `json:"effective_version"`
	Compensations    []compensationResponse       `json:"compensations,omitempty"`
	Events           []correctionEventResponse    `json:"events,omitempty"`
}

type submitCorrectionResponseDTO struct {
	Outcome string             `json:"outcome"`
	Data    correctionResponse `json:"data"`
}

type approveCorrectionRequest struct {
	Handler string `json:"handler"`
	Basis   string `json:"basis"`
}

type originalFillResponse struct {
	ExternalID string `json:"external_id"`
	Bidder     string `json:"bidder"`
	Rank       int    `json:"rank"`
	Price      string `json:"price"`
	Quantity   int64  `json:"quantity"`
	Amount     string `json:"amount"`
}

type correctionTraceItemResponse struct {
	ExternalID        string                     `json:"external_id"`
	Scenario          string                     `json:"scenario"`
	TargetUnitPrice   string                     `json:"target_unit_price"`
	Original          originalFillResponse       `json:"original"`
	SubmitSnapshot    correctionSnapshotResponse `json:"submit_snapshot"`
	Compensation      *compensationResponse      `json:"compensation,omitempty"`
	FinalSettlement   *settlementEntryResponse   `json:"final_settlement"`
	CurrentSettlement *settlementEntryResponse   `json:"current_settlement,omitempty"`
}

type correctionTraceResponse struct {
	CorrectionID     string                        `json:"correction_id"`
	BatchID          int64                         `json:"batch_id"`
	Reason           string                        `json:"reason"`
	Basis            string                        `json:"basis"`
	Handler          string                        `json:"handler"`
	PriceVersion     int64                         `json:"price_version"`
	BaseVersion      int64                         `json:"base_settlement_version"`
	EffectiveVersion int64                         `json:"effective_version"`
	CurrentVersion   int64                         `json:"current_settlement_version"`
	Status           string                        `json:"status"`
	SubmittedAt      string                        `json:"submitted_at"`
	ApprovedAt       *string                       `json:"approved_at,omitempty"`
	Events           []correctionEventResponse     `json:"events"`
	Items            []correctionTraceItemResponse `json:"items"`
}

type errorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// —— handler ——

func (s *Server) createAuction(w http.ResponseWriter, r *http.Request) {
	var req createAuctionRequest
	if !decode(w, r, &req) {
		return
	}
	reserve, err := ParseMoney(req.ReservePrice)
	if err != nil {
		writeError(w, err)
		return
	}
	deadline, err := time.Parse(time.RFC3339, req.Deadline)
	if err != nil {
		writeError(w, ErrZeroDeadline)
		return
	}
	a, err := s.engine.CreateAuction(req.AvailableQty, deadline, reserve)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, toAuctionResponse(a))
}

func (s *Server) getAuction(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(w, r)
	if !ok {
		return
	}
	a, err := s.engine.GetAuction(id)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toAuctionResponse(a))
}

func (s *Server) submitBid(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(w, r)
	if !ok {
		return
	}
	var req submitBidRequest
	if !decode(w, r, &req) {
		return
	}
	price, err := ParseMoney(req.Price)
	if err != nil {
		writeError(w, err)
		return
	}
	res, err := s.engine.SubmitBid(BidInput{
		AuctionID:  id,
		ExternalID: req.ExternalID,
		Bidder:     req.Bidder,
		Qty:        req.Qty,
		Price:      price,
	})
	if err != nil {
		writeError(w, err)
		return
	}
	status := http.StatusCreated
	if res.Outcome == BidReplayed {
		status = http.StatusOK
	}
	writeJSON(w, status, submitBidResponse{Outcome: string(res.Outcome), Bid: toBidResponse(res.Bid)})
}

func (s *Server) listBids(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(w, r)
	if !ok {
		return
	}
	bids, err := s.engine.ListBids(id)
	if err != nil {
		writeError(w, err)
		return
	}
	out := make([]bidResponse, 0, len(bids))
	for _, b := range bids {
		out = append(out, toBidResponse(b))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) withdrawBid(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(w, r)
	if !ok {
		return
	}
	ext := r.PathValue("ext")
	res, err := s.engine.WithdrawBid(id, ext)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, withdrawResponse{Outcome: string(res.Outcome), Bid: toBidResponse(res.Bid)})
}

func (s *Server) settle(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(w, r)
	if !ok {
		return
	}
	res, err := s.engine.SettleAuction(id)
	if err != nil {
		writeError(w, err)
		return
	}
	status := http.StatusOK
	if res.Outcome == SettleSettled {
		status = http.StatusCreated
	}
	writeJSON(w, status, settleResponse{Outcome: string(res.Outcome), Data: toSettlementResponse(res.Data)})
}

func (s *Server) getSettlement(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(w, r)
	if !ok {
		return
	}
	st, err := s.engine.GetSettlement(id)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toSettlementResponse(st))
}

func (s *Server) generateSettlement(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(w, r)
	if !ok {
		return
	}
	res, err := s.engine.GenerateSettlement(id)
	if err != nil {
		writeError(w, err)
		return
	}
	status := http.StatusCreated
	if res.Outcome == SettlementReplayed {
		status = http.StatusOK
	}
	writeJSON(w, status, generateSettlementResponse{Outcome: string(res.Outcome), Data: toSettlementRecordResponse(res.Data)})
}

func (s *Server) getSettlementRecord(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(w, r)
	if !ok {
		return
	}
	rec, err := s.engine.GetSettlementRecord(id)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toSettlementRecordResponse(rec))
}

func (s *Server) confirmFill(w http.ResponseWriter, r *http.Request) {
	id, ext, ok := parseIDAndExt(w, r)
	if !ok {
		return
	}
	en, err := s.engine.ConfirmFill(id, ext)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toSettlementEntryResponse(en))
}

func (s *Server) freezeFunds(w http.ResponseWriter, r *http.Request) {
	id, ext, ok := parseIDAndExt(w, r)
	if !ok {
		return
	}
	en, err := s.engine.FreezeFunds(id, ext)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toSettlementEntryResponse(en))
}

func (s *Server) markDelivered(w http.ResponseWriter, r *http.Request) {
	id, ext, ok := parseIDAndExt(w, r)
	if !ok {
		return
	}
	var req deliverRequest
	if !decode(w, r, &req) {
		return
	}
	en, err := s.engine.MarkDelivered(id, ext, req.Quantity)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toSettlementEntryResponse(en))
}

func (s *Server) submitCorrection(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(w, r)
	if !ok {
		return
	}
	var req submitCorrectionRequest
	if !decode(w, r, &req) {
		return
	}
	items := make([]CorrectionItem, 0, len(req.Items))
	for _, it := range req.Items {
		price, err := ParseMoney(it.TargetUnitPrice)
		if err != nil {
			writeError(w, err)
			return
		}
		items = append(items, CorrectionItem{ExternalID: it.ExternalID, TargetUnitPrice: price})
	}
	res, err := s.engine.SubmitCorrection(id, CorrectionInput{
		CorrectionID: req.CorrectionID,
		Reason:       req.Reason,
		Basis:        req.Basis,
		Handler:      req.Handler,
		Items:        items,
	})
	if err != nil {
		writeError(w, err)
		return
	}
	status := http.StatusCreated
	if res.Outcome == CorrectionReplayed {
		status = http.StatusOK
	}
	writeJSON(w, status, submitCorrectionResponseDTO{Outcome: string(res.Outcome), Data: toCorrectionResponse(res.Data)})
}

func (s *Server) listCorrections(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(w, r)
	if !ok {
		return
	}
	apps, err := s.engine.ListCorrections(id)
	if err != nil {
		writeError(w, err)
		return
	}
	out := make([]correctionResponse, 0, len(apps))
	for _, a := range apps {
		out = append(out, toCorrectionResponse(a))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) getCorrection(w http.ResponseWriter, r *http.Request) {
	id, cid, ok := parseIDAndCorrection(w, r)
	if !ok {
		return
	}
	app, err := s.engine.GetCorrection(id, cid)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toCorrectionResponse(app))
}

func (s *Server) getCorrectionTrace(w http.ResponseWriter, r *http.Request) {
	id, cid, ok := parseIDAndCorrection(w, r)
	if !ok {
		return
	}
	trace, err := s.engine.GetCorrectionTrace(id, cid)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toCorrectionTraceResponse(trace))
}

func (s *Server) approveCorrection(w http.ResponseWriter, r *http.Request) {
	id, cid, ok := parseIDAndCorrection(w, r)
	if !ok {
		return
	}
	var req approveCorrectionRequest
	if r.ContentLength != 0 {
		if !decode(w, r, &req) {
			return
		}
	}
	res, err := s.engine.ApproveCorrection(id, cid, ApproveCorrectionInput{Handler: req.Handler, Basis: req.Basis})
	if err != nil {
		writeError(w, err)
		return
	}
	status := http.StatusCreated
	if res.Outcome == CorrectionReplayed {
		status = http.StatusOK
	}
	writeJSON(w, status, submitCorrectionResponseDTO{Outcome: string(res.Outcome), Data: toCorrectionResponse(res.Data)})
}

func (s *Server) retryCompensation(w http.ResponseWriter, r *http.Request) {
	id, cid, ok := parseIDAndCorrection(w, r)
	if !ok {
		return
	}
	app, err := s.engine.RetryCompensation(id, cid)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toCorrectionResponse(app))
}

// —— 映射与工具 ——

func toAuctionResponse(a Auction) auctionResponse {
	return auctionResponse{
		ID:           a.ID,
		AvailableQty: a.AvailableQty,
		Deadline:     a.Deadline.UTC().Format(time.RFC3339Nano),
		ReservePrice: a.ReservePrice.String(),
		Status:       string(a.Status),
		CreatedAt:    a.CreatedAt.UTC().Format(time.RFC3339Nano),
	}
}

func toBidResponse(b Bid) bidResponse {
	return bidResponse{
		ExternalID:  b.ExternalID,
		Bidder:      b.Bidder,
		Qty:         b.Qty,
		Price:       b.Price.String(),
		SubmittedAt: b.SubmittedAt.UTC().Format(time.RFC3339Nano),
		UpdatedAt:   b.UpdatedAt.UTC().Format(time.RFC3339Nano),
		Status:      string(b.Status),
	}
}

func toSettlementResponse(st Settlement) settlementResponse {
	fills := make([]fillResponse, 0, len(st.Fills))
	for _, f := range st.Fills {
		fills = append(fills, fillResponse{
			ExternalID:   f.ExternalID,
			Bidder:       f.Bidder,
			Price:        f.Price.String(),
			RequestedQty: f.RequestedQty,
			FilledQty:    f.FilledQty,
			Rank:         f.Rank,
		})
	}
	return settlementResponse{
		ClearingPrice: st.ClearingPrice.String(),
		SoldQty:       st.SoldQty,
		TotalProceeds: st.TotalProceeds.String(),
		Fills:         fills,
		SettledAt:     st.SettledAt.UTC().Format(time.RFC3339Nano),
	}
}

func parseID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeJSON(w, http.StatusBadRequest, errorBody{Code: "invalid_argument", Message: "invalid auction id"})
		return 0, false
	}
	return id, true
}

func parseIDAndExt(w http.ResponseWriter, r *http.Request) (int64, string, bool) {
	id, ok := parseID(w, r)
	if !ok {
		return 0, "", false
	}
	return id, r.PathValue("ext"), true
}

func parseIDAndCorrection(w http.ResponseWriter, r *http.Request) (int64, string, bool) {
	id, ok := parseID(w, r)
	if !ok {
		return 0, "", false
	}
	cid := r.PathValue("cid")
	if cid == "" {
		writeJSON(w, http.StatusBadRequest, errorBody{Code: "invalid_argument", Message: "invalid correction id"})
		return 0, "", false
	}
	return id, cid, true
}

func timePtrString(t *time.Time) *string {
	if t == nil {
		return nil
	}
	s := t.UTC().Format(time.RFC3339Nano)
	return &s
}

func toSettlementEntryResponse(en SettlementEntry) settlementEntryResponse {
	return settlementEntryResponse{
		ExternalID:         en.ExternalID,
		Bidder:             en.Bidder,
		Rank:               en.Rank,
		MatchedQty:         en.MatchedQty,
		OriginalUnitPrice:  en.OriginalUnitPrice.String(),
		PriceVersion:       int64(en.PriceVersion),
		EffectiveUnitPrice: en.EffectiveUnitPrice.String(),
		SettledAmount:      en.SettledAmount.String(),
		Confirmed:          en.Confirmed,
		FundsFrozen:        en.FundsFrozen,
		DeliveredQty:       en.DeliveredQty,
		DeliveredAmount:    en.DeliveredAmount.String(),
		DeliveredAt:        timePtrString(en.DeliveredAt),
		Status:             string(en.Status()),
	}
}

func toSettlementRecordResponse(rec SettlementRecord) settlementRecordResponse {
	entries := make([]settlementEntryResponse, 0, len(rec.Entries))
	for _, en := range rec.Entries {
		entries = append(entries, toSettlementEntryResponse(en))
	}
	return settlementRecordResponse{
		PriceVersion: int64(rec.PriceVersion),
		Version:      int64(rec.Version),
		CreatedAt:    rec.CreatedAt.UTC().Format(time.RFC3339Nano),
		Superseded:   rec.Superseded,
		Entries:      entries,
		TotalSettled: rec.TotalSettled.String(),
	}
}

func toCorrectionResponse(app CorrectionApplication) correctionResponse {
	items := make([]correctionItemResponse, 0, len(app.Items))
	for _, it := range app.Items {
		items = append(items, correctionItemResponse{ExternalID: it.ExternalID, TargetUnitPrice: it.TargetUnitPrice.String()})
	}
	snaps := make([]correctionSnapshotResponse, 0, len(app.Snapshots))
	for _, s := range app.Snapshots {
		snaps = append(snaps, correctionSnapshotResponse{
			ExternalID:      s.ExternalID,
			Rank:            s.Rank,
			MatchedQty:      s.MatchedQty,
			DeliveredQty:    s.DeliveredQty,
			OriginalPrice:   s.OriginalPrice.String(),
			PriceVersion:    int64(s.PriceVersion),
			SettlementState: string(s.SettlementState),
		})
	}
	comps := make([]compensationResponse, 0, len(app.Compensations))
	for _, c := range app.Compensations {
		comps = append(comps, compensationResponse{
			ExternalID:   c.ExternalID,
			Rank:         c.Rank,
			Quantity:     c.Quantity,
			LockedAmount: c.LockedAmount.String(),
			TargetPrice:  c.TargetPrice.String(),
			Amount:       c.Amount.String(),
			Status:       string(c.Status),
			CreatedAt:    c.CreatedAt.UTC().Format(time.RFC3339Nano),
			SettledAt:    timePtrString(c.SettledAt),
			FailureNote:  c.FailureNote,
		})
	}
	events := make([]correctionEventResponse, 0, len(app.Events))
	for _, ev := range app.Events {
		events = append(events, correctionEventResponse{
			At:          ev.At.UTC().Format(time.RFC3339Nano),
			Handler:     ev.Handler,
			Basis:       ev.Basis,
			Changed:     ev.Changed,
			FromVersion: int64(ev.FromVersion),
			ToVersion:   int64(ev.ToVersion),
			Note:        ev.Note,
		})
	}
	return correctionResponse{
		CorrectionID:     app.CorrectionID,
		BatchID:          app.BatchID,
		Reason:           app.Reason,
		Basis:            app.Basis,
		Handler:          app.Handler,
		PriceVersion:     int64(app.PriceVersion),
		BaseVersion:      int64(app.BaseSettleVersion),
		Items:            items,
		Snapshots:        snaps,
		Status:           string(app.Status),
		SubmittedAt:      app.SubmittedAt.UTC().Format(time.RFC3339Nano),
		ApprovedAt:       timePtrString(app.ApprovedAt),
		EffectiveVersion: int64(app.EffectiveVersion),
		Compensations:    comps,
		Events:           events,
	}
}

func toCorrectionTraceResponse(trace CorrectionTrace) correctionTraceResponse {
	events := make([]correctionEventResponse, 0, len(trace.Events))
	for _, ev := range trace.Events {
		events = append(events, correctionEventResponse{
			At:          ev.At.UTC().Format(time.RFC3339Nano),
			Handler:     ev.Handler,
			Basis:       ev.Basis,
			Changed:     ev.Changed,
			FromVersion: int64(ev.FromVersion),
			ToVersion:   int64(ev.ToVersion),
			Note:        ev.Note,
		})
	}
	items := make([]correctionTraceItemResponse, 0, len(trace.Items))
	for _, it := range trace.Items {
		out := correctionTraceItemResponse{
			ExternalID:      it.ExternalID,
			Scenario:        string(it.Scenario),
			TargetUnitPrice: it.TargetUnitPrice.String(),
			Original: originalFillResponse{
				ExternalID: it.Original.ExternalID,
				Bidder:     it.Original.Bidder,
				Rank:       it.Original.Rank,
				Price:      it.Original.Price.String(),
				Quantity:   it.Original.Quantity,
				Amount:     it.Original.Amount.String(),
			},
			SubmitSnapshot: correctionSnapshotResponse{
				ExternalID:      it.SubmitSnapshot.ExternalID,
				Rank:            it.SubmitSnapshot.Rank,
				MatchedQty:      it.SubmitSnapshot.MatchedQty,
				DeliveredQty:    it.SubmitSnapshot.DeliveredQty,
				OriginalPrice:   it.SubmitSnapshot.OriginalPrice.String(),
				PriceVersion:    int64(it.SubmitSnapshot.PriceVersion),
				SettlementState: string(it.SubmitSnapshot.SettlementState),
			},
			FinalSettlement:   entryResponsePtr(it.FinalSettlement),
			CurrentSettlement: entryResponsePtr(it.CurrentSettlement),
		}
		if it.Compensation != nil {
			cp := compensationResponse{
				ExternalID:   it.Compensation.ExternalID,
				Rank:         it.Compensation.Rank,
				Quantity:     it.Compensation.Quantity,
				LockedAmount: it.Compensation.LockedAmount.String(),
				TargetPrice:  it.Compensation.TargetPrice.String(),
				Amount:       it.Compensation.Amount.String(),
				Status:       string(it.Compensation.Status),
				CreatedAt:    it.Compensation.CreatedAt.UTC().Format(time.RFC3339Nano),
				SettledAt:    timePtrString(it.Compensation.SettledAt),
				FailureNote:  it.Compensation.FailureNote,
			}
			out.Compensation = &cp
		}
		items = append(items, out)
	}
	return correctionTraceResponse{
		CorrectionID:     trace.CorrectionID,
		BatchID:          trace.BatchID,
		Reason:           trace.Reason,
		Basis:            trace.Basis,
		Handler:          trace.Handler,
		PriceVersion:     int64(trace.PriceVersion),
		BaseVersion:      int64(trace.BaseVersion),
		EffectiveVersion: int64(trace.EffectiveVersion),
		CurrentVersion:   int64(trace.CurrentVersion),
		Status:           string(trace.Status),
		SubmittedAt:      trace.SubmittedAt.UTC().Format(time.RFC3339Nano),
		ApprovedAt:       timePtrString(trace.ApprovedAt),
		Events:           events,
		Items:            items,
	}
}

func entryResponsePtr(en *SettlementEntry) *settlementEntryResponse {
	if en == nil {
		return nil
	}
	r := toSettlementEntryResponse(*en)
	return &r
}

func decode(w http.ResponseWriter, r *http.Request, dst any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		writeJSON(w, http.StatusBadRequest, errorBody{Code: "invalid_argument", Message: "malformed JSON body: " + err.Error()})
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeError 把领域错误映射为明确区分四类语义的 HTTP 状态码与错误码：
// 参数错误 400 / 不存在 404 / 状态与冲突 409 / 其余 500。
func writeError(w http.ResponseWriter, err error) {
	var body errorBody
	status := http.StatusInternalServerError
	switch {
	case errors.Is(err, ErrInvalidMoney):
		status, body.Code = http.StatusBadRequest, "invalid_argument"
	case errors.Is(err, ErrInvalidArgument):
		status, body.Code = http.StatusBadRequest, "invalid_argument"
	case errors.Is(err, ErrAuctionNotFound), errors.Is(err, ErrBidNotFound),
		errors.Is(err, ErrSettlementNotFound), errors.Is(err, ErrFillNotFound),
		errors.Is(err, ErrCorrectionNotFound):
		status, body.Code = http.StatusNotFound, "not_found"
	case errors.Is(err, ErrBidConflict):
		status, body.Code = http.StatusConflict, "bid_conflict" // 幂等冲突
	case errors.Is(err, ErrCorrectionConflict):
		status, body.Code = http.StatusConflict, "correction_conflict"
	case errors.Is(err, ErrStaleSettlement):
		status, body.Code = http.StatusConflict, "stale_settlement"
	case errors.Is(err, ErrConcurrentSettlement):
		status, body.Code = http.StatusConflict, "concurrent_settlement" // 并发冲突
	case errors.Is(err, ErrDeadlinePassed):
		status, body.Code = http.StatusConflict, "deadline_passed"
	case errors.Is(err, ErrAlreadySettled):
		status, body.Code = http.StatusConflict, "already_settled"
	case errors.Is(err, ErrAuctionNotClosed):
		status, body.Code = http.StatusConflict, "auction_not_closed"
	case errors.Is(err, ErrNoSettlement):
		status, body.Code = http.StatusConflict, "no_settlement"
	case errors.Is(err, ErrNotConfirmed):
		status, body.Code = http.StatusConflict, "fill_not_confirmed"
	case errors.Is(err, ErrDeliveryExceedsFill):
		status, body.Code = http.StatusConflict, "delivery_exceeds_fill"
	case errors.Is(err, ErrCorrectionNotPending):
		status, body.Code = http.StatusConflict, "correction_not_pending"
	default:
		body.Code = "internal"
	}
	body.Message = err.Error()
	writeJSON(w, status, body)
}
