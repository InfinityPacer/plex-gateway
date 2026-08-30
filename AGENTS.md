# Repository Guidelines

## 项目结构与模块职责

- `cmd/plex-gateway/` 是生产入口和依赖装配层，`cmd/plex-probe/` 提供只读 Plex 行为探测。
- `internal/gateway/` 负责 HTTP 路由、Plex 反向代理、Direct Play 适配和响应处理。
- `internal/mediainfo/`、`plexmeta/`、`playback/`、`resolver/`、`pathmap/`、`database/` 等包分别管理分析、协议、播放、解析、路径和持久化职责。
- 测试与源码同目录，统一使用 `*_test.go`。架构、兼容性、性能和发版文档位于 `docs/`。
- 部署模板为 `docker-compose.example.yml` 和 `app.env.example`，运维工具位于 `scripts/`。

## 构建、测试与本地开发

```sh
go run ./cmd/plex-gateway     # 使用环境变量启动 Gateway
go test ./...                 # 运行完整测试
go test -race -count=1 ./... # 与 CI 一致的竞态检查
go vet ./...                  # Go 静态检查
go build ./cmd/plex-gateway   # 构建二进制
docker build -t plex-gateway:dev .
```

聚焦测试可使用 `go test ./internal/gateway -run TestName`。性能基准与验收条件见 `docs/performance-matrix.md`。

## 编码风格与命名

所有 Go 文件必须通过 `gofmt`。包名使用简短的小写职责名。导出标识符应有 Go doc；注释用于说明协议契约、不变量、兼容边界和失败策略，不复述代码。保持现有包边界，避免混合 Gateway、MediaInfo 和 Plex 协议职责。

## 测试要求

使用标准库 `testing`。测试命名为 `TestBehavior`，基准命名为 `BenchmarkOperation`。XML/JSON、客户端兼容和配置组合优先使用表驱动测试。协议改动应覆盖成功、异常输入、取消、超时和 fail-open；涉及授权、请求身份、缓存或并发时必须运行 race 测试。性能验收必须区分冷缓存、热缓存、本地媒体、STRM、快速切换和多客户端场景；请求成功不等于性能达标。

## 提交与 Pull Request

提交遵循简洁的 Conventional Commits，例如 `feat: support Plex Web direct play`、`fix: trust Plex materialized media info`。在主题分支开发。PR 说明应包含用户可见结果、兼容性边界、验证结果和剩余限制；除非界面变化需要说明，否则不要求截图。

## 文档与配置

`README.md` 面向安装和使用者，只说明能力、配置、兼容性和限制；实现原理、取舍与性能证据放入 `docs/`。运行默认值以代码为准，`app.env.example` 提供完整中文说明，Compose 示例只保留必要挂载与 `env_file`，不得写入特定 NAS 路径、域名或私有拓扑。

## 变更日志与发版说明

`CHANGELOG.md` 是给用户看的中文发版说明，不是代码实现记录。按重要性描述新增能力、体验变化和已知限制，最重要的用户功能必须放在最前。不要堆砌 Stream ID、投影层级、缓存、队列或 ffprobe 等内部细节，除非它们直接影响用户配置或使用边界。

`CHANGELOG.en.md` 必须同步表达相同内容。发版时同时更新 `VERSION` 和两个 changelog 的带日期版本章节；普通开发不要提前修改版本号。发版前比较上一个版本 Tag 到当前 HEAD，确认所有用户可见变化均已覆盖，且没有把 roadmap 或未交付能力写成已发布功能。发布源只使用 `main` 上的 `VERSION`；手工 Release 仅用于按当前版本重建，不接受自定义版本或源码 ref。Push、PR、Tag、镜像和 Release 等外部动作必须遵守当前维护者授权边界。

## 架构、兼容性与性能边界

Plex 负责媒体库、认证和观看状态；Gateway 只适配控制流和 302，不代理媒体字节；MediaVault 负责 STRM 和直链解析。所有改动必须保持本地媒体透明代理，Part、universal start 和 302 路径不得同步执行 SQLite、ffprobe 或其他分析 I/O。

客户端兼容改动应先比较 Plex 对本地媒体与 STRM 的原始响应，并按协议语义适配。只有真实请求证明 Plex 本身存在客户端差异时，才允许增加客户端分支。Gateway 只补缺失的描述性 MediaInfo，不伪造 Plex Stream ID 或播放选择状态；Plex 已返回物化 Stream 行时必须原样透传并跳过重复探测。

实验能力必须通过独立开关隔离。关闭时不得创建额外状态、缓存、锁或远程 I/O。未来 Plex MediaInfo 持久投影必须作为隔离组件，具备 PMS/schema 检测、备份、回滚和 API 回读，不能进入 Gateway 热路径。不得提交真实凭据、私有地址、媒体名称或未脱敏抓包。
