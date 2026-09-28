# go-auction-clearing

多单位**密封竞价**（sealed-bid）及其**原子清算**服务的 Go 实现。
支持拍卖创建、报价、撤回、统一价格清算与成交结果查询，全部数据持久化到事件日志，
并对参数错误、状态错误、幂等冲突与并发冲突做了明确区分。

开发环境：Go 1.23.0，无第三方依赖。

## 业务规则

### 拍卖

每场拍卖记录三个要素：

- `available_qty`：可售数量（必须为正）；
- `deadline`：报价截止点。判断口径为左闭右开——**当前时间 `< deadline` 可报价/撤回，
  当前时间 `== deadline` 即视为已截止**。所有时间判断都取自同一个可替换的 `Clock`，
  生产用系统时钟，测试用可控时钟精确覆盖边界；
- `reserve_price`：最低成交价，单价低于该值的报价不参与分配。金额为 `int64` 定点数
  （精度 4 位小数，见 `Money`），全程不使用浮点数；HTTP 边界也只收/发十进制字符串。

### 报价与撤回

每份报价带：外部编号、竞买方、数量、单价与**首次有效提交时间**。

- 截止前可以报价或撤回；截止后报价、撤回均被拒绝（迟到报价不会进入快照）。
- 同一拍卖内，外部编号唯一。同一编号重复提交：
  - 内容（竞买方/数量/单价）完全一致 → 幂等返回原记录（`replayed`），首次提交时间不变；
  - 内容不一致 → `ErrBidConflict`（HTTP 409 `bid_conflict`），原记录不受影响。
- 幂等判定优先于状态判定：即使在截止后或清算后用相同内容重试，也稳定拿到原结果。
- 撤回是终态且同样幂等（重复撤回返回 `replayed`）；已撤回的报价不会被“相同内容的
  重复提交”恢复。

### 清算（统一价格、按序分配、边界部分成交）

清算只读取**同一份原子快照**（认领时刻复制全部报价），规则：

1. 剔除已撤回报价，剔除单价 `< reserve_price` 的报价；
2. 按**单价降序**排序；同价按**首次有效提交时间升序**排序；时间仍相同则按外部编号兜底，
   保证顺序在任何输入下唯一确定；
3. 依次分配，累计成交不得超过可售量；恰好位于售罄边界的报价**可以部分成交**，
   排在其后的报价不成交；
4. 所有成交执行**统一清算价**（边界报价的单价；需求未售罄时为最后一份成交报价的单价；
   无成交时清算价为 0）。

### 原子性与“至多一次清算”

- 清算分两阶段：先在互斥锁内**认领**拍卖并复制快照（`settling` 标记），
  再在锁外执行纯函数撮合，最后回锁把**一个**包含全部成交明细的 `settled` 事件
  原子追加落盘。撮合或落盘失败时认领释放，**不会遗留任何部分成交记录**，可安全重试。
- 一场拍卖至多成功清算一次：重复清算返回原结果（`replayed`）；
  并发清算只有一个请求胜出，其余等待后读取同一份结果，绝不产生重复成交。
- 报价/撤回恰好撞上清算认领窗口时立即返回 `ErrConcurrentSettlement`
  （HTTP 409 `concurrent_settlement`）：快照已固定，请求不可能改变结果——
  裁决仍然唯一，调用方应改查成交结果而不是重试。纯幂等的重复请求（相同内容重报、
  重复撤回）在窗口内照常返回原结果。

### 持久化

采用事件溯源，最小事实单元为 `Event`（`event.go`）：

| 事件 | 含义 |
| --- | --- |
| `auction_created` | 拍卖创建 |
| `bid_submitted` | 新报价被接受 |
| `bid_withdrawn` | 报价被撤回 |
| `settled` | 清算完成，事件体内携带**完整**成交结果（原子单位） |

- `MemoryEventStore`：进程内存储，用于测试；
- `FileEventStore`：JSON Lines 文件，每批一次 `write` + `fsync`，
  序号连续；重启时重放恢复全部状态。最后一行残缺（写入中途崩溃）会在加载阶段被明确拒绝。

## HTTP 接口

所有金额为十进制字符串（`"10.5000"`），时间为 RFC3339。

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| POST | `/v1/auctions` | 创建拍卖 |
| GET | `/v1/auctions/{id}` | 查询拍卖 |
| POST | `/v1/auctions/{id}/bids` | 提交报价（幂等） |
| GET | `/v1/auctions/{id}/bids` | 列出全部报价 |
| POST | `/v1/auctions/{id}/bids/{ext}/withdraw` | 撤回报价（幂等） |
| POST | `/v1/auctions/{id}/settle` | 触发清算（幂等） |
| GET | `/v1/auctions/{id}/settlement` | 查询成交结果 |

错误码与状态码的对应（响应体形如 `{"code":"...","message":"..."}`）：

| 类别 | 领域错误（Go `errors.Is`） | HTTP | code |
| --- | --- | --- | --- |
| 参数错误 | `ErrInvalidArgument`（及其包装） | 400 | `invalid_argument` |
| 不存在 | `ErrAuctionNotFound` / `ErrBidNotFound` / `ErrSettlementNotFound` / `ErrFillNotFound`（均包装 `ErrNotFound`） | 404 | `not_found` |
| 状态错误 | `ErrDeadlinePassed` / `ErrAlreadySettled` / `ErrAuctionNotClosed` | 409 | `deadline_passed` / `already_settled` / `auction_not_closed` |
| 幂等冲突 | `ErrBidConflict`（包装 `ErrConflict`） | 409 | `bid_conflict` |
| 并发冲突 | `ErrConcurrentSettlement` | 409 | `concurrent_settlement` |

幂等接口的响应带 `outcome`：报价 `accepted`/`replayed`，撤回 `withdrawn`/`replayed`，
清算 `settled`/`replayed`；HTTP 首次成功为 201，幂等重放为 200。

### 调用示例

```bash
# 创建：10 个可售单位，10:00 截止，保留价 5
curl -s -XPOST localhost:8080/v1/auctions -d '{
  "available_qty": 10,
  "deadline": "2026-09-01T10:00:00Z",
  "reserve_price": "5"
}'

# 报价
curl -s -XPOST localhost:8080/v1/auctions/1/bids -d '{
  "external_id":"b1","bidder":"A","qty":4,"price":"10"
}'
curl -s -XPOST localhost:8080/v1/auctions/1/bids -d '{
  "external_id":"b2","bidder":"B","qty":8,"price":"9"
}'

# 撤回（幂等）
curl -s -XPOST localhost:8080/v1/auctions/1/bids/b2/withdraw

# 截止后清算（幂等，可安全重复调用）
curl -s -XPOST localhost:8080/v1/auctions/1/settle

# 查询成交结果
curl -s localhost:8080/v1/auctions/1/settlement
```

## 作为 Go 库使用

```go
clk := auctionclearing.NewControlledClock(time.Now())
store := auctionclearing.NewMemoryEventStore() // 或 NewFileEventStore("events.jsonl")
engine, _ := auctionclearing.NewEngine(store, clk)

a, _ := engine.CreateAuction(10, clk.Now().Add(time.Hour), auctionclearing.MustParseMoney("5"))
_, _ = engine.SubmitBid(auctionclearing.BidInput{
    AuctionID: a.ID, ExternalID: "b1", Bidder: "A", Qty: 4,
    Price: auctionclearing.MustParseMoney("10"),
})
// ... 截止后
res, err := engine.SettleAuction(a.ID) // res.Data 即成交结果
```

## 运行服务

```bash
go run ./cmd/server -addr :8080 -events ./data/events.jsonl
```

重启进程后自动重放事件日志恢复全部拍卖、报价与成交结果。

## 运行测试

```bash
go test -race ./...
```

测试覆盖：金额定点解析/序列化与溢出、截止边界（含恰等于截止点的 1ns 精度用例）、
排序与边界部分成交、保留价过滤、撤回剔除、报价幂等与冲突、撤回幂等、
至多一次清算（32 并发清算恰好 1 个胜出）、撤回/迟到报价撞上清算窗口的唯一裁决、
事件日志持久化与损坏检测、重启重放，以及全部 HTTP 接口端到端用例。

## 代码结构

| 文件 | 职责 |
| --- | --- |
| `money.go` | 定点金额类型、解析、运算与 JSON 序列化 |
| `clock.go` | 统一时间来源（系统时钟 / 可控时钟） |
| `domain.go` | 拍卖、报价、成交、清算等领域模型与结果类型 |
| `errors.go` | 参数/状态/幂等/并发四类错误 |
| `clearing.go` | 纯函数清算算法（排序、分配、统一价格） |
| `event.go` / `store.go` | 领域事件与持久化（内存 / JSONL 文件） |
| `engine.go` | 并发安全的服务内核：校验、幂等、快照认领、重放 |
| `api.go` | HTTP 接口与错误码映射 |
| `cmd/server/main.go` | 可运行服务入口 |
