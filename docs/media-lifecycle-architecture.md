# 媒体生命周期与 115 分享架构设计

状态：Draft，供架构评估使用，尚不代表已承诺的实现范围。

本文定义 MoviePilot、MediaVault、Plex、`plex-gateway` 以及可选 Plex
MediaInfo Helper 之间的职责边界。设计同时覆盖本地下载、115 自有文件、
115 分享转存和 115 分享挂载，不假定所有媒体都来自 MoviePilot，也不假定
MediaVault 会提供尚未公开的接口。

## 1. 决策摘要

核心决策如下：

1. **MoviePilot 是用户可控的期望状态、策略与生命周期控制平面。**
   MoviePilot 管理任务状态、幂等、重试、做种约束、本地清理门禁以及跨系统
   reconciliation。外部资源是否存在、属于谁、当前是否可用，仍由对应 provider
   持有事实，MoviePilot 只保存带时间戳和版本的观察结果。
2. **MediaVault 是 115、STRM 和直链能力提供者。**
   已有稳定能力优先复用，但 MoviePilot 只能依赖公开 HTTP API、明确的文件契约或
   已验证的版本化接口，不能依赖 MediaVault 私有数据库、内部模块或未来承诺。
3. **Plex 是媒体库、用户权限和观看状态权威。**
   技术 MediaInfo 可以投影到 Plex，但 Plex 私有数据库不能成为 MediaInfo 的唯一
   事实来源。
4. **独立 Gateway 模式下，Gateway 是在线播放协议层。**
   Gateway 负责透明反向代理、认证透传、Direct Play 兼容、STRM 解析、MediaVault
   直链解析、302、Plex 过载保护和可观测性，并在隔离的 analysis worker 中提供有界
   MediaInfo fallback。Gateway 不负责下载、上传、分享转存、做种、本地清理或 Plex
   数据库写入。MediaVault 自带的 Plex 反代是另一种互斥部署模式，不能与 Gateway
   串联形成双重播放代理。
5. **按能力集成，不按产品名称耦合。**
   MediaVault 有稳定接口就由 `MediaVaultAdapter` 实现对应能力；没有接口时保持
   `manual_only`，或由用户可控的 MoviePilot 插件/provider 独立实现。
6. **115 分享转存和分享挂载产生不同类型的 backing。**
   分享转存形成自有云端 backing；分享挂载只是依赖外部分享持续有效的借用 backing。
   同一 asset 可以同时存在多个 backing，不能用单一所有权状态互相覆盖。

## 2. 目标与非目标

### 2.1 目标

- 让多种媒体来源进入同一套可审计、可重试、可恢复的生命周期；
- 最终允许本地仅保留下载、做种或短期处理所需的数据；
- 在安全回收本地文件前验证云端副本、STRM、Plex 和播放链路；
- 允许 MediaVault 能力存在时直接复用，缺失时由可控代码补齐；
- 支持将技术 MediaInfo 投影到 Plex，改善 Apple TV 和官方 Plex 客户端兼容性；
- 保持 Gateway 可独立开源、部署和演进，不依赖 MoviePilot 或 MediaVault 内部实现；
- 保留未来把成熟能力移交 MediaVault 实现的可能性。

### 2.2 非目标

- 不把 MoviePilot、MediaVault 或 Gateway 合并为一个进程；
- 不让 Gateway 直接调用 115 API；
- 不让 Gateway 读取 MoviePilot、MediaVault 或 Plex 数据库；
- 不使用未公开的 MediaVault endpoint 作为生产契约；
- 不把临时 CDN 签名 URL、115 Cookie、分享提取码或 Plex Token 写入任务记录；
- 不因云端上传成功就立即删除本地媒体；
- 不让模糊的“删除媒体”操作同时删除索引、STRM、本地文件和云端文件。

## 3. 总体架构

```text
Acquisition sources

MoviePilot download     Existing 115     115 share       Other provider
        │                    │            │   │                │
        │                    │       transfer mount            │
        └──────────────┬─────┴────────────┴───┴────────────────┘
                       ▼
          MoviePilot MediaVaultOrchestrator
          task state / policy / reconciliation
                       │
               capability provider layer
             ┌─────────┼──────────┐
             ▼         ▼          ▼
      MediaVault    Self-owned   Manual-only
      adapter       providers    capability
             │
             ▼
      cloud object / share lease / STRM
             │
             ├──────────────► Plex library scan
             │                       │
             │                       ▼
             │               ratingKey / PartID
             │                       │
             ▼                       ▼
Plex client ─────► plex-gateway ─────► Plex control plane
      │          playback │ analysis       │
      │                   │    ├── L1 / SQLite / worker
      │                   │    └── optional MediaInfo projection
      │                   └────► MediaVault /redirect
      │                                  │
      └──────────────────────────────────► CDN media bytes
```

MoviePilot 插件是编排者，但不是所有业务能力的实现者。Provider 层把“编排什么”和
“由谁执行”分开，避免 MediaVault 缺少某项接口时迫使整个路线停滞。

### 3.1 单一播放代理约束

一个 Plex 对外入口只能选择一个播放代理：

```text
Mode A, independent gateway
Client → plex-gateway → Plex
                  └──→ MediaVault /redirect → CDN

Mode B, MediaVault native proxy
Client → MediaVault Plex proxy → Plex / CDN
```

禁止部署 `Client → plex-gateway → MediaVault Plex proxy → Plex`。双重代理会重复执行
metadata 观察、权限校验、fallback 和端口接管，故障归属也无法清晰判断。

当前蓝图选择 Mode A，原因是 Gateway 需要保持开源可审计、独立发布，并承载 Plex 请求
保护、协议兼容实验和非 MediaVault resolver 的演进空间。MediaVault 原生代理继续作为
部署替代方案和兼容性基线；若它未来覆盖全部需求，可以停用 Gateway 播放路径，而不影响
MoviePilot 生命周期编排、MediaInfo 权威记录或 Plex 观看状态。

## 4. 组件职责

| 组件 | 应负责 | 不应负责 |
| --- | --- | --- |
| MoviePilot | 搜索、订阅、下载、识别、整理历史、任务编排、做种状态、本地回收策略 | 解析 Plex 在线播放协议、代理视频流量 |
| `MediaVaultOrchestrator` 插件 | 期望状态、任务模型、provider 调度、幂等、重试、reconciliation、清理门禁 | 冒充外部资源事实源、导入 MediaVault 私有模块、直接操作 Plex DB |
| MediaVault | 115 文件操作、分享能力、云端整理、STRM 生成、`/redirect`、上传预缓存；在替代部署模式中可独立承担 Plex 反代 | MoviePilot 做种状态、Plex 用户状态；在独立 Gateway 模式中不接管 Plex 对外入口 |
| Plex | 媒体库、描述元数据、认证、播放会话、时间线、观看状态 | 云盘上传、115 分享生命周期 |
| `plex-gateway` | Plex 透明代理、认证透传、Part 识别、Direct Play 适配、302、请求准入、熔断，以及隔离的 MediaInfo fallback | 入库任务、下载、做种、本地清理、播放路径同步 ffprobe、Plex DB 写入 |
| Plex MediaInfo 投影 | 评估官方 API、受支持接口和独立 Helper，受控补充 Plex 技术信息 | MediaInfo 唯一事实源、跨系统任务编排 |

## 5. Capability-first 集成

### 5.1 能力状态

MoviePilot 插件对每项外部能力记录以下状态：

| 状态 | 含义 | 是否允许自动编排 |
| --- | --- | --- |
| `stable_api` | 有公开、版本化并验证过的 API | 是 |
| `candidate_api` | 有公开文档，但当前部署尚未完成契约验证 | 否 |
| `file_contract` | 有原子、可验证的共享文件契约 | 是 |
| `runtime_option` | 有公开部署说明，但不是供编排调用的 API | 由部署策略决定 |
| `manual_only` | 产品支持，但没有稳定自动化接口 | 否，只记录人工检查点 |
| `self_hosted` | 由用户可控 provider 实现 | 是 |
| `unavailable` | 当前无法实现或未配置 | 否 |

产品界面中存在某项功能，不等于该能力可以被 MoviePilot 稳定编排。禁止通过抓取 Web
界面、调用隐藏 endpoint 或读取 MediaVault 数据库把 `manual_only` 伪装成
`stable_api`。

### 5.2 当前已知 MediaVault 能力

| 能力 | 状态 | 当前可依赖边界 | 规划处理 |
| --- | --- | --- | --- |
| STRM `/redirect` | `stable_api` | 已验证 HTTP 重定向契约 | Gateway 继续直接使用 |
| 触发目录监控整理 | `candidate_api` | 公开文档列出 `/api/v1/monitor/trigger-organize` | Phase A 使用 `X-API-Key` 完成写契约测试后升级为 `stable_api` |
| STRM 文件 | `file_contract` | 共享只读文件树 | Gateway 按文件契约读取 |
| Plex 302 反代 | `runtime_option` | 官方文档提供独立反代、权限自检和 Plex fallback | 与 `plex-gateway` 二选一，不串联 |
| 分享转存 | `manual_only` | 产品能力已存在，公开自动化 API 尚未确认 | 发现稳定 API 后增加 adapter |
| 分享挂载 | `manual_only` | 产品能力已存在，公开自动化 API 尚未确认 | 不猜测隐藏接口 |
| 完整技术 MediaInfo 查询 | `self_hosted` | 未发现公开稳定 API | 先使用可控 provider，未来可替换 |
| 生命周期事件/Webhook | `unavailable` | 未确认完整可靠契约 | 任务以轮询和 reconciliation 兜底 |

认证后的 OpenAPI schema 可以用于能力发现，但只有公开文档、版本说明和契约测试共同
确认后，endpoint 才能进入 `stable_api`。前端正在调用的隐藏接口不构成生产契约。

MediaVault 后续增加能力时，只新增或替换一个 provider，不改变任务模型、Gateway 或
Plex 的职责。

## 6. MoviePilot 编排插件

暂称 `MediaVaultOrchestrator`。名称仅用于设计讨论，不代表最终插件名。

### 6.1 内部模块

```text
MediaVaultOrchestrator
├── WorkflowCoordinator
├── AssetRepository
├── ProviderRegistry
│   ├── MediaVaultAdapter
│   ├── LocalMediaInfoProvider
│   ├── RemoteMediaInfoProvider
│   └── optional CloudProvider
├── PlexProjectionClient
├── Reconciler
└── CleanupPolicy
```

- `WorkflowCoordinator` 只推进经过验证的状态转换；
- `AssetRepository` 保存编排事实和外部引用，不保存媒体数据或凭据；
- `ProviderRegistry` 根据能力状态选择 MediaVault、可控实现或人工检查点；
- `Reconciler` 定期回读外部状态，修复丢事件、进程重启和部分失败；
- `CleanupPolicy` 是唯一允许把任务标记为本地可回收的决策者。

### 6.2 集成模式

支持两种 MediaVault 对接强度：

1. **松耦合模式**
   MoviePilot 整理到共享 staging 目录，MediaVault 目录监控接管上传、整理和 STRM。
   插件通过已公开的 trigger API 发起处理，并通过文件/Plex 结果回读确认完成。
2. **API 编排模式**
   仅当 MediaVault 发布稳定、版本化接口后，由 adapter 直接创建分享转存、分享挂载、
   STRM 或 MediaInfo 任务。

松耦合模式是当前可落地基线。API 编排模式不能成为首版前置条件。

## 7. 统一媒体制品模型

`MediaAsset` 是 MoviePilot 插件中的生命周期记录，不等于 Plex item、STRM 文件或
云盘文件。

```json
{
  "schema_version": 1,
  "asset_id": "internal-stable-id",
  "revision": 3,
  "media_identity": {
    "source": "tmdb",
    "id": "12345",
    "season": 1,
    "episode": 2,
    "edition": null
  },
  "origin": {
    "kind": "torrent|existing_cloud|share_transfer|share_mount|manual",
    "reference": "opaque-reference"
  },
  "content": {
    "size": 0,
    "fingerprint": "optional-content-fingerprint"
  },
  "backings": [
    {
      "backing_id": "stable-backing-id",
      "kind": "local_file|owned_cloud_file|borrowed_share",
      "ownership": "owned|borrowed",
      "status": "pending|available|verified|unavailable|retired",
      "fingerprint": "optional-content-fingerprint",
      "external_reference": "opaque-reference",
      "retention_lease": {
        "reason": "seeding|policy|manual|none",
        "valid_until": null
      },
      "observed_at": "timestamp",
      "provider_revision": "opaque-version"
    }
  ],
  "presentation": {
    "status": "missing|ready|stale|unavailable",
    "strm_path": "logical-path",
    "strm_fingerprint": "hash",
    "plex_rating_key": null,
    "plex_part_id": null
  },
  "plex_projection": {
    "status": "missing|indexed|stale|failed"
  },
  "technical_metadata": {
    "status": "missing|probing|ready|failed|stale",
    "fingerprint": null,
    "provider": null
  },
  "playback": {
    "status": "unknown|verified|degraded|failed",
    "verified_at": null
  },
  "cleanup": {
    "status": "blocked|eligible|queued|reclaimed",
    "decision_revision": null
  }
}
```

### 7.1 身份规则

- `asset_id` 是编排内部稳定 ID，不使用 `ratingKey`、`PartID` 或 STRM 路径代替；
- 同一 asset revision 可以同时拥有多个 backing，例如本地做种文件与已验证的自有云端
  副本，不能用单值 ownership 字段互相覆盖；
- `ratingKey` 和 `PartID` 是 Plex 投影引用，重新扫描后允许变化；
- STRM fingerprint 由规范化 STRM 内容和逻辑路径生成，用于发现重新生成；
- 内容 fingerprint 优先使用可验证的内容哈希与大小；无法取得时允许为空；
- 分享链接、提取码、Cookie 和签名 URL 不进入 `MediaAsset`，只保存不透明引用。
- 每个外部事实都记录 `observed_at` 和 provider revision；过期观察不能直接触发删除。

## 8. 来源与所有权

### 8.1 MoviePilot 本地下载

```text
torrent discovered
→ downloaded
→ organized locally
→ MediaInfo probed while local bytes exist
→ MediaVault processing requested
→ owned cloud backing verified
→ STRM presentation ready
→ Plex indexed
→ optional MediaInfo projected
→ playback verified
→ seeding policy satisfied
→ local cleanup eligible
```

MoviePilot 持有下载器和做种事实，因此只有 MoviePilot 可以决定何时删除种子任务和本地
数据。MediaVault 不得根据上传成功自行删除仍在做种的文件。本地 backing 与云端
backing 会同时存在，直到 MoviePilot 的清理策略确认本地 retention lease 已结束。

### 8.2 已存在的 115 自有文件

```text
existing own file
→ owned cloud backing
→ MediaVault organize
→ STRM ready
→ Plex indexed
```

此路径可以完全绕过 MoviePilot 下载，但仍由插件建立 `MediaAsset` 并跟踪后续投影。

### 8.3 115 分享转存

分享转存把外部分享中的文件保存到自己的 115 账号：

```text
share reference
→ transfer requested
→ own cloud copy verified
→ create owned cloud backing
→ organize / STRM / Plex
```

转存成功后，自有副本不再依赖原分享持续有效。转存记录清理、原分享失效和自有云端
文件删除必须是三个不同操作。

### 8.4 115 分享挂载

分享挂载不产生自有副本：

```text
share reference
→ create borrowed share backing
→ share STRM
→ Plex
```

它的可用性为：

```text
share valid
AND source file exists
AND consumer account is valid
AND upstream permits playback
```

设计约束：

- borrowed share backing 不能原地改成 owned cloud backing；转存成功后新增 owned backing，
  再按策略退休 borrowed backing；
- 切换消费账号不应改变 `asset_id`，通常也不要求重新生成 STRM；
- 分享失效时标记 `unavailable`，不能自动删除 Plex 观看状态；
- 允许显式执行 `promote_to_owned`，将分享转存到自有账号后产生新 revision；
- 分享链接和提取码按凭据处理，禁止进入 Gateway 日志、metrics 或 debug API；
- 是否让借用资源进入正式 Plex 主库，应作为部署策略，而不是硬编码。

## 9. 正交事实与操作账本

媒体生命周期不是单一线性状态机。以下维度必须独立保存和退化：

```text
asset identity and revision
├── acquisition facts
├── backings[]
├── STRM presentation
├── Plex projection
├── MediaInfo record
├── playback verification
└── cleanup decision
```

例如，STRM 可以已经就绪但 Plex 尚未索引；Plex 已索引但 MediaInfo 过期；已经播放验证
过的分享也可能后来失效。新 revision 验证期间，旧 revision 仍可继续提供播放。

`cleanup.status=eligible` 是由当前 revision、可用 backing、Plex/播放验证、做种策略和
retention lease 计算出的派生决策，不是外部任务可以直接写入的流水线状态。执行删除前
必须重新计算并使用 compare-and-set 确认决策 revision 未变化。

外部副作用单独记录到 operation ledger：

```text
operation_key = asset_id + revision + action + provider
status        = pending | leased | retryable | succeeded | terminal
```

每项 operation 记录 provider 能力版本、外部任务引用、租约、尝试次数、下一次重试时间、
验证证据摘要和失败类别。所有操作必须幂等；事件只负责唤醒，reconciliation 才负责回读
事实，因此不能只依赖一次 webhook。

## 10. MediaInfo 架构

### 10.1 最佳探测时机

MoviePilot 本地下载路径应在真实文件仍在本地时执行 ffprobe。这是成本最低、信息最完整
且不会依赖临时 CDN URL 的时机。

其他来源按以下顺序选择 provider：

1. MediaVault 已发布并验证的稳定 MediaInfo API；
2. MediaVault 或其他生产者输出的版本化 sidecar；
3. 用户可控的受限远程 ffprobe provider；
4. 标记缺失，保留 Gateway 保护，不反复无界探测。

远程探测必须限制 ffprobe 的 `probesize`、`analyzeduration`、timeout、并发和负缓存，
并监测实际网络读取量。`probesize` 是分析窗口，不是 HTTP 总下载量硬上限；Gateway 不能
仅凭该参数承诺固定 Range 字节数。分享挂载失效时应标记 source unavailable，不应持续
重试拖垮 Plex 或 115。

### 10.2 记录契约、存储与投影

统一的 `MediaInfoRecord` 目标契约必须版本化，至少包含：

- `schema_version`、记录身份、provider revision、上游关联和内容 fingerprint；
- container、duration、size；
- 视频、音频和字幕 stream；
- provider、`probed_at`、`ready|stale|negative` 状态；
- 负缓存期限和生成该记录时的 backing fingerprint。

当前 Gateway SQLite 只持久化成功记录，负缓存状态及期限保存在有界内存中；因此当前表
结构不是上述跨生产者公开契约的完整实现。

发布方式优先使用原子 sidecar 或稳定只读 API。消费者不得直接读取 MoviePilot 插件表。
内容或 STRM fingerprint 变化时记录立即失效；provider schema 不兼容时消费者必须拒绝
使用而不是猜测字段。

MoviePilot 已建立生命周期记录时，上游关联包含 `asset_id` 和 asset revision；未经过
MoviePilot 的既有云盘或分享媒体不伪造 `asset_id`，由 Gateway 使用 Plex server identity、
PartID 和 STRM fingerprint 建立本地稳定键。ratingKey 和 PartID 仍是允许变化的 Plex
关联，不单独充当跨扫描、跨内容版本的媒体唯一身份。

```text
authoritative MediaInfo record
        │
        ├── MoviePilot plugin workflow and storage
        ├── versioned sidecar / read-only API
        ├── Gateway read cache
        └── optional Plex projection
```

对于 MoviePilot 管理的媒体，首版权威记录采用 MoviePilot 插件自有表，使用 MoviePilot
支持的数据库引擎和插件表生命周期；少量配置、游标和快照继续使用插件基类存储。为了解耦
消费者，同时原子输出版本化 sidecar，或由插件暴露只读 API。该路径不要求 Gateway 再建
一份权威记录。

未来 MediaVault 提供稳定 API 后，可以替换探测 provider；是否同时替换权威存储应通过
迁移评估决定，不能因为 provider 改变就丢失历史记录和版本契约。

当 MoviePilot、MediaVault 或其他生产者提供稳定记录时，Gateway 的 SQLite 只是消费
缓存。当媒体来自既有云盘、分享挂载或其他未提供 MediaInfo 的来源时，Gateway fallback
记录是该探测结果的持久事实源。Plex 中的技术信息始终是可重建投影。任何一份投影丢失
都不能导致已验证的 MediaInfo 无法恢复。

### 10.3 Gateway MediaInfo fallback

Gateway fallback 是当前优先实现的兜底能力，不要求媒体先经过 MoviePilot，也不依赖
MediaVault 尚未公开的 MediaInfo API：

```text
Plex Part / cloud redirect
        │
        ▼
Gateway discovery and prewarm queue
        │
        ├── L1 immutable snapshot
        ├── SQLite WAL result store
        └── bounded MediaInfo worker
                    │
                    ▼
          MediaVault /redirect → CDN → ffprobe
```

单实例首版只使用 L1 与 SQLite。Redis 仅在多个 Gateway 或 worker 需要共享热缓存、
singleflight 或短租约时加入；PostgreSQL 仅在 SQLite 单写者成为实测限制时替代 SQLite。
首版不同时维护 Redis、SQLite 与 PostgreSQL，也不维护两份持久权威。

探测任务使用稳定身份和内容指纹，不使用短期 CDN URL 作为缓存键。至少关联 Plex server
identity、ratingKey、PartID、STRM fingerprint、可用时的 provider/content fingerprint、
MediaInfo schema 版本和探测实现版本。探测失败不得覆盖已知良好记录；相同指纹使用
singleflight 和负缓存抑制重复失败。当前不执行无法识别最终错误类型的自动重试或退避。

Gateway fallback 默认将成功记录视为 `30d` fresh，并将最近使用记录保留 `180d`。
fresh 到期后可以立即返回仍在 retention 内的已知良好记录，同时在后台低优先级复验；
活跃访问会按限频机制续租 retention，而不是每次播放执行完整 ffprobe。超过 retention 且
长期未访问的记录才允许由 GC 删除。

Gateway 支持以下范围：

| 范围 | 触发方式 | 约束 |
| --- | --- | --- |
| 单 Part | metadata miss/stale 或成功播放 | 自动、高优先级、去重 |
| 邻近媒体 | 当前项云端重定向就绪后的 P1 预取 | 当前实现范围；默认前 2 后 3，可配置双向窗口，远程启动统一限速 |
| 单季 | 显式预热或计划任务 | 后续批量能力；分页、checkpoint |
| 整剧 | 显式后台任务 | 后续批量能力；按季分页，不由一次播放自动扩散 |
| STRM 目录 | 管理员扫描或 reconciliation | 后续批量能力；限制到配置 mapping root，可取消和恢复 |

当前实现不执行启动全库、单季、整剧或目录批量预热。当前项在 302 写出后立即投递 P0；
邻近项默认按后 3、前 2 的顺序准备并非阻塞投递 P1。只有 L1 和 SQLite 都未命中的 P1/P2
才共享 `5s` 远程启动间隔。该信号只表示云端重定向就绪，不代表客户端已跟随 302 或真正
起播。快速切换产生的新当前项优先于旧窗口中尚未投递的候选；已经投递的任务保留自身
PartID 与 STRM fingerprint，并由 singleflight 去重。远程 probe 初始并发候选值为 `1`，经 CDN、CPU、Plex 和 MediaVault
压测后再决定是否提高。每次播放可以触发 freshness 判断，但不等于每次执行完整 ffprobe；
未过期记录只更新访问时间，过期记录优先异步校验，可靠指纹变化才重新探测。

一次真实 MediaVault/115 环境的匿名初始样本中，六个文件的 `/redirect` 耗时为
`0.018–0.477s`，受限 ffprobe 耗时为 `0.33–0.46s`，组合约为 `0.46–0.89s`。解析直链和
MediaVault redirect 与 ffprobe 必须在同一次任务中使用相同 User-Agent，否则当前 CDN
样本会拒绝请求。交互探测使用触发 metadata 请求的真实客户端 User-Agent；没有活动
客户端上下文的后台任务使用可配置的稳定 fallback User-Agent。该样本只证明方案值得
实现，仍需覆盖 HDR、Dolby Vision、多音轨、字幕、大型 MKV、尾部索引容器、高延迟和
失败源。

ffprobe 成功但未返回 format size 时，Gateway 可以在同一 worker 中使用相同 User-Agent
对同一直链发起一次 `Range: bytes=0-0`。该请求使用独立 `2s` 上限，只接受有效的
`206 Content-Range` 总大小，不读取响应体，也不跟随重定向。失败只保留 size 为空，不能
丢弃其他已验证字段或延长 302 路径。成功结果随当前 Provider revision 持久化，缓存命中
在 fresh 期间不重复访问 CDN，fresh 到期后的 retained 记录仍可触发后台复验。

首次冷缓存长期返回空 MediaInfo 不可作为正常体验，metadata 请求一直无响应同样不可
接受。处理顺序为：Gateway 启动时按需从 SQLite 回填 L1；精确冷 miss 时创建最高优先级
任务，并允许当前单项 metadata response 在可配置的硬截止时间内等待。当前 PoC 候选
上限为 `5s`；窗口内完成就补充当前 response，超时或失败必须立即返回原始 Plex
response，任务继续在后台完成。当前项云端重定向已就绪后，管理面只发现并限速预热
可配置邻近窗口，不扩散为季、整剧或目录扫描。提高首次完整率应优先使用有界窗口，而不是
无限增加请求等待或启动无界批量任务。

Gateway 不缓存原始文件头尾。MediaVault 的上传预缓存利用上传时仍在本地的数据，适合
减少其后续扫描对 115 的请求，但只覆盖经 MediaVault 上传的新文件；Gateway 若从 CDN
下载相同头尾再缓存，并不能降低首次探测风险。Gateway 只持久化解析后的 MediaInfo。
未来若 MediaVault 提供稳定的 MediaInfo 或预缓存读取 API，可作为优先 Provider，未命中
时再回退 CDN ffprobe。

## 11. Plex MediaInfo 投影

从客户端体验看，让 Plex 自身持有 container、duration、codec 和 Stream 信息最完整，
也最有机会避免 STRM 触发大量 on-the-fly analysis。

Gateway response enrichment 先用于验证 Plex 字段映射和客户端消费行为，但不预先决定
最终投影方式。必须评估三类候选：

1. Plex 官方 API 是否能写入并稳定保留 Media、Part 和 Stream 技术字段；
2. PMS 是否存在其他受支持、版本化且不依赖私有 schema 的接口；
3. 独立 Helper 直接写 Plex SQLite 是否可控。

在没有受支持的写入接口时，直接写 Plex SQLite 属于高风险实验能力，不能直接进入生产
默认路径，但仍保留为需要验证的候选。Helper 必须隔离并首先执行离线 PoC：

- 默认关闭并通过 feature flag 启用；
- 与 Plex 同机部署，Gateway 和 MoviePilot 不直接挂载数据库；
- 首个 PoC 只允许在 Plex 已停止或经实验确认的维护窗口执行；
- 检测 PMS 版本、数据库 schema 和目标列；
- 使用包含 WAL/SHM 的一致性备份，并完成真实 restore 演练；
- 使用精确 PMS/schema allowlist、事务、busy 检测和幂等命令；
- 写入前后对 ratingKey、PartID 和 STRM/content fingerprint 执行 compare-and-set；
- 写入后通过 Plex metadata API 回读验证；
- Plex 升级或 schema 未识别时停止写入；
- ratingKey、PartID 或 STRM fingerprint 变化时由 Reconciler 重新投影；
- Plex 数据库不能成为技术 MediaInfo 唯一来源。

`PlexProjectionClient` 仅表示对 helper 的版本化命令客户端，不表示 MoviePilot 可以直接
打开 Plex 数据库。如果不启用 helper，Gateway 仍需保留 metadata 请求准入、缓存与
熔断，并可在已确认的 STRM 项目上做客户端响应 enrichment。

response enrichment 只修改当前已认证 Plex response 中的精确 Media 和 Part，Plex 已有
字段不覆盖。Part 完全没有 Stream 时，可以按 ffprobe 的流类型和源索引创建描述性
Stream，但不生成 Plex Stream ID、`selected`、`default`、`decision` 等播放选择状态。
Plex 已有 Stream 时只补充身份匹配项的缺失字段，不创建缺少的其他流。首版只处理成功的
单项 `GET /library/metadata/{ratingKey}`；XML、JSON、gzip、body 大小或结构不支持，以及
转换失败时，都必须原样返回 Plex response。带 `skipRefresh` 且产品名以 `-Library`
结尾的后台媒体库同步只消费现有缓存，不准入冷探测，避免一次媒体库同步扩散为全库 CDN
ffprobe。
修改成功后同步更新 Content-Length，并移除失效的 ETag、Content-MD5 或 Digest。HDR 和
Dolby Vision 映射必须使用同一片源的本地文件与 STRM Plex response 建立 fixture，不能
仅按 ffprobe 字段名猜测。

## 12. Gateway 边界

Gateway 对所有来源使用同一播放契约：

```text
Plex Part
→ mapped STRM
→ trusted resolver
→ final CDN 302
```

对于分享挂载 STRM，Gateway 只把路径和查询交给可信 MediaVault `/redirect`。它不解析
share code、fid、消费账号或 Cookie，不判断分享所有权，也不持久化直链。

Gateway 应负责：

- 透明转发 Plex API 和观看状态；
- 只对已确认 STRM Part 进行 Direct Play/302 适配；
- 对所有未知和已知项目的详细 metadata 请求执行通用有界准入，保护冷缓存和首次浏览；
- 对相同认证范围、ratingKey、query profile、内容协商和 Gateway 版本做 singleflight
  与短缓存，禁止跨 Plex 用户或 token 权限域复用；
- 过载时只对非关键详细 metadata 返回合规的缓存响应或明确 `429/503`，不能 fail-open
  绕过保护；时间线、观看状态和本地播放请求不进入该限流池；
- 云项目定向策略只能使用独立、稳定、只读的分类索引，不能依赖 MoviePilot 私有表，
  也不能只依赖 Plex 响应返回后才建立的 PartCache；
- 本地播放和未纳入保护策略的未知 endpoint 继续透明代理；详细 metadata 过载是明确例外；
- 对 URL、query、Token、Cookie、分享引用执行日志脱敏；
- 输出按 Part、resolver、redirect 和 fallback 等协议类别聚合的指标，不感知分享所有权。

Gateway 不应负责：

- 决定分享转存或分享挂载；
- 自动把借用分享晋升为自有副本；
- 扫描 MoviePilot 下载历史；
- 维护做种或本地清理状态；
- 在播放 decision、Part、universal start 或 302 请求中执行 ffprobe 或数据库访问；
- 写 Plex 数据库。

Gateway analysis worker 当前只执行精确当前 Part 和可配置邻近窗口探测。单季、整剧和 STRM
目录探测保留为后续有界批量能力；这些任务不拥有下载、做种或清理决策，也不能由一次
播放无界扩散到整库。

### 12.1 Plex 管理凭据与管理面

Gateway 保存一个可选 `PLEX_TOKEN`，当前用于启动校验、Part 发现和邻近媒体预热。
它与活动请求中的客户端 Plex Token 分离，不替代当前用户的播放授权，也不复制到任务
记录。Go 程序只读取环境变量；Docker Compose 部署使用不纳入 Git 的 `app.env`，通过
`env_file: ./app.env` 注入。`.env` 保留给 Compose 默认插值语义。

Token 缺失或失效时，透明代理、客户端播放和当前项预热继续工作，只有邻近媒体发现降级。
启动时验证 Token；稳定 Plex server identity 从无需管理 Token 的 PMS `/identity` 获取。日志、
metrics、任务、CLI 和管理响应都不得输出 Token 或 machine identifier。管理 Token 只允许
发送到配置的 Plex origin，不能注入客户端播放请求或 MediaInfo 任务。播放链路继续按既有
契约向可信 MediaVault 透传客户端 header；CDN ffprobe 只复用探测任务选定的客户端或
fallback User-Agent，不继承管理 Token。

Gateway 当前没有后台页面。当前管理面只提供状态查询和启动时 Token 验证，当前项及邻近
窗口预热由成功 302 自动触发；创建显式控制接口、单季、整剧和目录任务、批任务 checkpoint
与恢复留到后续。以后若增加 HTTP
API 或页面，使用默认关闭的独立 admin listener 或 Unix socket 和独立认证，不能挂到
客户端可访问的 Plex listener。

### 12.2 云项目分类契约

通用 admission control 不依赖云项目分类，因此即使冷启动或分类源失效，也能先保护
Plex。定向策略可选消费版本化 `CloudPartManifest`：

```text
schema_version
generation
rating_key
part_id
plex_file_path
strm_fingerprint
observed_at
```

Manifest 由生命周期控制平面或独立 Plex reconciler 在 Plex 扫描后生成，通过原子文件或
只读 API 发布。它不得包含 Plex Token、分享引用、MediaVault API Key 或 CDN URL。
Gateway 不读取 MoviePilot 数据库；manifest 缺失、过期、格式不兼容或权限范围不明确时，
只禁用云项目定向优化，通用准入保护仍继续工作。

## 13. 本地回收策略

`cleanup.status=eligible` 与“立即删除”是两个状态。默认先进入 dry-run 和待回收队列，再由
MoviePilot 根据下载器策略执行。

### 13.1 硬门禁

MoviePilot 本地下载只有同时满足以下条件才可回收：

- MediaVault 或自有 provider 已确认云端对象存在且大小/指纹符合策略；
- STRM 已通过原子写入并可读取；
- Plex 已扫描到目标 item 和 Part；
- Gateway 通过一次不传输媒体字节的解析探针；
- 满足配置的播放可用性证据，例如人工验收或一次成功的目标客户端播放记录；在证据类型、
  有效期和来源尚未确定前，自动回收保持关闭；
- 可选的 MediaInfo 投影达到配置要求；
- 下载器做种时间、分享率或人工保留规则允许删除；
- 没有仍引用相同 inode/数据文件的其他任务；
- 当前 revision 仍是活动版本，未被洗版任务替代。

### 13.2 删除作用域

所有删除操作必须明确指定一种作用域：

```text
delete_index
delete_presentation
delete_local_backing
delete_owned_cloud_backing
delete_share_reference
```

禁止提供会隐式级联这些作用域的通用 `delete_media` 操作。

MoviePilot `CleanupPolicy` 是本地回收的唯一决策者。每个删除 operation 必须指定执行
provider、目标 backing、decision revision、人工确认或策略依据，以及可用的恢复来源。
`delete_owned_cloud_backing` 默认禁止自动执行；没有可验证替代 backing 和单独确认时，
不能因本地回收、Plex 删除或分享失效联动触发。

## 14. 故障与恢复

| 故障 | 处理原则 |
| --- | --- |
| MediaVault 不可达 | 不推进任务，不删除本地文件；Plex 本地媒体和 Gateway 透明代理保持可用 |
| MediaVault 缺少需要的 API | 标记 capability unavailable/manual，选择可控 provider 或人工检查点 |
| STRM 半写入 | 生产者必须临时文件写完后原子 rename；Gateway 读取失败则 fallback Plex |
| Plex 未扫描到 Part | 延迟重试定向刷新，不猜测 PartID，不写数据库 |
| MediaInfo 冷探测超时 | 在硬截止时间返回原始 Plex metadata，后台继续任务，不影响播放和观看状态 |
| MediaInfo 探测失败 | 保留已知良好记录并进入负缓存；没有旧记录时保持 Plex 原始字段 |
| Plex 管理 Token 失效 | 停止邻近媒体发现；当前项预热和客户端请求继续工作 |
| Plex DB helper 拒绝写入 | 保留权威 MediaInfo，播放按 Gateway 能力降级，不删除本地文件 |
| 分享挂载失效 | 标记 borrowed source unavailable，保留观看状态，等待换源、转存或人工退休 |
| 分享转存部分成功 | 逐项验证自有 backing，未成功项不得标记为 `verified` |
| 事件丢失或服务重启 | Reconciler 从 MoviePilot、MediaVault 文件/API、Plex metadata 回读恢复 |
| 新版本洗版 | 新 revision 全链路验证后再退休旧 revision，禁止原地覆盖后立即清理 |

## 15. 安全与隐私

- MoviePilot 到 MediaVault 的管理调用只使用 Header API Key，不使用 query API Key；
- Gateway 可以通过部署专用 `app.env` 注入 `PLEX_TOKEN`，当前仅用于启动校验和邻近
  媒体发现；
- 客户端 Plex Token 只属于当前请求，不转换为后台管理凭据；
- Gateway 不保存 115 Cookie、分享提取码或 MediaVault 管理 API Key；
- 分享引用、云盘 file ID、下载 hash 和本地路径只在必要的私有状态中保存；
- 公共日志、metrics、debug API 和开源示例不得包含真实域名、路径、账号或签名 URL；
- provider 必须声明其网络目标和凭据边界，不能把 STRM 变成通用 SSRF 入口。

## 16. 分阶段实施

### Phase A：能力清单与任务模型

- 实现只读 capability matrix；
- 对 MediaVault 原生 Plex 反代与独立 Gateway 保持互斥部署检查；
- 建立 `MediaAsset`、revision、正交事实和 operation ledger；
- 通过 MoviePilot 公开 SDK/event 契约接入 `TransferComplete`，只记录任务，不执行删除；
- 检测宿主版本和 capability，禁止依赖私有 Model、Session 或内部事件实现；
- 对 MediaVault 当前接口完成契约测试；
- 全部清理操作保持 dry-run。

退出条件：同一媒体重复事件不会生成重复任务，进程重启后可恢复状态。

### Phase B：Gateway 请求保护

- 对详细 metadata 建立通用 admission control；
- 使用认证隔离的 singleflight 和短期缓存；
- 明确禁止缓存的响应和过载返回策略；
- 建立独立只读的云项目分类索引契约；
- 完成 Apple TV 大媒体库浏览压测。

退出条件：冷缓存下的批量详细 metadata 请求不能拖死 PMS，本地媒体播放、时间线和
观看状态不受影响。

### Phase C：本地下载到 MediaVault

- 使用共享 staging 和目录监控/trigger API；
- 本地文件存在时生成完整 MediaInfo；
- 验证云端对象、STRM、Plex Part 和 Gateway 解析；
- 保持本地文件，不自动清理。

退出条件：电影和剧集完成端到端编排，失败可重试且不产生半成品。

### Phase D：MediaInfo 兜底与投影 PoC

- 优先实现 Gateway L1、SQLite、受限 ffprobe、`PLEX_TOKEN` 邻近发现和 response
  enrichment；
- 实现精确当前 Part 和重定向就绪后的双向邻近窗口，以及优先级、限速提交、singleflight、
  推测任务抢占、负缓存和已知良好保护；
- 将单季、整剧、STRM 目录任务、批任务 checkpoint 和恢复留到后续批量预热能力；
- 验证 `5s` 冷等待候选上限、首次 metadata 完整率和 metadata p50/p95/p99；
- 验证 decision、Part、universal start 和 302 不执行分析 I/O，单项 metadata 的有界等待
  单独计入和验收；
- 比较 Plex 官方 API、其他受支持 PMS 接口和独立 Plex Helper，不提前固定写入方案；
- Helper 候选只在 Plex 停止或经过验证的维护窗口执行；
- 测试库验证一致性备份与 restore、schema allowlist、CAS、写入和 API 回读；
- 验证刷新、Analyze、PMS 重启和升级后的保持/恢复；
- 验证 Apple TV 和其他目标客户端是否消费 Gateway enrichment 或 Plex 持久字段。

退出条件：Gateway 兜底可独立关闭并通过客户端与性能验收；Plex 持久投影必须形成基于
测试库证据的采用或拒绝结论，任何写入失败可以完整回滚，在此之前不得进入生产默认路径。

### Phase E：115 分享来源

- 先接入已确认稳定的分享转存/挂载接口；
- 若接口不存在，保留人工检查点或实现可控 provider；
- 区分 owned cloud backing 与 borrowed share backing；
- 增加分享健康检查、换源和 `promote_to_owned`；
- 决定借用资源是否进入独立 Plex 媒体库。

退出条件：分享失效不会删除观看状态、误删自有文件或触发无限重试。

### Phase F：本地空间回收

- dry-run 报告；
- 人工确认回收；
- 小范围自动回收；
- 做种、洗版和硬链接引用验证；
- 提供明确回滚和恢复来源。

退出条件：经过定义观察期后，没有误删、重复下载或无法恢复的媒体。

## 17. 待评估决策

1. `5s` 冷等待能否覆盖 HDR、Dolby Vision、大型 MKV 和高延迟 CDN 的 p95/p99；
2. L1 启动恢复、当前项预热和 Plex 管理 Token 邻近发现能覆盖多少首次请求；
3. MediaInfo 公开契约使用原子 sidecar、只读 API，还是同时提供；
4. MediaVault 是否愿意提供版本化的分享任务和 MediaInfo API；
5. 未提供接口时，自有 115 provider 的允许范围与维护成本；
6. Plex 官方 API、其他受支持接口或 Helper 哪种投影路径可行；
7. borrowed share 是否只进入独立 Plex 媒体库；
8. 分享失效后的策略是等待、搜索替代、手工转存还是自动晋升；
9. 本地自动回收需要怎样的做种、观察期和播放验证门槛；
10. Gateway 的云项目分类索引由原子 manifest、只读 API 还是两者提供。

## 18. 明确拒绝的方案

- Gateway 直接依赖 MoviePilot 插件或数据库；
- MoviePilot 导入 MediaVault 私有 Python 模块；
- 因 MediaVault 页面存在某项功能就调用未公开接口；
- 把分享挂载误认为自有云端副本；
- 仅在 Gateway 保存 MediaInfo，并让 Plex 永远保持空技术信息；
- 仅在 Plex DB 保存 MediaInfo，失去可重建事实来源；
- 上传成功后无条件删除本地文件；
- 用 Plex ratingKey、PartID、STRM 路径或短期 CDN URL 作为媒体唯一身份；
- 把下载、云盘、STRM、Plex DB、播放代理和 Marker 全部放进一个插件或服务。

## 19. 最终边界

```text
MoviePilot owns desired state, policy, workflow, and cleanup decisions.
Each provider owns the facts about its external resources.
MediaVault performs the 115 and STRM capabilities it actually exposes.
Plex owns the library and user state.
Gateway adapts live Plex playback and provides isolated MediaInfo fallback
without owning the media lifecycle.
```

当 MediaVault 能力不足时，路线不会停止：对应 capability 保持显式缺失，由
MoviePilot provider、独立 helper 或人工检查点补齐。未来 MediaVault 提供稳定接口时，
只替换 provider，不改写整条架构。
