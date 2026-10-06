# go-auction-clearing

用于承载多单位竞价与成交管理相关的 Go 服务代码。

开发环境：Go 1.23.0。

运行测试：

    go test ./...

## 功能概览

在清算成交与结算版本更正之上，本包补充了成交后的**交割批次与保证金结算**：

- 清算完成后创建交割批次，冻结成交清单、计划交割数量、价格与结算版本、截止时间；
- 按成交逐笔登记交割确认与保证金结果，支持部分交割与明确取消；
- 交割确认、成交更正、批次关闭并发时依据同一结算版本裁决；
- 每个交割确认/取消携带外部操作号，幂等重放；
- 批次视图查询与 JSON 快照持久化、崩溃恢复。

## 领域模型

- `Auction`：一场竞价，`SettlementVersion` 为结算版本号，每次成交更正递增。
- `Trade`：一笔成交，`DeliveredQuantity` 记录累计已交割数量。
- `Correction`：一次成交更正，生效后拍卖结算版本递增。
- `DeliveryBatch` / `BatchItem`：交割批次及冻结的成交项（数量、价格在建批时固定）。
- `DeliveryConfirmation` / `DeliveryCancellation`：逐笔交割确认与剩余数量取消，`OpID` 为外部操作号。
- `MarginRecord`：保证金台账记录，只随确认/取消在同一临界区内原子产生，不会出现孤立记录。

## 核心流程

1. `CreateAuction` → `RecordTrade` → `ClearAuction`：登记竞价、成交并完成清算。
2. `CreateBatch(auctionID, tradeIDs, deadline)`：清算完成后建批。一笔成交只能属于一个
   未完成批次；已全部交割的成交不能再次加入新批次。批次冻结当时的剩余数量、价格与
   结算版本。
3. `ConfirmDelivery(opID, batchID, tradeID, qty, margin)`：逐笔登记实际交割数量与保证金
   结果。部分交割只减少剩余数量（成交项置为 `PARTIAL`），数量补足后才置为 `COMPLETED`；
   超出剩余数量的确认被拒绝。
4. `CancelDelivery(opID, batchID, tradeID, reason, margin)`：明确取消成交项的剩余未交割
   数量（置为 `CANCELLED`），是更正淘汰批次后的清理路径。
5. `CloseBatch(batchID)`：只有所有成交项都 `COMPLETED` 或 `CANCELLED` 后才能关闭。

## 并发裁决

所有变更在 `Store` 的同一把互斥锁内完成校验、状态迁移与原子落盘：

- 更正先生效：拍卖结算版本递增，旧批次冻结版本过期，后续确认返回 `ErrVersionConflict`；
- 批次先关闭：迟到的确认/取消返回 `ErrBatchClosed`；
- 两种冲突都不会产生孤立的保证金记录——保证金记录只与确认/取消在同一临界区内写入。

## 幂等

确认与取消共用外部操作号空间：

- 同号同内容重放返回原结果，不重复扣减数量、不重复生成交割与保证金记录；
- 同号但成交、数量或保证金内容变化返回 `ErrIdempotencyConflict`；
- 失败重试因此是安全的：重发同一操作号即可。

## 查询

`GetBatchView(batchID)` 返回批次状态、是否过期（`Stale`）、当前结算版本、每笔成交的
计划/已交割/剩余数量、保证金处理结果，以及关联的成交更正。`ListBatches(auctionID)`
列出场次下的全部批次。

## 持久化

`NewStore(path)` 指定快照路径后，每次变更先写临时文件再 rename 原子落盘；
`LoadStore(path)` 用于崩溃恢复，恢复后幂等重放与版本裁决行为不变。

## 测试

    go test -race ./...

覆盖：部分交割、批次关闭竞态、更正与确认竞态、迟到确认、幂等重放、持久化恢复与批次视图查询。
