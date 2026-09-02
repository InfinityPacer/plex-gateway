# 性能矩阵

性能是 `plex-gateway` 的首要验收条件。功能覆盖不能以增加本地媒体延迟、把分析 I/O
放入 302 路径，或扩大 Plex、MediaVault 和 CDN 请求风暴为代价。

本文区分三类数据：本机微基准、脱敏真实链路样本和 NAS 候选版本端到端验收。微基准
只能衡量代码增量，不能替代客户端首帧、Plex 响应或 CDN 网络结果。

## 不可破坏的边界

- 本地 Part 在识别为非云媒体后立即交给 Plex，不读取 MediaInfo、不访问 SQLite、不执行
  veto，也不请求 MediaVault 或 CDN。
- 普通 STRM Part 和 universal start 不等待 MediaInfo，也不执行 veto。
- L1 命中直接返回，不因本次请求同步读取 SQLite。访问续期可能异步 touch SQLite，
  因此持久化 I/O 不得阻塞热路径。
- 单项 metadata 冷 miss 只允许内存级 P2 投递并立即返回 Plex 原响应，不同步读取 SQLite、
  请求 MediaVault 或等待 CDN probe。
- STRM response 已包含完整 Media/Part、真实媒体大小和带正数 Plex ID 的 Stream 行时，
  必须字节级透传，不读取 STRM、不查询 L1/SQLite，也不投递 ffprobe。
- 详细 metadata 请求默认只移除 `asyncAugmentMetadata`，避免浏览突发调度 Plex 后台分析；
  `checkFiles` 继续透传以保留 `Part.accessible` 与 `Part.exists`。
- 通用 metadata 微批只合并认证、query、Header、表示和远端身份完全一致的单项 GET。
  它不缓存响应；HEAD、Range、条件请求和任何不确定身份直接透传。
- 一个 universal decision 最多等待一次 `MEDIAINFO_COLD_WAIT`。veto 只复用响应增强已经
  取得的新鲜记录，不增加缓存读取或远程探测。
- 同一 MediaInfo key 的并发请求必须共享 singleflight。并发数不能突破配置的 worker
  上限；P0 实际播放优先于 P1/P2 排队任务，但不能抢占正在运行的后台 probe。
- P1/P2 在 worker 中先检查 SQLite。只有 L1 和 SQLite 都 miss 的任务才受全局远程启动
  间隔限制；SQLite 热命中不能因 CDN 风控节流额外等待 5 秒。
- 302 与 `control-v1` 的 204 响应只包含控制流。媒体字节始终由客户端直接从 CDN 读取。
- Plex Web 兼容层只缓冲最多 2 MiB 的 Web shell，并加载可长期缓存的静态脚本；Part、
  302、本地媒体和其他 Plex API 不缓冲响应，也不新增媒体数据面 I/O。

## 当前基准

基准采集于 2026-08-29 至 2026-08-30。代码运行于 `darwin/arm64`、Apple M3 Max、Go 1.26.5。
每项运行三到五轮；下表给出观测范围。工作站数据只用于发现代码回退，换架构或 Go
版本后必须重新采集。

| 操作 | 结果 | 分配 | 说明 |
| --- | ---: | ---: | --- |
| MediaInfo L1 cache `Get` | 469.0-476.0 ns/op | 960 B, 7 allocs | 纯缓存结构基准。 |
| MediaInfo service `GetMemory` | 1.356-1.423 us/op | 2880 B, 21 allocs | 包含记录防御性复制、兼容性校验和访问续期判断。 |
| MediaInfo service fresh `Offer` | 1.410-1.453 us/op | 2912 B, 22 allocs | P0/P1/P2 非阻塞投递前的 L1 热命中路径。 |
| MediaInfo service exact-key join `Offer` | 521.0-550.1 ns/op | 512 B, 7 allocs | 已有 flight 的内存去重，不读取 SQLite。 |
| STRM target fingerprint | 382.0-387.4 ns/op | 448 B, 6 allocs | MediaVault redirect URL 规范化和 SHA-256。 |
| SQLite 单记录 `Get` | 14.60-15.37 us/op | 4016 B, 74 allocs | 独立本机 SQLite 读取；生产启动会先恢复到 L1。 |
| 本机 HTTP 合成媒体 ffprobe | 22.48-33.36 ms/op | 约 119-125 KiB, 232-234 allocs | 只验证 worker 代码，不代表 NAS/CDN。 |
| Metadata 分析过滤器，非 metadata | 6.247-6.429 ns/op | 0 B, 0 allocs | 普通代理路径仅执行方法和前缀判断。 |
| Metadata 分析过滤器，无目标参数 | 75.36-77.49 ns/op | 32 B, 2 allocs | 保留原请求和完整 raw query。 |
| Metadata 分析过滤器，移除目标参数 | 377.3-386.3 ns/op | 768 B, 7 allocs | 克隆请求 URL，其他 query 字节不变。 |
| 32 项 metadata 直接内存 handler | 76.87-78.02 us/op | 约 219.5 KiB, 929 allocs | 仅作为微批代码增量对照，不包含 Plex 网络或数据库。 |
| 32 项 metadata 微批、拆分与 fan-out | 422.9-448.4 us/op | 约 577.1 KiB, 4856-4857 allocs | 相对直接 handler 增加约 0.35-0.37 ms，包含 goroutine、JSON 拆分及 32 个响应。 |
| control ticket 同 attempt 复用 | 689.7-729.7 ns/op | 688 B, 8 allocs | 只衡量内存 ticket 更新，不包含 Plex 或 MediaVault。 |
| control ticket 单次 lease | 289.3-293.7 ns/op | 432 B, 3 allocs | 包含 idle TTL 续期和请求上下文复制。 |
| control ticket 并行 lease | 413.7-417.3 ns/op | 432 B, 3 allocs | 同一 ticket 的并行锁竞争下限，不包含 HTTP 或解析。 |

真实 MediaVault/CDN 抽样为 5/5 成功：MediaVault redirect p50 141 ms、范围
109-533 ms；受限 CDN ffprobe p50 601 ms、范围 403-674 ms；合计 p50 761 ms、范围
543-955 ms。每次读取约 1.22 MiB 并发生 2 次 seek。样本量较小，只证明当前探测通常能在
默认 5 秒冷等待内完成，不能作为 CDN SLA。

真实 Plex 控制面 A/B 中，直连 Plex 与 Gateway 透明代理单项 metadata p50 均约
`46-47ms`。Plex 原生 batch 的 16、32、64、100 项响应约为 `50ms`、`94ms`、`182ms`、
`285ms`。Apple TV 长剧集样本曾在约一秒内发出 1010 个单项请求，其中 581 个唯一条目，
峰值约 180 请求/秒；固定并发 Guard 会将这种突发转换为多轮排队。32 项约为
340 item/s，64 项约为 352 item/s，吞吐只增加约 3.5%，但单批延迟和失败回退范围接近
翻倍，因此生产上限固定为 32，不开放 64 项实验值。

2026-08-30 的真实 Apple TV 长剧集 A/B 使用 20ms 窗口和 32 项 batch。批量 Guard 为 3 时，
54 个 batch 承载 1127 个条目，Guard 等待 p50/p95/max 为 `0/349/902ms`，Plex 处理为
`317/941/1466ms`，总耗时为 `418/1043/1518ms`。批量 Guard 调为 4 后，57 个 batch 承载
1163 个条目，Guard 等待降为 `0/44/537ms`，Plex 处理为 `140/927/1174ms`，总耗时为
`208/1037/1199ms`。两轮均为 0 fallback、0 Guard timeout；客户端复测体感接近本地媒体。
A/B 顺序没有清理 Plex 内部缓存，因此不能把 Plex p50 的全部下降归因于并发调整，但
Guard 尾部等待和客户端结果共同支持批量默认值 4。

同日使用 128 个本地 episode 的固定 metadata burst 做了独立开关 A/B。微批关闭的三轮
耗时为 `2579-2888ms`，开启后的三轮为 `2214-2470ms`，改善约 `335-674ms`，即
`13%-23%`。开启组累计 512 个原请求合成 18 个 batch，0 fallback、0 Guard timeout。
这说明通用微批没有为 STRM 牺牲本地媒体，但几百毫秒的 API 改善未必能被页面渲染体感
稳定识别。

当前 Metadata Guard 默认值为单项全局 16、单客户端 16、批量 4；微批默认窗口 20ms，
单批最多 32 项。合成 batch 占用批量池，异常回退占用单项池。是否继续调整只依据候选
版本的 Plex 延迟、错误率和真实客户端请求矩阵，不与未来 Plex MediaInfo 写入方案绑定。

2026-08-30 的 NAS 阻塞 A/B 将旧候选判定为不可接受：同一冷 metadata 请求直连 Plex
约 `0.52s`，经旧候选约 `5.15s`，Gateway 热缓存约 `0.15s`。同期实际 CDN ffprobe 平均
约 `0.54s`、最大约 `0.84s`，且没有 probe 失败。约五秒的增量来自 metadata 请求同步等待
后台调度窗口并持有 Guard 准入，不是 ffprobe 执行时间。该结果确立了“浏览 miss 立即透传，
仅实际播放 decision 可使用冷等待预算”的当前门槛；修正版已经通过上述 NAS 长剧集与
本地媒体 A/B。

## 端到端验收矩阵

| 场景 | 请求期允许的 I/O | 必须采集 | 验收条件 |
| --- | --- | --- | --- |
| 本地媒体透明代理 | Plex | 直连 Plex 与经 Gateway 的 p50/p95/p99、错误率、上游请求数 | 0 功能错误；Gateway 增量 p95 <= 20 ms、p99 <= 50 ms，且相对增加 <= 10%。 |
| STRM Part 302 | Plex Part 授权、MediaVault redirect | 302 p50/p95/p99、Plex/MV 次数、响应字节 | 100% 健康样本返回 302；p95 < 2 s、p99 < 5 s；Gateway 不传输媒体字节。 |
| 普通 universal start | Plex Part 授权、MediaVault redirect | grant 命中、上游次数、302 延迟 | 不读取 metadata、STRM 或 MediaInfo；无 grant 时透明回退。 |
| ShimWeave control-v1 start | Plex Part 授权 | 204 descriptor、Plex/MV 次数、响应字节 | 原始 manifest 请求止于 204；创建描述时 0 MV、0 媒体字节；无 grant 或协商无效时透明回退。 |
| ShimWeave control-v1 Range | MediaVault redirect | 控制 p50/p95/p99、Range/控制/MV 次数、错误率 | 正常路径每个未命中 Range 解析一次临时 URL；CDN 403 恢复最多再解析一次。Gateway 只返回 204 header，不跟随 CDN、不传媒体字节。 |
| Plex Web shell | Plex HTML、Gateway 静态脚本 | shell body、注入次数、首屏延迟、脚本缓存 | shell <= 2 MiB 时只修改一次；脚本命中长期缓存；其他 Web 静态资源和 API 不变。 |
| Plex Web 云端 Direct Play | Plex decision/Part 授权、MediaVault redirect、CDN | 首帧、seek、Part 302、Gateway 媒体响应字节 | 浏览器原生支持的样本正常播放和 seek；Gateway 媒体响应保持 302 且不传输视频字节。 |
| Plex Web 需要 DASH/转码且无 ShimWeave | Plex | decision、start manifest、Gateway redirect | 内置薄层明确保持不支持；不得把原始 MP4/MKV 作为 `start.mpd` manifest 宣称成功，也不得让媒体字节进入 Gateway。 |
| Plex Web + ShimWeave 转封装 | Plex、MediaVault、CDN | 首帧、Seek、持续缓冲、Range/控制/MV 次数 | manifest 请求止于 control-v1；转封装在浏览器完成；Gateway 媒体响应字节为 0。 |
| Plex 已物化 STRM MediaInfo | Plex | response 字节、STRM/L1/SQLite/probe 次数 | metadata 与 decision 字节级透传；0 STRM、0 L1、0 SQLite、0 ffprobe。 |
| Decision，Infuse | Plex metadata 和 decision | decision 延迟、veto 结果 | veto 始终放弃判断；不增加缓存、MediaVault 或 CDN 请求。 |
| Decision，L1 热缓存 | Plex metadata 和 decision，L1 | p50/p95/p99、SQLite/MV/CDN 次数 | 0 SQLite、0 MV、0 CDN；p95 <= 1 s、p99 <= 2 s。 |
| SQLite 启动恢复 | 启动期 SQLite，播放期 L1 | 恢复耗时、首个 decision 延迟 | 首个请求 0 ffprobe、0 请求期 SQLite。 |
| Decision，真实冷探测 | Plex、MediaVault、CDN、SQLite 写入 | resolver/probe/总延迟、读取字节、seek、fail-open | 成功样本 p95 < 2 s、p99 <= 5 s；超过冷等待返回可用的 fail-open 响应。 |
| Apple TV DV5，veto 开启 | Plex metadata、decision 和既有 MediaInfo 流程 | reject 延迟、grant、额外 I/O | 新鲜完整记录命中时 reject 且不创建 grant；相对 veto 关闭增加 0 次缓存、MV、CDN 请求。 |
| 同一 Part 并发 2/4/8/16 | 一个共享 probe | 唯一 probe 数、延迟、队列、active | 每个 key 最多 1 次 probe，无重复 CDN 读取。 |
| 不同 Part 并发 2/4/8 | 受 worker 上限约束 | active、队列、CPU、内存、错误率 | 不突破 `MEDIAINFO_CONCURRENCY`，不拖慢本地或 302 路径。 |
| A/B/C 快速切换 | 当前项 P0 probe、P1 邻近预热 | 每项延迟、身份对应、替换/队列指标 | 新当前项优先排队；不能抢占正在运行的 probe；无跨项 MediaInfo、grant 或 redirect。 |
| 长剧集 metadata 突发 | Plex、L1；P2 后台可查 SQLite/探测 | 前台响应、P2 队列/过期/远程启动次数 | 前台不等待 P2；唯一 pending P2 <= 50；远程启动间隔 >= 5 s；过期任务 0 MV/CDN。 |
| 长剧集 metadata 微批 | Plex、L1 | 原始单项数、合成 batch 数、唯一 item、fallback、p50/p95/p99 | 同组 batch <= 32；身份不跨组；正常响应 0 fallback；相对透明代理显著降低上游请求并改善页面完成时间。 |
| 长剧集分析参数过滤 A/B | Plex、L1 | 过滤计数、Scanner 进程、`accessible/exists`、metadata p50/p95/p99 | `asyncAugmentMetadata` 不到达 Plex；`checkFiles` 和对应字段保留；Scanner 为 0。 |

表中的验收条件是候选版本的门槛，不代表本机微基准已经替代端到端验收。当前已有证据
包括本机代码基准和 5/5 成功的 MediaVault/CDN 探测抽样；本地透明代理、302 热冷路径、
决策热冷路径的 p95/p99、并发矩阵和快速切换矩阵，仍需使用候选版本在目标 NAS 上串行采集。

客户端首次画面必须单独测量。Gateway 302 时间、客户端跟随 Location、CDN 首字节和
播放器首帧是不同阶段，不能用 302 延迟代替起播性能。

## 复现命令

```sh
go test -run '^$' \
  -bench 'Benchmark(MediaInfoL1Get|MediaInfoServiceGetMemory|MediaInfoServiceOfferFresh|MediaInfoServiceOfferJoin|FingerprintSTRMTarget|MediaInfoSQLiteGet|MediaInfoHTTPFFProbe)$' \
  -benchmem -count=5 ./internal/mediainfo

go test -run '^$' -bench '^BenchmarkMetadataAnalysisFilter$' \
  -benchmem -count=5 ./internal/gateway

go test -run '^$' -bench '^BenchmarkMetadataCoalescerBurst32$' \
  -benchmem -count=5 ./internal/gateway

go test -run '^$' -bench '^BenchmarkControlTicketStore$' \
  -benchmem -count=5 ./internal/gateway
```

真实 NAS 验收必须使用候选镜像、固定样本和串行 A/B，记录版本、配置、样本数、分位数、
错误率和聚合 I/O。公开结果不得包含真实域名、地址、媒体标题、PartID、ratingKey、Token、
Cookie、签名 URL、完整 User-Agent 或原始 trace。候选代码、Go 版本、Plex/MediaVault
版本、配置或网络路径变化都会使旧结果失效。
