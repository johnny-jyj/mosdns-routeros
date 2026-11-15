# mosdns-routeros 代码审查归档

## 追溯信息

- 被审查范围：`f946944de231cc27ad027f0d80221ab73b30fc4d^..4dfa312f7e20759baa65a711e51c4077e3467726`
- 范围基点（exclusive）：`9cfb7ce985599c087cb7ccfb1531d0c0f4021242`
- 首个被审查提交：`f946944de231cc27ad027f0d80221ab73b30fc4d`
- 被审查 HEAD：`4dfa312f7e20759baa65a711e51c4077e3467726`
- 最终修复 HEAD：`efedd59a7992d7af4a3f151ff83cf374571367f9`
- 修复提交：
  - `624eb2f712e6100b406caa79e08781f39beb8e39` — `routeros: harden async lifecycle and reconciliation`
  - `efedd59a7992d7af4a3f151ff83cf374571367f9` — `routeros: close remaining timeout races`
- 实现依据：`mosdns_routeros_plugin_c0119527.plan`
- 重点目录：`plugin/executable/routeros/`
- 状态：**已关闭；15 个有效问题全部修复，0 个已知未关闭项**

机器可读的完整 commit 关联见同目录 `metadata.yaml`。

## 最终结论

本次审查最初发现 12 项问题，修复后的后置并发复核又确认 3 项有效遗漏。全部问题均已修复，并通过完整 race/vet、失败注入、确定性交错重复测试、零分配性能基准和 RouterOS 实机 E2E。

最终实现仍符合原 plan 的核心约束：

- 单持久 Async RouterOS API 连接；
- IPv4-only，路由以 comment 标识归属；
- 启动、重连、Flush 和退出时进行确定性 reconciliation；
- 没有周期定时器；
- 动态路由以 CLOCK 近似 LRU 控制上限；
- 重复命中 fast path 不分配。

当前审查范围内没有已知未关闭问题。该结论不表示对未来代码或审查范围外组件作绝对无漏洞保证。

## 依赖覆盖维护要求

根 `go.mod` 中的 `replace github.com/go-routeros/routeros/v3 => ./third_party/go-routeros` 是**临时本地覆盖**。实际构建使用 `third_party/go-routeros`，而不是 `require` 行中的版本；因此只修改 require 或运行常规依赖升级不会同步 go-routeros 上游。

保留本地副本的原因是：上游 v3.0.1 尚无 Async 修复，未合并的 PR #32 也没有包含本次故障注入发现的 writer/client 锁序、Close 解阻塞和 Cancel/Close 竞态修复。Go pseudo-version 只能引用远程仓库中已存在的 commit，不能引用这些仅存在于本项目的补丁。

以后每次检查或升级 go-routeros 都必须：

1. 检查 upstream tag/HEAD 与 PR #32 的状态。
2. 对照 `third_party/go-routeros/PATCHES.md` 逐项判断补丁已合并、仍需要或已废弃。
3. 只迁移仍需要的补丁，禁止直接用上游源码覆盖本地目录。
4. 重跑依赖 race 测试、项目失败注入/并发测试、FastHit benchmark 和 RouterOS 实机 E2E。
5. 补丁发布到远程 fork 后，优先改用 fork pseudo-version；上游包含全部修复后，必须删除本地 `replace`。

该检查不是可选清理项，而是依赖升级的阻断条件。

## Change Description

以下逐项记录问题原因、修复方法和验证结果。

### 1. [高] Async 快速回复竞态与 goroutine 泄漏

- 原因：go-routeros v3.0.1 先发送 sentence 再注册 tag，快速回复可能在查不到 handler 时被丢弃；单请求取消还会取消共享 reader。
- 方法：固定 upstream PR #32 的最小修复并纳入仓库内依赖；发送前注册 tag，单请求取消只注销自身 tag，连接使用可取消 `AsyncContext`。
- 结果：即时回复循环、请求取消后继续复用连接、Async 结束后的 writer 行为均通过 race 测试。

### 2. [高] API 命令没有连接级超时

- 原因：DNS 的 wait timeout 只释放 DNS 请求，不会终止后台 API 命令；静默连接可永久占住 worker 和 inflight 条目。
- 方法：所有命令增加 context deadline 和 generation watchdog；deadline 到期时仅摘除并关闭对应 generation，由唯一 watcher reconnect 后 sync。
- 结果：无回复、发送阶段阻塞和 writer 排队均能在 deadline 后返回；不会产生重复 reconnect loop。

### 3. [高] Close/Flush 与在途 Apply 竞态

- 原因：清理可能早于迟到 add 完成，导致 `on_exit: clean` 返回后又出现持久路由。
- 方法：以 `lifeMu + opsWG + controlMu` 建立任务登记和生命周期屏障；暂停生产者、等待在途操作、回收 late orphan，再做最终 sweep。
- 结果：wait/async 模式下并发 Close、Flush 和 Apply 的确定性交错及 race 测试通过。

### 4. [中] 首次失败 sync 错误消耗 adopt

- 原因：首次 ListRoutes 失败后状态已被标成完成，下一次重连错误地执行 clean。
- 方法：只在成功列出并完成首次 reconciliation 后设置 `firstSynced`；失败时保留 start mode。
- 结果：首次失败、第二次连接成功时仍执行 adopt。

### 5. [中] Flush 未删除全部动态路由

- 原因：旧逻辑跳过 inflight 条目，Flush 成功返回后仍可能有动态路由。
- 方法：Flush 先建立操作屏障，再清空动态池、按 id 删除并执行 clean sync；迟到 add 立即回收。
- 结果：Flush 返回时动态池和远端测试路由均为 0。

### 6. [中] queue-full 破坏 pool/eviction 状态

- 原因：先调用 `Learn` 修改 pool，之后才发现异步队列已满，回滚会伪造 synced 状态并丢失淘汰路由的删除责任。
- 方法：在 `Learn` 前预留 `queueSlots`；队列满时直接返回，不修改 pool。
- 结果：queue-full、stale-id retry-set、eviction 和 hard-cap 回归测试通过。

### 7. [中] static 重叠 CIDR 选错 gateway

- 原因：按 gateway 分组查找不能表达 RouterOS 最长前缀优先级。
- 方法：静态配置完成后，按实际出现的 prefix length 做 IPv4 LPM。
- 结果：`/8 + /16` 跨 gateway 用例选择更具体前缀；热路径不新增分配。

### 8. [中] transport/write 错误不触发 reconnect

- 原因：旧逻辑只依赖 reader 报错，写方向失败时连接可能继续显示 connected。
- 方法：除确定性 `!trap` 外，命令错误均按 generation 原子失效连接，由 watcher 统一 reconnect。
- 结果：writer 失败和 reader 阻塞的故障注入能恢复并重新 sync。

### 9. [低] static 文件扫描错误被忽略

- 原因：`bufio.Scanner` 超长行错误没有检查，后续配置会静默丢失。
- 方法：设置明确 buffer 上限，并在扫描后检查 `sc.Err()`，错误包含文件名。
- 结果：超长行输入启动失败，不再静默接受。

### 10. [中] sync 忽略 routing table 和 distance

- 原因：仅比较 dst/gateway 会把错误路由表或管理距离误判成一致。
- 方法：ListRoutes 读取并规范化 `routing-table,distance`，reconciliation 使用完整属性匹配并修正。
- 结果：跨表、distance 变化和重复远端路由测试通过。

### 11. [中] `!fatal` 被误判为确定性设备拒绝

- 原因：go-routeros 的 `DeviceError` 同时包装 `!trap` 和 `!fatal`，旧逻辑会删除本应保留的 pool 条目。
- 方法：仅 `!trap` 视为命令级拒绝；`!fatal` 视为 session/transport failure，条目保留 offline 并失效连接。
- 结果：trap/fatal 分类、add/set/sync 错误路径测试通过。

### 12. [低] 配置边界缺少启动校验

- 原因：非法 address、distance、max_routes 等会退化为永久重试或运行期设备错误。
- 方法：Init 阶段校验 address（包括显式端口 hostname）、gateway、timeout、distance、max_routes、static 文件和策略枚举。
- 结果：边界表驱动测试通过，明显错误均 fail fast。

## 后置并发复核

### 13. [高] deadline 不覆盖发送阶段，Close 存在锁序阻塞

- 原因：`RunArgsContext` 在 sentence 发送完成后才等待 context；对端不读时 Flush 可永久阻塞。go-routeros 同时持有 client mutex 等待 writer，使 Close 无法自救。
- 方法：generation watchdog 在 deadline 主动关闭连接；tag 在拿 writer 前注册，但不持有 client mutex 等待或刷新 writer；Close 在发布 closing 后无锁关闭 I/O。
- 结果：对端完全不读且两个命令竞争 writer 时，命令和 Close 均按期返回；race 重复测试通过。

### 14. [中] replace-set stale id 永久伪 synced

- 原因：路由被外部删除后，replace set 的 `no such item` 会回滚旧 gateway 和 synced 状态，FastHit 此后不再修复。
- 方法：识别 not-found trap，清空 stale id 并立即重新 add 期望 gateway。
- 结果：replace 后路由存在、gateway 正确，随后查询恢复为 fast hit。

### 15. [中] Flush 暂停窗口丢失 reconnect sync

- 原因：paused 时 reconnect callback 被直接丢弃，Flush 自身失败后可能长期不再收敛。
- 方法：以 `reconnectPending` 合并记录暂停期间的连接事件，解除 Flush 屏障时补调一次 reconciliation。
- 结果：连接在 Flush 返回前恢复、Flush list 失败的确定性交错最终仍补齐 static 路由。

后置复核曾报告“显式端口 hostname 未校验”，核对最终 diff 后确认该项已由第 12 项修复并有 `bad/name:8728` 测试，因此不重复计数。

## 验证结果

- `go test -race ./...`：通过。
- `go vet ./...`：通过。
- 仓库内 go-routeros 修正版 race 测试：通过。
- 关键并发/失败注入测试重复 100 次：通过。
- IDE lints 与 `git diff --check`：通过。
- 原始热路径基线：约 `6.02 ns/op`，`0 B/op`，`0 allocs/op`。
- 最终 `FastHit`：`5.10–5.30 ns/op`，`0 B/op`，`0 allocs/op`。
- 结论：修复没有造成性能下降，重复命中热路径略有改善。

## RouterOS 实机验证

- 管理链路：`enx00e04c680028`，源地址 `192.168.88.100/24`，直连 `192.168.88.1:8728`。
- 安全范围：只操作 `198.51.100.0/24` 内带唯一测试 comment 的临时路由，并仅按 API 返回的 `.id` 删除。
- 顺序 100 条 `/32`：add p50 `23.97 ms`、p99 `57.94 ms`；remove p50 `21.59 ms`、p99 `66.01 ms`。
- 路由状态：`active=true`、`hw-offloaded=true`，实际下一跳 `192.168.5.1%vlan5`。
- comment 查询准确返回 100 条；测试结束后剩余 0 条。
- RouterOS 接受重复 `dst-address`，不会返回 trap；插件连续解析同一 A 记录两次后仅保留 1 条路由，证明池内去重有效。
- 完整 mosdns DNS→RouterOS E2E：`routeros-e2e.test` 返回 `198.51.100.200`，路由写入并硬件卸载；`SIGTERM` 后 clean 删除成功。
- `144.34.229.181` 保护检查：测试前后主表 LPM 唯一命中均为 `0.0.0.0/0 via 192.168.5.1%vlan5`，未创建、修改或删除该目的地址路由。

## 说明

本文件是该代码修复 HEAD 的权威审查归档。Git commit message 只保留摘要；修复原因、方法、结果和验证证据以本文件及 `metadata.yaml` 为准。
