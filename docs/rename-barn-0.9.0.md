# Barn 0.9.0 重命名实施记录

日期：2026-09-29。用户已决定一次切换，不保留旧名称兼容；本记录取代此前的兼容迁移方案。

## 已确定的范围

- 产品名 Barn；CLI `barn`；Go module 与 GitHub 主仓库 `github.com/pgsty/barn`。
- 文档仓库与主域名 `barn.pgsty.com`；新 Pages 项目配置 `barn-pgsty-com`。
- 配置 `barn.yml` / `barn.yaml`，环境变量 `BARN_*`，数据根 `~/.barn`，持久状态字段 `barn_version`。
- 新系统资源 `/opt/barn`、`barn0`、Barn network/hosts/SSH/runtime 标记；原生组件 `Barn Mac.app` 和 `barn-mac-runner`。
- 安装包、formula、构建脚本、CI、SBOM、版本信息及 helper 配对统一 Barn。
- 两地镜像源采用 `/barn`；COS/R2 对象的旧前缀在新位置完整校验后移除，不保留旧 URL 或旧前缀兼容。

## 一次切换规则

不提供 Farrow CLI 别名、环境变量 fallback、旧字段解析、旧数据根发现、Homebrew rename mapping、网络资源接管或 Mac 迁移命令。旧内部实验环境由原程序停止、导出需要的数据并退役，再创建 Barn 环境。产品改名不会自动删除原磁盘或宿主资源，也不会把旧状态改名后当作新状态使用。

真正的历史发行记录、验收日志、已签名 Catalog 的 provenance 与密钥字节保持原貌。它们不是兼容接口。签名密钥文件如改名，只改变文件名，不改变密钥材料。GitHub 自动保留的仓库重定向由平台管理。用户另行明确要求文档域名例外：`farrow.pgsty.com` 永久重定向到 `barn.pgsty.com`，保留路径和查询参数；此项由独立 Cloudflare 会话实施。

## 并行工作

| 分工 | 范围 |
| --- | --- |
| 云仓库独立聊天 | 核对 co/COS 与 cf/R2 的 source/target、冲突、metadata、校验、可恢复移动步骤及公网结果 |
| Runtime | `internal/**`：状态、环境、host/guest 资源、签名源策略；删除旧 Mac 迁移代码 |
| CLI / Native / 文档站 | `cmd/**`、原生组件、CLI 对齐与补全、双语文档和视觉资源 |
| Packaging / Homebrew | 构建、安装器、CI、包资产、新 formula 与自动更新器 |
| 主聊天 | 统一接口、主文档、GitHub/域名与外部入口协调、整体验证和交付 |

## 关键验收

- 当前代码和当前文档中的命名一致；残留只允许真正不可改写的历史记录。
- 官方 `/barn` 仓库仍强制签名；两地 Catalog 与内嵌版本字节一致。
- 40/80/120 列、ANSI/中文、管道、JSON/YAML 与补全正确；保持现有显示宽度算法。
- CLI、helper 与 app 从同一构建配套；真实新资产产生后才写入 formula 的 SHA-256。
- Go 测试、race、静态分析、四平台构建、安装器与镜像流水线检查通过；包验证与原生 VM 验证分别记录。
- 双语站点构建及内链检查通过，检查器主域名同步更新；历史版本不得被批量伪装为 Barn 0.8。
- GitHub Rename、remote、云对象移动、Pages/DNS、公开下载与文档发布分别核验，不以本地构建代替发布结果。

## 当前边界

实施前主仓库基线为 `0101e6f`，已含 28 个尚未推送的 0.9 提交；保留这些工作。文档站的 README 追加及两个审计报告保留。门户站的其他未提交修改不归本次改名覆盖。

现有 0.9 的旧验收记录不能证明 Barn 改名后的结果；本轮结果以最后交付说明和具体验证日志为准。Mac 原生 app 仍按 Developer ID 签名、公证是否完成决定正式包内是否包含，不因改名扩大已验证范围。

## 本地改名验收

- 完整 `make check` 已通过，包括 module、shell、maintenance、Go test/race、vet、Staticcheck、deadcode、errcheck、govulncheck、四平台构建、镜像流水线、安装器与许可证检查。
- CLI/helper 测试及 40/80/120 列输出测试通过。删除旧 Mac 迁移命令和默认模板的旧版识别分支，不保留虚构的 Barn 历史身份。
- 进一步移除旧进程身份迁移、单仓库 Catalog 状态升级、缺省网络 backend 和未声明 inherited files 的兼容分支；当前进程、仓库与网络状态均使用显式结构。
- Native 单元测试、完整 `make mac-build`、新 app/runner 的严格 ad-hoc 签名验证、解包摘要与 probe、空数据根列表检查通过。新归档仅包含 Barn payload。
- 本轮未重跑真实 VM 生命周期；本机无运行中的 QEMU/Mac runner，也未找到可复用的 macOS prepared base，因此没有为品牌改名额外下载大型恢复镜像。
- 文档站 `make check` 与中英文首页视觉检查通过，227 个 HTML 的内链/锚点/资源校验通过。
- GitHub 两仓库已原地改名，仓库 ID 不变；源码与网站的提交、制品生成、云端移动及域名部署按各自结果分别记录。
