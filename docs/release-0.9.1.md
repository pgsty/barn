# Barn 0.9.1 发布基线与验收

发布日期：2026-10-22。版本为 **0.9.1**，沿用 0.x 预发布频道。
本页记录源码定稿时的本地验证，以及标签制品和公开发布的验收流程。
托管执行结果见 [Actions](https://github.com/pgsty/barn/actions)，
最终发行资产见 [v0.9.1](https://github.com/pgsty/barn/releases/tag/v0.9.1)。

## 版本与兼容性

| 项目 | 0.9.1 口径 |
| --- | --- |
| 版本号 / 标签 | `0.9.1` / `v0.9.1` |
| 本地候选 / 打包快照 | `0.9.1-dev.1` / `0.9.1-next` |
| Go | `1.27.3`，与 `go.mod`、工具链配置及工作流一致 |
| Go 模块 | `go.mod` 声明的 19 项依赖为 2026-10-22 核实的最新稳定版本 |
| 构建与检查工具 | 以 `packaging/toolchain.env` 为准；GoReleaser 自带 nFPM 与独立 nFPM 分别校验 |
| Mac 原生组件 | `Barn.app`，应用标识 `com.pgsty.barn`，runner 协议仍为 2 |
| 发行频道 | 按现有 pre-1.0 规则标记为 GitHub pre-release |
| 文档站 | `barn.pgsty.com` 中英文现行指南、首页与发布说明统一到 0.9.1 |

版本通过构建参数注入。`internal/version` 的默认值继续使用 `dev`；本地修改过的源码
报告 `uncommitted`，不得据此宣称是正式标签制品。

CLI、hosts helper 与 App 一起升级。新 CLI 保留对旧 `Barn Mac.app` 的发现能力，
旧 CLI 无法发现新名称。清单格式、CLI 命令、内置镜像目录 `2026093001` 和
`~/.barn` / `~/.barn/mac` 状态路径不变。桌面窗口偏好改用
`~/Library/Preferences/com.pgsty.barn.plist`，窗口位置可能重置。
升级应用不要求重建虚拟机、更新镜像或迁移磁盘。

## 源码定稿前的本地验证（2026-10-22）

| 检查 | 状态与范围 |
| --- | --- |
| `make check` | 通过：模块、语法、单元与 race 测试、静态检查、漏洞检查、四目标编译、安装器与许可证检查 |
| `make mac-native-test` | 通过：Swift/C 原生组件测试；未启动真实虚拟机 |
| 固定工具链与 GoReleaser 配置 | 通过；本地使用 `packaging/toolchain.env` 对应的工具 |
| CLI-only 发布快照 | 通过：四平台归档、DEB/RPM、配套 helper、SPDX 与摘要 |
| 0.9.1 候选 CLI 与完整 Mac 快照 | 通过：CLI 为 `0.9.1-dev.1`；`0.9.1-next` 完整快照通过原生载荷、签名完整性、SBOM、摘要与软件包验证 |
| 安装与升级 | 通过：真实 0.9.0 发行包 → 0.9.1-next 的隔离安装、重复安装、旧版保留、helper 摘要及原生 App 探测；Homebrew 目录布局探测通过 |
| 中英文文档站 | 通过：生产构建、231 个 HTML 页面的内部链接，以及双语首页、发布说明和安装入口检查 |

`govulncheck` 未发现受影响的已导入包或可达函数。模块层面仍有
[GO-2026-5932](https://pkg.go.dev/vuln/GO-2026-5932)，涉及 `x/crypto/openpgp`；
Barn 在四个支持目标均未导入该包。上述结果不能替代之后的漏洞数据库检查。

升级验证使用校验过的真实发行包与本地完整快照，下载由本地夹具提供；
Homebrew 验证覆盖 `bin/` / `libexec/` 布局，不代表已更新线上配方或执行真实 `brew upgrade`。
`barn mac doctor` 验证原生组件与宿主能力，没有创建或启动虚拟机。

需要复现时，先将固定版本的工具加入 PATH，然后执行：

```bash
BARN_VERSION=0.9.1-dev.1 make build
bin/barn version
make check
make mac-native-test
make release-check
```

完整原生快照使用 [Mac 发布指南](mac-release.md) 中的构建与组合流程。当前标签基线
为 v0.9.0，因此快照版本为 `0.9.1-next`。原生载荷的版本、提交与 CLI 必须相同。
本地 ad-hoc 签名仅用于测试，不代表 Developer ID 签名或 Apple 公证。
制品与验证日志保存在 Git 之外；不要把可执行文件、归档或 `public/` 加入提交。

文档站使用已发布的 OINK 1.3.2 与 Hugo Extended 0.168.0，在伴生仓库运行
`make check`。该目标清除本地主题替换并校验生成链接；LibSass 弃用告警按该仓库
当前基线接受，不启用 `--panicOnWarning`。

## 标签制品与公开发布验收

本次发布四平台 CLI/helper 归档及 Linux DEB/RPM，**不包含 `Barn.app`**。
GitHub 未配置六项 Mac 签名与公证 secrets，原生组件继续作为源码预览；
本地 ad-hoc 原生快照不进入公开发行资产。

按以下顺序验收，并以最终提交上的工作流和公开资产为准：

1. 按上述资产范围与发布日，将 [发布说明](../.github/releases/0.9.1.md)、CHANGELOG
   和文档站定稿，核对发行状态及安装提示。检查新增文件大小、类型与暂存列表，
   保留既有工作树变更的归属；文档站此时仍不部署。
2. 冻结两个仓库的最终提交。在选定源码提交上通过托管源码、Mac 与打包 CI，
   用隔离环境验收 Linux VM 启停与 SSH。若分发 Mac 原生载荷，另验收 Mac VM
   启停、桌面、SSH 和升级后的实例标识、磁盘、密钥与 host pins。后续源码改动
   需要重新验证。
3. 仅在包含 App 的发行中按选定提交构建 Mac 发行载荷。六项签名与公证 secrets 全部缺省时可发 CLI-only
   版本；部分配置会失败，全部配置时原生构建必须通过。若包含 App，完成 Developer ID
   签名、Apple 公证、票据与 Gatekeeper 验证；详见 [Mac 发布指南](mac-release.md)。
4. 创建并推送 `v0.9.1` 后，检查工作流生成的草稿：四平台归档、DEB/RPM、helper
   摘要、许可证、SBOM、checksums、`release.json`、安装脚本及生成的 `barn.rb`。
   原生载荷加入后重新生成的摘要与 SBOM 必须匹配最终归档。
5. 发布发行资产，将生成的 `barn.rb` 更新至 `pgsty/homebrew-infra`，再发布伴生文档站。
   在公网验证固定版本下载、SHA-256、安装/重复安装/升级、Homebrew 与中英文入口。
   GitHub `releases/latest` 不选择 pre-release，安装器需要显式指定 `BARN_VERSION=0.9.1`。

本地检查、托管 CI、签名公证、Git 提交、版本发布和文档部署分别记录结果。
