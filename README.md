# go-auction-clearing

多单位**密封竞价**（sealed-bid）、**原子清算**与**结算后成交更正**服务。
支持拍卖创建、报价、撤回、统一价格清算、成交结果查询，以及清算后的结算流水
（成交确认 → 资金冻结 → 交割）和成交更正（申请、批准、补偿、追溯）。
全部状态由同一条事件日志重放得到，对参数错误、状态错误、幂等冲突与并发冲突做了明确区分。

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

清算结果（`Settlement` 成交结果，见 `GET /settlement`）**永远不可变**，
之后的结算与更正都不会改写它。

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

### 结算流水（成交确认 / 资金冻结 / 交割）

清算给出的是不可变的撮合结果；成交后进入结算流水，按**版本**管理：

- `POST .../settlements` 在清算后生成**首版结算**（版本号 1）；重复调用幂等返回原结果。
- 每笔成交依次推进 **成交确认 → 资金冻结 → 交割**：必须先确认才能冻结；
  确认/冻结重复执行幂等；交割为**增量**记录，数量不得为负、不得超过未交割余量。
- 交割金额按**交割时刻有效版本单价**锁定；已交割事实（数量/金额/时间）只增不减，
  随后任何更正都原样继承，绝不回滚。
- 任一时点每笔成交只有一个**当前有效**结算条目；旧版本标记 `superseded` 但原样保留，
  可按版本号查询，供更正追溯。
- 价格版本（`PriceVersion`）标识成交来自哪一次撮合定价；更正不重新撮合，
  因此价格版本恒为 1。

### 成交更正（结算后 correction）

结算后如需更正成交，必须先**登记申请**再**批准执行**。更正范围受
**竞价批次（拍卖 ID）与结算版本**双重限制。

1. **申请留痕**：申请冻结成交清单的逐笔快照（成交量、已交割量、原始单价、
   价格版本、提交时结算状态 `pending/confirmed/frozen/delivered`），并记录
   申请原因、更正依据、处理人，以及挂接的价格版本与基准结算版本。
2. **并发唯一结果**：成交确认、资金冻结、交割与更正批准共用同一把批次锁。
   批准采用与清算一致的两阶段——锁内认领（`correcting` 标记）并复制基准结算，
   锁外执行可能失败的补偿结算，回锁再原子提交。窗口内到达的流水动作在条件变量上
   等待批准落定，然后作用在唯一有效的新版本上；一个订单任一时刻只保留一套有效结果，
   新版本继承全部确认/冻结/交割状态。
3. **幂等与冲突**：相同更正号在同批次、同价格版本、同基准结算版本上以相同内容
   （原因/依据/处理人/逐笔目标价）重复提交，返回原结果（`replayed`）；
   同号但批次、价格版本、基准结算版本或内容变化，返回 `ErrCorrectionConflict`
   （409 `correction_conflict`）。批准时若基准版本已被后续更正取代，返回
   `ErrStaleSettlement`（409 `stale_settlement`），申请保持 `submitted`；
   **更正失败时原结算继续可用**。
4. **已交割只能补偿**：
   - 未交割部分：直接在新结算版本上改结算单价/结算金额；
   - 已交割部分：原成交不动，按 `已交割锁定金额 − 目标单价×已交割数量`
     生成**补偿单**（差额有符号：为正表示退回，为负表示补收）走外部补偿结算通道；
   - 一笔成交部分交割时两者并存（部分补偿）。
5. **价格未变也留痕**：目标价与当前有效价一致时不产生新版本，但仍写入一条
   审计事件（依据、处理人、时间、`changed=false`），更正置为 `completed`，
   绝不只返回“无变化”。
6. **字段限制**：更正只能改 `effective_unit_price` 与 `settled_amount`；
   订单的原始撮合信息（外部编号、竞买方、成交量、原始单价、价格版本、`rank`）
   与交割状态（已交割数量/金额、确认/冻结标志、交割时间）在新版本中原样保留；
   清算结果（`/settlement`）永远不可变。
7. **补偿失败不伪装完成**：补偿结算通道失败时，**不切换结算版本**（未交割部分也
   不会提前生效），补偿单保持 `pending` 并记录失败原因，更正整体停在 `pending`，
   原成交仍可正常查询；通道恢复后用 `retry-compensation` 从补偿步骤重试整个原子动作，
   全部结清后才切换版本、更正转为 `completed`。

### 链路追溯

`GET .../corrections/{cid}/trace` 可从一笔更正一路追到**原成交、申请原因/依据/
处理人、补偿单与最终结算**。三种情形的条目结构不同：

| 情形 | `scenario` | 补偿单 | 最终结算条目 |
| --- | --- | --- | --- |
| 未交割 | `undelivered` | 无 | 直接体现新单价与新金额 |
| 已全额交割 | `delivered_compensated` | 全额差额补偿 | 保留原单价、原金额与已交割状态 |
| 部分交割 | `partial_compensation` | 仅覆盖已交割数量的差额 | 已交割金额保留，未交割数量按新单价 |

尚未批准的申请追溯停在申请快照（`scenario=submitted`）；当更正的生效版本已被
后续更正取代时，条目额外附带 `current_settlement` 指针，指向最新版本。

### 持久化

采用事件溯源，最小事实单元为 `Event`（`event.go`）：

| 事件 | 含义 |
| --- | --- |
| `auction_created` | 拍卖创建 |
| `bid_submitted` | 新报价被接受 |
| `bid_withdrawn` | 报价被撤回 |
| `settled` | 清算完成，事件体内携带**完整**成交结果（原子单位） |
| `settlement_generated` | 首版结算生成，携带完整结算快照 |
| `fill_confirmed` / `funds_frozen` | 成交确认 / 资金冻结（记录作用的结算版本） |
| `fill_delivered` | 增量交割（数量与按当时单价锁定的金额） |
| `correction_submitted` | 更正申请登记（含逐笔成交快照） |
| `correction_approved` | 批准落定；价格变化且补偿全成时携带完整新结算版本，与申请原子提交 |

- `MemoryEventStore`：进程内存储，用于测试；Append 时对结构化载荷做深拷贝，
  已提交事件与调用方此后的内存修改完全隔离。
- `FileEventStore`：JSON Lines 文件，每批一次 `write` + `fsync`，
  序号连续；重启时重放恢复全部状态。最后一行残缺（写入中途崩溃）会在加载阶段被明确拒绝。
- 结算版本链、更正申请（快照、补偿单、审计事件）同属事件流：补偿失败停在
  `pending` 的中间态也会完整落盘，重启重放后可继续重试。

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
| GET | `/v1/auctions/{id}/settlement` | 查询不可变的清算成交结果 |
| POST | `/v1/auctions/{id}/settlements` | 生成首版结算（幂等） |
| GET | `/v1/auctions/{id}/settlements` | 查询当前有效结算版本 |
| POST | `/v1/auctions/{id}/fills/{ext}/confirm` | 成交确认（幂等） |
| POST | `/v1/auctions/{id}/fills/{ext}/freeze` | 资金冻结（须先确认，幂等） |
| POST | `/v1/auctions/{id}/fills/{ext}/deliver` | 增量交割 `{"quantity":n}` |
| POST | `/v1/auctions/{id}/corrections` | 提交更正申请（幂等，201/200） |
| GET | `/v1/auctions/{id}/corrections` | 列出本批次全部更正 |
| GET | `/v1/auctions/{id}/corrections/{cid}` | 查询单张更正（快照/补偿/审计事件） |
| GET | `/v1/auctions/{id}/corrections/{cid}/trace` | 更正链路追溯（三种情形结构不同） |
| POST | `/v1/auctions/{id}/corrections/{cid}/approve` | 批准更正（幂等，201/200；body 可空） |
| POST | `/v1/auctions/{id}/corrections/{cid}/retry-compensation` | 重试待处理补偿 |

错误码与状态码（响应体形如 `{"code":"...","message":"..."}`）：

| 类别 | 领域错误（Go `errors.Is`） | HTTP | code |
| --- | --- | --- | --- |
| 参数错误 | `ErrInvalidArgument`、`ErrInvalidMoney` 及其包装 | 400 | `invalid_argument` |
| 不存在 | `ErrAuctionNotFound` / `ErrBidNotFound` / `ErrSettlementNotFound` / `ErrFillNotFound` / `ErrCorrectionNotFound` | 404 | `not_found` |
| 状态/流程冲突 | `ErrDeadlinePassed` / `ErrAlreadySettled` / `ErrAuctionNotClosed` | 409 | `deadline_passed` / `already_settled` / `auction_not_closed` |
| 结算流程冲突 | `ErrNoSettlement` / `ErrNotConfirmed` / `ErrDeliveryExceedsFill` / `ErrCorrectionNotPending` | 409 | `no_settlement` / `fill_not_confirmed` / `delivery_exceeds_fill` / `correction_not_pending` |
| 幂等冲突 | `ErrBidConflict` | 409 | `bid_conflict` |
| 更正冲突 | `ErrCorrectionConflict` / `ErrStaleSettlement` | 409 | `correction_conflict` / `stale_settlement` |
| 并发冲突 | `ErrConcurrentSettlement` | 409 | `concurrent_settlement` |

幂等接口的响应带 `outcome`：报价 `accepted`/`replayed`，撤回 `withdrawn`/`replayed`，
清算 `settled`/`replayed`，首版结算 `generated`/`replayed`，
更正申请与批准 `accepted`/`replayed`；HTTP 首次成功为 201，幂等重放为 200。
补偿结算失败**不是**接口错误：批准正常返回，但更正 `status` 为 `pending`、
对应补偿单 `status=pending` 且带 `failure_note`。

### 调用示例

```bash
# 创建：10 个可售单位，10:00 截止，保留价 5
curl -s -XPOST localhost:8080/v1/auctions -d '{
  "available_qty": 10,
  "deadline": "2026-09-01T10:00:00Z",
  "reserve_price": "5"
}'

# 报价 / 撤回（幂等）/ 截止后清算 / 查询不可变成交结果
curl -s -XPOST localhost:8080/v1/auctions/1/bids -d '{
  "external_id":"b1","bidder":"A","qty":3,"price":"10"}'
curl -s -XPOST localhost:8080/v1/auctions/1/bids/b2/withdraw
curl -s -XPOST localhost:8080/v1/auctions/1/settle
curl -s localhost:8080/v1/auctions/1/settlement

# 生成首版结算，然后确认/冻结/交割
curl -s -XPOST localhost:8080/v1/auctions/1/settlements
curl -s -XPOST localhost:8080/v1/auctions/1/fills/b1/confirm
curl -s -XPOST localhost:8080/v1/auctions/1/fills/b1/freeze
curl -s -XPOST localhost:8080/v1/auctions/1/fills/b1/deliver -d '{"quantity": 3}'

# 提交成交更正（已交割成交批准时自动转为补偿单）
curl -s -XPOST localhost:8080/v1/auctions/1/corrections -d '{
  "correction_id": "COR-20260930-001",
  "reason": "行情源录入了错误成交价",
  "basis": "feed-v2-reconciliation-2026-09-30",
  "handler": "risk-officer-1",
  "items": [
    {"external_id": "b1", "target_unit_price": "6.0000"},
    {"external_id": "b2", "target_unit_price": "5.0000"}
  ]
}'

# 批准；若补偿结算失败，通道恢复后重试；查询链路追溯
curl -s -XPOST localhost:8080/v1/auctions/1/corrections/COR-20260930-001/approve
curl -s -XPOST localhost:8080/v1/auctions/1/corrections/COR-20260930-001/retry-compensation
curl -s localhost:8080/v1/auctions/1/corrections/COR-20260930-001/trace
```

## 作为 Go 库使用

```go
clk := auctionclearing.NewControlledClock(time.Now())
store := auctionclearing.NewMemoryEventStore() // 或 NewFileEventStore("events.jsonl")

// 注入补偿结算通道（失败通道可用于演练 pending → retry）；默认通道总是成功。
gw := auctionclearing.WithCompensationGateway(myGateway)
engine, _ := auctionclearing.NewEngine(store, clk, gw)

a, _ := engine.CreateAuction(10, clk.Now().Add(time.Hour), auctionclearing.MustParseMoney("5"))
_, _ = engine.SubmitBid(auctionclearing.BidInput{
	AuctionID: a.ID, ExternalID: "b1", Bidder: "A", Qty: 3,
	Price: auctionclearing.MustParseMoney("10"),
})
// ... 截止后清算、生成结算
engine.SettleAuction(a.ID)
engine.GenerateSettlement(a.ID)

// 登记并批准一笔更正；已交割成交自动生成补偿单，补偿失败返回 pending。
engine.SubmitCorrection(a.ID, auctionclearing.CorrectionInput{
	CorrectionID: "COR-1",
	Reason:       "wrong feed price",
	Basis:        "feed-v2-reconciliation",
	Handler:      "risk-officer-1",
	Items: []auctionclearing.CorrectionItem{
		{ExternalID: "b1", TargetUnitPrice: auctionclearing.MustParseMoney("6")},
	},
})
res, _ := engine.ApproveCorrection(a.ID, "COR-1", auctionclearing.ApproveCorrectionInput{})
if res.Data.Status == auctionclearing.CorrectionPending {
	engine.RetryCompensation(a.ID, "COR-1") // 通道恢复后重试
}
trace, _ := engine.GetCorrectionTrace(a.ID, "COR-1")
```

## 运行服务

```bash
go run ./cmd/server -addr :8080 -events ./data/events.jsonl
```

重启进程后自动重放事件日志，恢复全部拍卖、报价、成交结果、结算版本链与更正申请
（含停在 `pending` 的补偿）。

## 运行测试

```bash
go test -race ./...
```

测试覆盖（除 001 轮已有的清算/并发/持久化用例外，新增）：

- 首版结算幂等与前置条件；确认/冻结/交割顺序、幂等、超量与负值交割拒绝；
- 更正申请的逐笔快照（成交量/已交割量/原价/价格版本/结算状态）与参数校验；
- 同号幂等重放、内容/依据/处理人变化冲突、同号跨批次冲突；
- 基准版本被后续更正取代时批准冲突，申请保持 `submitted`；
- 未交割直接改价且只改允许字段、撮合信息与确认/冻结状态原样继承；
- 已全额交割只出全额补偿单、部分交割部分补偿、上调价格的负差额补偿；
- 价格未变不产生新版本但写入审计事件（依据/处理人），更正仍置 `completed`；
- 混合条目补偿失败时未交割部分不提前生效、版本不切换、原成交可查，重试成功后整体完成；
- 连续更正的版本链与旧版本/旧追溯保留（含当前版本指针）；
- 三种情形（已交割/未交割/部分补偿）追溯结构不同，未批准为 `submitted`；
- 确认/冻结/交割/新申请与批准两阶段窗口并发的唯一有效结果（`-race`，多轮）；
- 内存与 JSONL 事件存储下，pending/completed 更正状态的重启重放恢复与恢复后重试；
- 更正全链路 HTTP 用例与错误码映射；金额定点减法 `Money.Sub`。

## 代码结构

| 文件 | 职责 |
| --- | --- |
| `money.go` | 定点金额类型、解析、加减乘与 JSON 序列化 |
| `clock.go` | 统一时间来源（系统时钟 / 可控时钟） |
| `domain.go` | 拍卖、报价、成交、清算等领域模型与结果类型 |
| `errors.go` | 参数/状态/幂等/并发/结算/更正错误 |
| `clearing.go` | 纯函数清算算法（排序、分配、统一价格） |
| `event.go` / `store.go` | 领域事件与持久化（内存深拷贝 / JSONL 文件） |
| `engine.go` | 并发安全内核：校验、幂等、快照认领、事件重放 |
| `settlement.go` | 结算版本、结算条目、更正申请、补偿单、审计事件模型 |
| `correction_trace.go` | 更正链路追溯视图（三种情形） |
| `settlement_service.go` | 首版结算、成交确认/冻结/交割、新版本构造与补偿计算 |
| `correction_gateway.go` | 补偿结算通道接口与引擎选项 |
| `correction_service.go` | 更正申请登记：快照、批次/版本约束、幂等与冲突 |
| `correction_approve.go` | 两阶段批准、补偿失败 pending、补偿重试 |
| `correction_query.go` | 更正查询、列表与端到端追溯组装 |
| `api.go` | HTTP 接口与错误码映射 |
| `cmd/server/main.go` | 可运行服务入口 |
