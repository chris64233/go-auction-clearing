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
	case errors.Is(err, ErrInvalidArgument):
		status, body.Code = http.StatusBadRequest, "invalid_argument"
	case errors.Is(err, ErrAuctionNotFound), errors.Is(err, ErrBidNotFound),
		errors.Is(err, ErrSettlementNotFound), errors.Is(err, ErrFillNotFound):
		status, body.Code = http.StatusNotFound, "not_found"
	case errors.Is(err, ErrBidConflict):
		status, body.Code = http.StatusConflict, "bid_conflict" // 幂等冲突
	case errors.Is(err, ErrConcurrentSettlement):
		status, body.Code = http.StatusConflict, "concurrent_settlement" // 并发冲突
	case errors.Is(err, ErrDeadlinePassed):
		status, body.Code = http.StatusConflict, "deadline_passed"
	case errors.Is(err, ErrAlreadySettled):
		status, body.Code = http.StatusConflict, "already_settled"
	case errors.Is(err, ErrAuctionNotClosed):
		status, body.Code = http.StatusConflict, "auction_not_closed"
	default:
		body.Code = "internal"
	}
	body.Message = err.Error()
	writeJSON(w, status, body)
}
