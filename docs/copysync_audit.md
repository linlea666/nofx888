# CopySync 扩展验收摘要

## 结论
- CopySync 服务已引入价格兜底重试、基线定期刷新与持仓对账逻辑，整体行为与原有跟单流程兼容且可持续运行。
- 跟单执行侧新增数量格式化回写与防超量平仓截断，配合订单日志补偿（TraceID 临时单标记不可同步），减轻订单漂移与审计缺口。
- 订单同步模块支持重试与不可同步 ID 跳过，结合错误码填充，降低同步阻塞和误报。

## 发现的问题/风险
- followerHasPosition 仅根据当前同向仓位跳过新开/加仓，未记录“领航员已平仓后重新开”的时序，长时间运行仍可能因残留同向仓导致漏单。
- parsePosition 依赖符号/posSide/positionSide 并以 positionAmt/size 解析，若交易所返回为空或字段名再变化，仍可能得到空 symbol 与 size=0，导致 close 截断/对账漏掉异常场景。
- logOrder 将 skip_reason 与错误码复用字段保存，虽然 ErrCode 区分了 unsyncable_order_id/status_query_failed 等，但前端若需结构化展示命中阈值（minHit/maxHit）仍需额外解析。

## 优化建议
- 为 followerHasPosition/handleFollowerPositions 增加“基线对账后允许再次开仓”的节奏控制，例如记录上次领航员平仓时间戳，超过窗口后允许重新开仓，并对反向残留增加对账确认。
- parsePosition 应在字段缺失时回传错误或在上层增加保护日志，避免 nil/类型断言失败静默导致截断失效；可针对 OKX/HL/币安的返回结构增加适配测试。
- 订单日志建议增加独立的 min_hit/max_hit 列或结构化字段，减少 skip_reason 复用造成的歧义。

## 代码参考
- 价格兜底重试、基线刷新与持仓对账：`copysync/service.go` 中 handleEvent、retrySnapshot、refreshBaselineLoop 和 reconcileFollowerPositions 等逻辑。 【F:copysync/service.go†L155-L347】
- 数量格式化回写、防超量平仓与临时 order_id 标记：`copysync/trader_executor.go` 的 ExecuteCopy/close/logOrder。 【F:copysync/trader_executor.go†L52-L238】
- 订单同步重试、不可同步 ID 跳过与错误码：`trader/order_sync.go` 的 syncOrders/syncTraderOrders/syncSingleOrder/markOrderFilled。 【F:trader/order_sync.go†L66-L264】
