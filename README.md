# go-auction-clearing

多单位密封竞价（sealed-bid）、原子清算与**结算后成交更正**服务。使用 Go 1.23 标准库实现，无第三方依赖。

## 业务规则

### 拍卖与报价

- 一场拍卖记录：**可售数量**、**报价截止点**、**最低成交价（保留价）**。
- 每份报价带：**外部报价号**（调用方提供，拍卖内唯一）、竞买方、数量、单价、首次有效提交时间。
- 金额采用定点整数精确表示（内部精度 10⁻⁹，基于 `math/big`），全程不经过 `float`；JSON 中金额一律为十进制**字符串**（如 `"10.00"`）。
- 所有截止边界判断使用同一个可注入的时间来源 `Clock`；边界归属规则：`now == deadline` 即视为**已截止**，只有 `now < deadline` 才允许报价/撤回。
- 截止前可以报价或撤回。
- **幂等**：同一外部报价号提交**完全相同**的内容（竞买方/数量/单价一致），返回原有结果（`replayed=true`），首次提交时间不变；内容不一致返回冲突。
- 已撤回的报价不会因相同内容重放而“复活”。
- 迟到报价（截止后）不能写入，更不可能混入清算快照。

### 清算

1. 在**同一份报价快照**上完成，快照只含：未撤回、且单价 ≥ 保留价的报价。
2. 排序：**单价降序**；同价按**首次有效提交时间先后**（FIFO）；时间再相同按外部编号，保证结果完全确定。
3. 按序分配可售量，排在边界的报价**可以部分成交**，最终成交量**绝不超过可售量**。
4. 成交按报价自身单价计价（单价歧视），逐笔记录数量、单价与金额。
5. “取快照 → 排序分配 → 写结果与状态”在**单个事务**内完成：提交要么整体成功，要么整体放弃，**执行失败不遗留任何部分成交记录**（无成交数据、拍卖仍为 open，可安全重试）。
6. 一场拍卖**至多成功清算一次**：重复请求读到同一份原结果；并发清算经单场拍卖互斥锁串行化，只有一个首次成交，其余全部重放原结果。
7. **撤回恰好撞上清算**时结局唯一：两者竞争同一把拍卖锁——
   - 撤回先提交：清算快照中没有该报价；
   - 清算先提交：撤回失败（`invalid_state`，拍卖已清算），撤回不生效。

### 结算流水

清算给出的是不可变的撮合结果；之后进入结算流水，按**结算版本**（首版为 1，
每次改变结算的更正递增）与**价格版本**（恒为 1，更正不重新撮合定价）管理：

- `POST .../settle` 在清算后生成首版结算；重复调用幂等返回原结果。
- 每笔成交可依次 **成交确认 → 资金冻结 → 交割**：必须先确认才能冻结；
  交割数量按增量记录、不得超过成交余量；交割金额按**交割时刻有效版本单价**
  锁定。确认/冻结重复执行幂等。
- 任一时刻每笔成交只有一个当前有效结算条目；旧版本标记 `superseded`
  但原样保留，供追溯。

### 成交更正（correction）

结算后如需更正成交，必须先登记**更正申请**再批准执行。更正范围受
**竞价批次（拍卖 ID）与结算版本**双重限制。

1. **申请留痕**：更正申请记录成交清单（逐笔快照：成交量、已交割量、原价、
   价格版本、提交时结算状态）、申请原因、更正依据、处理人，以及挂接的
   价格版本与基准结算版本。
2. **并发唯一结果**：成交确认、资金冻结与更正批准共用同一把批次事务锁，
   三个动作同时到达时严格串行化——一个订单任一时刻只保留一套有效成交结果，
   批准产生的新版本继承确认/冻结/交割状态，不出现“两版各做一半”。
3. **幂等与冲突**：相同更正号在同批次同版本上以相同内容重复提交，返回原结果
   （`replayed=true`）；同号但批次、价格版本、基准结算版本或申请内容变化，
   返回 `conflict`（409）。批准时若基准版本已被后续更正取代，同样报冲突，
   申请保持 `submitted`；更正失败时原结算继续可用。
4. **已交割只能补偿**：未交割部分直接在新结算版本上改结算单价/金额；
   已交割部分原成交不动，按 `已交割锁定金额 − 目标单价×已交割数量` 生成
   **补偿单**（差额有符号，可为负）走补偿结算通道；一笔成交部分交割时两者
   并存（部分补偿）。
5. **价格未变也留痕**：目标价与当前有效价一致时不产生新版本，但仍写入一条
   审计事件（依据、处理人、时间、`changed=false`），更正置为 `completed`，
   绝不只返回“无变化”。
6. **字段限制**：更正只能改 `effective_unit_price` 与 `settled_amount`；
   订单的原始撮合信息（外部报价号、竞买方、成交量、原单价、价格版本、seq）
   与交割状态（已交割数量/金额、交割时间）在新版本中原样保留；清算结果
   （`/result`）永远不可变。
7. **补偿失败不伪装完成**：补偿结算通道失败时，补偿单保持 `pending` 并记录
   失败原因，更正整体停在 `pending`，原成交仍可正常查询；通道恢复后用
   `retry-compensation` 重试，全部结清后更正才转为 `completed`。

**链路追溯**：`GET .../corrections/{id}/trace` 可从一笔更正一路追到原成交、
申请原因/依据/处理人、补偿单与最终结算，并按三种情形给出不同条目结构：

| 情形 | `scenario` | 补偿单 | 最终结算条目 |
| --- | --- | --- | --- |
| 未交割 | `undelivered` | 无 | 直接体现新单价与新金额 |
| 已全额交割 | `delivered_compensated` | 全额差额补偿 | 保留原单价、原金额与已交割状态 |
| 部分交割 | `partial_compensation` | 仅覆盖已交割数量的差额 | 已交割金额保留，未交割数量按新单价 |

## 代码结构

| 文件 | 职责 |
| --- | --- |
| `money.go` | 精确金额 `Money`（定点 big.Int，JSON 字符串） |
| `auction.go` | 领域模型：拍卖、报价、成交、清算结果、仓储接口 |
| `errors.go` | 带类别的领域错误 |
| `clearing.go` | 快照、排序与分配的**纯函数**清算算法 |
| `repository.go` | 线程安全内存仓储 + JSON 文件持久化（每拍卖一把锁，临时文件 + rename 原子提交） |
| `service.go` | 应用服务与统一时钟：创建/报价/撤回/清算/查询，全部判定在单事务内 |
| `settlement.go` | 结算领域模型：结算版本、结算条目（确认/冻结/交割状态）、更正申请、补偿单、审计事件 |
| `correction.go` | 结算生成与成交确认/资金冻结/交割用例 |
| `correction_apply.go` | 更正申请登记：快照、批次/版本约束、幂等与冲突 |
| `correction_approve.go` | 更正批准：新结算版本、补偿单、无变化留痕、补偿失败待处理 |
| `correction_helpers.go` | 版本构造、补偿计算、补偿重试、更正查询 |
| `correction_trace.go` | 更正链路追溯（原成交→申请→补偿→最终结算，三种情形） |
| `server.go` | JSON HTTP 接口与错误码映射 |
| `cmd/auctiond/main.go` | 可运行的服务入口 |

### 并发与持久化模型

- 每场拍卖持有独立互斥锁，针对同一场拍卖的“读-改-写”严格串行化；不同拍卖互不阻塞。
- 仓储的 `Update` 给事务函数的是聚合的**深拷贝**；函数返回错误则全部修改放弃。
- 文件模式下，提交时先 `fsync` 临时文件再 `rename` 原子替换 `data/<拍卖ID>.json`，先落盘后换内存；进程重启后自动加载，已清算状态与成交结果完整恢复。
- 结算版本链、更正申请（含成交快照、补偿单、审计事件）同属一个拍卖聚合，
  随每次提交原子落盘；补偿失败停在 `pending` 的中间态也会被完整持久化，
  重启后可继续重试。

## HTTP 接口

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| `POST` | `/api/v1/auctions` | 创建拍卖 |
| `GET` | `/api/v1/auctions/{id}` | 查询拍卖与状态 |
| `POST` | `/api/v1/auctions/{id}/bids` | 提交/幂等重放报价（新建 201，重放 200） |
| `GET` | `/api/v1/auctions/{id}/bids/{ref}` | 查询单份报价 |
| `DELETE` | `/api/v1/auctions/{id}/bids/{ref}` | 撤回报价（重复撤回报原状态） |
| `POST` | `/api/v1/auctions/{id}/clear` | 清算（首次 201，重复 200 + `replayed`） |
| `GET` | `/api/v1/auctions/{id}/result` | 查询成交结果 |
| `POST` | `/api/v1/auctions/{id}/settle` | 生成首版结算（首次 201，重复 200 + `replayed`） |
| `GET` | `/api/v1/auctions/{id}/settlement` | 查询当前有效结算版本 |
| `POST` | `/api/v1/auctions/{id}/fills/{ref}/confirm` | 成交确认（幂等） |
| `POST` | `/api/v1/auctions/{id}/fills/{ref}/freeze` | 资金冻结（须先确认，幂等） |
| `POST` | `/api/v1/auctions/{id}/fills/{ref}/deliver` | 增量交割 `{"quantity": n}` |
| `POST` | `/api/v1/auctions/{id}/corrections` | 提交/幂等重放更正申请（201/200） |
| `GET` | `/api/v1/auctions/{id}/corrections` | 列出本批次全部更正 |
| `GET` | `/api/v1/auctions/{id}/corrections/{cid}` | 查询单张更正（含补偿、审计事件） |
| `GET` | `/api/v1/auctions/{id}/corrections/{cid}/trace` | 更正链路追溯（三种情形结构不同） |
| `POST` | `/api/v1/auctions/{id}/corrections/{cid}/approve` | 批准更正（首次 201，重复 200 + `replayed`） |
| `POST` | `/api/v1/auctions/{id}/corrections/{cid}/retry-compensation` | 重试待处理补偿 |

金额字段为十进制字符串，时间字段为 RFC3339。示例：

```bash
curl -s localhost:8080/api/v1/auctions -d '{
  "available_qty": 25,
  "deadline": "2026-09-28T12:00:00Z",
  "reserve_price": "5.00"
}'

curl -s localhost:8080/api/v1/auctions/<id>/bids -d '{
  "external_ref": "ORD-1", "bidder": "alice",
  "quantity": 20, "unit_price": "10.00"
}'

curl -s -X POST localhost:8080/api/v1/auctions/<id>/clear

# 生成结算，然后确认/冻结/交割
curl -s -X POST localhost:8080/api/v1/auctions/<id>/settle
curl -s -X POST localhost:8080/api/v1/auctions/<id>/fills/ORD-1/confirm
curl -s -X POST localhost:8080/api/v1/auctions/<id>/fills/ORD-1/freeze
curl -s -X POST localhost:8080/api/v1/auctions/<id>/fills/ORD-1/deliver \
  -d '{"quantity": 20}'

# 提交成交更正（已交割成交会自动转为补偿单）
curl -s -X POST localhost:8080/api/v1/auctions/<id>/corrections -d '{
  "correction_id": "COR-20260930-001",
  "reason": "行情源录入了错误成交价",
  "basis": "feed-v2-reconciliation-2026-09-30",
  "handler": "risk-officer-1",
  "items": [
    {"external_ref": "ORD-1", "target_unit_price": "9.50"}
  ]
}'

# 批准、重试补偿、追溯
curl -s -X POST localhost:8080/api/v1/auctions/<id>/corrections/COR-20260930-001/approve
curl -s -X POST localhost:8080/api/v1/auctions/<id>/corrections/COR-20260930-001/retry-compensation
curl -s localhost:8080/api/v1/auctions/<id>/corrections/COR-20260930-001/trace
```

### 错误类别与状态码

错误体形如 `{"error":{"kind":"...","message":"..."}}`，明确区分：

| kind | HTTP | 典型场景 |
| --- | --- | --- |
| `invalid_argument` | 400 | 参数缺失、数量/价格非正、金额或时间格式错误 |
| `not_found` | 404 | 拍卖/报价/清算结果不存在 |
| `conflict` | 409 | 同一外部报价号内容不一致；拍卖 ID 冲突 |
| `invalid_state` | 422 | 截止后报价/撤回、未截止就清算、对已清算拍卖写操作（含撤回撞清算）、未确认先冻结、超量交割、对非待处理更正重试补偿 |

更正相关补充：同更正号批次/版本/内容变化也是 `conflict`（409，
`correction ... conflicts ...` / `... superseded settlement version`）；
补偿结算失败不是接口错误——批准正常返回，但更正 `status` 为 `pending`、
对应补偿单 `status` 为 `pending` 且带 `failure_note`。

幂等重放不是错误，返回 200 并带 `"replayed": true`。

## 运行与测试

```bash
# 启动服务（文件持久化，默认目录 ./data，监听 :8080）
go run ./cmd/auctiond

# 纯内存存储 / 自定义地址
AUCTION_DATA_DIR=memory ADDR=:9090 go run ./cmd/auctiond

# 全部测试（含竞态检测）
go test -race ./...
```

测试覆盖：金额精确运算与序列化、排序/部分成交/保留价/撤回排除/不超卖、
报价幂等与冲突、截止边界（含恰好等于截止点）、撤回撞清算的两种确定性交错、
32 路并发清算恰好成交一次、文件持久化与重启恢复、失败事务无残留、HTTP 全链路。

成交更正测试覆盖：首版结算幂等、确认/冻结/交割顺序与超量交割拒绝、
更正申请快照（清单/价格版本/结算状态）与参数校验、
同号幂等重放与批次/版本/内容冲突、基准版本过期冲突、
未交割直接改价且只改允许字段、已交割全额补偿、部分交割部分补偿、
上调价格的负差额补偿、价格未变仍写审计事件且不产生新版本、
补偿结算失败停在 pending 且原成交可查、重试后完成、
连续更正的版本链与旧版本追溯、确认/冻结/批准并发的唯一有效结果（含竞态检测）、
更正状态与补偿的文件持久化及重启恢复/重试、更正 HTTP 全链路。
