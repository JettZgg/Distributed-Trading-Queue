## Worker 服务的 NACK 场景与处理逻辑

在本项目中，所有 Worker 服务都订阅 `OrderPlacedEvent`。它们在处理消息时，除了成功处理并发送 ACK 外，还需要考虑处理失败的情况并发送 NACK 或 Terminate 信号。

**通用 NACK/Terminate 场景 (适用于所有 Worker):**

1.  **无法获取消息元数据:**
    * **场景:** 调用 `msg.Metadata()` 失败。这通常是基础通信问题。
    * **处理逻辑:**
        * 记录错误日志。
        * 发送 `msg.Nak()`: 立即让消息对其他消费者可见以进行重试。因为这可能是暂时的网络或服务器问题。
2.  **消息反序列化失败 (无法解析为 `OrderPlacedEvent`):**
    * **场景:** `json.Unmarshal(msg.Data, &event)` 失败。这通常表明消息格式错误或损坏，很可能是永久性问题。
    * **处理逻辑:**
        * 记录错误日志，包含消息的原始数据（部分）以便排查。
        * 发送 `msg.Term()`: 终止这条消息，因为它很可能永远无法被正确处理，避免"毒丸消息"阻塞队列。
3.  **发送 ACK 失败:**
    * **场景:** 业务逻辑已成功处理，但在调用 `msg.Ack()` 时发生错误（例如，与NATS服务器连接中断）。
    * **处理逻辑:**
        * 记录错误日志。
        * **不需要显式发送 NACK 或 Term。** NATS JetStream 的 `AckWait` 机制会处理这种情况。当消息的 `AckWait` 超时后，JetStream 会自动认为该消息未被成功确认，并会将其重新投递。确保配置了合理的 `AckWait` 时间和 `MaxDeliver` 次数。

---

### 1. `InventoryWorker` (库存工作服务)

* **核心职责:** 模拟检查和更新商品库存。

* **特定 NACK/Terminate 场景:**

    * **场景 1: 瞬时库存服务不可达 (模拟)**
        * **描述:** 模拟在尝试"更新库存"时，依赖的外部库存服务（或数据库）暂时不可用（例如，网络超时、服务临时维护）。
        * **模拟方法:** 在业务逻辑中通过随机数或特定条件模拟此错误。
        * **处理逻辑:**
            1.  记录错误日志，说明是瞬时错误。
            2.  调用 `msg.NakWithDelay(delay duration)`: 例如，延迟 5-30 秒后重试。这给了下游服务恢复的时间。
            3.  如果 `NakWithDelay` 调用失败，记录错误。
        * **NATS JetStream 行为:** 消息将在指定的延迟后重新对消费者可见。如果配置了 `MaxDeliver`，多次此类 NACK 后消息可能会根据策略被丢弃或（如果未来实现）进入死信队列。

    * **场景 2: 商品 ID 不存在 (永久性业务错误 - 模拟)**
        * **描述:** `OrderPlacedEvent` 中的某个 `itemID` 在"库存系统"中查询不到。这是一个明确的业务逻辑错误，重试通常无效。
        * **模拟方法:** 在业务逻辑中检查 `event.Items` 中的 `itemID`，如果包含特定"无效"ID 则触发此场景。
        * **处理逻辑:**
            1.  记录严重错误日志，指明是无效的商品ID。
            2.  调用 `msg.Term()`: 终止此消息，因为它无法被正确处理。
            3.  如果 `Term` 调用失败，记录错误。
        * **NATS JetStream 行为:** 此消息将不再被投递给这个消费者组。

    * **场景 3: 库存不足，无法预留/扣减 (永久性业务错误 - 模拟)**
        * **描述:** 商品存在，但库存数量不足以满足订单需求。这也是一个业务规则层面的失败，对于当前订单而言，简单重试可能无法解决（除非有并发的补货操作，但这超出了当前模型的简化范围）。
        * **模拟方法:** 维护一个简单的内存库存（或文件），检查库存是否足够。
        * **处理逻辑 (可选策略):**
            * **策略 A (推荐用于本项目):** 视为永久性失败。
                1.  记录错误日志，说明库存不足。
                2.  调用 `msg.Term()` 终止消息。
                3.  (在真实系统中，可能还会发布一个"订单因库存不足失败"的事件供其他系统处理，如通知用户、客服介入等)。
            * **策略 B (更复杂，本项目可不实现):** 短期内 Nak几次，期望有补货。
                1.  记录警告日志，说明库存不足，将进行有限次重试。
                2.  调用 `msg.NakWithDelay()` 进行几次带延迟的重试。
                3.  如果多次重试后仍然库存不足，最终调用 `msg.Term()`。
        * **NATS JetStream 行为:** 根据所选策略，消息被终止或在几次重试后终止。

---

### 2. `NotificationWorker` (通知工作服务)

* **核心职责:** 模拟向买家和卖家发送订单确认通知。

* **特定 NACK/Terminate 场景:**

    * **场景 1: 瞬时通知服务不可达 (模拟)**
        * **描述:** 模拟连接外部邮件服务器、短信网关或推送服务时发生临时性网络故障或服务超时。
        * **模拟方法:** 随机数或特定条件触发。
        * **处理逻辑:**
            1.  记录错误日志。
            2.  调用 `msg.NakWithDelay(delay duration)`: 给予通知服务恢复的时间。
            3.  如果 `NakWithDelay` 调用失败，记录错误。
        * **NATS JetStream 行为:** 消息延迟后重投。

    * **场景 2: 用户联系方式无效或用户拒绝接收通知 (永久性业务错误 - 模拟)**
        * **描述:** `OrderPlacedEvent` 中的 `UserID` 对应的用户没有有效的邮箱/手机号，或者用户设置了"拒收此类通知"。
        * **模拟方法:** 检查 `UserID` 是否为某个特定"无效"用户。
        * **处理逻辑:**
            1.  记录错误/警告日志。
            2.  调用 `msg.Term()`: 此订单的此类型通知无法发送，无需重试。
            3.  (真实系统中，可能会记录此失败状态，但不再尝试发送此特定通知)。
        * **NATS JetStream 行为:** 消息被终止。

    * **场景 3: 通知模板渲染失败 (技术性但可能因数据导致永久失败 - 模拟)**
        * **描述:** 尝试使用订单数据渲染通知模板时发生错误，例如模板文件缺失（不太可能在容器化后发生），或订单数据中缺少模板必需的字段导致渲染引擎恐慌。
        * **模拟方法:** 检查 `event` 中的某些字段，如果缺失则模拟渲染失败。
        * **处理逻辑:**
            1.  记录严重错误日志。
            2.  调用 `msg.Term()`: 如果模板渲染依赖的数据持续有问题，重试也无效。
        * **NATS JetStream 行为:** 消息被终止。

---

### 3. `PaymentSimulatorWorker` (支付模拟工作服务)

* **核心职责:** 模拟支付处理流程。

* **特定 NACK/Terminate 场景:**

    * **场景 1: 瞬时支付网关连接失败或超时 (模拟)**
        * **描述:** 模拟调用"第三方支付网关"API 时发生网络超时或网关返回"系统繁忙"等临时性错误。
        * **模拟方法:** 随机数或特定条件触发。
        * **处理逻辑:**
            1.  记录错误日志。
            2.  调用 `msg.NakWithDelay(delay duration)`: 支付操作通常对时间敏感，但瞬时网络问题值得重试。延迟时间不宜过长。
            3.  如果 `NakWithDelay` 调用失败，记录错误。
        * **NATS JetStream 行为:** 消息延迟后重投。

    * **场景 2: 支付明确被拒绝 - 例如余额不足、风控拒绝 (永久性业务错误 - 模拟)**
        * **描述:** "支付网关"明确返回支付失败，原因为用户余额不足、信用卡无效、触发风控规则等。这些通常不是通过简单重试就能解决的。
        * **模拟方法:** 检查 `event.UserID` 或 `event.TotalAmount` 是否满足特定"失败"条件。
        * **处理逻辑:**
            1.  记录错误日志，详细记录支付失败原因。
            2.  调用 `msg.Term()`: 终止此消息的支付处理。
            3.  (真实系统中，会发布一个 `OrderPaymentFailedEvent`，并更新订单状态，可能触发通知用户等后续流程)。
        * **NATS JetStream 行为:** 消息被终止。

    * **场景 3: 订单金额无效 (例如为零或负数 - 业务规则校验)**
        * **描述:** `OrderPlacedEvent` 中的 `TotalAmount` 不符合业务规则。
        * **模拟方法:** 检查 `event.TotalAmount`。
        * **处理逻辑:**
            1.  记录严重错误日志。
            2.  调用 `msg.Term()`: 这是无效订单数据，不应进行支付。
        * **NATS JetStream 行为:** 消息被终止。

---

### 4. `InventoryWorker` NACK/Term 实际代码修改与测试指引

基于前述NACK场景理论，我们对 `cmd/inventoryworker/main.go` 进行了具体修改，关键点包括引入 `nats.MaxDeliver(3)` 防止无限重试，以及在特定错误场景下调用 `msg.Term()` 或 `msg.NakWithDelay()`。

**主要代码变更点回顾:**

1.  **`AppConfig` 中增加 `MaxDeliver` 字段并初始化为 `3`。**
2.  **`js.Subscribe` 调用时添加 `nats.MaxDeliver(appConfig.MaxDeliver)` 订阅选项。**
    ```go
    // AppConfig 定义
    type AppConfig struct {
        // ... 其他字段
        MaxDeliver   int
    }

    // appConfig 初始化
    appConfig = AppConfig{
        // ... 其他配置 ...
        MaxDeliver:   3, // 设置 MaxDeliver
    }

    // js.Subscribe 调用
    sub, err := js.Subscribe(appConfig.Subject, func(msg *nats.Msg) {
        // ... 消息处理逻辑 ...
    },
    nats.Durable(appConfig.ConsumerName),
    nats.ManualAck(),
    nats.AckWait(appConfig.AckWait),
    nats.MaxDeliver(appConfig.MaxDeliver)) // <--- 应用 MaxDeliver 设置
    ```
3.  **消息处理回调函数 (`func(msg *nats.Msg)`) 内的逻辑调整:**
    *   **获取元数据失败 (`msg.Metadata()`):** 保持调用 `msg.Nak()`。
    *   **消息反序列化失败 (`json.Unmarshal`):**
        *   日志级别调整，明确提示 "Terminating message."。
        *   调用 `msg.Term()` 终止消息。
    *   **`InventoryWorker` 特定错误场景模拟:**
        *   **瞬时库存服务不可达 (通过 `event.UserID == "user_transient_error"` 模拟):**
            *   调用 `msg.NakWithDelay(5 * time.Second)`。
            *   日志中会包含 `meta.NumDelivered` 以显示当前投递次数。
        *   **商品 ID 不存在 (通过检查 `item.ItemID == "ITEM_INVALID"` 模拟):**
            *   调用 `msg.Term()`。
            *   日志中会包含 `meta.NumDelivered`。
        *   **库存不足 (通过 `event.UserID == "user_stock_issue"` 模拟, 策略A):**
            *   调用 `msg.Term()`。
            *   日志中会包含 `meta.NumDelivered`。
    *   **日志增强:** 在各个关键日志点（如接收消息、处理、各错误场景、ACK/NACK/Term）添加 `meta.NumDelivered` 的打印，便于追踪消息的投递状态。

**具体测试方法 (使用 `curl` 和 `nats-cli`):**

在进行测试前，请确保 `OrderService` (`cmd/orderservice/main.go`) 和已修改的 `InventoryWorker` (`cmd/inventoryworker/main.go`) 均已启动并正在运行。

1.  **测试场景: 瞬时错误 (`user_transient_error`) 与 `MaxDeliver(3)` 效果**
    *   **操作:** 向 `OrderService` 发送特定请求，触发 `InventoryWorker` 的瞬时错误模拟。
        ```bash
        curl -X POST -H "Content-Type: application/json" -d '{ 
          "userID": "user_transient_error", 
          "items": [{"itemID": "ITEM_OK", "quantity": 1, "price": 10}] 
        }' http://localhost:8080/api/orders
        ```
    *   **预期 `InventoryWorker` 日志行为:**
        *   首次接收消息: 日志显示 `NumDelivered: 1`，模拟瞬时错误，调用 `NakWithDelay(5s)`。
        *   约5秒后再次接收同一消息: 日志显示 `NumDelivered: 2`，再次模拟瞬时错误并 `NakWithDelay(5s)`。
        *   约5秒后第三次接收同一消息: 日志显示 `NumDelivered: 3`，最后一次模拟瞬时错误并 `NakWithDelay(5s)`。
        *   此后，由于 `MaxDeliver(3)` 的限制，该消息将不再被NATS JetStream投递给此消费者。`InventoryWorker` 将能够处理后续发送的新消息。
    *   **(可选) 验证NATS Consumer状态:**
        ```bash
        nats consumer info ORDERS INVENTORY_WORKER
        ```
        观察 `num_waiting` (等待处理的消息数) 和 `num_ack_pending` (已投递但未ACK/NACK/Term的消息数)。在消息达到`MaxDeliver`后，相关计数应有所变化，表明消息不再被视为待处理或等待ACK。

2.  **测试场景: 无效商品ID (`ITEM_INVALID`) - 消息终止 (`Term`)**
    *   **操作:** 发送包含特定 "无效" ItemID 的订单请求。
        ```bash
        curl -X POST -H "Content-Type: application/json" -d '{ 
          "userID": "user_normal", 
          "items": [{"itemID": "ITEM_INVALID", "quantity": 1, "price": 10}] 
        }' http://localhost:8080/api/orders
        ```
    *   **预期 `InventoryWorker` 日志行为:**
        *   日志显示接收到消息 (例如, `NumDelivered: 1`)。
        *   日志显示检测到无效 `ItemID`，并记录 "Simulating invalid ItemID (...) Terminating message."。
        *   消息被 `Term()` 后，将不再被此消费者接收或重试。

3.  **测试场景: 库存不足 (`user_stock_issue`) - 消息终止 (`Term`)**
    *   **操作:** 发送特定用户ID的订单，模拟库存不足。
        ```bash
        curl -X POST -H "Content-Type: application/json" -d '{ 
          "userID": "user_stock_issue", 
          "items": [{"itemID": "ITEM_OK", "quantity": 1, "price": 10}] 
        }' http://localhost:8080/api/orders
        ```
    *   **预期 `InventoryWorker` 日志行为:**
        *   日志显示接收到消息。
        *   日志显示检测到库存不足，并记录 "Simulating insufficient stock (...) Terminating message."。
        *   消息被 `Term()` 后，不再重试。

4.  **测试场景: 消息反序列化失败 - 消息终止 (`Term`)**
    *   **操作:** 使用 `nats-cli` 直接向 `ORDERS.placed` 主题发布一条格式错误（非预期JSON结构）的消息。
        ```bash
        nats pub ORDERS.placed "{ \"orderID\": \"malformed_payload_test\", \"userID\": \"test_user\", \"items\": \"this_is_not_an_array_of_items\" }"
        ```
        (注意：上述 `nats pub` 命令中的JSON字符串内部的引号需要转义。)
    *   **预期 `InventoryWorker` 日志行为:**
        *   日志显示接收到消息。
        *   日志显示 "Error unmarshalling message data: ..." 错误，并记录 "Terminating message."。
        *   消息被 `Term()` 后，不再重试。

通过以上步骤，可以验证 `InventoryWorker` 中针对不同错误场景的NACK和Terminate处理逻辑，以及 `MaxDeliver` 机制的有效性。

通过上述设计，你的 Worker 服务将能更健壮地处理各种预期的和意外的错误情况。