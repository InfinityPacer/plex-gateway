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
- 一个 universal decision 最多等待一次 `MEDIAINFO_COLD_WAIT`。veto 只复用响应增强已经
  取得的新鲜记录，不增加缓存读取或远程探测。
- 同一 MediaInfo key 的并发请求必须共享 singleflight。并发数不能突破配置的 worker
  上限；当前项只能优先排队，不能抢占正在运行的后台 probe。
- 302 响应只包含控制流。媒体字节始终由客户端直接从 CDN 读取。

## 当前基准

基准日期为 2026-08-29。代码运行于 `darwin/arm64`、Apple M3 Max、Go 1.26.5。
每项运行三到五轮；下表给出观测范围。工作站数据只用于发现代码回退，换架构或 Go
版本后必须重新采集。

| 操作 | 结果 | 分配 | 说明 |
| --- | ---: | ---: | --- |
| MediaInfo L1 cache `Get` | 469.0-476.0 ns/op | 960 B, 7 allocs | 纯缓存结构基准。 |
| MediaInfo service `GetMemory` | 1.356-1.423 us/op | 2880 B, 21 allocs | 包含记录防御性复制、兼容性校验和访问续期判断。 |
| STRM target fingerprint | 382.0-387.4 ns/op | 448 B, 6 allocs | MediaVault redirect URL 规范化和 SHA-256。 |
| SQLite 单记录 `Get` | 14.60-15.37 us/op | 4016 B, 74 allocs | 独立本机 SQLite 读取；生产启动会先恢复到 L1。 |
| 本机 HTTP 合成媒体 ffprobe | 22.48-33.36 ms/op | 约 119-125 KiB, 232-234 allocs | 只验证 worker 代码，不代表 NAS/CDN。 |

真实 MediaVault/CDN 抽样为 5/5 成功：MediaVault redirect p50 141 ms、范围
109-533 ms；受限 CDN ffprobe p50 601 ms、范围 403-674 ms；合计 p50 761 ms、范围
543-955 ms。每次读取约 1.22 MiB 并发生 2 次 seek。样本量较小，只证明当前探测通常能在
默认 5 秒冷等待内完成，不能作为 CDN SLA。

当前 Metadata Guard 默认值为单项全局 8、单客户端 4、批量 3。单项全局 16、单客户端 4、
批量 3 只有在 Plex MediaInfo 写库达到较高覆盖率并完成重新压测后才作为候选，不属于当前
版本的性能承诺。

## 端到端验收矩阵

| 场景 | 请求期允许的 I/O | 必须采集 | 验收条件 |
| --- | --- | --- | --- |
| 本地媒体透明代理 | Plex | 直连 Plex 与经 Gateway 的 p50/p95/p99、错误率、上游请求数 | 0 功能错误；Gateway 增量 p95 <= 20 ms、p99 <= 50 ms，且相对增加 <= 10%。 |
| STRM Part 302 | Plex Part 授权、MediaVault redirect | 302 p50/p95/p99、Plex/MV 次数、响应字节 | 100% 健康样本返回 302；p95 < 2 s、p99 < 5 s；Gateway 不传输媒体字节。 |
| universal start | Plex Part 授权、MediaVault redirect | grant 命中、上游次数、302 延迟 | 不读取 metadata、STRM 或 MediaInfo；无 grant 时透明回退。 |
| Decision，Infuse | Plex metadata 和 decision | decision 延迟、veto 结果 | veto 始终放弃判断；不增加缓存、MediaVault 或 CDN 请求。 |
| Decision，L1 热缓存 | Plex metadata 和 decision，L1 | p50/p95/p99、SQLite/MV/CDN 次数 | 0 SQLite、0 MV、0 CDN；p95 <= 1 s、p99 <= 2 s。 |
| SQLite 启动恢复 | 启动期 SQLite，播放期 L1 | 恢复耗时、首个 decision 延迟 | 首个请求 0 ffprobe、0 请求期 SQLite。 |
| Decision，真实冷探测 | Plex、MediaVault、CDN、SQLite 写入 | resolver/probe/总延迟、读取字节、seek、fail-open | 成功样本 p95 < 2 s、p99 <= 5 s；超过冷等待返回可用的 fail-open 响应。 |
| Apple TV DV5，veto 开启 | Plex metadata、decision 和既有 MediaInfo 流程 | reject 延迟、grant、额外 I/O | 新鲜完整记录命中时 reject 且不创建 grant；相对 veto 关闭增加 0 次缓存、MV、CDN 请求。 |
| 同一 Part 并发 2/4/8/16 | 一个共享 probe | 唯一 probe 数、延迟、队列、active | 每个 key 最多 1 次 probe，无重复 CDN 读取。 |
| 不同 Part 并发 2/4/8 | 受 worker 上限约束 | active、队列、CPU、内存、错误率 | 不突破 `MEDIAINFO_CONCURRENCY`，不拖慢本地或 302 路径。 |
| A/B/C 快速切换 | 当前项交互 probe、后台邻近预热 | 每项延迟、身份对应、替换/队列指标 | 新当前项优先排队；不能抢占正在运行的 probe；无跨项 MediaInfo、grant 或 redirect。 |

表中的验收条件是候选版本的门槛，不代表本机微基准已经替代端到端验收。当前已有证据
包括本机代码基准和 5/5 成功的 MediaVault/CDN 探测抽样；本地透明代理、302 热冷路径、
决策热冷路径的 p95/p99、并发矩阵和快速切换矩阵，仍需使用候选版本在目标 NAS 上串行采集。

客户端首次画面必须单独测量。Gateway 302 时间、客户端跟随 Location、CDN 首字节和
播放器首帧是不同阶段，不能用 302 延迟代替起播性能。

## 复现命令

```sh
go test -run '^$' \
  -bench 'Benchmark(MediaInfoL1Get|MediaInfoServiceGetMemory|FingerprintSTRMTarget|MediaInfoSQLiteGet|MediaInfoHTTPFFProbe)$' \
  -benchmem -count=5 ./internal/mediainfo
```

真实 NAS 验收必须使用候选镜像、固定样本和串行 A/B，记录版本、配置、样本数、分位数、
错误率和聚合 I/O。公开结果不得包含真实域名、地址、媒体标题、PartID、ratingKey、Token、
Cookie、签名 URL、完整 User-Agent 或原始 trace。候选代码、Go 版本、Plex/MediaVault
版本、配置或网络路径变化都会使旧结果失效。
