# plex-gateway

> **MediaVault**，`plex-gateway` 的云端 STRM 直链能力基于
> [MediaVault](https://doc.mediavault.qzz.io/) 实现，使用 MediaVault 生成的
> STRM 文件和 `/redirect` 接口。MediaVault 用户交流，
> [Telegram @mediavault3](https://t.me/mediavault3)。

> **状态：** 实验性。透明代理和 STRM 重定向链路已经实现，但客户端兼容性仍然有限，必须针对每个 Plex 客户端和播放模式分别验证。

`plex-gateway` 是一个面向 STRM 云端媒体 Direct Play 的故障回退型 Plex
协议适配器。Plex 仍然是元数据、身份认证、媒体库和观看状态的权威来源。Gateway
观察 Plex 元数据，通过 MediaVault 解析符合条件的 STRM，并返回一次 HTTP 302，
让客户端直接从云端 CDN 下载媒体。

本地 Plex 媒体不会被重写。缓存未命中、路径映射失败、STRM 文件无效、MediaVault
失败以及所有不支持的 endpoint 都会无条件回退到原始 Plex 请求。

在返回云端重定向之前，Gateway 会使用当前客户端请求凭据，请 Plex 授权访问同一个
Part。Part 缓存未命中时，请求会原样发送给 Plex；Gateway 不会读取媒体路径前缀来
推断媒体是否为 STRM。

部分官方和第三方客户端会先发起通用播放决策，而不是直接请求 Part。对于已认证且
明确选中的 STRM Part，Gateway 会在保留完整客户端 profile、并由 Plex 生成完整决策
响应的前提下，请求 Plex 将该播放选择为 Direct Play。只有当该完整响应明确将同一个
Part 标记为 Direct Play 后，Gateway 才会创建短期授权。如果客户端随后请求通用的
`start`、`start.mpd` 或 `start.m3u8`，Gateway 会要求该播放会话及精确 Media/Part
对应的授权，再次为 Part 请求授权，并返回同一个 CDN 重定向，而不会启动 Plex
Transcoder 或合成 Plex 元数据。

Gateway 对所有客户端使用同一套 MediaInfo 投影规则。默认关闭的实验性播放 veto 只在
Plex 已返回同一 STRM Part 的 Direct Play decision、且本次 MediaInfo 增强已经取得新鲜
记录时检查 Apple TV Plex 与 Dolby Vision Profile 5 组合。命中时返回 Plex 兼容的
不支持决策，其他情况保持原播放链路。该判断不保存 verdict、不拦截 Part，也不会触发
额外探测。Apple TV 硬件支持 Profile 5，实际兼容性还受 Plex 版本、容器和片源影响，
因此 veto 默认关闭。

能够在这些路径上接受原始媒体重定向的原生客户端可以直接使用此链路。Plex Web
兼容层默认开启：Gateway 只在 Plex Web shell 中加载一个版本化脚本，并只对同源、
呈 STRM Part 形态的 `/library/parts/.../file` 或 `.strm` 媒体元素移除 `crossorigin`。浏览器原生支持该容器和编码时，可以按
普通媒体元素跟随 302 直接读取 CDN，即使最终源站没有 CORS header；媒体字节仍不经过
Gateway。需要 DASH、转封装或转码的片源仍不支持，Gateway 不合成 manifest，也不代理
视频流。关闭 `PLEX_WEB_DIRECT_PLAY_ENABLED` 后，Plex Web 页面恢复完全透明代理。

## 兼容性状态

| 客户端或路径 | 状态 | 边界 |
| --- | --- | --- |
| Plex 本地媒体 | 透明代理支持 | Gateway 不会重写本地 Part。 |
| Infuse Direct Play | 已验证 Direct Play | 使用 Plex Part 重定向路径。 |
| Plex iOS ExperimentalPlayer | 已验证 Direct Play | STRM 使用通用 decision/start 重定向链路。 |
| Plex for Apple TV | 已验证 Direct Play | HDR STRM 使用通用 decision/start 重定向链路。Apple TV 4K 支持 DV Profile 5 Direct Play，但 Plex 版本、容器和片源组合仍可能影响兼容性；Gateway 默认不拒绝。 |
| Plex Web 云端 Direct Play | 已验证 Direct Play | MP4/H.264/AAC 样本已验证首播、暂停、继续和 seek，并保持 CDN 直达；其他容器与编码取决于浏览器原生能力，需要 DASH、转封装或转码的片源不支持。 |

本项目是独立的社区项目，与 Plex、MediaVault、Infuse 或其各自所有者没有关联，
也未获得其认可或赞助。

## 容器镜像

发布后的镜像将使用：
`ghcr.io/infinitypacer/plex-gateway:X.Y.Z` 和
`ghcr.io/infinitypacer/plex-gateway:latest`。如果部署不应自动跟随下一次发布，
请固定使用版本标签。用户可见变更请参阅
[CHANGELOG.md](CHANGELOG.md)。

## 支持的 STRM 目标

- MediaVault 查询参数格式：`/redirect?path=...&pickcode=...`；
- MediaVault pickcode 格式：`/redirect/{pickcode}`；
- MediaVault pickcode 加文件名格式；
- 使用 `fid` 和 `source=share:...` 的 MediaVault 分享格式；
- 简约 STRM 模式使用的绝对本地路径；
- 第三方 HTTP(S) 直链：Gateway 不发起出站请求，直接将其返回给客户端。

MediaVault URL 输入会被重写为配置的内部 MediaVault origin。同源的短
`/redirect` 重定向链会在 Gateway 内部解析；第一个跨源 HTTP(S) `Location` 会在
不访问 CDN 的情况下返回给客户端。

## 配置

当 `MEDIAVAULT_URL` 和 `PATH_MAPPINGS` 都未设置时，Gateway 仍作为透明代理运行。
要启用云端重定向处理，请同时配置这两个变量。

使用 `docker-compose.example.yml` 前，先将 `app.env.example` 复制为 `app.env`，
再按部署环境填写地址、路径映射和可选 Token。Compose 示例会通过
`env_file: ./app.env` 读取该文件。

```sh
PLEX_URL=http://plex:32400 \
MEDIAVAULT_URL=http://mediavault:7811 \
PATH_MAPPINGS='[
  {"plex_prefix":"/media/cloud","local_prefix":"/strm"},
  {"plex_prefix":"/media/archive","local_prefix":"/archive-strm"}
]' \
go run ./cmd/plex-gateway
```

重要环境变量：

| 变量 | 默认值 | 用途 |
| --- | --- | --- |
| `PLEX_URL` | 必填 | Plex 内部 origin；配置中的凭据会被拒绝。 |
| `PLEX_TOKEN` | 禁用 | 可选 Plex 管理 Token，仅用于启动验证和邻近媒体发现。缺失时仍预热当前播放项。 |
| `MEDIAVAULT_URL` | 禁用 | MediaVault 内部 HTTP(S) origin。 |
| `PATH_MAPPINGS` | 禁用 | Plex 到容器内 STRM 路径的 JSON 映射数组。 |
| `LISTEN_ADDR` | `:32400` | Gateway 监听地址。 |
| `PART_CACHE_TTL` | `24h` | 内存 Part 缓存的有效期。 |
| `RESOLVER_TIMEOUT` | `15s` | MediaVault 解析的端到端超时时间。 |
| `OBSERVE_MAX_BYTES` | `8388608` | 观察解析时复制的最大元数据 body 大小。 |
| `PART_PROBE_TIMEOUT` | `15s` | 无 body 的 Plex Part 授权 probe 超时时间。 |
| `CLOUD_EXTENSIONS` | `.strm` | 以逗号分隔的云端控制文件扩展名。 |
| `TRACE_ENABLED` | `true` | 启用经过脱敏的 Plex 请求顺序追踪。 |
| `PLEX_WEB_DIRECT_PLAY_ENABLED` | `true` | 为 Plex Web shell 加载仅作用于同源 STRM Part 媒体元素的 Direct Play 兼容脚本；不代理媒体字节。 |
| `METADATA_ANALYSIS_FILTER_ENABLED` | `true` | 从详细 metadata 读取中移除会触发 Plex 后台分析的 `asyncAugmentMetadata`；保留 `checkFiles`。 |
| `METADATA_GUARD_ENABLED` | `true` | 限制单项详细 metadata 请求进入 Plex 的并发量。 |
| `METADATA_GUARD_GLOBAL_CONCURRENCY` | `16` | 所有客户端共享的详细 metadata 并发上限。 |
| `METADATA_GUARD_CLIENT_CONCURRENCY` | `16` | 每个 Plex 客户端标识的详细 metadata 并发上限。 |
| `METADATA_GUARD_BATCH_ENABLED` | `true` | 限制逗号分隔的批量 metadata 读取进入 Plex 的并发量。 |
| `METADATA_GUARD_BATCH_CONCURRENCY` | `4` | 所有客户端共享的批量 metadata 并发上限。 |
| `METADATA_GUARD_QUEUE_TIMEOUT` | `10s` | 等待准入的最长时间，超时返回 `429`。 |
| `METADATA_COALESCE_ENABLED` | `true` | 将认证和表示上下文完全一致的单项 metadata GET 合并为 Plex 原生 batch。 |
| `METADATA_COALESCE_WINDOW` | `20ms` | 同组请求的微批聚合窗口，最大 `100ms`。 |
| `METADATA_COALESCE_MAX_ITEMS` | `32` | 单个合成 batch 的唯一 ratingKey 上限，允许 `2-32`。 |
| `METADATA_COALESCE_TIMEOUT` | `5s` | 合成 batch 请求 Plex 并完成拆分的总超时。 |
| `MEDIAINFO_ENABLED` | `true` | 启用 MediaInfo 缓存、受限探测和单项 metadata 增强；初始化失败不影响透明代理。 |
| `DATABASE_PATH` | `./data/plex-gateway.db` | Gateway SQLite 持久数据库路径；容器镜像默认使用 `/app_data/plex-gateway.db`。 |
| `MEDIAINFO_PLAYBACK_QUEUE_SIZE` | `16` | P0 实际播放任务的独立队列容量。 |
| `MEDIAINFO_NEIGHBOR_QUEUE_SIZE` | `50` | P1 邻近项预热的队列容量。 |
| `MEDIAINFO_METADATA_QUEUE_SIZE` | `50` | 前台 metadata 冷 miss 的 P2 队列容量。 |
| `MEDIAINFO_PENDING_TTL` | `5m` | P1/P2 未开始任务的保留时间。 |
| `MEDIAINFO_BACKGROUND_INTERVAL` | `5s` | L1/SQLite miss 后 P1/P2 远程探测的全局最小启动间隔。 |
| `MEDIAINFO_USER_AGENT` | `Infuse-Library/8.5.1` | 没有活动客户端上下文时，后台探测使用的 fallback User-Agent。 |
| `MEDIAINFO_COLD_WAIT` | `5s` | 实际播放 decision 的冷缓存等待上限；metadata 浏览冷 miss 不使用该等待。 |
| `PLAYBACK_VETO_ENABLED` | `false` | 启用实验性 Apple TV Plex Dolby Vision Profile 5 veto；只复用 decision 已取得的新鲜 MediaInfo，不发起额外探测。 |
| `MEDIAINFO_RESPONSE_MAX_BYTES` | `8388608` | 可以缓冲并尝试增强的单个 Plex metadata 响应上限。 |
| `MEDIAINFO_ENRICHMENT_CONCURRENCY` | `8` | 同时缓冲并尝试 L1 投影的单项 metadata 响应上限。 |
| `MEDIAINFO_PREWARM_BEFORE` | `2` | 当前项之前的邻近媒体预热数量。 |
| `MEDIAINFO_PREWARM_AFTER` | `3` | 当前项之后的邻近媒体预热数量。 |

普通 Plex 请求保持透明转发，云端播放请求按下文契约将客户端 header 发送给可信
MediaVault。可选管理 `PLEX_TOKEN` 通过环境变量注入，只发送到配置的 Plex origin，
用于邻近媒体发现；不会写入数据库、日志或任务，也不会发送给 MediaVault、CDN 或
ffprobe。Compose 部署可将它放在不纳入
Git 的 `app.env` 中。推荐让 Compose 只通过 `env_file: ./app.env` 加载运行配置，完整的
中文变量说明和默认值参考 [app.env.example](app.env.example)。

对于云端播放，客户端的
所有请求 header 都会转发到配置的 MediaVault origin，使其能够为相同客户端上下文
生成直链。内部查找始终使用 `GET`，因此即使 MediaVault 不支持 `HEAD`，客户端的
`HEAD` 请求也能收到相同的 302。请将 MediaVault 部署为可信上游，并避免将解析请求
及其 header 写入日志。

客户端像连接 Plex 一样连接 Gateway，并通过 Plex 完成身份认证。Gateway 不需要
单独配置 Plex 用户名或密码，播放也不依赖管理 Token。STRM `/redirect` 播放协议不需要
MediaVault 用于 `/api/v1` 集成的 API key。

### 播放 veto

`PLAYBACK_VETO_ENABLED=false` 时不创建 veto 函数，decision 主线路只经过一次 nil 判断。
显式开启后，Gateway 只对 Apple TV Plex、tvOS、Dolby Vision Profile 5 且 BL
compatibility ID 为 0 的完整 MediaInfo 返回不支持 decision。它只读取 MediaInfo 增强
已经取得的新鲜记录，不新增缓存读取、SQLite、MediaVault 或 CDN 请求，不创建状态，
也不进入 Part 和 universal start 路径。证据缺失、过期、冲突或不匹配一律 fail-open。

这是用于已知片源兼容问题的实验性开关，不代表 Apple TV 普遍不支持 Dolby Vision
Profile 5。配置和性能验收见
[docs/architecture.md](docs/architecture.md) 与
[docs/performance-matrix.md](docs/performance-matrix.md)。

### MediaInfo 响应增强

对于已认证的单项 STRM metadata 请求，Gateway 可以从 L1 或 SQLite 记录补充缺失的
Media、Part 和 Stream 技术字段。若 Plex 的 Part 完全没有 Stream，Gateway 会使用
ffprobe 的流类型和源索引创建描述性的视频、音频和字幕 Stream，并投影 HDR10、Dolby
Vision、bit depth、codec、codecID、bitrate、声道布局和语言码等字段。合成 Stream 不包含 Plex
Stream ID，也不生成 `selected`、`default` 或 `decision` 等播放选择字段。Plex 已经存在
Stream 时只补充身份匹配 Stream 的缺失字段，不创建缺少的其他流，也不覆盖 Plex 值。
当前版本只将兜底探测结果写入 Gateway SQLite，不写入 Plex DB。Plex DB 生产写入属于下一
阶段评估内容，需先完成覆盖率、兼容性、备份和回滚验证。

L1 热路径直接返回，不会为了本次请求同步读取 SQLite。访问续期可能异步 touch SQLite，
不会把持久化 I/O 放进请求关键路径。热缓存投影可以并发执行；metadata 浏览冷 miss 立即
返回原始 Plex 响应，只通过内存投递有界 P2，不等待 SQLite、MediaVault 或 CDN。后台任务
成功后才写入 L1 和 SQLite。实际播放 decision 保留独立的 P0 冷探测预算，因此技术字段可
在播放协商时补齐，而首屏和季页面不会被远程分析阻塞。

当 ffprobe 无法返回媒体总大小时，Gateway 会使用与本次 MediaVault 解析和 ffprobe 相同的
User-Agent 对同一临时直链发送一次 `Range: bytes=0-0`。该请求最多等待 `2s`，只接受包含有效
`Content-Range` 总大小的 `206` 响应，并且不读取媒体响应体。超时、重定向、完整 `200`
响应或字段格式异常都会忽略，不影响已经成功取得的其他 MediaInfo，也不会阻塞 302
播放路径。成功解析后的大小随 MediaInfo 一起进入 L1 和 SQLite，fresh cache hit 不会
重复请求 CDN。

Infuse 等客户端的后台媒体库同步可能逐项请求整个库。带 `skipRefresh` 且产品名以
`-Library` 结尾的后台同步请求只读取现有 MediaInfo 缓存，不创建冷探测。普通单项访问
的冷 miss 也立即返回原始响应，仅非阻塞投递有界 P2；成功的云端 302 和邻近窗口继续按
既定边界提交。因此浏览媒体库不会无界
扩散成全库 CDN ffprobe。任何缓存 miss、超时、结构不支持或投影失败都会返回原始 Plex
response。

### 邻近媒体 MediaInfo 预热

Gateway 在云端 Part 已通过 Plex 授权、MediaVault 已返回最终直链且 Gateway 已写出
302 后，只向有界内存协调器投递一次事件。该信号表示“云端重定向已就绪”，不宣称
客户端已经跟随 302 或真实起播。当前项预热不需要管理 Token；配置有效
`PLEX_TOKEN` 后才会额外发现邻近媒体。

当前项会进入 P0 实际播放队列。后台优先使用明确的 Plex playQueue 顺序，并允许其中的
电影、跨剧条目和多 Media/Part；候选始终采用自身的 PartID 与 STRM fingerprint 落库，
不会写入当前项。当前项只能优先排队，不能抢占正在运行的探测。没有可靠 playQueue 时，
按 Plex 返回的剧集和季顺序查找邻近项，包括 S00 和缺失索引的条目。

默认窗口为前 `2`、后 `3`，后续项先提交；两个方向合计最多配置 `50`。邻近项发现和准备
完成后立即非阻塞投递 P1；只有 L1 和 SQLite 都未命中时，远程探测才受全局最小 `5s`
启动间隔约束。MediaInfo worker 默认并发为 `1`，用于避免 MediaVault/CDN 请求突发。
快速切换 A、B、C 时，新当前项立即进入 P0，旧窗口中尚未投递的候选会被取消；已经投递
或正在运行的任务仍按自己的身份完成并由 singleflight 去重。正在运行的后台探测不会被
当前项抢占。整个发现、STRM 读取、指纹计算和 SQLite 查询均在 302 响应之后执行。

Gateway 只持久化解析后的 MediaInfo，不缓存短期 CDN URL 或原始文件头尾。MediaVault 的
上传预缓存只覆盖经其上传的新文件；若未来提供稳定 MediaInfo/预缓存读取 API，可作为优先
Provider，未命中时再回退 CDN ffprobe。探测失败由 `MEDIAINFO_NEGATIVE_TTL` 抑制重复请求；
协调器不执行无法感知最终错误类型的盲目重试。Token 缺失或失效只禁用邻近媒体发现，
不影响当前项预热、透明代理、metadata 增强或播放重定向。

### Metadata 微批与并发保护

部分客户端会在打开大型媒体库时并发请求大量
`GET/HEAD /library/metadata/{ratingKey}`。启用 `METADATA_GUARD_ENABLED` 后，
Gateway 会在这些单项详细 metadata 请求进入 Plex 前应用全局和单客户端并发上限。
客户端标识只用于进程内限流，并以摘要形式保存，不会写入日志或 metrics。

`METADATA_COALESCE_ENABLED` 默认将短窗口内认证、原始 query、完整客户端 Header、内容
协商和远端身份完全一致的单项 `GET` 合成为 Plex 原生逗号 batch，默认最多包含 `32` 个唯一
ratingKey，可配置上限同样为 `32`。Gateway 按 ratingKey 拆分 XML 或 JSON 响应，并为每个
原请求重新计算 `Content-Length`；gzip 会解压、拆分后重新编码。相同 ratingKey 的重复请求
共享同一 batch 结果。Token、Cookie、Authorization、客户端 profile、query 或身份不同的
请求绝不合批。

微批不缓存 metadata，也不处理 `HEAD`、Range、条件请求、请求体、播放路径、图片、时间线
或写请求。Plex batch 超时、返回非 200、结构异常、缺项、编码不支持或超过 body 上限时，
每个唯一 ratingKey 最多经过单项 Guard 回退一次，重复请求共享该结果；不同条目的回退仍受
Guard 并发与排队上限保护。同组请求会短暂停止再次合批，所有调用者取消后，上游 batch 也会取消。

`METADATA_ANALYSIS_FILTER_ENABLED` 默认从单项和逗号分隔的详细 metadata 读取中只移除
`asyncAugmentMetadata`，避免浏览突发反复调度 Plex 后台分析。`checkFiles` 保持透传，因为
它会影响 Plex 返回的 `Part.accessible` 与 `Part.exists`。过滤器保留其他 query 的原始编码，
不修改 Header、响应或 Plex 数据库，也可以独立关闭用于兼容性回归。

该保护不处理媒体库列表、时间线、观看状态、播放决策、`/library/parts` 或其他 Plex
路径。请求最多排队 `METADATA_GUARD_QUEUE_TIMEOUT`，超时后返回 `429`，不会绕过保护
继续请求 Plex。Guard 不缓存 metadata；微批仅在可证明等价的成功响应上进行协议拆分。

`METADATA_GUARD_BATCH_ENABLED` 使用独立的全局并发池保护
`GET/HEAD /library/metadata/1,2,...`。批量读取不会占用交互式单项 metadata 的全局或
单客户端槽位，metadata PUT 等修改请求也不会进入批量池。

当前默认 Guard 为单项全局 16、单客户端 16、批量 4。合成 batch 使用批量池，异常回退使用
单项全局与单客户端池。提高或关闭 Guard 必须以候选版本的真实客户端压测为依据。

## 通过 Plex 发布 Gateway 地址

在 Plex Web 中打开服务器的 **Settings > Network > Custom server access URLs**，
添加外部可访问的 Gateway origin；当端口不是默认端口时，同时填写 scheme 和公网端口：

```text
https://plex-gateway.example.com:443
```

这是 Plex 通过 plex.tv 获取服务器连接信息时应向客户端发布的地址。请填写面向客户端
的反向代理或 Gateway 地址，不要填写内部 `PLEX_URL`，也不要填写 MediaVault 地址。
客户端仍然通过 Plex 登录；Gateway 不会另外接收 Plex 用户名或密码。

发布的 URL 必须将所有 Plex 路径都路由到 Gateway。随后 Gateway 会将普通控制流量
代理到内部 Plex origin，并仅处理下文所述符合条件的 Direct Play 路径。

### 让 Gateway 成为唯一可访问的 Plex 前端

Plex 的 Custom server access URLs 会增加连接候选地址，不会移除自动发现的地址或
之前缓存的地址。如果所有客户端都必须经过 Gateway，就必须通过部署拓扑确保连接由
Gateway 负责：

Plex 使用 `network_mode: host` 与这种独占模式不兼容。它会在 Gateway 使用之前绑定
宿主机的 `32400` 端口，并通过局域网发现、账户连接候选地址和客户端缓存暴露直接的
Plex 路由。即使 Plex 发布了自定义 Gateway URL，客户端仍可能绕过 Gateway。仅配置
自定义 URL 不能替换这些直接路由。

- 将 Plex 和 Gateway 放在同一个用户定义的 Docker network；
- 使用 bridge 模式运行 Plex，而不是 host 模式；
- 不要在宿主机发布 Plex 容器的 `32400` 端口；
- Plex 的 **Allowed Networks** 不得包含 Gateway 地址或所在 Docker 网段；
- 将宿主机的 `32400` 端口发布到 Gateway 容器的 `32400` 端口，使现有 Plex 连接也在 Gateway 终止；
- 将 Gateway 的 `PLEX_URL` 配置为 Plex 的内部服务名，例如 `http://plex:32400`，避免经由面向宿主机的 Gateway 形成回环；
- 当自动局域网发现会暴露 Plex 直连路径时，禁用 Plex GDM，并且只在 Plex Network 设置中发布面向客户端的 Gateway URL。

```yaml
services:
  plex:
    networks: [media]
    # 不要面向宿主机发布端口。保留现有的 volumes、devices、environment
    # 以及 restart policy。

  plex-gateway:
    ports:
      - "32400:32400"
    environment:
      PLEX_URL: "http://plex:32400"
    networks: [media]

networks:
  media:
    name: media
```

反向代理应指向 Gateway 的 listener，而不是 Plex。Plex 仍可能向 plex.tv 报告其
私有容器地址；该地址必须对客户端网络不可访问。验收条件是：所有可访问的、已发布或
已缓存的 Plex 地址都终止于 Gateway。

Gateway 启动时会使用随机无效 Plex Token 检查内部 Plex 是否仍执行客户端认证；只有
Plex 返回 `401` 或 `403` 才会开始监听。若 Plex 将 Gateway 网段配置为免认证网络，
Gateway 会拒绝启动，避免透明代理继承该权限。修改 Plex 网络认证设置后需要重启
Gateway 重新执行检查。

将 Plex 运行在另一台机器、继续使用 host 模式或保留可访问的 Plex 宿主机端口，仍可
提供明确的可选 Gateway endpoint，但不能保证客户端不会选择 Plex 直连。这些拓扑不
属于独占 Gateway 部署契约。

## Endpoints

- `GET /health` 返回进程健康状态，以及不含媒体身份和凭据的 MediaInfo 缓存、探测队列和邻近媒体预热摘要。
- `GET /metrics` 返回固定结构的 JSON 计数器，以及 resolver 和完整重定向链路的
  延迟总计、样本数、最近值和最大值。`metadata_analysis_params_removed_total` 记录已过滤
  的详细 metadata 请求；启用 metadata 保护时还会返回准入、超时、
  活动和排队计数；微批另记录 offered、batch、唯一 item、fallback 和 active；MediaInfo
  调度器按固定 P0/P1/P2 结构返回准入和丢弃结果。所有指标都不包含请求标签或凭据。
- 其他所有 endpoint 都遵循 [docs/architecture.md](docs/architecture.md) 中说明
  的 Plex 代理或 Direct Play 拦截规则。

### 清空 MediaInfo 冷热缓存

在 Compose 项目目录执行：

```sh
./scripts/reset-mediainfo-cache.sh
```

脚本通过容器内 loopback 维护接口热清理缓存，Gateway 和 Plex 代理不会停止。清理前
会把完整 SQLite 数据库备份到 `app_data/backups/`，然后同步清空 L1、当前 MediaInfo
队列和可重建的 `media_info_records`。数据库、迁移记录和其他模块表不会被删除。

Compose 服务名不是 `plex-gateway` 时，可以通过环境变量指定：

```sh
PLEX_GATEWAY_COMPOSE_SERVICE=gateway ./scripts/reset-mediainfo-cache.sh
```

镜像内同时提供 `/usr/local/bin/plex-gateway-reset-mediainfo-cache`，因此也可以直接执行：

```sh
docker exec plex-gateway plex-gateway-reset-mediainfo-cache
```

维护接口只接受容器本机 loopback 请求，不通过公开 Gateway endpoint 或反向代理开放。

## 探测 Plex 媒体项

`plex-probe` 会记录一个元数据项的 Part 标识符和路径。请将 Token 放在环境变量中，
不要作为命令行参数传入：

```sh
PLEX_URL=http://plex:32400 \
PLEX_TOKEN='...' \
go run ./cmd/plex-probe \
  -rating-key 12345 \
  -output plex-probe-report.json
```

Probe 报告权限为 `0600`，并且不会包含 Plex origin 或 Token。报告仍会保留 Plex 可见
的文件路径和 rating key，分享前请先进行脱敏。

## 验证

```sh
go test ./...
go test -race ./...
go vet ./...
docker build -t plex-gateway:mvp .
docker run --rm plex-gateway:mvp version
```

Copyright (C) 2026 InfinityPacer。

本项目依据 GNU General Public License v3.0 only
（`GPL-3.0-only`）授权。详见 [LICENSE](LICENSE)。
