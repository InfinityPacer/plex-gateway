# 更新日志

[English](CHANGELOG.en.md)

本文件记录项目的所有重要变更。

格式基于 [Keep a Changelog](https://keepachangelog.com/zh-CN/1.1.0/)，
项目版本遵循 [语义化版本](https://semver.org/lang/zh-CN/spec/v2.0.0.html)。

## [未发布]

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

[未发布]: https://github.com/InfinityPacer/plex-gateway/compare/v0.1.1...HEAD
[0.1.1]: https://github.com/InfinityPacer/plex-gateway/compare/v0.1.0...v0.1.1
[0.1.0]: https://github.com/InfinityPacer/plex-gateway/releases/tag/v0.1.0
