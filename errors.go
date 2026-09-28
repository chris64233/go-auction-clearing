package auctionclearing

import "errors"

// ErrorKind 错误类别，便于接口层明确区分参数、状态、幂等与并发冲突。
type ErrorKind string

const (
	// KindInvalidArgument 参数错误（400）。
	KindInvalidArgument ErrorKind = "invalid_argument"
	// KindNotFound 资源不存在（404）。
	KindNotFound ErrorKind = "not_found"
	// KindConflict 冲突（409）：同一外部报价号内容不一致等。
	KindConflict ErrorKind = "conflict"
	// KindInvalidState 状态错误（422）：已截止仍报价/撤回、未截止就清算、
	// 对已清算拍卖执行写操作（含撤回恰好撞上清算）等。
	KindInvalidState ErrorKind = "invalid_state"
)

// Error 带类别的领域错误。
type Error struct {
	Kind ErrorKind
	Msg  string
}

func (e *Error) Error() string { return string(e.Kind) + ": " + e.Msg }

// NewError 构造一个带类别错误。
func NewError(kind ErrorKind, msg string) *Error {
	return &Error{Kind: kind, Msg: msg}
}

// ErrorKindOf 提取错误类别；非领域错误返回空串。
func ErrorKindOf(err error) ErrorKind {
	var e *Error
	if errors.As(err, &e) {
		return e.Kind
	}
	return ""
}

// 哨兵错误，便于测试与调用方用 errors.Is 判定。
var (
	ErrNotFound       = NewError(KindNotFound, "resource not found")
	ErrAuctionCleared = NewError(KindInvalidState, "auction already cleared")
	ErrNotYetClosed   = NewError(KindInvalidState, "auction bidding is not closed yet")
	ErrBiddingClosed  = NewError(KindInvalidState, "bidding is closed")
	ErrBidConflict    = NewError(KindConflict, "bid content conflicts with existing submission")
)
