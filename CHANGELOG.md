# 更新日志

[English](CHANGELOG.en.md)

本文件记录项目的所有重要变更。

格式基于 [Keep a Changelog](https://keepachangelog.com/zh-CN/1.1.0/)，
项目版本遵循 [语义化版本](https://semver.org/lang/zh-CN/spec/v2.0.0.html)。

## [未发布]

## [0.1.2] - 2026-08-29

### 新增

- 增加 MediaInfo L1 LRU 与 SQLite 持久缓存、受限远程 ffprobe 和单项 Plex metadata
  响应增强；冷缓存等待超时后保持原响应并在后台完成探测。
- 云端重定向就绪后将当前 Part 以交互优先级提交，并可使用独立 Plex 管理 Token 按默认
  前 2、后 3 的窗口限速预热邻近媒体。当前任务只会优先排队，不能抢占正在运行的探测。
  Token 缺失或失效只禁用邻近发现。
- 增加按客户端隔离的快速切换协调、跨 User-Agent 失败接力，以及缓存命中、加入现有
  任务、新入队和拒绝的预热指标。
- 为没有 Stream 的 STRM Part 创建不含 Plex ID 或播放选择状态的描述性视频、音频和
  字幕 Stream，并补充 HDR10、Dolby Vision、bit depth、bitrate、声道和语言等字段。
- Infuse 风格的后台媒体库同步只读取现有 MediaInfo 缓存，不再因逐项浏览触发全库冷探测。
- MediaInfo ffprobe 记录升级到新的 Provider revision，旧解释版本的缓存不会被复用。
- 当 ffprobe 缺少媒体总大小时，使用同一 User-Agent 发起有界的单字节 Range 请求，
  并从有效 `Content-Range` 补充 Part size，失败保持已有 MediaInfo 并正常回退。
- 后台探测 fallback User-Agent 更新为当前已验证的 Infuse Library 版本。
- 增加默认关闭的实验性 Apple TV Plex Dolby Vision Profile 5 播放 veto。它只复用
  decision 已取得的新鲜 MediaInfo，不保存状态、不拦截 Part，也不发起额外探测。
- 增加公开性能矩阵。本地媒体与 302 路径不执行 MediaInfo 分析 I/O，同一 decision
  最多等待一次冷探测。

### 变更

- 本版本只将兜底 MediaInfo 持久化到 Gateway SQLite。Plex DB 生产写入保留到下一阶段，
  待覆盖率、兼容性、备份和回滚方案完成评估后再决定采用 API 或数据库辅助方案。

### 修复

- 限制一次连续 metadata 浏览突发最多同步等待一个冷探测。释放本次同步准入后建立固定
  5 秒窗口，窗口内的其他冷 miss 立即透传 Plex，拒绝请求不会续期窗口。已准入的探测
  在请求等待结束后仍可在后台完成，并成功写入 L1 和 SQLite，避免持续流量造成饥饿。
- 将单项 metadata Guard 的默认值固定为全局 8、单客户端 4、批量 3。Plex MediaInfo
  写库获得较高覆盖率并完成重测后，再评估全局 16、单客户端 4、批量 3 的候选值。

## [0.1.1] - 2026-08-28

### 新增

- 为单项和批量 Plex metadata 请求提供可配置的并发保护，限制原生客户端
  metadata 扇出对 Plex Server 的压力。
- 提供 Metadata Guard 的准入、排队、活动和超时指标。

### 修复

- 支持 Apple TV Plex 客户端对 STRM 媒体的 Direct Play 决策和播放协商，
  同时保留 Plex 对客户端凭据和媒体 Part 的授权。

## [0.1.0] - 2026-08-27

### 新增

- 为不支持或无法解析的请求提供故障回退型 Plex 透明反向代理。
- 通过 MediaVault 解析符合条件的 STRM Part，并返回 Direct Play 重定向。
- 对支持的原生客户端使用的 Plex 通用播放决策和 start 路径进行身份认证处理。
- 提供内存 Part 缓存、路径映射、健康检查、指标、脱敏追踪和 `plex-probe`
  可行性探测工具。
- 提供容器镜像和独占 Gateway Docker 部署说明。

### 安全

- Gateway 返回云端播放重定向前，由 Plex 使用当前客户端凭据授权对应 Part。
- 日志和指标不包含 Plex Token 或完整的签名媒体 URL。

[未发布]: https://github.com/InfinityPacer/plex-gateway/compare/v0.1.2...HEAD
[0.1.2]: https://github.com/InfinityPacer/plex-gateway/compare/v0.1.1...v0.1.2
[0.1.1]: https://github.com/InfinityPacer/plex-gateway/compare/v0.1.0...v0.1.1
[0.1.0]: https://github.com/InfinityPacer/plex-gateway/releases/tag/v0.1.0
