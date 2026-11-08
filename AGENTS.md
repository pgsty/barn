# Barn 源码仓库说明

## 范围与文档归属

- 本仓库维护 Barn CLI、Linux QEMU 后端、Apple Silicon 原生 macOS VM、镜像流水线和发布工具。入口见 [README.md](README.md)；开发约束与完整流程见 [CONTRIBUTING.md](CONTRIBUTING.md)。
- 伴生文档站是独立仓库 `../barn.pgsty.com`（本机 `~/pgsty/barn.pgsty.com`）。
- 面向用户的指南、参考手册和架构设计统一维护在伴生站点：任务与契约放 `content/docs/`，设计决策与方案取舍放 `content/blog/design/`，英文页面与 `.zh.md` 页面成对维护。
- 本仓库不再保留或新建 `docs/` 目录。面向用户的指南、参考手册、设计决策、研究结论与验收记录统一在伴生站点维护，注明日期、实现状态和代码/测试依据；临时笔记放仓库外，不再创建 `reports/`、`research_notes/`。发布脚本只生成必要的短载荷说明，不在源码仓库维护文档副本。
- 开始修改前检查目标仓库的 `git status`。保留已有改动，不自动 `reset`、`stash`、`clean` 或覆盖其他工作；源码与站点的提交、推送和部署分别核实。

## 实现与验证

- CLI 在 `cmd/barn/`；Linux 生命周期在 `internal/private/`、`internal/vm/`；平台与启动参数在 `internal/platform/`、`internal/qemu/`；镜像在 `internal/image/`；原生 Mac 在 `internal/macvm/`、`native/mac-runner/`。
- 保持单用户单 Linux 部署、严格的 `vm_*` 清单、显式销毁和进程身份核验。`barn mac` 独立管理具名 VM；当前宿主为 macOS/Linux、amd64/arm64，客机架构以实际平台代码和目录为准。
- 工具版本以 `packaging/toolchain.env` 为准。开发用 `make build`、`make test`；代码交付跑 `make check`，原生 Mac 改动补 `make mac-native-test`，打包改动补 `make release-snapshot`。涉及虚拟化、网络和磁盘语义时补目标平台真机验收。
- 纯站点内容改动在伴生仓库运行 `make check`；文档中的 `main` 行为、已发布行为和未来提案必须分清。

## 源码与大文件

- Git 默认只保存源代码、配置、轻量文档和必要元数据。构建产物、可执行文件、软件包、镜像、压缩包、数据库导出和大体积数据放在仓库外，或先加入合理的 `.gitignore` 并用 `git check-ignore` 验证。
- 暂存或提交前检查新增文件的类型、大小和 staged 列表，只加入本次需要的源文件；不得用强制添加绕过忽略规则。优先保留来源、校验值和可复现的下载或构建方法。
- 二进制素材、数据集或生成文件是否入库拿不准时，先询问用户；例外须有明确授权。
- 这条规则不授权删除已有文件、stash 或恢复快照，也不授权清理 Git 对象或改写历史；清理必须有独立的明确范围。

## 正式发布的镜像目录门禁

- 发布前从官方镜像仓库取得最新的签名 `catalog.json`，先验证签名，再同步到 `internal/image/default-catalog.json`。
- 同步时更新 `internal/image/catalog.go` 的 `EmbeddedManifestVersion` 和 `internal/image/manifest.go` 的内置目录时间；仓库源文件变化时同步 `packaging/image-repository/repo.yaml`。
- 核对 revision、默认镜像以及每个 stable artifact 的路径、架构、摘要和大小；运行镜像目录测试，并用全新的临时 `BARN_HOME` 执行 `barn update`，验证远程目录可以验签并激活。
- 执行现有 `make catalog-check`：它对两个官方源分别验签、激活、比较内置目录字节，并检查 stable 制品可达性与大小；需要复算制品摘要时用 `go run ./tools/catalogcheck -verify-bytes`。同一 revision 不得重新发布不同字节。
- 目录门禁通过后才能构建和发布正式版本；正式版本不得继续引用官方已下线的 artifact。
