# mosdns-routeros

基于原版 [mosdns](https://github.com/IrineSistiana/mosdns) 新增一个 `routeros` 插件：把 DNS 解析结果中的 IPv4 地址，通过 RouterOS 二进制 API 以 `/32` 静态路由写入 MikroTik 设备（目标是 CRS504 这类支持 L3 硬件卸载的交换机），下一跳为配置的 gateway。路由由交换芯片硬件转发，DNS 分流即路由分流。

原版 mosdns 的功能、配置方式见 [wiki](https://irine-sistiana.gitbook.io/mosdns-wiki/)。本仓库除 `plugin/executable/routeros/` 与本 README 外不改动上游代码，可直接合并上游更新。

## 工作方式

- 使用 [go-routeros v3](https://github.com/go-routeros/routeros) 建立**一条**持久 API 连接（8728，或 8729 TLS），`Async` 模式多路复用，所有命令复用该连接，没有每命令握手。
- 插件自己维护路由池：同一 IP 重复解析不再调用 API；同一 IP 出现不同 gateway 时打 warning，按 `conflict` 策略处理；被 `static` CIDR 覆盖的 IP 不重复添加。
- 所有路由带统一 `comment` 作为归属标记。启动时读取路由器上带该标记的路由并对账（接管 / 纠正 gateway / 补加 / 删除），退出时按 `on_exit` 清理，断线重连后自动再对账一次。
- 运行期间**不会**按时间删除动态路由（RouterOS 侧没有过期机制，插件也不做，避免与 cache 的 TTL 纠缠）。清理手段：退出、`POST /flush`、`max_routes` 上限触发的近似 LRU 淘汰，或定时重启 mosdns。
- 只处理 A 记录；AAAA 忽略并限频 warning（CRS 的 IPv6 L3HW 开销大，不建议使用）。
- 启动后做一次只读的 L3HW 体检并写日志，之后不再主动查询路由器。

## 配置

```yaml
plugins:
  - tag: ros
    type: routeros
    args:
      # --- 连接 ---
      address: 192.168.88.1:8728   # 省略端口时按 tls 自动用 8728 / 8729
      username: mosdns             # 建议专用用户，policy 至少 api,read,write
      password: "******"
      tls: false                   # true 走 API-SSL；只影响启动时的一次握手
      insecure_skip_verify: false  # tls=true 且自签证书时置 true
      timeout: 2s                  # 拨号/登录超时，也是 wait 模式下等待 add 的上限

      # --- 路由属性 ---
      gateway: 192.168.88.254      # 默认下一跳；$ros 不带参数时使用；必须是 IPv4 主机地址
      comment: mosdns              # 归属标记；多套 mosdns 指向同一路由器时请用不同值
      # routing_table: main        # 可省略
      # distance: 1                # 可省略

      # --- 动态路由池 ---
      max_routes: 40000            # 动态 /32 上限，超过按近似 LRU 淘汰（CRS504 host 表 64K，与 ARP 邻居共用）
      conflict: keep               # 同 IP 不同 gateway：keep 保留旧的 / replace 改为新 gateway；都打 warning
      wait: true                   # true：等路由器确认 add 再返回 DNS 应答（超时则直接返回）；false：后台队列异步下发
      on_exit: clean               # clean：退出时删除本插件的全部路由（含 static）；keep：留在路由器上
      on_start: adopt              # adopt：接管路由器上带标记的残留路由；clean：删除非 static 的残留

      # --- 静态路由：启动时添加、永不淘汰 ---
      static:
        - gateway: 192.168.88.254
          ips: ["1.1.1.1", "8.8.8.0/24"]
          files: ["/etc/mosdns/proxy_ip.txt"]   # 每行一个 IP/CIDR，# 注释；IPv6 行跳过
        - gateway: 192.168.88.253
          files: ["/etc/mosdns/game_ip.txt"]

  - tag: proxy_seq
    type: sequence
    args:
      - exec: $forward_remote
      - exec: $ros                 # 默认 gateway
      # - exec: $ros 192.168.88.253   # 本条 sequence 指定其他 gateway
```

校验规则：`address`、`username`、`gateway` 必填；`static[].gateway` 必填（不继承默认值）；同一 CIDR 在不同 `static` 组里指向不同 gateway 会启动报错。

### 与 cache 配合

- 动态路由不过期，所以 cache 命中跳过 `$ros` 不会造成路由丢失。
- 定时重启方案：`on_exit: clean`（默认），重启时 cache 和路由一起清空，状态一致。
- lazy cache + `dump_file` 热重启方案：`on_exit: keep` + `on_start: adopt`，重启后缓存命中的 IP 仍有路由。

### HTTP 接口

需启用 mosdns 的 `http_server` 插件，路径前缀 `/plugins/<tag>/`：

| 接口 | 说明 |
|---|---|
| `GET /routes` | 池内全部路由（static + dynamic，含状态与 RouterOS `.id`） |
| `GET /stats` | 池计数、最近一次对账结果、连接状态，以及当场拉取的 `l3hw-settings/monitor` 字段 |
| `POST /flush` | 删除全部动态路由并对账一次 |

这些接口没有自动调用者，不访问就不产生任何流量。

## RouterOS 侧准备

```routeros
/ip/service set api disabled=no            # 或 api-ssl
/user/group add name=mosdns policy=api,read,write
/user add name=mosdns group=mosdns password=******
/interface/ethernet/switch set 0 l3-hw-offloading=yes
/interface/ethernet/switch/port set [find] l3-hw-offloading=yes   # 按需选择端口
```

L3HW 要点：

- `/ip/route print` 的 `H` 标志只表示该路由**可以**被卸载；实际卸载情况看 `/interface/ethernet/switch/l3hw-settings/monitor`：`ipv4-hosts` 是硬件里的 `/32` 条目数（本插件的动态路由落在这里），`ipv4-shortest-hw-prefix` 非 0 表示硬件表满、短前缀走 CPU，`route-queue-size` 应恒为 0。
- gateway 必须是连通网段内的 IPv4 地址（不能是接口名），且该接口 / VLAN / bridge 成员端口的 `l3-hw-offloading` 为 yes。启动体检会按 `/ip/address` 找到每个 gateway 所在接口并检查，异常时打 warning。
- 驱动出错时 RouterOS 会自动关闭 `l3-hw-offloading` 并显示错误码；建议 `l3hw-settings set autorestart=yes`。
- 管理口（CRS504 的 `ether1`）挂在独立的小交换芯片上、不参与 L3HW，gateway 不要放在管理网段。

## 实测数据（CRS504-4XQ，RouterOS 7.23.8，直连管理口，ping 0.4 ms）

一条动态路由从 DNS 应答到硬件转发要经过三个阶段，`wait` 只影响其中一个：

```
$ros 执行 ──① API add，RouterOS 提交配置──▶ 路由出现在 /ip/route（CPU 可转发）──② 驱动写入交换芯片──▶ 硬件转发
            wait=true 时 DNS 应答等这一步                 任何模式都不等，RouterOS 内部异步
```

| 阶段 | 结果（轮询分辨率 5 ms） |
|---|---|
| ① API `add` 往返，单连接顺序 | min 21 ms，p50 22 ms，max 24 ms（n=20） |
| ① 并发 `add` | RouterOS **串行提交**：1 路 37 条/s，4 路 50/s，16 路 55/s；并发只拉长单条延迟（16 路 p50 293 ms） |
| ② `/32` 从 `add` 返回到出现在硬件 `ipv4-hosts` | min 26 ms，p50 31 ms，max 43 ms（n=20）；走 host 表，`route-index-delay-min` 设 1s 或 0s 结果相同 |
| ② 前缀路由（static CIDR）从 `add` 返回到硬件 | `route-index-delay-min=1s`（默认）：min 1.02 s，p50 1.03 s，max 1.11 s（n=8）；设 `0s`：min 26 ms，p50 28 ms，max 36 ms（n=8） |
| `remove` 200 条 | 一条命令带 200 个 `.id`：4.6 s；路由器侧脚本 `remove [find comment=…]`：5.4 s。约 23 ms/条，没有更快的办法 |
| 重复添加同一 `dst-address` | RouterOS **静默接受**，不报错；去重完全由插件的路由池负责，启动对账会删除路由器上的重复项 |

一条 `/32` 从 `$ros` 执行到硬件转发合计约 50 ms；`wait: true` 时客户端在约 22 ms 后拿到 DNS 应答，此时路由已在 RouterOS 中（CPU 转发），再过约 30 ms 进硬件。

### `wait` 对 DNS 延迟的影响（mosdns 实例实测，`hosts` 插件返回固定 IP，上游耗时为 0）

| 场景 | `wait: true` | `wait: false` |
|---|---|---|
| 新 IP，顺序查询 ×20 | p50 35 ms，max 43 ms | 0 ms |
| 同 20 个再查一次（池命中，不调 API） | 0 ms | 0 ms |
| 新 IP，20 条并发（模拟打开一个网页） | min 151 ms，p50 278 ms，**max 346 ms** | 0 ms |
| 新 IP，60 条并发 | min 366 ms，p50 807 ms，**max 966 ms** | 1 ms |

并发时延迟**串行叠加**：RouterOS 每秒只提交约 55 条，第 k 条应答要等前 k−1 条提交完，所以 20 条并发的最后一条等了 346 ms。只有**首次出现、且未命中 cache 的 IP** 才会进入这条路径；池命中（同一 IP 再次解析）和 cache 命中都是 0 ms。

真实页面（`www.cloudflare.com` 等 10 个域名并发，上游 223.5.5.5 本身约 300 ms）：`wait: true` 首次 p50 620 ms / max 844 ms，`wait: false` 首次 p50 296 ms / max 967 ms（max 来自上游 CNAME 链，与插件无关）。第二次两者都约 300 ms。

是否可感知：首次打开一个全新站点时，`wait: true` 让各域名的 DNS 应答多等 20–350 ms（取决于同时有多少新 IP），之后不再有；浏览器通常并行请求，整体多花的时间约等于最后一条的等待（几百毫秒），一般可感知但不明显。`wait: false` 则 DNS 零延迟，但第一个连接的首包在路由就位前（约 25–50 ms）会按默认路由走，若默认路由和目标 gateway 不是同一个出口，这条连接可能在中途换路径。

建议：

- 默认 `wait: true`，分流正确性优先。若更在意首次打开速度且能接受首个连接走默认路由，改 `wait: false`。
- 退出清理耗时 ≈ 23 ms × 路由数（1000 条约 23 s，10000 条约 4 min）。用 systemd 管理时相应调大 `TimeoutStopSec`；池很大时考虑 `on_exit: keep` + `on_start: adopt`（接管不调 API，瞬间完成）。
- 动态路由全是 `/32`，`route-index-delay-*` 对它们无影响；仅当 `static` 里有很多 CIDR 且希望启动后立刻进硬件时，才考虑 `/interface/ethernet/switch/l3hw-settings/advanced set route-index-delay-min=0s`。
- RouterOS 的 `/ip/service set api disabled=yes` 不会断开已建立的 API 会话；插件靠 TCP 层错误感知断线，实测断线后 1 s 起指数退避重连，期间学到的 IP 标记 offline，重连后由对账统一补加。

## 构建

```sh
go build -o mosdns .
```

Release 由 `routeros*` 前缀的 tag 触发（与上游 `v*` 分开）。

## 后续

- 第二版：`health_interval` 周期健康检查（monitor 告警 + count 对账）。
- 第三版：`idle_timeout` 按最近命中时间淘汰动态路由。
