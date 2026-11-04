# Barn 1.0.0 最终发布验收

日期：2026-11-04 至 2026-11-05（Asia/Shanghai）。起始源码：`2f3fd1e7e017dd5a6aa75cdfdc2874af2d74a9f2`。
本文记录本次实际执行的检查；尚未完成的检查不能由历史记录或模拟测试替代。
生成物、虚拟机数据、下载和完整日志全部放在仓库外；文档站已有候选改动保留。

## 执行计划

1. **发布基线**：核对本地与远程 main、tag、Release、CI、版本、发布日期、安全支持
   策略和安装渠道；检查既有改动，区分已准备与已发布。
2. **源码**：完整 `make check`，固定工具链、格式、依赖、race、静态检查、漏洞扫描、
   四平台构建、安装器/包/许可证测试；Mac 原生组件测试。
3. **镜像仓库**：从两个官方源取得并验证最新签名目录，与内置目录逐字节比较，核对
   revision、默认镜像、全部 stable artifact 的路径、架构、摘要、大小、源文件和
   HTTP 可达性；用全新临时 `BARN_HOME` 验证 `barn update`。
4. **交付与真机**：构建完整 Mac 原生载荷与四平台 GoReleaser 归档、DEB/RPM、SBOM、
   checksums、配方；验证 Developer ID、公证、安装和升级；在隔离状态目录验证实际
   VM 生命周期、数据保护、控制节点接任与 Mac 重建恢复。宿主网络破坏性检查仅使用
   一次性测试机。
5. **文档与收尾**：构建中英文站点并检查内部链接、命令与版本；核对发布说明与代码。
   保存执行结果和剩余阻碍，列出最终 tag、制品上传、正式发布、tap 与站点切换顺序。

## 2026-11-05 最新目录与最终 draft 复验

用户要求创建 `v1.0.0` draft Release 并重新下载验收，**不得正式发布**。
文档站与 Homebrew tap 的公开切换因此留到发布草稿之后；本地站点保留可发布候选内容。
本轮工作目录为 `/tmp/barn-1.0.0-draft-20261105`，正式制品与 VM 数据均在仓库外。

- 两个官方源均已验证 Minisign 签名，当前 revision 为 `2026110502`，key ID 为
  `4686B39A40F9B562`。内置 catalog、版本常量、内置日期 `2026-11-05`、
  `repo.yaml` 与 Debian 13 构建输入已同步。
- catalog SHA-256 为 `d41e46d20bc57cb1b071e3919a27a8f7f703df2addca879970a528e7ab004065`；
  `repo.yaml` SHA-256 为 `b1c994afbb848fc9e927233314206e4a6c49caa14abddf60130a65824ef5838b`。
- 默认值仍为 `u24 / stable / native / uefi`，9 个发行版系列、17 个 stable artifact。
  Debian 13 两个架构更新为 `20261103.2651.1`；amd64 为 539885568 字节，
  arm64 为 595853312 字节，两者虚拟容量均为 3221225472 字节。
- M3 已用本轮目录测试构建执行全新 `barn update`、`image pull d13`、实际 VM 启动、
  SSH、Debian 13/trixie 身份、固定地址、XFS 数据哨兵、stop/start/restart、destroy。
  machine-id 与数据在重启后保持；destroy 后返回 `no_deployment`，无残留 QEMU。
  两个架构的 qcow2 摘要、容量、格式均核对，arm64 的 `qemu-img check` 通过。
- 更新后的中英文文档采用新目录，0.9.3 等历史记录保留原 revision；本站 OINK 基线已为
  `v1.5.0`，本轮重新构建和检查，不能用首轮 `v1.4.0` 的结果代替。

本轮源码冻结前已通过 `make check`、`make mac-native-test`、`make catalog-check`，
并验证全部锁定工具版本。初始 shell 的 deadcode 较旧，改用隔离工具目录后重跑
`make deadcode vuln` 通过；可达符号和导入包漏洞为零，所需模块仍有一条未被调用的公告。
目录检查工具的全量下载超时放宽到每个镜像 30 分钟，并输出逐镜像进度；常规 HEAD 门禁
保留 2 分钟超时，相关测试、vet 和 staticcheck 通过。两个本地完整下载尝试分别遇到
HTTP/2 中断和两分钟超时，保留失败日志并在 M3 用 HTTP/1.1 重跑。
站点已用 OINK `1.5.0`、Hugo Extended `0.169.0` 和独立模块缓存构建通过，219 个
HTML 页面的内部链接与资源校验通过；仅有仓库明确接受的 LibSass 弃用提示。

本轮剩余步骤：提交与 hosted CI、正式 tag 的 Developer ID 签名及 Apple 公证、
完整发布组装、上传 draft、重新下载所有资产逐文件校验，再用下载的 Linux amd64
产物在 M3 全流程验收。最后复核草稿仍未发布并保存证据。

## 首轮执行记录（旧目录与 RC 演练）

**首轮结论：当时覆盖范围内的源码、目录、安装升级、核心实机行为和完整发布组装通过。
以下矩阵、目录摘要和 RC 签名属于首轮历史证据；新目录和最终 1.0.0 draft 必须重新验收。
正式 1.0.0 尚未发布，候选包不能直接改名当作正式包。**

本次修复了删除节点后元数据刷新超时的恢复提示：不再建议 `barn up 已删除节点`，
而是要求更新剩余节点和控制角色的清单，再运行整体 `barn up`，保留原有来源选项。
通过暂停剩余 VM、触发真实 QMP 超时，验证提示不会引导重新创建已删除节点。

另增加 `make catalog-check`，并接入正式 Mac 构建、`release-local.sh` 和发布 CI。
它使用运行时信任根分别激活两个官方源的签名目录，要求远程目录与内置字节、revision
完全相同，再检查每个 stable artifact 的 HTTP 状态和签名大小。
`go run ./tools/catalogcheck -verify-bytes` 可额外逐个下载流并核对 SHA-256，不保留镜像。
普通 `make check` 不依赖镜像仓库在线。

### 验收矩阵

| 项目 | 实际执行与结果 |
|---|---|
| 完整源码检查 | 最后一次运行时代码修改后重新运行 `make check`，退出 0；包含模块一致性、shell、测试/race、vet、静态与死代码检查、漏洞检查、四平台构建、镜像管线、安装器、Mac 发布与许可证检查 |
| Mac 原生组件 | `make mac-native-test` 通过；Apple Silicon/macOS 27.0.1、Xcode 27.1 实机 |
| 漏洞 | `govulncheck` 报告可达符号 0、导入包 0；所需模块中另有 1 条未被调用的 openpgp 公告，不表述为全部传递依赖零公告 |
| 官方目录 | 两个官方源签名校验、全新 `BARN_HOME` 的 `barn update` 与目录激活通过；17 个 stable artifact 的路径和大小全部有效 |
| 镜像实际字节 | 在官方仓库主机重新计算全部 17 个 qcow2 的 SHA-256、大小，并核对格式与虚拟容量；全部符合签名目录，`repo.yaml` 同步一致 |
| 原生镜像管线 | 一次性 Linux 宿主使用真实 libguestfs 完成离线规范化；产物维持 unsigned/testing，不具备自动晋级资格 |
| Linux 生命周期 | Ubuntu/EL8 实际启动、SSH、固定地址；删除控制节点后接任与跨节点 SSH 通过；数据哨兵及 VM 身份保留；整套 recreate 省略已有节点时退出 4，未停止或删除已有节点 |
| Linux 数据盘 | 实际分区表、缺 UUID 的 ext4、新版 XFS 遇旧版 EL8 检查器均保持全盘摘要；明确损坏的 ext4/XFS 才重置；dm-error 映射被拒绝，原盘保留；原盘及数据哨兵最终恢复 |
| Linux 取消 | 实际 sudo 子命令超时约 1.05 秒返回，flock 和 dpkg frontend 锁均可重新取得 |
| Linux 网络 | 原先 inactive/disabled 的 networkd 经 Barn 启用；随后加入其他网络接口，再卸载 Barn，`preserve_networkd` 为真，服务、接口、配置及静态管理路由均保留 |
| 0.9.3 Linux 升级 | 使用公开旧归档创建 VM，执行新 CLI；guest helper 从 `20260916.2` 升到 `20261104.1`，VM UUID、generation、创建时间及数据保留 |
| Mac 生命周期 | 15 个自动阶段全部覆盖并通过：双 VM 隔离、身份、网络、DNS/HTTPS、远程 argv/退出码、共享目录读写、停启、离线配置、运行数量限制、重建身份轮换、基础镜像复用及单节点销毁 |
| Mac 中断恢复 | 在真实 recreate 的 moving 阶段中断；仅被恢复日志引用的基础镜像未被清理，后续 up 恢复原实例并清理日志 |
| Mac GUI/剪贴板 | 实际打开桌面，双向文本剪贴板通过；关窗后 VM 与 SSH 仍运行；恢复宿主原剪贴板文本，临时文档未保存 |
| 安装器与 Mac 升级 | 原始安装器在隔离目录执行首次安装、重复安装、0.9.3 到候选版升级；旧 release 保留；旧 VM 的 instance、machine-id、SSH 公钥与数据哨兵保留；签名及公证票据有效 |
| DEB/RPM | 一次性 Ubuntu 中实际 apt 安装；EL8 中实际 dnf 解析依赖并安装，`rpm -V` 通过；CLI/helper 的权限、属主与版本正确 |
| 完整候选发布 | 干净临时源码、真实本地 commit/tag，完整执行 `release-local.sh`；四平台归档、四个 DEB/RPM、8 个 SPDX、formula、installer、release.json、checksums 共 20 个文件；19 项校验和及包/归档字节一致性通过 |
| Homebrew 布局 | 真实 Homebrew Ruby/Formula 运行时执行生成配方的原始 `install` 和 `test`，只重定向至隔离 prefix；bin/libexec/doc 布局、helper 配对、Developer ID、staple、Gatekeeper 均通过 |
| 文档站 | Hugo Extended 0.169.0 + 经校验的 OINK 1.4.0 构建中英文候选站；219 个 HTML 页面的内部链接及资源检查通过；LibSass 弃用提示按站点规则接受 |

### 目录与签名身份

- 官方源：`https://repo.pigsty.io/barn`、`https://repo.pigsty.cc/barn`。
- 目录 revision：`2026102802`；签名 key ID：`4686B39A40F9B562`。
- 目录 SHA-256：`dd622ef2222c888a8e1caa4d8111c1c9fceb3f0c848d55c500eb7caa54be66a5`。
- `repo.yaml` SHA-256：`621f7f487fddc3f5ee64a57c13ffeabef30d860bdb3f753c7f4b46e30832540b`。
- 默认值：`u24 / stable / native / uefi`；9 个发行版系列，17 个架构制品；内置日期 2026-10-28。
- 内置 JSON、revision、日期和仓库源文件均已同步，因此未无意义地改动或重新签名目录。
- 发布演练 tag：**仅在临时副本中的** `v1.0.0-rc.1`；commit：
  `3d570f672eea09e327a57bd5fd1627cc580c1bd1`；epoch：`1793807785`。
- 候选 Mac CLI、helper、app 使用 PGSTY PTE. LTD. 的 Developer ID（team `G9G4RB4WRU`）；
  Apple 公证接受 ID：`d9d11f95-992e-4a4d-96da-6e3286375752`。
  归档导出、发布组装及 Homebrew 移动安装后再次验证签名和 app 公证票据。

三种身份各有范围：catalog 的 Minisign 验证镜像元数据；Developer ID/Apple 公证验证
Apple Silicon 原生载荷；Git tag、通用 `checksums.txt` 和 release envelope 当前没有独立
密码学签名或 provenance attestation。`release.json` 的 `signed:false, attested:false`
如实表示后者，不能被解释为 Mac app 未签名，也不能声称整个发行包已有供应链证明。
既有 tag 为未签名的 annotated tag，本机未配置 Git 签名身份。额外引入 tag/制品签名
属于新的发布策略，不应借用镜像签名私钥或伪造 attestation。

### 失败记录与覆盖边界

- Mac 初次自动验收在新启动机器的 5 秒 SSH 观察上失败。测试改为最多等待 60 秒，保留
  每次失败观察；缺失机器或错误生命周期状态仍立即失败。前 9 阶段和后续 6 阶段在同一
  隔离状态继续完成。GUI 另行实测，原报告的 GUI pending 由独立证据补齐，未改写原始失败。
- Linux 测试宿主最初放在远端 tmpfs，资源压力造成客体 I/O 故障。保留故障磁盘及串口日志，
  将本次专用 VM 移至 `/data`，校验/修复客体文件系统、重核镜像摘要后完成实测。
  未修改远端原有虚拟机或网络。
- networkd 测试宿主本身使用 netplan/DHCP。测试暂存并恢复 netplan 配置，在服务停用后
  显式建立管理地址和静态路由，再断言 Barn 操作前后的路由保留。准备阶段遗漏生成器状态
  或 DHCP 地址的失败日志保留，未归为 Barn 缺陷。
- 本机全局 OINK 模块缓存被修改，普通 `go mod verify` 拒绝该缓存。最终站点验收使用独立
  `GOMODCACHE` 与 Hugo cache，下载的 OINK 1.4.0 与 `go.sum` 一致；未覆盖全局缓存中
  可能属于其他工作的改动。默认代理超时后从 goproxy.cn 下载并执行模块校验。
- 安装器测试使用已核验公开 release 校验和的旧归档及本地候选归档；curl transport shim
  仅替换下载运输，实际安装脚本与二进制均执行。不证明尚不存在的 1.0.0 公网下载。
- Homebrew 执行真实配方和库的安装/测试方法，未改动用户已安装的 tap 或 Cellar；公开 tap
  的 `brew install/upgrade` 需在正式制品上线后验证。
- 实机覆盖 **darwin/arm64 与 linux/amd64**；darwin/amd64、linux/arm64 完成交叉构建、
  归档/包/架构/SBOM/依赖/配对校验，未完成这两类物理宿主的全矩阵启动测试。
- dm-error 在“不是整块磁盘”的保护分支拒绝操作，不证明每一种底层 I/O 故障均经过
  文件系统检查器的相同诊断分支。
- 实机初期使用 notarized `0.9.4-next` 快照；最后的 destroy 提示修复由最终源码构建单独
  实测，之后完整重建并公证 `1.0.0-rc.1`。候选源码冻结后只更新验收记录和发布日期文案。

### 证据与复核

完整工作目录：`/tmp/barn-1.0.0-acceptance-20261104.O8Pu7g`。
其中 `release-candidate/` 为隔离源码，`rc-assets/` 为完整候选制品。轻量日志、脚本、摘要
另存于 `/Users/vonng/.local/share/barn-acceptance/20261104.O8Pu7g`，索引为
`acceptance-summary.json`。VM 数据与镜像不进入 Git，也不混入轻量证据归档。

关键证据：

- `make-check-post-live-fix.log`、`mac-native.log`、`toolchain-final.log`。
- `catalog-gate.log`、`repository-bytes.json`、`update-global.json`、`update-china.json`。
- `linux-control-election-final.log`、`linux-disk-live.log`、`linux-destroy-hint-final.log`、
  `linux-cancel.log`、`linux-upgrade-live.log`、`linux-final-evidence/`。
- `image-pipeline-native-root.log`、`linux-deb-install.log`、`linux-rpm-install.log`。
- `mac-live-evidence/`、`mac-live-final/`、`mac-interrupted-summary.json`、
  `mac-gui-summary.json`、`mac-install-upgrade-verified.log`。
- `rc-mac-signing.log`、`rc-release-local.log`、`rc-assembly-summary.json`、
  `homebrew-layout-complete-final.log`、`site-canonical-final.log`。

核心复核命令如下。发布前必须对最终源码重跑目录门禁，不能仅引用本次旧日志。

```sh
make check
make mac-native-test
make catalog-check
BARN_MAC_REQUIRE_NOTARIZATION=1 python3 packaging/mac-release.py check VERSION COMMIT /absolute/native.tar.gz
BARN_MAC_PAYLOAD=/absolute/native.tar.gz make release-local VERSION=VERSION OUTPUT=/absolute/new-output
```

### 首轮结束时尚未执行的发布动作（历史清单）

1. 审阅并提交最终源码和文档。原工作树的既有/并发修改保留；本次只在临时副本创建验收
   commit/tag，没有替用户提交或推送混合改动。
2. 最终源码 commit 的 hosted CI 通过后，建立正式 `v1.0.0`。基线 commit 的源码/Mac CI
   已通过（Actions `41847108488`、`41847108551`），不代替尚未提交修改的 hosted CI。
3. 在正式 tag 的干净源码重跑目录门禁，构建版本/commit 匹配的签名、公证载荷及全部文件。
   GitHub 仓库未配置六项 Mac signing secrets，tag 自动发布会跳过；本次已跑通的本地
   签名路径可直接使用，不必为发布临时导出私钥到 CI。
4. 上传 draft Release，复核上传文件数和逐文件摘要，再发布为 stable/latest。最终核对时
   官方最新 release 仍为 prerelease `v0.9.3`，`v1.0.0` ref 尚不存在。
5. 公开制品可用后更新 `pgsty/homebrew-infra` 配方并部署中英文站点。候选文案日期已统一
   到 2026-11-05，实际发布日期若再次变化需同步；文案描述发布后的状态，不应提前上线。
6. 从公开 URL 下载校验，验证无版本参数安装器、固定 `BARN_VERSION=1.0.0`、Homebrew
   首次安装/升级及 `releases/latest`。这些只能在正式公开后完成。

## 发布顺序

先完成验收与最终源码提交，在干净源码上建立 `v1.0.0`，以该 tag 的完整 commit 和
`SOURCE_DATE_EPOCH` 重建正式签名制品；验证完整输出后上传 draft Release，并复核
上传字节。本轮按用户要求停在 draft 验收完成。后续经用户要求将 Release 发布为
stable/latest 后，再更新 Homebrew tap 和文档站，最后通过公开下载地址复测安装。候选快照与先前提交的签名不能代替最终 tag 的制品证据。
