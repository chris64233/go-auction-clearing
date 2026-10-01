package auctionclearing

import (
	"errors"
	"fmt"
)

// 参数类错误（调用方入参非法，与系统当前状态无关）。
// 所有参数错误均包装 ErrInvalidArgument，可用 errors.Is 统一判别。
var (
	// ErrInvalidArgument 是参数错误的统一包装根。
	ErrInvalidArgument = errors.New("invalid argument")

	ErrAvailableQtyNotPositive = fmt.Errorf("%w: available quantity must be positive", ErrInvalidArgument)
	ErrEmptyBidder             = fmt.Errorf("%w: bidder must not be empty", ErrInvalidArgument)
	ErrEmptyExternalID         = fmt.Errorf("%w: external bid id must not be empty", ErrInvalidArgument)
	ErrBidQtyNotPositive       = fmt.Errorf("%w: bid quantity must be positive", ErrInvalidArgument)
	ErrNegativePrice           = fmt.Errorf("%w: price must not be negative", ErrInvalidArgument)
	ErrNegativeReservePrice    = fmt.Errorf("%w: reserve price must not be negative", ErrInvalidArgument)
)

// 状态类错误（请求本身合法，但与拍卖/报价当前状态冲突）。
var (
	// ErrNotFound 表示拍卖或报价不存在（其余 *NotFound 错误均包装它）。
	ErrNotFound = errors.New("not found")
	// ErrAuctionNotFound 表示指定拍卖不存在。
	ErrAuctionNotFound = fmt.Errorf("%w: auction", ErrNotFound)
	// ErrBidNotFound 表示该外部编号下没有报价。
	ErrBidNotFound = fmt.Errorf("%w: bid", ErrNotFound)
	// ErrSettlementNotFound 表示拍卖尚未清算，没有成交结果。
	ErrSettlementNotFound = fmt.Errorf("%w: settlement", ErrNotFound)
	// ErrFillNotFound 表示该外部编号的报价在清算结果中没有成交记录。
	ErrFillNotFound = fmt.Errorf("%w: fill", ErrNotFound)

	// ErrDeadlinePassed 表示报价窗口已截止（当前时间 >= 截止点）。
	ErrDeadlinePassed = errors.New("bidding deadline has passed")
	// ErrZeroDeadline 表示创建拍卖时未提供截止点。
	ErrZeroDeadline = fmt.Errorf("%w: deadline must not be zero time", ErrInvalidArgument)
	// ErrAuctionNotClosed 表示清算请求到达时报价窗口尚未关闭（当前时间 < 截止点）。
	ErrAuctionNotClosed = errors.New("auction is still open: deadline not reached")
	// ErrAlreadySettled 表示拍卖已成功清算，报价与撤回不再允许。
	ErrAlreadySettled = errors.New("auction has already settled")
)

// 幂等冲突错误：同一外部编号重复提交但内容不一致。
var (
	// ErrConflict 是请求冲突的统一包装根。
	ErrConflict = errors.New("conflict")
	// ErrBidConflict 表示同一外部报价号已有不同内容的报价（竞买方/数量/单价不一致）。
	ErrBidConflict = fmt.Errorf("%w: bid with same external id but different content", ErrConflict)
)

// 并发冲突错误。
var (
	// ErrConcurrentSettlement 表示报价/撤回恰好撞上正在进行的清算：
	// 清算快照已在原子裁决中固定，请求未抢到快照之前的窗口。
	// 结果仍然唯一确定——该报价是否进入快照已不可改变，调用方应在清算完成后
	// 查询成交结果确认报价去向，而不是重试。
	ErrConcurrentSettlement = errors.New("concurrent conflict: settlement in progress, snapshot already fixed")
)

// 结算流水与成交更正错误。
var (
	// ErrNoSettlement 表示拍卖已清算但尚未生成首版结算，
	// 成交确认/资金冻结/交割/更正都无从谈起。
	ErrNoSettlement = errors.New("settlement has not been generated yet")
	// ErrNotConfirmed 表示尚未完成成交确认就尝试资金冻结。
	ErrNotConfirmed = errors.New("fill must be confirmed before funds are frozen")
	// ErrDeliveryExceedsFill 表示本次交割数量超过该成交尚未交割的余量。
	ErrDeliveryExceedsFill = errors.New("delivery quantity exceeds undelivered fill quantity")
	// ErrNegativeDeliveryQty 表示交割数量为负。
	ErrNegativeDeliveryQty = fmt.Errorf("%w: delivery quantity must not be negative", ErrInvalidArgument)

	// ErrCorrectionNotFound 表示该更正号下没有更正申请。
	ErrCorrectionNotFound = fmt.Errorf("%w: correction", ErrNotFound)
	// ErrEmptyCorrectionID 表示更正申请未提供更正号。
	ErrEmptyCorrectionID = fmt.Errorf("%w: correction id must not be empty", ErrInvalidArgument)
	// ErrEmptyCorrectionReason 表示更正申请未提供原因。
	ErrEmptyCorrectionReason = fmt.Errorf("%w: correction reason must not be empty", ErrInvalidArgument)
	// ErrEmptyCorrectionBasis 表示更正申请未提供更正依据（价格不变也必填）。
	ErrEmptyCorrectionBasis = fmt.Errorf("%w: correction basis must not be empty", ErrInvalidArgument)
	// ErrEmptyCorrectionHandler 表示更正申请未提供处理人。
	ErrEmptyCorrectionHandler = fmt.Errorf("%w: correction handler must not be empty", ErrInvalidArgument)
	// ErrEmptyCorrectionItems 表示更正申请没有任何成交更正项。
	ErrEmptyCorrectionItems = fmt.Errorf("%w: correction items must not be empty", ErrInvalidArgument)
	// ErrDuplicateCorrectionItem 表示同一张申请内同一成交出现多次。
	ErrDuplicateCorrectionItem = fmt.Errorf("%w: duplicate fill in correction items", ErrInvalidArgument)
	// ErrNegativeTargetPrice 表示更正目标单价为负。
	ErrNegativeTargetPrice = fmt.Errorf("%w: target unit price must not be negative", ErrInvalidArgument)

	// ErrCorrectionConflict 表示同一更正号重复提交，但竞价批次、
	// 价格版本、基准结算版本或申请内容与首次不一致。
	ErrCorrectionConflict = fmt.Errorf("%w: correction with same id but different batch, version or content", ErrConflict)
	// ErrStaleSettlement 表示更正申请挂接的基准结算版本已被后续更正取代。
	ErrStaleSettlement = fmt.Errorf("%w: correction applies to a superseded settlement version", ErrConflict)
	// ErrCorrectionNotPending 表示对并非停在待处理的更正申请执行补偿重试。
	ErrCorrectionNotPending = errors.New("correction is not awaiting compensation settlement")
)
