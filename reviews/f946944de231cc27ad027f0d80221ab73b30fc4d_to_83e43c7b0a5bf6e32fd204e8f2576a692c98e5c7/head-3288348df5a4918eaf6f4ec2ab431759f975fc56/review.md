# mosdns-routeros 第二轮审查修复归档

## 追溯信息

- 审查范围：`f946944de231cc27ad027f0d80221ab73b30fc4d^..83e43c7b0a5bf6e32fd204e8f2576a692c98e5c7`
- 修复提交：`3288348df5a4918eaf6f4ec2ab431759f975fc56` — `routeros: close reconciliation and ownership gaps`
- 实现 diff SHA-256：`6bd2b8f46b402567b4cb5c39f7afcf7fe65b7b84f01a2c34f2a86d9a6d07a025`
- 实现依据：`mosdns_routeros_plugin_c0119527.plan`
- 审查边界：完全排除 `third_party/**` 及其维护说明
- 结果：7 项问题全部修复，0 个已知未关闭项

## 修复结果

### 1. [高] reconciliation 与 Learn/Add 交错产生重复路由

- 为 miss 路径增加覆盖完整 `Learn + Apply` 的读屏障，Sync/Flush/Close 使用独占屏障。
- FastHit 在屏障外，重复命中路径没有新增锁、分配或队列操作。
- `wait:false` 任务只在 worker 真正执行时取得屏障，不让队列积压阻塞重连 Sync。
- 新增阻塞 List 的确定性交错测试，验证 Sync 快照与 Add 不再交叉。

### 2. [中] 首次对账吞掉修复操作失败

- `syncResult` 保留首个具体错误；只有 List 与全部修复操作成功后才设置 `firstSynced`。
- 初始连接上的确定性 RouterOS trap 使 Init fail fast，且失败清理不会删除既有 comment 路由。
- 延迟连接或运行期修复失败使用 1s 到 30s 的退避重试；transport 失败仍由 reconnect 驱动。

### 3. [中] retry-set 把所有 trap 当作 not-found

- 仅 `no such item` 清除 stale id 并重新 Add。
- 其他 trap 保留 `.id` 与 offline 状态，避免权限错误后重复 Add 并丢失旧路由所有权。

### 4. [中] 默认 comment 跨插件碰撞

- 默认值从全局 `mosdns` 改为 `mosdns:<plugin-tag>`。
- README 要求同一路由器上的每个管理实例使用唯一 comment，并记录旧默认值迁移方法。

### 5. [中] HTTP 管理接口无访问保护

- 未配置 `api_token` 时只接受 loopback 客户端。
- 配置 token 后要求大小写兼容的 Bearer 认证。
- `POST /flush` 额外要求 `X-Mosdns-Confirm: flush`，降低跨站误触发风险。
- README 改为正确的核心 `api.http` 配置，并要求远程访问使用 loopback 上的 HTTPS 反向代理、token 和防火墙。

### 6. [低] static Set-not-found 保留虚假 synced 状态

- static 和 dynamic 在 Sync 的 Set 返回 not-found 时立即重新 Add。
- static 只有 Set/Add 成功后才标记 synced；其他错误明确保留未同步状态。

### 7. [低] README 指向错误的 HTTP server

- 文档已从 `http_server` 插件修正为核心 `api.http`，并加入认证与 flush 示例。

## 自动验证

- `go test -race ./...`：通过。
- `go vet ./...`：通过。
- 关键并发、错误注入、API 安全测试在 race 模式下重复 50 次：通过。
- `git diff --check`：通过。
- FastHit 修复前：`5.088–5.111 ns/op`，`0 B/op`，`0 allocs/op`。
- FastHit 最终：`5.087–5.129 ns/op`，`0 B/op`，`0 allocs/op`。
- 结论：差异处于测量波动内，没有可测性能下降。

## RouterOS 实机 E2E

- 管理链路固定使用 `enx00e04c680028`，源地址 `192.168.88.100/24`，未修改本机网关。
- 分别验证 `wait:true` 与 `wait:false`。
- 只创建 `198.51.100.200/32`、distance 250、独立 comment 的临时测试路由。
- 两种模式均得到 `active=true`、`hw-offloaded=true`、`immediate-gw=192.168.5.1%vlan5`。
- 两次均通过 SIGTERM clean；测试 comment 下最终剩余 0 条路由。
- 测试前、测试中、测试后，`144.34.229.181` 的 RouterOS 最长前缀选择始终为 `0.0.0.0/0 via 192.168.5.1%vlan5`；没有创建、修改或删除该目的地址的专用路由。
