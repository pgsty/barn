# Barn 1.0 发布审核与修正设计

后续完整实机、签名与发布组装结果见 [1.0.0 最终发布验收](release-1.0-acceptance.md)。
本文保留专项加固当时的检查范围和未完成记录。

日期：2026-11-04。源码基线：`7b4c95b10c3c515b395124849b27645fe2407758`，
发布基线：0.9.3；Claude 修复提交：`647902d`（含前序修复）。本文记录 1.0 候选的最终设计、审核结论和验收范围；不是已发布说明。

Claude 的修复保留，补上独立复核发现的遗漏。是否发布取决于完整质量检查和下述真机、
签名制品验收，不能把单元测试通过直接等同于 1.0 可以发布。当前不创建 tag、不改安装
链接为尚不存在的 1.0 制品、不发布站点。

## 审核结论与交付范围

| 问题 | 判断与最终处理 | 自动验证 |
| --- | --- | --- |
| 全量 recreate 删除 inventory 中缺席的已部署节点 | 确认 P0。提示前拒绝，exit 4 / `nodes_removed`；预检后在销毁锁内复核范围、部署哈希和持久盘 | `TestWholeRecreateRefusesNodeOmittedFromInventory`、`TestRecreateRechecksDeploymentAfterPreflight`，检查根盘和节点状态保持 |
| 删除控制节点后节点与部署哈希不一致 | 确认。保留 guest seed hash，更新接任节点的定义 hash；部署是角色变更的提交记录，锁内恢复可精确归因于控制角色翻转的 hash | `TestDestroyControlNodeRehashesElectedPeer`、`TestControlElectionRecoversInterruptedHashWrite`、`TestControlRecoveryDoesNotAcceptUnrelatedDrift` |
| 混合 prepared / destroying 状态不能继续销毁 | 确认。无 PID 的 destroying 节点作为已停止参与清理；仍保留进程、运行目录归属检查 | `TestDestroyFinishesMixedPreparedAndDestroyingNodes` 及既有进程归属测试 |
| sudo 超时与继承管道 | 确认。取消发送 SIGTERM，默认 WaitDelay 10 秒；按时返回不等于所有后代都已退出 | execx 的 TERM 与管道超时测试；真实 sudo 留给验收 |
| 旧版本 reset 后升级的 embedded 指针 | 确认。embedded 选择跟随当前二进制，保留 high-water mark | `TestEmbeddedSelectionFromEarlierBinaryAdoptsCurrentBaseline` |
| 中国官方源没有跟随新内置基线 | 确认。两个官方仓库采用相同基线规则，自定义仓库保持独立版本空间 | `TestOfficialMirrorsAdoptNewEmbeddedBaseline` |
| 相同目录重新签名遭拒 | 确认。验签成功且目录字节相同时，允许新签名；有效旧签名可复用，坏缓存可用已验证签名替换 | `TestIdenticalCatalogMayBeResigned` 及 equivocation 测试 |
| 签名失败退出码与文档不一致 | 确认。验签、缺失/空本地签名、equivocation 与活动指针错误分类为 integrity / exit 7；网络获取失败仍按获取错误报告；缓存签名的权限或 I/O 读取错误属于宿主错误，不归入 integrity | `TestCatalogSignatureFailuresAreIntegrity`、CLI error contract |
| 隐式 setup 未询问就授权特权操作 | 确认。终端 lifecycle 复用 setup 的计划与一次确认，非终端不自动准备宿主 | `TestImplicitSetup*` 及 setup 原有测试 |
| 持久盘因工具不支持新特性被格式化 | 确认高风险。未知失败保留数据；只允许无签名盘初始化或明确识别的 ext4/XFS 损坏重置 | `TestDiskRecoveryOutcomes` 检查 mkfs 哨兵和原始数据 |
| Mac recreate 的隐藏备份未被 prune 计入引用 | 确认。有效重建日志里的旧基础镜像也被引用，损坏日志阻止清理 | `TestPrunePreservesInterruptedRecreateBase`、`TestPruneRefusesUnreadableRecreateJournal` |
| networkd 卸载按历史状态关闭后来被共用的服务 | 风险确认，实际断网取决于宿主。当前存在其他非 loopback 链路或无法证明独占时，保留全局 unit 状态 | `TestUninstallPreservesSharedNetworkd`；实际宿主验收另列 |
| 裸 barn 缺少完整帮助 | 实现文本完整 Cobra help，JSON/YAML 继续返回 `actions[]`，exit 0 | 根帮助、结构化输出、平台命令树测试 |
| 环境输出选项先于 CLI 失败 | 确认。先应用显式展示选项，再验证最终默认值；help 忽略坏展示环境变量 | `TestInvalidPresentationEnvironmentCanBeOverridden`，保留远端 `--` 与参数值边界 |
| purge 与确认参数不一致 | 属于已有契约，非 P0。终端 purge 增加确认；非终端保持旧行为。destroy/recreate 及 Mac 对应命令增加 `--yes` 别名 | 帮助与参数测试，临时目录 CLI/PTY 验证 |

## 生命周期与控制角色

全量 recreate 不能把缺席节点当成删除意图。显式选择部分节点时继续保留其余节点的已应用
定义。提示列出实际会被替换的已有节点；执行预检后若另一命令修改了部署，拒绝并要求重试。
持久盘和缺席节点还要在同一把销毁锁内复核，防止“待创建”节点在预检期间变成已提交状态。

删除控制节点会选取剩余定义中的第一个节点接任。`SpecHash` 表示当前应用定义；
`GuestSpecHash` 表示 guest 内已有 ready marker，两者不能强制相等。接任节点已提交的
prepare 日志在任何节点被标记删除之前完成清理；无法完成时 destroy 直接拒绝、不删除任何
内容，并指出接任节点。先原子发布部署状态，再更新接任节点；下次取得独占部署锁时，只有
“当前定义的 Control 布尔值翻转”能精确生成旧 hash，才允许恢复节点 hash，其他漂移仍由
现有校验拒绝。恢复不创建缺失状态、不更换 VMUUID、不修改根盘或进程身份。

**用户操作边界**：更新 inventory 中的控制角色后再运行 `barn up`；若工作目录没有 inventory，
`up` 可回退到已应用部署。原 inventory 若仍把已删除节点指定为控制节点，直接 `up` 会拒绝
接任节点的角色漂移。想用原名称重新增加节点，可以保留接任节点为 control，把新增节点设为
非 control；重新迁回控制角色目前仍需显式规划/recreate。

不在本次发布中隐式引入任意控制角色热迁移。完整支持需要同时定义旧 guest 凭据撤销、
新 guest 安装、离线节点的重试与部分失败语义；仅放宽 hash 检查会掩盖未完成的 guest 收敛。

## 数据盘恢复

保留 Barn 的“可丢弃测试数据”产品契约：`persistent` 控制 destroy/recreate 生命周期，
不承诺损坏数据永久保留。但“不认识”不是“损坏”。分区表、缺失 UUID 的已有签名、
不支持的文件系统特性、权限/I/O/内存错误及未知 checker 诊断均不授权 mkfs。

ext4 的 4/12 和 XFS 的 1 都不足以单独证明损坏，必须伴随已识别的损坏诊断。正向诊断之外
的失败保留磁盘并通过 guest warning 报告，可继续使用管理 SSH。`SetupVersion` 同步提升至
`20261104.1`，让已有 guest 下一次 `up` 的 readiness 检查安装新脚本；`--no-wait` 会延后更新。

这仍是有明确产品约束的自动恢复，不能宣传成通用数据恢复工具。真实 el8 工具读取新版 XFS
的行为需按下面的真机验收执行。

## 镜像与网络的边界

官方目录以签名、revision、完整字节摘要及 high-water mark 为约束。重复签名不是新内容；
同 revision 不同目录字节仍是 equivocation。自定义源不继承官方 revision 命名空间。
reset 选择当前 embedded 不会抹掉已经接受过的更高 revision。

Mac 镜像引用不仅来自公开机器槽位，还来自未完成 recreate 日志里的旧机器。即便新机器
已 ready，日志尚未完成清理前也保留旧 base。这里选择少删一个暂存 base，不容许删掉可恢复
虚拟机所需的 base。未知日志不能通过“跳过坏记录”让 prune 猜测哪些镜像无引用。

networkd 是宿主级服务，Barn 的安装前状态不是永久所有权凭证。卸载会再次核对受管文件、
桥成员和 helper；其他非 loopback 链路仍存在或配置检查不确定时，只删除 Barn 资源，保留
当前全局 unit 状态，并在计划与报告的 `preserve_networkd` / `checks.networkd` 中说明。
即便 `.network` 已删除，运行中的链路也可能仍受 networkd 管理，因此不能仅凭当前配置文件
证明独占。副作用是一些原本由 Barn 启用的 unit 会保留启用状态，管理员可在确认后手动处理。

SIGTERM 转发和 `WaitDelay` 解决正常取消与输出管道拖住的问题。它们不是进程树监督器，
不保证守护化后代或忽略 TERM 的特权进程退出；不据此宣称“彻底消除 root 孤儿进程”。

## CLI 契约与有意延期

- 文本裸命令与 `--help` 保持一致；`--json` / `--yaml` 的 `actions[]` 保留。
- `--yes` 是 destroy/recreate 确认别名，不添加 `-y`，不占用已有 `-f`；`mac stop --force`
  仍表示强制断电。终端 purge 要求输入 `purge`，非终端兼容旧脚本的无确认执行。
- network/hosts 非终端无 `--yes` 时显示计划并 exit 0 是现有契约，本次不偷偷改为失败。
- 不为 1.0 拆分整个 package main，不增加 Viper/新的命令分发器，也不把 `--format` 或统一
  日志命令当成阻塞项。后续 CLI 方案见 [Cobra 设计](cobra-cli-design.md)。
- 单个坏 Mac state 阻塞全局枚举的问题保留为后续改进。健康机器按名称读取仍可使用；显示层
  可支持逐项失败，但 IP 分配和 prune 不能忽略坏记录，否则会误判资源空闲。
- 损坏的 Mac recreate 日志除阻止 prune 外，也会让 `mac image ls` 和 doctor 的空间估算失败。
  后续改为列表告警、prune 保持严格；1.0 保持失败关闭。
- `state.Transaction` 有缓存恢复/prune 读者；Mac 旧 bundle 搜索有历史兼容承诺，不按“死代码”删除。
- x/net 升级 PR 的既有 CI 失败是许可证清单漂移，不能据此推导为安全漏洞。依赖更新需单独
  验证模块、许可证和构建，不和本轮修复混为一谈。

## 依赖通告的复核

固定版本 govulncheck 1.9.0 在本轮宿主 Go 构建图上报告 0 条可达漏洞、0 条已导入包漏洞，
另有以下模块级通告。它们对应 Barn 未使用的包，单凭模块版本不能判为已确认的运行时漏洞。
后续仍应通过依赖更新流程处理，保持许可证清单与真实版本一致。

| 通告 | 模块与扫描版本 | 后续处理 |
| --- | --- | --- |
| [GO-2026-6866](https://pkg.go.dev/vuln/GO-2026-6866) | x/net v0.61.0，HTML raw text 渲染 | v0.62.0 修复；接续依赖升级及许可证审核 |
| [GO-2026-6860](https://pkg.go.dev/vuln/GO-2026-6860) | x/net v0.61.0，HTML 文本合并复杂度 | 同上；不由现有 CI 的 license failure 推导可达性 |
| [GO-2026-5932](https://pkg.go.dev/vuln/GO-2026-5932) | x/crypto v0.58.0，未维护的 openpgp 包 | Barn 未导入 openpgp；升级时核对正式版本和许可证，不为消除模块提示直接选预发布版本 |

## 验证与发布门禁

自动验证包括改动包回归、`make check` 的全部项目、固定质量工具版本检查、四平台 Go 编译，
以及站点中英文文档构建/内链检查。每次实际运行的结果在本节更新，失败不得用旧分支结果替代。

本轮自动验证结果（2026-11-04）：

- `make check` 完整通过：module verify/tidy、shell/maintenance、全量 test/race、vet、
  staticcheck、四目标 deadcode、errcheck、govulncheck、四平台编译、image-pipeline、
  install/mac-release、license。质量工具版本门禁通过，包括 deadcode 0.52.0、
  govulncheck 1.9.0；所需新工具安装于仓库外临时目录。
- 磁盘与取消逻辑的最后细化已单独复测 test/race，均通过。新增控制选举日志清理、
  活 PID 拒绝及同 hash 下新提交节点的 recreate 保护测试也已通过；private 的 race，private/cloudinit/execx 的 vet、
  staticcheck、errcheck 补检全部通过。
- 两个官方源在独立新 `BARN_HOME` 下 update 均 exit 0；revision `2026102802`，
  签名 key `4686B39A40F9B562`，目录 SHA-256
  `dd622ef2222c888a8e1caa4d8111c1c9fceb3f0c848d55c500eb7caa54be66a5`。
  完整目录内容与 embedded 相同，因此默认选择和每个 stable artifact 的字段均一致，
  无需改动内置 JSON、版本或时间。
- 构建后的 CLI 在真实 PTY 中取消 purge 得到 exit 130，保留全部测试密钥；非终端 purge
  exit 0，删除测试密钥并保留镜像。裸命令与 help 字节一致，JSON `actions[]` 保留。
- 中英文 CLI/Mac 文档本地构建通过，217 个 HTML 页面内链/资产检查通过。
  仅有仓库已接受的 Hugo LibSass 弃用提示；没有发布站点。
- 镜像流水线明确跳过原生 guest 修改；这些单元/stub/构建结果不能替代下列实机验收。

测试制品、完整日志、原始差异备份和官方目录均保存在仓库外的
`$TMPDIR/barn-hardening-0p0k_fdn`。

整合（2026-11-04）：Claude 的修复提交与本轮补强逐文件审查后按问题拆分提交，并合并进
main。main 原有 WIP 中的裸命令帮助改动由同一意图、保留 `actions[]` 的实现取代；Cobra
提案随本记录收录。合并前的原始 WIP 逐字节保存在分支 `backup/main-wip-20261104`。

### 最终整合验收（2026-11-04）

在 main 合并提交 `605df87` 及两项验收修正之上执行，最终代码提交 `377061c`。合并前 main 的
未提交工作逐字节保存在 `backup/main-wip-20261104`（`8a4787b`）。

- **独立复核**：对 `v0.9.3..main` 全量差异重新做不受前序结论影响的代码审查，未发现 P0–P2。
  3 项 P3：删除控制节点时接任节点的 prepare 日志在删除后才处理（已修正为删除前完成，
  否则拒绝且不删除）；缓存签名的权限/I/O 读取错误被归为 integrity（已修正为宿主错误）；
  坏的 Mac recreate 日志同时阻止 `mac image ls`（延期，见上文）。
- **源码门禁**：固定工具版本检查全部通过（Go 1.27.3、GoReleaser 2.18.3 及内置 nFPM 2.47.0、
  nFPM 2.48.0、Syft 1.55.0、Staticcheck 2026.2.2、deadcode 0.52.0、golangci-lint 2.15.0、
  govulncheck 1.9.0）；完整 `make check` 与 `goreleaser check` 在最终代码上通过。
  govulncheck 的结论与上文依赖通告相同。
- **宿主测试噪声**：一次全量运行中，未改动的 `internal/process` 的
  `TestNumericIdentityBindsNonASCIIArgvInCLocale` 因 macOS 读取进程 argv 返回 EIO 失败，
  单独复跑 80 次全部通过，随后完整 `make check` 通过；Homebrew bash 偶发段错误会影响
  cloudinit 的 stub 测试，系统 bash 下稳定通过。两者都判为测试宿主问题，不作为代码缺陷。
- **发布链**：本地 GoReleaser 快照（`0.9.4-next`）生成 4 个归档、4 个 DEB/RPM、SPDX 与
  checksums，归档/包一致性校验通过；快照二进制的版本、提交与构建时间注入正确。快照未附带
  原生 Mac 组件，不能作为签名或公证证据。
- **CLI 契约**：裸命令与 `--help` 字节一致，`--json`/`--yaml` 返回 `actions[]`；无效
  `BARN_OUTPUT`/`BARN_VERBOSE` 下帮助可用、显式参数可覆盖、未覆盖时 exit 2；`--` 之后的
  `--help`/`-h` 交给远端命令；help、version、completion 无网络访问、无文件写入；坏签名
  `image sync` exit 7（`catalog_signature`）；真实 PTY 中取消 purge exit 130 且不删除，
  非终端 purge exit 0。
- **目录门禁**：清除继承的 `BARN_REPO` 与代理，用新的临时 `BARN_HOME` 对两个官方源执行
  `barn update`，均 exit 0；revision `2026102802`、key `4686B39A40F9B562`，激活的目录与
  内置 JSON 字节一致（SHA-256 `dd622ef2222c888a8e1caa4d8111c1c9fceb3f0c848d55c500eb7caa54be66a5`），
  `EmbeddedManifestVersion` 与内置时间一致，无需同步。打 tag 当天仍须重跑。
- **站点**：barn.pgsty.com 中英文 CLI/Mac 参考的候选修改与最终行为一致，保持未提交，
  制品公开后发布。

正式 tag 前仍需以下验收；不能以 stub 测试代替：

1. **目录门禁**：清除继承的 `BARN_REPO`/镜像环境，用新的临时 `BARN_HOME` 对两个官方源
   执行 `barn update`，确认签名、revision、默认选择、每个 stable artifact 的路径、架构、
   摘要、大小与内置一致；有变更先同步 catalog JSON、版本、内置时间及必要的 repo.yaml。
   目录通过后才能构建正式制品。预发布探测不豁免打 tag 当天的重跑。
2. **Linux guest**：删除控制节点、调整 inventory、运行 up，验证接任节点间 SSH；在真实
   el8 与较新镜像组合中测试旧工具读取新版 XFS、分区表、无 UUID 签名和损坏测试盘，验证
   保留数据/告警以及原有版本 guest 的脚本升级。
3. **Linux host**：一次性测试机上验证 sudo 转发 TERM、包管理器锁释放及有界等待；先让
   Barn 启用 networkd，再让其他接口使用它，卸载 Barn 后验证服务和宿主连通性。
4. **Mac**：在 macOS 27 Apple Silicon 上用最终候选验证中断 recreate → prune → 恢复，
   再检查发行包的 runner 协议、签名、公证、安装/升级路径。原生制品未变化也不能用 Go
   交叉编译替代这些检查。
5. **发行元数据**：决定当前 stable 分支的安全支持策略，再改 SECURITY；准备
   `.github/releases/1.0.0.md`、CHANGELOG 日期、README 安装版本与实际制品链接。
   本地签名发布或 CI 密钥发布选定后，执行对应发布检查。站点候选改动在制品公开后发布。

支持策略建议：仅支持最新稳定 1.x 补丁，0.x 不做 backport；是否承诺某条旧分支的维护周期
由维护者在发版时决定。这里是建议，不是已经对用户作出的服务承诺。

## 2026-11-05 catalog follow-up

The official signed catalog was refreshed to revision `2026110502` after the
initial acceptance run. The embedded bytes, revision, date, repository source,
and image-pipeline inputs now match both official mirrors. Debian 13 stable
is `20261103.2651.1` for amd64 and arm64. The earlier `2026102802` results above
remain historical evidence; the final release must pass the catalog gate again.
See [the final acceptance record](release-1.0-acceptance.md) for current results.
