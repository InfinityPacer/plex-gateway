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
明确选中的 STRM Part，Gateway 会在保留完整客户端 profile 和 Plex 自身决策响应的
前提下，请求 Plex 将该播放选择为 Direct Play。只有当该完整响应明确将同一个
Part 标记为 Direct Play 后，Gateway 才会创建短期授权。如果客户端随后请求通用的
`start`、`start.mpd` 或 `start.m3u8`，Gateway 会要求该播放会话及精确 Media/Part
对应的授权，再次为 Part 请求授权，并返回同一个 CDN 重定向，而不会启动 Plex
Transcoder 或合成 Plex 元数据。

能够在这些路径上接受原始媒体重定向的原生客户端可以使用此链路。Plex Web 不支持
云端播放：其浏览器播放器会将 `start.mpd` 作为 DASH manifest 获取，而重定向目标
是原始媒体文件，且可能不允许跨源浏览器请求。为 Gateway 的重定向添加 CORS header
无法改变最终媒体源的响应。Plex Web 中的本地媒体仍按普通 Plex 流量代理。

## 兼容性状态

| 客户端或路径 | 状态 | 边界 |
| --- | --- | --- |
| Plex 本地媒体 | 透明代理支持 | Gateway 不会重写本地 Part。 |
| Infuse Direct Play | 已验证 Direct Play | 使用 Plex Part 重定向路径。 |
| Plex iOS ExperimentalPlayer | 已验证 Direct Play | STRM 使用通用 decision/start 重定向链路。 |
| Plex for Apple TV | 已验证 Direct Play | STRM 使用通用 decision/start 重定向链路，本地媒体保持透明代理。 |
| Plex Web 云端播放 | 不支持 | 浏览器需要 DASH manifest，并受最终源站 CORS 限制。 |

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
| `MEDIAVAULT_URL` | 禁用 | MediaVault 内部 HTTP(S) origin。 |
| `PATH_MAPPINGS` | 禁用 | Plex 到容器内 STRM 路径的 JSON 映射数组。 |
| `LISTEN_ADDR` | `:32400` | Gateway 监听地址。 |
| `PART_CACHE_TTL` | `24h` | 内存 Part 缓存的有效期。 |
| `RESOLVER_TIMEOUT` | `15s` | MediaVault 解析的端到端超时时间。 |
| `OBSERVE_MAX_BYTES` | `8388608` | 观察解析时复制的最大元数据 body 大小。 |
| `PART_PROBE_TIMEOUT` | `15s` | 无 body 的 Plex Part 授权 probe 超时时间。 |
| `CLOUD_EXTENSIONS` | `.strm` | 以逗号分隔的云端控制文件扩展名。 |
| `TRACE_ENABLED` | `false` | 启用经过脱敏的 Plex 请求顺序追踪。 |
| `METADATA_GUARD_ENABLED` | `false` | 限制单项详细 metadata 请求进入 Plex 的并发量。 |
| `METADATA_GUARD_GLOBAL_CONCURRENCY` | `8` | 所有客户端共享的详细 metadata 并发上限。 |
| `METADATA_GUARD_CLIENT_CONCURRENCY` | `4` | 每个 Plex 客户端标识的详细 metadata 并发上限。 |
| `METADATA_GUARD_BATCH_ENABLED` | `false` | 限制逗号分隔的批量 metadata 读取进入 Plex 的并发量。 |
| `METADATA_GUARD_BATCH_CONCURRENCY` | `3` | 所有客户端共享的批量 metadata 并发上限。 |
| `METADATA_GUARD_QUEUE_TIMEOUT` | `10s` | 等待准入的最长时间，超时返回 `429`。 |

Plex Token 不会保存到配置中。普通 Plex 请求保持透明转发。对于云端播放，客户端的
所有请求 header 都会转发到配置的 MediaVault origin，使其能够为相同客户端上下文
生成直链。内部查找始终使用 `GET`，因此即使 MediaVault 不支持 `HEAD`，客户端的
`HEAD` 请求也能收到相同的 302。请将 MediaVault 部署为可信上游，并避免将解析请求
及其 header 写入日志。

客户端像连接 Plex 一样连接 Gateway，并通过 Plex 完成身份认证。Gateway 不需要
单独配置 Plex 用户名、密码或静态 Token。STRM `/redirect` 播放协议也不需要
MediaVault 用于 `/api/v1` 集成的 API key。

### Metadata 并发保护

部分客户端会在打开大型媒体库时并发请求大量
`GET/HEAD /library/metadata/{ratingKey}`。启用 `METADATA_GUARD_ENABLED` 后，
Gateway 会在这些单项详细 metadata 请求进入 Plex 前应用全局和单客户端并发上限。
客户端标识只用于进程内限流，并以摘要形式保存，不会写入日志或 metrics。

该保护不处理媒体库列表、时间线、观看状态、播放决策、`/library/parts` 或其他 Plex
路径。请求最多排队 `METADATA_GUARD_QUEUE_TIMEOUT`，超时后返回 `429`，不会绕过保护
继续请求 Plex。此功能不缓存 metadata，也不改变 Plex 响应。

`METADATA_GUARD_BATCH_ENABLED` 使用独立的全局并发池保护
`GET/HEAD /library/metadata/1,2,...`。批量读取不会占用交互式单项 metadata 的全局或
单客户端槽位，metadata PUT 等修改请求也不会进入批量池。

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

- `GET /health` 返回进程健康状态。
- `GET /metrics` 返回固定结构的 JSON 计数器，以及 resolver 和完整重定向链路的
  延迟总计、样本数、最近值和最大值。启用 metadata 保护时还会返回准入、超时、
  活动和排队计数，所有指标都不包含请求标签或凭据。
- 其他所有 endpoint 都遵循 [docs/architecture.md](docs/architecture.md) 中说明
  的 Plex 代理或 Direct Play 拦截规则。

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
