# opencode2api · 本地改动说明（Local fork）

> 本目录是 **上游 `jasonxu114514/opencode2api`（2026-10-22 起对齐 **main @ 0bb23c7，含 v1.4.3**）的本地魔改版**。
> 源码在 `src/opencode2api-1.4.3/`（旧树 `src/opencode2api-1.4.2/`、`src/opencode2api-1.4.0/` 保留作参照，升级前整份备份在 `_backup_pre143_*/`、`_backup_pre142_*/`）。
> 版本标记 `v1.4.3-local`（`/api/monitor` 与启动日志可见）。
>
> **升级方式（2026-10-20 实操，可复用）**：在含上游历史的工作克隆里
> `git switch -c local/merge v1.4.0` → 把本地树放进去提交 → `git merge v1.4.2` → 冲突逐文件解决 →
> 编译修错。**不要整体覆盖 `src/`**，会丢全部本地功能。

- 部署目录：`CherryStudioWorkspace\opencode2api\`
- 服务：API `0.0.0.0:8790`（网关）、WebUI `0.0.0.0:8791`
- 版本标识：`v1.4.0-patched`（`/healthz` 里可见）
- 计划任务：`opencode2api`（登录自启，跑 `start-opencode2api.cmd`）

---

## 〇ter、2026-10-24 对齐上游 main @ 57b906b + 代理池自维护 + 两级剔除

**基线**：`jasonxu114514/opencode2api` **main @ `57b906b`**（含我们提交并被上游合并的 PR #111/#112，以及 OpenCode 2.1 客户端伪装 #141/#142/#143、显式关闭推理 #150、意大利语本地化）。版本标记 `v1.4.3-local.2`，升级前备份 `_backup_pre_import_20261024_123337/`。

**合并方式（重要，以后照做）**：本地分支已重建在**真实上游历史**上（此前是从 tarball 快照建的合成历史，和上游不共享祖先，导致 `merge` 报 unrelated histories）。现在仓库结构是 `0bb23c7 → local features 提交 → merge upstream/main`，以后同步只需 `git fetch && git merge origin/main`，冲突面已缩到 2 处注释措辞。

**新功能一：WebUI 代理池自维护（热生效，无需重启）**
- `POST /api/pool/import`，body `{content, mode}`：`mode=replace` 重写主档、`append` 与现有文件合并去重；逐行校验（`direct` / `http(s)://` / `socks5://`），重复与无效分别计数。
- 写入前把旧文件备份为 `<proxyfile>.import.bak`；随后调用 `RuntimeController.Apply(cfg, false)` **热重建网关**（`Reload → build → New → RuntimeProxies()` 会重读代理文件），新池立即生效、进程不重启。
- 前端代理池页新增「导入代理」卡片：textarea + 模式下拉（控件高度同为 30px）+ 「导入并生效」；替换模式有二次确认。实测：替换导入 2 条有效 + 1 重复 + 1 无效 → `{imported:2, duplicates:1, invalid:1, reloaded:true}`，文件与内存池同步更新。

**新功能二：两级剔除（短期排除 → 永久删除）**
- **短期排除**：探测失败 / 真实请求被拒达阈值 → `retired`（沿用原机制），记录 `retired_at`。
- **复活探测**：`revive_interval_seconds`（默认 900s）一轮，每轮最多 32 条，轮转覆盖；探测通 → 自动恢复（`proxy_restored`，重置排除时钟），失败 → 累计 `revive_fails`。
- **永久删除**：`revive_fails ≥ revive_probes`（默认 3）**或** 排除时长 ≥ `purge_after_minutes`（默认 1440 = 24h）→ 从 `proxies_pool.txt` 物理删除、状态文件自动清理（`SaveState` 跳过已删除项），日志 `proxy_purged` 带 `reason`（`revival_probes_exhausted` / `exclusion_window_expired`）。`fallback`（付费桥接）与 `direct` **永不删除**。
- 配置键：`proxy_pool.revive_interval_seconds` / `revive_probes` / `purge_after_minutes`；负值 = 关闭对应阶段。状态文件新增 `retired_at` / `revive_fails`（向后兼容旧文件）。
- 实测四条分支全过：按次数删除（2 次测活失败）、按时间删除（60s 窗口到点）、恢复（Clash 出口复活，951ms）、健康条目不受影响。

**修复**：`excludedEntries()` 按 `retired_at` 升序，保证先排除的先处理；`purgeProxy` 拒绝删除 fallback/direct。

## 〇bis、2026-10-22 升级到上游 main（v1.4.3 发布版 + 8 个未发版修复）

**基线**：`jasonxu114514/opencode2api` **main @ `0bb23c7`**（含 `v1.4.3` tag 及其后 8 个提交）。
源码树 `src/opencode2api-1.4.3/`，可执行文件版本 `v1.4.3-local`，升级前整份备份 `_backup_pre143_20261022_001844/`。

**上游新增（全部吸收）**
- **v1.4.3 `c780e68` 免费层 v2 客户端伪装**：v2 `/inference` 的请求形状对齐 OpenCode 2.0.32，且 `anonymousUpstreams()` 改为**先 v2 再 v1**（原先本地是 v1 优先，已改回上游顺序）。
- **`8cc5e96` 匿名通道熔断器**：连续失败到阈值后整条通道暂停，窗口 `base` 起步、每次 trip 翻倍到 `max`；半开探针只放一个请求；成功即复位。配置 `models.anonymous_backoff.{threshold,base_hours,max_hours}`。
- **`07ba89a`/`0bb23c7` 匿名出口上限** `performance.max_anonymous_exits`：一次请求最多试几个出口（默认值见上游），避免把 1400 个出口走穿。
- **`3e83185` 免费层 IP 封禁是终局**：识别 403 的 abuse 文案（`abusive traffic` / `free models aren't available from your current network` 等），**封的是出口 IP**，同 IP 换 lane/换 key 都不会好 → 该出口冷却 15 分钟（`performance.free_tier_block_seconds` 可调），所有出口都被封时直接熔断通道，不再逐个撞。
- **`fa9a255`/`0bb23c7` 请求级拒绝不再重试**：确定性 4xx（上下文过长、请求非法）直接回给客户端，不再逐层重试把它埋掉；同时忽略占位 key（`internal/config/placeholder.go`）。

**本地机制的取舍**（减少分叉，便于以后线上同步）
- **通道级暂停改用上游熔断器**：本地 1.4.2 的 `anonymous_gate_cooldown_seconds` 保留在配置里但不再驱动暂停（上游 breaker 是它的超集）。
- **出口熔断保留本地 `rejections`**：上游只追 IP 封禁；本地「连续被上游 400/403 拒绝达阈值即剔除」仍在（`reject_after_failures`），两者互补。遭遇 abuse 封禁时先走上游的 15 分钟冷却、不重复计失败。
- **两档代理池（主档/备用档）、`keys_direct` 直连档、GeoIP 锁区绕行、扫描进度/取消、分组管理、状态持久化、前端排版**全部保留。

**修掉一个潜伏 bug（本地 1.4.2 起就有）**
- `anonymousCursor.Next()` 在**延迟分级开启**时扫完整个池只返回 **1 个节点**（offset 直接走到尾），等于「`anonymous_max_attempts=5` 实际只用 1 个出口」，而且**不报错**（铁律④的静默降级）。
- 改为**一次性构建本请求的走访顺序 `plan`**（按 tier 排序，fast→slow），`Next()` 既兑现分级偏好、又能逐个返回；可用性在**取用时**重新判定（健康/退休/封禁/冷却/区域都会在请求进行中变化）。

**2026-10-22 补记：探测修正与可用性诊断**
- **探测目标改指 opencode**：`proxyHealthCheckURL` 由 `cloudflare.com/cdn-cgi/trace` 改为 `https://opencode.ai/`——探测要测的是「出口能否到达 opencode」，不是「能否到达 cloudflare」。`checkClaimedProxy` 只要拿到任何 HTTP 响应就算通（不看状态码），404 也证明隧道可达。
- **探测改用 bun 客户端**：`checkClaimedProxy` 原先用 `proxy.client`（Go 原生 TLS），与真实免费流量（bun 伪装客户端）不同路。已改为优先 `proxy.bun`。
- **本次 1400 条全灭的根因是套餐流量耗尽**（探测错误 1309 次 `Not Enough Bandwidth`），不是探测误杀——先前「探测口径误伤好代理」的猜测**不成立，已撤销**；但上面两条探测修正依然是正确的工程改进（以前测的是 cloudflare 可达性，以后测的才是 opencode 可达性）。
- **公开代理路线已验证走不通（0/20 成功）**：2988 条公开 HTTP 代理 → TCP 可达 1137 → CONNECT 隧道可用 266 → 网关探测健康 60 → **真实请求全部 429**（opencode 免费层对这些被全世界共享白嫖的 IP 长期限流，隔 30 分钟复测依旧）。隧道本身是通的（能拿到 429 说明请求到达了 opencode），但免费层不放行。**结论：别再花时间找公开代理，免费层的瓶颈是「IP 没被限流」，不是代理数量。**
- **探测脚本自身的教训**：第一版预筛脚本把 `asyncio.open_connection` 返回的元组当 writer 调 `.close()`，连接成功反而被 `except` 吞成失败 → 0% 存活。对照组（8.8.8.8:53 等已知通地址）当场暴露探针 bug。**批量探针先拿已知答案的样本自测**（铁律⑰）。

**部署与验收**
- 线上实例已切 `v1.4.3-local`：单进程、8790/8791 双监听、`proxy_pool_loaded primary=1401 fallback=67`。
- 实测 8/8 请求 **200**，**全部由免费池承接**（`anon:200`），`attempts=1`，1.5–2.9s。
- WebUI 代理池页：1468 条、9 列、无横向溢出、无控制台报错。
- ⚠️ **不要用「探测通过率」判断池子死活**：本轮探测（打 `cloudflare.com/cdn-cgi/trace`、6s 超时）报 3.2%，但**真实请求 8/8 都从免费池拿到 200**。探测只反映「该出口此刻能否在 6 秒内到达 cloudflare」，与「opencode 是否接受它」不是一回事（旧结论已撤销）。

---

## 〇、2026-10-20 升级到上游 v1.4.2

**上游带来的（已全部吸收）**
- `d04ff337` **免费模型流量伪装成 Bun 运行时**：新增 `internal/httpx/bun.go`(590行) + `bun_dial.go`(312行)，uTLS 复刻 Bun 1.4.3 的 BoringSSL ClientHello（JA4 `t13d1713h1`），免费模型走 `proxy.bun` 客户端。**这是本次升级最主要的收益**（免费层指纹一致性）。
- `b0a65848`/`cece86bb` 同步 OpenCode 2.0.32 的 UA 与工具描述。
- `9aa2133f` 修复会话身份串号（派生上游 session 现在绑定调用方的 server key）。
- `a70e77fe` 一批网关审计修复（协议/模型/遥测）。
- `81ad2842` 全局重命名重构（`transportPool`→`proxyTransports`、`anonymousPool`→`anonymousNodes`、`nodePool`→`upstreamNodes`、`RuntimeManager`→`RuntimeController`、`newXxx` 构造器改名为 `xxx`、`telemetry.Monitor`→`Metrics` 等）。
- 支持 `#pool` 轮转代理条目（`ParseProxyEntry` / `anonymousNodes.HasPool()` / 每次尝试新建隧道）。

**本地功能（已全部重新贴回并验证）**
- 代理池管理：延迟分级、失败禁用、长期剔除、状态持久化、GeoIP 地区、锁区绕行、`rejections` 收敛、扫描进度/取消、分组管理、`keys_direct`、两档备用池。
- 新增 `anonymousScan()`：把上游「每代理依次试 v1/v2 上游 + Bun 客户端」的循环抽成一个函数，`doAnonymousUpstream` 只负责「先主档、再备用档」。

**前端（改为上游模块结构，本地页重写）**
- 我们的单体 `webui/app.js` / `styles.css` **已删**；改用上游 `webui/js/*` + `webui/js/pages/*` + `webui/css/*`。
- 代理池页重写为 **`webui/js/pages/pool.js`**（导航「系统 → 代理池」），功能与之前一致：指标卡、四维度分组、整组筛选/定点扫描/批量剔除·恢复、扫描进度条与停止、逐条剔除。
- 样式沿用上游 token（新增 `.pool-group*` 于 `webui/css/pages.css`）。
- ⚠️ **配置中心暂未恢复 `proxy_pool` 字段**（后端 `proxy_pool` 仍是"缺省即保留"，UI 保存不会清掉它），要改这些参数目前请直接改 `config.json`。
- ⚠️ 注意上游有 **CSP `style-src 'self'`**：页面里**不能写内联 `style` 属性**（会被拦），要用 CSSOM（`el.style.width = ...`）或类名。

**验收**：`gofmt`/`go vet`/`go test` 全过；真实配置启动 `primary 1201 / fallback 67`；请求 3/3=200；真实 Chromium 15 项断言全过（导航、页面切换、指标、分组卡、维度切换、筛选、表头 9 列、无横向溢出、**无控制台报错**）。

## 一、本地改动清单

### 1. WebUI 视觉重做（2026-10-16）
把"编辑风"（米色底 + 墨绿 + 亮橙 + Georgia 衬线）整成为 system_monitor 的设计语言：浅灰底 + 白卡片 + 蓝主色 + 无衬线 + 统一 30px 控件 + 暗色自动跟随。
- 文件：`src/.../webui/styles.css`（整表重写，类名全保留，`index.html`/`app.js` 无需改）

### 2. 左侧栏布局修复（已提 PR #100）
`.sidebar` 缺 `overflow-y`、窄屏 `1fr` 应为 `minmax(0,1fr)`——底部菜单项够不到、窄屏横向溢出。
- 同属 `webui/styles.css`；上游 PR：`https://github.com/jasonxu114514/opencode2api/pull/100`

### 3. 代理池管理 M1–M4（2026-10-17）
| 模块 | 能力 |
|---|---|
| M1 | 全量定时扫描（所有代理，不只挂的）+ 延迟测量 + **按延迟分级选择** + 连续失败禁用 + 长期失败**剔除** + 状态**持久化** |
| M2 | 内置 GeoIP（`github.com/phuslu/iploc`，无外部文件）给每个代理打**地区标签** |
| M3 | 官方 key 默认**直连**（`key_proxy_mode`），匿名才走代理池 |
| M4 | 锁区模型**排除受限区（含 CN/LOCAL）**只用外区代理；遇 **403 自动学习**该模型锁区 |
| 观测/管理 | `GET /api/pool` + `POST /api/pool/{sweep,retire,reset}`（管理登录 + CSRF）；WebUI 第 14 页「代理池」：立即扫描 / 解除全部剔除 / 逐条**剔除·恢复** |

### 4. 两档代理池：免费池优先 + 付费节点兜底（2026-10-17 晚）
需求原话：「优先用免费池，不行了再用付费节点作为备用」。
- **主档** = `proxyfile`（4800 条抓取的免费代理）；**备用档** = `proxy_pool.fallback_proxyfile`
- 选路：一次匿名请求**先把主档走完**（受 `anonymous_max_attempts` 限），**主档一个 2xx 都没拿到**才进备用档（受 `fallback_max_attempts` 限，默认 6）
- **备用节点永不「剔除」**（桥接只是可能没开，属临时状态；仍会按探测失败「禁用」）
- **启动日志可验证分层**：`event=proxy_pool_loaded primary=<n> fallback=<m>`
- **付费节点从哪来**：Clash 节点是 anytls/trojan，不是 HTTP 代理，不能直接填。用 `paid-bridge/gen_bridge.py` 把订阅里每个节点映射成一个**本地 HTTP 端口**（`127.0.0.1:30101+`），再喂给网关——**独立 mihomo 实例，不动你在跑的 Clash、也不走 7897**
- **A/B 实证**（主档=必然连不上的死代理）：备用层【开】8×200 / 【关】**10×502**

### 5. WebUI 代理池「分组管理」（2026-10-17 晚）
第 14 页在指标卡下新增**分组管理**区块：
- **四个分组维度**：档位（主/备）· 状态（健康/不通/已剔除）· 分级（快/中/慢/未测）· 地区；点按钮即切换
- **每张分组卡**显示：数量、健康 h/N（百分比）、平均延迟（只看未剔除且有测量值的）、已剔除数
- **每张分组卡三个操作**：
  - `筛选` / `取消筛选` —— 把下面明细表收窄到该组（顶部出现可关闭的筛选 chip）
  - `扫描本组` —— **定点探测**该组（跳过已剔除，**每组上限 200 个**，避免一次突发打爆代理套餐）
  - `整组剔除` / `整组恢复`
- 明细表新增 **档位** 列；搜索同时匹配「备用/主档」
- 分组在**浏览器端按当前快照计算**（`/api/pool` 一次取全量），所以切换维度/筛选**不再发请求**

配套后端新增：
- `POST /api/pool/retire` 现在也接受 `{"ids":[...],"retired":bool}`（单个 `id` 的老形式保留）
- `POST /api/pool/check` `{"ids":[...]}` —— 定点探测指定代理，返回 `{"checked":N}`

**验收**（真实 Chromium + 桩数据，13 项断言全过）：分组卡数量/文案、四维度切换、档位列、整组筛选 17→14、取消筛选复原、整组剔除的请求体（3 个 id + `retired:true`）、扫描上限与跳过已剔除（12 个 id）、无横向溢出、无硬裁切、**全程无控制台报错**。

### 6. 修掉 `key_proxy_mode="direct"` 的静默 no-op（2026-10-17 夜）
**问题**：`key_proxy_mode` 是「官方 key 不跟匿名一起走代理池」的开关，但原实现**只在池里真的存在 `direct` 条目时才生效**。本机是 `proxies: []` + `proxyfile: proxies_pool.txt` → 池里全是代理、没有 direct → `directIndex = -1` → **开关静默失效**，官方 key 仍被摊到 4800 个免费代理上（IP 乱跳，正是这个开关想避免的）。典型的「静默回落」。

**修法**——不是改配置绕过，而是把这类降级从代码里拿掉：
- `config.EnsureDirect()`：开启该模式时**保证池里有 direct 条目**（没有就补一个）
- `gateway.New()`：启动打 `event=keys_direct`（带 `direct_index`/`transports`）；万一补不上就打 **WARN**——绝不让设置"看起来生效了"
- `proxyTransport.direct` + `newAnonymousPool()` **跳过 direct**：匿名通道的意义就是**离开本机出口**（免费层按出口 IP 限流/锁区），让它去试 direct 是白费一次尝试

**验收**：启动日志 `event=keys_direct direct_index=4800 transports=4850`、`proxy_pool_loaded primary=4801 fallback=49`；匿名通道冒烟 **6/6 = 200**；无 `keys_direct_unavailable` 告警。

> 同一套修法已做成上游 **PR #113**（`feat(proxy): keys_direct`）。

### 7. 手动扫描：进度 / 速度 / 剔除·恢复 / 取消（2026-10-18）
**报的现象**：点「立即扫描」没反应、也不剔除不恢复。
**真实根因**：日志里 `proxy_sweep` 记录 **0 条** —— 全量扫描**从来没跑完过**（4744 个 × `health_concurrency=5` × 单超时 6s，后台裸跑、**无进度**、重启即丢）。另外 `restoreProxy` 不清 `retired`，而 `SweepAll` 又跳过 retired → **被剔除的节点永远不可能自动回来**。

**改动**
- `proxySweepOnce` → **分块探测（64/块）+ 实时进度**；新增 `SweepProgress`（running/total/done/passed/failed/retired/restored/duration_ms），随 `GET /api/pool` 的 `sweep` 字段返回
- 新增 `proxy_pool.sweep_concurrency`（默认 **24**，**只作用于手动扫描**；后台滚动检查仍用 `health_concurrency=5`，保持温和不突发）
- 手动扫描**把已剔除节点也纳入**，探测通过就**自动恢复**（`reviveRetired`）；`CheckIDs`（分组定点扫描）同样纳入已剔除
- 新增 `POST /api/pool/sweep/cancel`；已在跑时再触发返回 `started:false`（不再排第二次全量）
- WebUI：**进度条** + `done/total（pct%）· 通过 · 失败 · 新剔除 · 恢复 · 已用`；进行中禁用「立即扫描」并显示「停止扫描」；结束后显示「上次扫描」摘要（含「中途停止，只扫到 N」）

**验收**（临时实例 + 合成池，真实 HTTP API 驱动，**19 项断言全过**）：进度采样 `[0,64,128,192,201]` 单调不回退、扫完 201/201、failed=200、passed=1、**本次剔除 200**、**恢复 1**（预置成「已剔除但可达」）、进行中再触发被拒、取消停在 64、实例全程存活。

**⚠️ 顺带探到一个更深的问题（本次未改）**：健康探测打的是 `https://cloudflare.com/cdn-cgi/trace`，量的是「这个代理能不能上网」，**不是「opencode 认不认这个出口 IP」**。实机全量扫 4744 个的结果：`passed=3786（80%）`，但实际可用率只有 ~27% → **被 opencode 拒的出口永远攒不够失败次数、永远不会被剔除**。这才是「扫描不剔除不可用节点」的深层原因，也是池子长期「看着 3770 健康、实际只有千余个能用」的来源。

**实机实测**：`sweep_concurrency=24` 全量扫 4744 个 = **9.6 分钟**（576.8s，8.2 个/秒）。嫌慢只能调大 `sweep_concurrency`（WebUI 配置中心可改），代价是并发突发更猛。

### 8. 池子按「真实请求结果」收敛（2026-10-18）
**背景**（承接第 7 条的发现）：健康探测打的是 cloudflare，量的是「能不能上网」，**看不出 opencode 认不认这个出口**。实机全量扫 4744 个：`passed 3786（80%）`，而实际可用率仅 ~27% → 被 opencode 拒的出口**永远攒不够失败数、永远不被剔除**。

**改动**（用户选定：按真实请求结果收敛）
- `proxyTransport.rejections`：**连续**被真实请求拒的次数（bare 400/403）；**任一真实 2xx 立即清零** → 好出口不会被误伤
- `transportPool.rejectExit / acceptExit`，挂在 `observeAnonymousResult` 上；只作用于**匿名通道**，且 **fallback（付费桥接）节点豁免**（与"备用节点不自动剔除"一致）
- 新增 `proxy_pool.reject_after_failures`（默认 **4**）。**语义刻意设为：0=用默认、负数=显式关闭** —— 免得老配置因为缺这个字段就静默不生效
- `rejections` 纳入状态持久化（`.proxy-pool.json`）与 `ProxyStat`；WebUI 失败列显示 `N / 拒M`，配置中心多一个输入框；**手动恢复/解除剔除会一并清零**

**验收**（**真实上游**，不是桩数据）：临时实例放 6 个「活着但被 opencode 拒」的出口，打 36 次 →
`状态分布 {403:23, 502:13}`；5 个出口 `rej=4 → retired=True`，1 个 `rej=3 → 未剔`；**未被拒的一个都没误剔**；服务端日志 `proxy_retired_rejected status=403`；手动恢复后 `rejections=0`。

**实机**：15 次请求后状态文件里 **121 个出口已有拒绝记录**（分布 1:106 / 2:12 / 3:3）。

**⚠️ 收敛速度的诚实说明**：要 **4 次连续被拒**才剔，而 `MarkFailure` 会把被拒出口**冷却**（403 触发 900s 闸门冷却）→ 一个坏出口得「用满 4 次、每次隔一个冷却期」才会被剔。**流量越大收敛越快**；想更激进就把 `reject_after_failures` 调小（比如 2）。

---

## 二、文件清单（相对 `src/opencode2api-1.4.0/`）

**修改**
- `webui/styles.css`（视觉重做 + 布局修复）
- `internal/config/config.go`（`ProxyPoolConfig` + 默认值/校验；`fallback_proxyfile` / `fallback_max_attempts`；`RuntimeFallbackProxies()`）
- `internal/config/proxy.go`（解析 `fallback_proxyfile`，与主档去重；`IsDirect` + `EnsureDirect`）
- `internal/gateway/pool.go`（延迟/失败/剔除/分级/地区/持久化/Stats；`fallback` 档位、`CursorForRegionTier`、`LenTier`、备用节点不剔除；`SetRetiredMany` 批量剔除、`CheckIDs` 定点探测、`RetireProxies`/`CheckProxiesNow`）
- `internal/gateway/refresh.go`（全量 `SweepAll` 调度 + retire 日志 + 状态保存）
- `internal/gateway/gateway.go`（`keysDirect` + 锁区字段 + `sync` 导入 + 备用档接线）
- `internal/gateway/upstream.go`（按模型选池 + 403 学习 + **主/备两档轮询**）
- `internal/gateway/runtime.go`（`ProxyStats` + 状态路径设置/加载 + **启动分层计数日志**）
- `internal/admin/server.go`（`proxy_pool` 保真 + `GET /api/pool`；`POST /api/pool/retire` 支持 `ids` 批量、新增 `POST /api/pool/check`）
- `internal/gateway/runtime.go`（`RetireProxies` / `CheckProxiesNow` 转发）
- `webui/index.html`（第 14 页 + 导航项 + 配置中心两个备用档字段 + **分组管理区块** + 明细表档位列）
- `webui/app.js`（pool 页 loader/render + 路由 + 备用档字段 + 备用行标记 + **分组计算/筛选/整组操作**）
- `webui/styles.css`（+ **分组卡 / 维度切换 / 筛选 chip** 样式）
- `go.mod` / `go.sum`（+`github.com/phuslu/iploc`）

**新增**
- `internal/gateway/region.go`（锁区国家表 + 名称识别 + 学习）
- `paid-bridge/gen_bridge.py`（从 Clash 订阅抽节点 → 生成 mihomo 桥接配置 + `paid_proxies.txt`）
- `paid-bridge/start-paid-bridge.cmd`（启动桥接；计划任务 `opencode2api-bridge` 登录自启）

---

## 三、配置段 `proxy_pool`

写进 `config.json`，或直接在 WebUI **配置中心 → 代理池管理** 里改（保存即热载）：

```json
"proxy_pool": {
  "health_mode": "rolling",         // rolling=滚动轮询(每次一小批) | sweep=整池定时全扫
  "health_tick_seconds": 30,        // rolling: 每批间隔
  "health_batch_size": 30,          // rolling: 每批探测数量
  "health_interval_seconds": 1800,  // sweep: 整池周期（0=关闭，退回只测挂的）
  "health_concurrency": 5,          // 探测并发（调低以避免突发）
  "health_timeout_seconds": 6,      // 单次探测超时
  "fast_latency_ms": 800,           // 延迟≤此值为"快"档
  "mid_latency_ms": 2500,           // ≤此值为"中"档，其余"慢"
  "disable_after_failures": 1,      // 连续失败N次→临时禁用（1=立即）
  "retire_after_failures": 8,       // 连续失败N次→永久剔除
  "anonymous_max_attempts": 10,     // 单请求最多试几个代理（防死池拖死请求）
  "key_proxy_mode": "direct",       // "direct"=官方key直连 | "pool"=key也走池
  "tiered": true,                   // 按延迟分级选择
  "fallback_proxyfile": "paid-bridge/paid_proxies.txt", // 备用档（付费节点桥接）；空=不用备用档
  "fallback_max_attempts": 6        // 主档全挂后，备用档最多试几个
}
```

**代理来源**：`proxies`（内联）+ `proxyfile`（每行一个，支持 `#`/`;`/`//` 注释）。
本机 `proxyfile` = **`proxies_pool.txt`**（4800 行 / 4515 个唯一 `ip:port`，来自工作区 `all_accounts_proxies_20261017 (1).txt`）。
实测**约 27% 的出口 IP 可用**（抽样 60 个经网关各打一次：16×200 / 44×400），池内 ~1200 个可用 IP，足够轮转。
`all_proxies.txt`（3000，旧池，全 403）、`proxies_fast.txt`（2631，旧筛过的死池）已停用。

**状态文件**：`config.json.proxy-pool.json`（延迟/失败/剔除，**删掉它即可解除全部剔除**）。

### 付费备用档（paid-bridge）

```bash
# 1) 从 Clash 订阅抽节点（可再叠加一个 vless 分享链接文件），生成桥接配置 + 端口清单
python paid-bridge/gen_bridge.py --links "..\22号到期100g流量.txt"
#    不带 --links 就只读 Clash profile（默认夜煞云），端口 30101+ 按顺序排
# 2) 起桥接（独立 mihomo 实例，不碰你在跑的 Clash）
paid-bridge\start-paid-bridge.cmd           # 计划任务 opencode2api-bridge，已登录自启 + 失败自动重启
# 3) config.json 里指到 paid-bridge/paid_proxies.txt（已配好）
```

**`--links` 支持 `vless://` 分享链接**（每行一条）。这类链接不是 HTTP 代理，不能直接进池，必须经桥接转成本地端口；支持两种 authority 写法：
- `vless://<uuid>@host:port?…`
- `vless://<base64("enc:uuid@host:port")>?…` ← 本机 `22号到期100g流量.txt` 就是这种

会解析 `tls / sni(peer) / fingerprint / pbk(Reality 公钥) / sid / flow(xtls-rprx-vision)`。

**当前构成**：49 个 Clash 节点 + **18 个 vless 节点**（2026-10-20，100G/22号到期）→ 端口 30101–30167。实测 vless 那 18 个里 **15 个可用**（3 个底层 TCP 超时，属供应商侧）。
- 每个节点 = 一个 `127.0.0.1:30101+` 的 HTTP 代理；实测 49 节点里 45 个可用（HK/TW/SG/JP/US/KR…）
- **订阅更新后节点会变**，要重跑 `gen_bridge.py`（端口按顺序重排，`paid_proxies.txt` 同步更新），再重启网关
- 改桥接配置要重启桥接（`Stop-ScheduledTask`/`Start-ScheduledTask opencode2api-bridge`），改完**用 `netstat | grep LISTENING` 数端口**核对（**别用不带 LISTENING 的 grep 计数——会把 TIME_WAIT 也算进去**）

---

## 四、编译 / 部署 / 重启

```bash
export GOPROXY=https://goproxy.cn,direct GOSUMDB=off GOTOOLCHAIN=local
SRC="C:/Users/Administrator/Documents/CherryStudioWorkspace/opencode2api/src/opencode2api-1.4.0"

# 1) 停服务（否则 exe 被占用无法覆盖）
powershell -Command "Stop-ScheduledTask -TaskName opencode2api; Stop-Process -Name opencode2api -Force"

# 2) 编译（WebUI 是 //go:embed 内嵌，改前端必须重编译）
go build -C "$SRC" -ldflags "-X main.version=v1.4.0-patched" \
  -o "C:/Users/Administrator/Documents/CherryStudioWorkspace/opencode2api/opencode2api.exe" ./cmd/opencode2api

# 3) 起服务
powershell -Command "Start-ScheduledTask -TaskName opencode2api"
```

> exe 约 16.9MB（比原版大 ~4.5MB，是内嵌的 GeoIP 数据）。
> 只改 `webui/*` 也要重编译；只改 `config.json` 重启即可（无需编译）。

---

## 五、升级上游时

1. 重新下载上游 release 源码，对照本文重新把上述文件改动打上（或直接以上游版跑，放弃本地功能）。
2. WebUI 视觉重做整表替换 `styles.css` 即可（类名稳定）。
3. 代理池功能涉及 6 个 Go 文件，改动较多，建议保留 `src/` 做参照。
4. PR #100 的 2 行修复若已合并进上游，则不必再打。

---

## 六、已知事项

- **`cat-jam-free` 已从 opencode 模型目录下架**（不是本地 bug）。上游对"不在目录的模型"返回的报错文案是 `the model uses an upstream protocol that opencode2api does not expose`，措辞会误导——其实是"模型不在目录"。
- **锁区路由已用 `muse-spark-1.3-contributor-free` 验证**（走外区代理成功；中国区直连原本 403）。
- **免费层按出口 IP 限流**：单个 IP burst 会 429 → 池化 + 多出口是对策。
- **⚠️ 「模型不可用」有两种码，别混**：本机直连（CN 出口）与部分代理 IP 会被免费层拒，返回 **`400 Model is unavailable`**；而**不符合 agent 形态**（非流式 / 缺 system+core tools）的请求返回 **`403 FreeTierError`**。排障时先看是 400 还是 400/403——400 是「出口不行」，换 IP 即可。
- **⚠️ 池健康 ≠ 瞬时可用**：刚改完配置 / 滚动探测刚跑过一批时，短窗口内请求可能连续失败（400/403），**不代表池坏了**——换个时间/多打几次即可恢复（实测同一池：某刻 0/12，几分钟后 15/15 全 200）。
- **⚠️ 免费层对「出口国家」敏感**：池里可用 IP 美区占比高。**港/台/新/日/韩实测也放行**（付费桥接 49 节点里 45 个可用，出口含 HK 12 / TW 8 / JP 6 / SG 5 / KR 2）。
- **⚠️ 延迟波动很大，且随尝试次数线性增长**：免费池 27% 可用率意味着平均要试 ~4 个出口才通；每个失败出口约 2–3s。实测（10 次）：
  | 配置 | 成功率 | 平均耗时 |
  |---|---|---|
  | `anonymous_max_attempts=15`（不启用备用档就试满） | 4/6 | **30–40s** |
  | `anonymous_max_attempts=5` + `fallback_max_attempts=5`（**当前**） | **10/10** | **15.2s** |
  **结论：主档预算别设太大**——设 15 时几乎每次都把预算用满（免费池慢），反而更慢；设 5 让备用档更早接管，成功率和延迟同时改善。两个值都在 WebUI 配置中心可调。
- **⚠️ Windows 批处理 `%~dp0` 以反斜杠结尾**：`-d "%~dp0"` 里的收尾 `\` 会**把引号转义掉**，参数被拼成一整串路径（mihomo 报 `Parse config error: ...\" -f config.yaml\config.yaml`）。**写 `"%~dp0."`**（末尾点号挡住转义）。
- **⚠️ 数监听端口要用 `netstat | grep LISTENING | grep -c`**：不带 `LISTENING` 的计数会把 **TIME_WAIT** 也算进去（本次实测：进程已停，计数仍是 44），据此判断「桥接没死」会得出完全错误的结论。
- **🔄 2026-10-20 池子换代**：`proxies_pool.txt` ← 工作区 `proxies_20261020_004243.txt`（**1200 行 / 1188 个唯一 ip:port**，12 个新账号，与旧池零重叠）。旧池归档 `proxies_pool_20261017.bak.txt`；健康状态整体归档为 `config.json.proxy-pool.20261017.bak.json` 后重建（旧 4800 条**全部已剔除**，是 10-17 那批真的死了）。换代后实测：探测 120 个只失败 5 个、0 剔除；**停掉付费桥接后仍能 200**（`anon:200:1261ms`）→ 免费池确实在干活。
- **⚠️ 判断这些代理是否可用，不要用 curl**：`curl -x <代理>` 从本机**全部超时**，但**同一个代理、同一台机器，Go 网关正常**（对比：curl 直连 000/10s，网关 `anon:200`/1.2s）。我先用 curl 得出了「本机 IP 被服务商封了」的错误结论，已纠正。**要测就用网关自己打请求**（或看 `/api/pool` 的探测结果）。
- **🔧 付费桥接任务已加「失败自动重启」**（`RestartCount=999 / RestartInterval=1m / ExecutionTimeLimit=0`）。此前只在登录时启动，mihomo 崩了不会自愈——2026-10-20 发现它已停摆，害得 49 个备用端口全不可用。
- **🎨 前端按 `system_monitor/UI_DESIGN_GUIDE.md` 重排（2026-10-20）**：上游 v1.4.2 的排版是 14px/1.6 黑白强调色，与本机规范不符。已按规范对齐（追加在 `webui/css/pages.css` 末尾的「system_monitor design system alignment」段落，**不改上游原文件**，便于将来再升级）：
  - **三级字号**（红线①）：页标题 **20px** / 区块标题 **16px**（`--fs-lg`）/ 正文 **13px** / 辅助 **12px** / 表头 **11px**，行高 1.5。
  - **同行控件等高 30px**（红线②）：`input/select`、`.btn-sm` 统一 `--ctrl-h: 30px`（上游原本输入框 ~35.5px、按钮 30px，同一行参差）；**checkbox/radio 保持原生 14px** 并显式压掉 `min-width`（规范「踩坑 1」）。
  - **蓝色强调色**：上游把强调色走在 `--inverse-*` 通道，重映射即可全局换色——浅色 `#1d4ed8`（白字对比度 **6.70:1**，实测）、深色 `#3b82f6`；导航选中态改为主色弱底 + 主色文字。
  - **密度**：卡片圆角 8px、内边距 16px；表格单元格 6×10px；`.table td` 默认 `nowrap`（长 URL 不再从中间折断），代理列 `.truncate`（420px + 省略号 + `title`）。
  - **验收**：1440/1280 × 明暗四组实测——字号 20/16/13/12/11、工具行四个控件高度全为 30、主按钮对比度 6.70:1、无横向溢出、**无控制台报错**。
  - ⚠️ 顺手修掉一个自己引入的 bug：按钮同时带 `btn-primary btn-ghost`（`renderSweep` 里 toggle 的），导致主按钮变成透明底（对比度只剩 3.24:1），已删除该 toggle。
- **📐 分辨率 / 缩放自适应（2026-10-20）**：原先字号是固定 px，高 DPI 缩放后（例如 1440p@200% 只剩 ~1024 CSS px）与 2560px 屏一样大，显得挤。改为**流式字号 + 分级密度**：
  - `clamp()` 在规范三级区间内随视口平滑变化：`--fs-base` **12.5→13.5px**、`--fs-sm` 11.5→12.5、`--fs-xs` 10.5→11.5、`--fs-lg` 15→16.5、页标题 `--fs-2xl` 18→20（1024px 取下限，1920px 取上限）。
  - **控件高度固定 30px 不随视口浮动**（红线②：同一行必须等高）。
  - 断点：**≤1180px** 指标卡改 2 列、内边距收一档、代理池表隐藏「分级/上次探测」两列（9→7 列，窄屏无需横向滚动）；**≤980px** 上游已把侧栏变顶部横排，这里再收紧表格内边距；**≥1600px** `--content-max` 放宽到 1440px，避免宽屏内容挤成中间一条；**≥1.5dppx** 边框略提亮，抵消 2x 屏 1px 边框偏粗。
  - 分组卡最小宽 220→**252px**，让「筛选 / 扫描本组 / 整组剔除」三个按钮一行放得下。
  - **验收**：1920/1536/1440/1280/1180/1024 × 明暗共 **12 组**全过——字号在区间内、控件等高、无页面横向溢出、窄屏 7 列且表格不需横滚、无控制台报错；并断言**字号确实随分辨率变化**（base 12.51@1024 → 13.5@1920）。
- **⚡ 真实传输失败即刻移出选择（2026-10-20）**：`syncProxyResult` 原先只在**异步校验探测**返回后才把代理标成不健康（最坏要再等一个 6s 探测超时）。现在传输失败**当场 `healthy=false`**，后续请求立刻跳过它；探测成功会自动恢复。实测：黑洞代理一次失败后 `healthy=false` 且 `failures=0`（`failures` 未变 → 证明是即时标记而不是探测结果）。
- **轮换本身早就是即时的**：匿名通道每次失败会按 `failure_cooldown_seconds`（当前 **15s**）冷却该节点，**同一次请求内立刻换下一个**；`anonymous_max_attempts=5` + `fallback_max_attempts=6`。真正花时间的是**每个死节点的连接超时（约 5s）**——这是探测本身无法避免的成本；要更快只能调小 `performance.connect_timeout_seconds`（会牺牲少量慢但可用的出口）。
- **⚠️ 升级/换版后浏览器必须强刷一次（Ctrl+Shift+R）**。原因：旧版 `index.html` 引用的 `/styles.css`、`/app.js` 在 v1.4.2 里已不存在，而服务端原本把**任何**缺失路径都回退成首页 → 浏览器拿到 **HTML 当 CSS/JS 解析** → 整页没样式、脚本也不执行（症状就是"排版全乱/像裸 HTML"）。已在 `internal/admin/server.go` 收紧：**只有无扩展名的路由路径才回退首页，带扩展名的资源缺失一律 404**（实测 `/styles.css`、`/app.js`、`/nonexistent.js` 都 404；`/`、`/overview` 仍正常回退）。这条同时适用于以后任何一次前端结构调整。
- **⚠️ 探针失败判据已修**（2026-10-17）：原 `isProxyFailure` 只认"超时/连接被拒"，代理返回 `Not Enough Bandwidth`（套餐流量耗尽）这类 CONNECT 层错误时被当成"不确定"，**失效代理永远标不死**。已改为"探针只要报错就判失败"。
- **⚠️ 代理套餐流量**：全量探测按"请求数/流量"计费，频繁全扫会把额度烧光。默认已改为**滚动轮询**（每 30s 一小批）。
- **本机出口 IP 可能被 opencode 免费层限流**（返回 `Model is unavailable`，非地区 403）；换出口可恢复。
