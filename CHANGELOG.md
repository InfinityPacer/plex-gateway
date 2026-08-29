# 更新日志

本文件记录项目的所有重要变更。

格式基于 [Keep a Changelog](https://keepachangelog.com/zh-CN/1.1.0/)，
项目版本遵循 [语义化版本](https://semver.org/lang/zh-CN/spec/v2.0.0.html)。

## [未发布]

## [0.1.2] - 2026-08-30

### 新增

- 支持 Plex for Apple TV 正确识别 STRM 的 HDR 和 Dolby Vision 信息。Gateway 会补齐
  Plex 缺失的 MediaInfo，同时保留 Plex 自己已有的媒体信息和播放决策。
- 自动探测并缓存 STRM 的视频、音频和字幕信息。开始播放时优先处理当前影片，配置 Plex
  管理 Token 后还会预热相邻剧集，减少连续观看时的等待。
- 增加默认关闭的实验性 Apple TV Dolby Vision Profile 5 播放拒绝开关。
- 增加 MediaInfo 缓存热清理命令，清理前自动备份 Gateway 数据库，无需停止容器。

### 变更

- 优化长剧集页面加载。Gateway 会合并重复的媒体详情请求并保护 Plex Server，首次访问
  没有缓存时先返回 Plex 原始结果，再在后台补齐 MediaInfo。
- MediaInfo、请求保护和脱敏追踪默认开启。官方镜像内置 ffprobe，使用 `/app_data`
  保存数据，并通过 `app.env` 管理运行配置。
- 已验证 Infuse、Plex iOS 和 Plex for Apple TV 的 Direct Play。本版本不写 Plex 数据库，
  本地媒体继续透明转发。

### 修复

- 修复远程探测、请求取消或请求合并失败时可能造成的重复等待和额外 Plex 请求。

## [0.1.1] - 2026-08-28

### 新增

- 增加 Plex 媒体详情请求并发保护和监控指标，避免原生客户端一次打开大量媒体时压垮
  Plex Server。

### 修复

- 修复 Apple TV Plex 客户端无法为部分 STRM 创建播放会话的问题。Gateway 只在 Plex
  已确认当前用户、媒体和 Direct Play 决策后才进入 302 播放，其他请求继续透明转发。

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
