package auctionclearing

// CompensationGateway 是已交割成交差额补偿所走的外部结算通道。
// 它可能失败；批准更正时若补偿结算失败，补偿单与更正都停在待处理，
// 原成交不受影响，可稍后用 RetryCompensation 重试。
type CompensationGateway interface {
	SettleCompensation(auctionID int64, externalID string, amount Money) error
}

// noopCompensationGateway 默认通道：补偿结算总是成功。
type noopCompensationGateway struct{}

func (noopCompensationGateway) SettleCompensation(int64, string, Money) error { return nil }

// EngineOption 配置 Engine 的可选依赖。
type EngineOption func(*Engine)

// WithCompensationGateway 注入补偿结算通道（测试可注入失败通道）。
func WithCompensationGateway(g CompensationGateway) EngineOption {
	return func(e *Engine) {
		if g != nil {
			e.compGateway = g
		}
	}
}
