# Barn CLI 整理与 Cobra 架构改造提案

日期：2026-11-04。状态：**Proposal，架构与命令重组尚未实施**。

1.0 候选已单独落实裸命令完整帮助、保留 `actions[]`、确认参数别名及环境参数优先级修复，
见 [1.0 发布审核与修正设计](release-1.0-hardening.md)。其余提案不作为 1.0 发布门禁。

源码基线：Barn `7b4c95b10c3c515b395124849b27645fe2407758`；参考 Pig
`56c858ca76d76afd016fba0948805afb93e9efcc` 的命令层。现状以当前源码和实际帮助输出为准，
不把历史设计文章、README 的版本号或本提案当作已发布行为。

## 1. 结论与改造范围

Barn **已经使用 Cobra 1.10.2**，与 Pig 当前依赖相同。现有实现也已经具备命令分组、
局部参数、动态补全、类型化退出码、JSON/YAML、远端 `--` 边界及相应测试。
本次应定位为“基于现有 Cobra 的 CLI 产品化整理”，无需重新选型或重新生成项目。

建议方向：

1. 保留 `barn up → barn status → barn ssh` 的短工作流，以及单个 `BARN_HOME`
   管理一套 Linux 部署、`barn mac` 独立管理具名 Mac 的模型。
2. 用一份 Cobra 命令树承载语法、帮助、补全和可导出的命令说明；收敛额外的参数扫描。
3. 增加 `--format text|json|yaml`，保留 `--json` / `--yaml`；保留 `init -o PATH`。
4. 统一动作名称、确认参数和可预览行为，明确“显示了计划”与“实际执行成功”的区别。
5. 把约 9,000 行的 `package main` 拆成入口、CLI 和应用编排，继续复用现有领域包。
6. 分成**兼容改造 A**和**行为收敛 B**交付；先得到可试用版本，再集中切换有破坏性的契约。

这次设计不引入项目注册表、远程 VM 管理、多后端统一配置、自更新器或新的 TUI。
Linux 的 `up/start/reload/resize/recreate` 分别对应不同操作，不能为了缩短帮助合并行为。

## 2. 现状核对与问题定位

在 macOS arm64 上从上述源码构建临时二进制，逐层读取帮助，得到：

- 28 个产品顶层命令。
- 63 个具名命令节点，其中 57 个叶子命令；另外 `mac` 自身可执行列表操作。
- 以上不计根命令、自动 `help`、补全内部命令和别名；其他平台的根帮助会隐藏 `mac`。
- `cmd/barn` 有 23 个非测试 Go 文件，共 8,959 行；其中 `main.go` 2,540 行、
  `setup.go` 1,411 行、`command_mac.go` 837 行、`output.go` 753 行。

### 2.1 已有能力应当保留

| 已实现能力 | 证据 | 改造要求 |
| --- | --- | --- |
| Cobra 根、分组、局部参数 | `command_root.go`、`command_*.go` | 延续现有构造器，不重新做一套分发器 |
| 每次执行构造新命令树 | `newRootCommand`、`executeCLI` | 保留，避免 flag 状态跨调用污染 |
| 节点、模板、镜像引用和枚举补全 | `cli.go` | 保留离线补全，强化平台和来源规则 |
| 参数与远端命令隔离 | `remoteArguments`、`splitRemoteArguments` | 保留 `--`、空参数、第二个 `--` 的测试 |
| 单一结果渲染和错误分类 | `commandOutcome`、`typedCommandError`、`internal/failure` | 提取并改进，不另建不兼容协议 |
| SSH 真实终端与信号处理 | `rawWriter`、`runSSHChild`、`signal_test.go` | 保留文件描述符和子进程信号语义 |
| 生命周期恢复、镜像校验和资源归属检查 | `internal/private`、`internal/image` 等 | CLI 重组不重写这些算法 |

### 2.2 真正影响体验的问题

| 现状 | 影响 | 处理方向 |
| --- | --- | --- |
| 根帮助列出 28 个入口，在常用命令前放较长说明和大量短别名 | 上手路径不够突出 | 根帮助优先展示工作流，把细节放到子命令帮助 |
| 根介绍只强调 Linux，Mac 已是一级能力 | 产品入口与实际能力不一致 | 根短说明覆盖 Linux labs 与 macOS VMs |
| `update` 实际更新镜像目录；`repo` 是制作者工具 | 容易误解为升级 Barn 或配置下载源 | 新规范入口为 `image update`；把 `repo` 放到维护工具组 |
| `ssh-config` 用 `--install/--remove` 表示动作，`hosts/network` 用子命令 | 同类动作语法不同 | 为 SSH 配置补齐 `show/install/uninstall` |
| Linux 使用 `status`，Mac 使用 `ls`；Mac 镜像也以 `ls` 为主名 | 难以形成统一记忆 | `status` 查询机器，`image list` 查询镜像；保留旧别名 |
| Linux 日志无尾行数选项，Mac 有 `--lines` 但无 follow | 日常排查方式不同 | 补齐 `--tail` 和 `--follow`，显式说明默认值迁移 |
| `setup` 用 `--yes`，destroy/recreate 用 `--force` 确认，`mac stop --force` 却表示断电 | 参数含义混杂 | 确认用 `--yes`；强制行为继续用 `--force`，逐命令兼容 |
| `purge` 无需确认直接清理全套部署 | 与其他删除命令差异很大 | B 阶段改成明确确认的完整清理动作 |
| network/hosts 非终端无 `--yes` 时只打印计划并返回成功 | 脚本可能把“未执行”当作“已执行” | 增加明确 `--dry-run`，B 阶段对缺少确认返回 usage |
| `prepareOutput` 在 Cobra 前扫描、移除展示参数，`commandFlagValues` 又构造一棵命令树 | 两处参数解释和两次构造增加维护负担 | 由 Cobra 解析；仅保留隔离的解析失败展示适配 |
| `main` 同时包含 SSH、生命周期、网络、镜像与配置生成编排 | 命令定义和业务执行难以独立演进 | 按 CLI、应用编排、领域实现分层 |

实际探针还确认了一个优先级缺陷：

```console
$ BARN_OUTPUT=invalid barn version --json
error: BARN_OUTPUT must be text, json, or yaml, got "invalid"
# exit 2，stdout 为空
```

显式 CLI 参数尚未处理，环境默认值就已经导致失败。目标规则应是“先确定最终来源，
再验证最终值”，有效 CLI 参数能够覆盖无效环境默认值。

以下则是**已有契约**，不能误判为没有实现或偷偷修改：

- `barn --json --help` 仍输出文本帮助，这是当前明确测试的行为。
- 基线 `7b4c95b` 的裸 `barn` 是欢迎摘要；1.0 候选改成完整帮助并退出 0，
  `--json`/`--yaml` 继续返回 `actions[]`。`barn image` 无子命令退出 2；裸 `barn mac` 列表退出 0。
- 普通输出已支持管道适配、进度和颜色控制环境；无需重新引入一套终端框架。
- 普通 `status` 可能核对并修复中断操作的运行记录，不能在命令元数据中宣称绝对无写入。

### 2.3 从 Pig 借鉴什么

| Pig 实践 | Barn 的采用方式 |
| --- | --- |
| `cmd/root.go` 集中注册，按命令族组织平铺文件 | Barn 保留二进制目录，命令定义放平铺的 `internal/cli/command_*.go` |
| `cmd/` 负责声明和参数映射，`cli/*` 负责操作 | 提取 Barn 的应用编排，不在 Cobra 闭包内展开复杂执行流程 |
| `GroupID`、描述、示例和局部 flags | 形成统一帮助模板和命令完整性检查 |
| `cmd/utils.go` 汇集输出、确认和参数规则 | Barn 建小型公共组件，避免每个命令复制确认逻辑 |
| ANCS 从 Cobra annotations 导出机器可读说明 | 借鉴“命令可自描述”，先做最小字段，不照搬所有抽象 |
| 统一结构化结果和 stderr 诊断 | 延续 Barn 现有 payload 和退出码，以兼容为优先 |
| `-o json` | 不直接移植：Barn 已有 `init -o FILE`，改用全局 `--format` |

Pig 当前仍有全局 `rootCmd`、全局参数变量、Viper 初始化和旧输出捕获适配。
这些是它自己的演进背景；Barn 已有每次创建命令树和类型化错误的优点，应继续保留。
仅为使用 Cobra 而引入 Viper、Logrus 或 `cobra-cli` 生成器没有必要。

## 3. 目标命令树

以下是规范名称。旧名称的映射见第 10 节。`ssh-config` 子命令和 `image update`
为 A 阶段新增入口；B 阶段只收敛确认、选择范围和日志默认行为。

```text
barn
├── init [template]                 生成清单
├── setup [template]                准备宿主和实验环境
├── validate                        校验清单
│
├── plan [node...]                   比较期望配置与已应用状态
├── up [node...]                     创建、启动并完成配置
├── status [node...]                 查看部署；别名 st、ls、list
├── start [node...]                  启动已存在节点
├── stop [node...]                   停止节点
├── restart [node...]                按已应用状态重启
├── reload [node...]                 重新读取清单后 stop + up
├── resize [node...]                 应用 CPU/内存调整
├── recreate [node...]               按清单重建
├── destroy [node...]                删除节点或整套部署
├── purge                           完整清理 Linux 部署
│
├── ssh [node] [-- command...]       SSH 会话或 shell 命令
├── exec [node] -- command...        执行远端命令
├── logs [node]                     串口、QEMU 或事件日志
├── provision [node...]              执行本地 Bash 脚本
├── ssh-config                      兼容无子命令时显示配置
│   ├── show [node...]
│   ├── install [node...]
│   └── uninstall
├── hosts
│   ├── install
│   └── uninstall
│
├── image
│   ├── list                        镜像目录和本地镜像
│   ├── info [image]
│   ├── pull [image]
│   ├── import <path>
│   ├── prune
│   ├── update                      更新镜像目录，不升级 Barn
│   ├── sync <url|path>             激活指定目录
│   └── reset                      恢复内置目录
├── network
│   ├── status
│   ├── install
│   └── uninstall
├── doctor
│
├── mac
│   ├── status                     别名 ls、list、st；裸 mac 仍列出状态
│   ├── up [name]
│   ├── start [name...]
│   ├── stop [name...]
│   ├── restart [name]
│   ├── configure <name>
│   ├── recreate <name>
│   ├── destroy <name...>
│   ├── open [name]
│   ├── ssh [name] [-- command...]
│   ├── exec [name] -- command...
│   ├── password [name]
│   ├── ssh-config
│   │   ├── show
│   │   ├── install
│   │   └── uninstall
│   ├── logs [name]
│   ├── setup
│   ├── image
│   │   ├── list
│   │   ├── update
│   │   └── prune
│   └── doctor
│
├── repo                            镜像仓库制作者工具
│   ├── scan <root>
│   ├── build <root>
│   └── verify <root>
├── version
├── completion <bash|zsh|fish|powershell>
└── help [command...]
```

不增加 `barn vm up` 或 `barn node start` 层级。高频动作保持一级，资源型功能放命名空间。
也不把 Linux 镜像仓库与 Mac 的 Apple IPSW、base 存储强行合并。

根帮助分为“常用操作、配置与维护、访问与集成、镜像与宿主、macOS、开发与工具”，
优先列出 `up/status/ssh/stop`。完整 `--help` 仍能找到全部公开规范入口。
短别名只在对应命令的 Aliases 中展示，不再在根示例末尾挤入别名词典。

## 4. 现有命令、位置参数和选项完整清单

本节记录**当前行为**，作为兼容验收输入。每行自动继承 `--json`、`--yaml`、
`--verbose/-v` 和帮助；`--version` 仅为根本地参数。未列出选项的命令不应偷偷接受它们。
`[x]` 可省略，`<x>` 必填，`...` 可重复；节点位置参数当前是精确名称，不是 Ansible selector。

### 4.1 Linux 配置和生命周期

| 当前命令 | 局部选项 | 默认值及边界 |
| --- | --- | --- |
| `init [template]` | `--cidr/-c`、`--image`、`--output/-o`、`--force` | 模板 `meta`；输出 `./barn.yml`；`-o -` 输出内容；force 才覆盖文件 |
| `setup [template]` | `--file/-f`、`--cidr/-c`、`--image`、`--repo/-r`、`--mirror`、`--mode/-m`、`--dry-run/-d`、`--yes/-y` | 模式 `host`；模板与 file 互斥；已有清单不能用 image/cidr 隐式改写；dry-run 与 yes 互斥 |
| `validate` | `--file/-f`、`--repo/-r` | 无位置参数；必须有显式或发现的清单 |
| `plan [node...]` | `--file/-f`、`--repo/-r` | 只读差异计划；不下载或激活新目录 |
| `up [node...]` | `--file/-f`、`--repo/-r`、`--mirror`、`--no-wait/-n`、`--rollback` | 创建/启动/重试未完成配置；运行中节点不无故重启 |
| `start [node...]` | `--no-wait/-n` | 从已应用状态启动；不读清单或创建新节点 |
| `stop [node...]` | 无 | 从已应用状态停止 |
| `restart [node...]` | `--no-wait/-n` | stop + start；不读新清单 |
| `reload [node...]` | 与 up 相同 | 先校验变化，再 stop + up；不自动 resize/recreate/delete |
| `resize [node...]` | `--file/-f` | 只应用 CPU/内存；热增失败或缩容可能停启；保留身份和磁盘 |
| `recreate [node...]` | `--file/-f`、`--repo/-r`、`--mirror`、`--force`、`--no-wait/-n` | 当前 force 表示跳过确认；根盘和非持久盘被替换 |
| `status [node...]` | 无 | 核对运行状态；可能修复中断记录 |
| `destroy [node...]` | `--force`、`--delete-persistent`、`--purge` | 默认保留持久盘；后两项只接受整套部署，不接受节点选择 |
| `purge` | 无 | 当前直接执行，无确认；删除 Linux 节点、持久盘、密钥和状态，保留镜像缓存及宿主网络 |

模板枚举：`meta|dual|trio|full`。默认 Linux 镜像引用是 `u24`；指定镜像支持别名、
`image:channel` 和 `image@version-prefix`。CLI 不另造一套 `--cpu/--memory` 覆盖 Linux 清单。

### 4.2 访问、日志和宿主集成

| 当前命令 | 局部选项 | 默认值及边界 |
| --- | --- | --- |
| `ssh [node] [-- command...]` | 无 | 缺省选控制节点；`--` 后是 SSH 的远端 shell 语义 |
| `exec [node] -- command...` | 无 | 必须有远端命令；多参数保持 argv 语义，单字符串保留旧 shell 简写 |
| `logs [node]` | `--source/-s`、`--follow/-f` | source=`serial`；枚举 serial/qemu/events；events 不接受节点名；当前默认读完整日志 |
| `provision [node...]` | `--script/-s`、`--sudo`、`--parallel/-p`、`--timeout/-t` | script 必填；parallel=1 且 >0；timeout=1h 且 >0；sudo 是客机 `sudo -n` |
| `ssh-config [node...]` | `--install/-i`、`--remove`、`--name` | 默认显示；name=`barn`；install/remove 互斥；remove 不接受节点，也不要求部署存在 |
| `hosts install` / `hosts uninstall` | `--yes/-y` | 管理 `/etc/hosts` 中 Barn 自有块；非终端无 yes 时只显示计划 |
| `network status` | `--cidr/-c` | 检查宿主实际配置和网络就绪状况 |
| `network install` | `--cidr/-c`、`--mode/-m`、`--archive/-a`、`--interface-id/-i`、`--yes/-y` | cidr=`10.10.10.0/24`，mode=`host`；host/shared、archive、interface-id 按平台检查 |
| `network uninstall` | `--yes/-y` | 只移除 Barn 所有的网络；有运行节点时拒绝 |
| `doctor` | 无 | 检查 QEMU、加速、固件、SSH 和网络；包含短暂的加速启动探测 |

`network install` 的 archive/interface-id 是 macOS 专用；Linux 不接受 shared 模式。
`doctor` 可以执行外部探测，不等于“零进程调用”。

### 4.3 镜像与仓库制作

| 当前命令 | 局部选项 | 默认值及边界 |
| --- | --- | --- |
| `update` | `--repo/-r`、`--mirror` | 显式刷新并激活镜像目录；不升级 Barn |
| `image list` | `--repo/-r` | 列出各架构目录项和本地镜像 |
| `image info [image]` | `--arch/-a`、`--repo/-r` | 缺省镜像由目录解析；架构默认宿主，显式值 amd64/arm64 |
| `image pull [image]` | `--arch/-a`、`--repo/-r`、`--mirror` | 下载并验证大小、摘要及镜像元数据 |
| `image import <path>` | `--sha256/-s`、`--name`、`--boot/-b`、`--source-user/-u` | name/boot/source-user 要一起指定；boot=bios/uefi；命名别名前缀 local- |
| `image prune` | `--dry-run/-d`、`--yes/-y`、`--repo/-r` | 默认预览；yes 才删除；dry-run/yes 互斥 |
| `image sync <url\|path>` | `--allow-downgrade`、`--repo/-r` | 激活指定目录；显式控制版本降级 |
| `image reset` | `--repo/-r` | 恢复内置目录；保留已见最高 revision 的降级保护记录 |
| `repo scan <root>` | 无 | 只读检查 repo.yaml 与 images 目录 |
| `repo build <root>` | 无 | 生成 catalog.json；不改 repo.yaml 或镜像字节，不负责签名发布 |
| `repo verify <root>` | 无 | 校验目录与源元数据、镜像字节一致 |

官方仓库继续使用原有签名信任规则；自定义源继续遵循 `RepositoryAllowsUnsigned`
的既有策略。CLI 改造不借参数更名扩大或收紧信任域。

### 4.4 Mac 命令

| 当前命令 | 局部选项 | 默认值及边界 |
| --- | --- | --- |
| `mac` / `mac ls` | 无 | 列出全部机器；ls 已有 list/status/st 别名 |
| `mac up [name]` | `--cpu`、`--memory`、`--disk`、`--user`、`--share`、`--clipboard`、`--subnet`、`--ipsw`、`--yes/-y`、`--no-wait/-n`、`--open` | 默认名通常 mac1；新机器 CPU=4、内存=8G、磁盘沿用 base；已有机器拒绝不一致创建选项 |
| `mac start [name...]` | `--all`、`--no-wait/-n`、`--open`、`--recovery` | 缺省选默认机器；all 与 names 互斥 |
| `mac stop [name...]` | `--all`、`--force` | force 是立即断电，不是确认；all 与 names 互斥 |
| `mac restart [name]` | `--no-wait/-n`、`--open` | 当前仅单台 |
| `mac configure <name>` | `--cpu`、`--memory`、`--share`、`--unshare`、`--clipboard`、`--subnet` | CPU 至少 2，内存至少 4G；subnet 接受 auto；只修改显式传入值 |
| `mac recreate <name>` | `--update`、`--force`、`--no-wait/-n` | 单台；update 使用最新已准备 base；force 跳过重建确认 |
| `mac destroy <name...>` | `--force` | 必须指定至少一台；force 跳过删除确认 |
| `mac open [name]` | 无 | 打开桌面，必要时启动机器，因此是有副作用操作 |
| `mac ssh [name] [-- command...]` | 无 | 与 Linux SSH 保持相同边界 |
| `mac exec [name] -- command...` | 无 | 与 Linux exec 保持相同边界 |
| `mac password [name]` | `--copy/-c` | 显示密码或复制到剪贴板，明确敏感输出 |
| `mac ssh-config` | `--install/-i`、`--remove` | 全部机器的片段；install/remove 互斥 |
| `mac logs [name]` | `--lines/-n` | 默认 100 行，要求正数；当前没有 follow |
| `mac setup` | `--ipsw`、`--disk`、`--yes/-y` | 预备 base，默认磁盘 100G；yes 确认 Apple 下载 |
| `mac image ls` | 无 | 已有 list 别名；列出 IPSW、base 和引用 |
| `mac image update` | `--ipsw`、`--yes/-y` | 准备较新 base；现有机器不会自动重建 |
| `mac image prune` | `--installers`、`--yes/-y` | 默认预览；installers 包含恢复镜像及未完成下载 |
| `mac doctor` | 无 | 检查宿主、组件和机器，不启动机器 |

`--share [name=]path[:ro]` 可重复；`--unshare NAME` 可重复；clipboard 接受 on/off。
Mac 状态位于 `$BARN_HOME/mac`，不读取 Linux inventory。不同默认选择范围源于不同资源模型，
应明确解释，而不是把无参数 `mac stop` 突然改成停止所有机器。

### 4.5 通用命令

| 命令 | 参数与输出 |
| --- | --- |
| `version` / `--version` | 名称、版本、commit、build time、OS/arch；支持既有结构化结果 |
| `completion <shell>` | bash/zsh/fish/powershell；文本为脚本，既有结构化模式为 shell/script 对象 |
| `help [command...]` / `--help` | 当前人类可读帮助；目标增加显式 help 命令的结构化说明 |

## 5. 参数设计与配置来源

### 5.1 全局参数

全局只放展示和交互策略。依赖清单、镜像、宿主权限或特定业务行为的选项仍是局部参数。

| 参数 | 目标语义 | 阶段 |
| --- | --- | --- |
| `--format text\|json\|yaml` | 统一格式入口，默认 text，不占用 `-o` | A |
| `--json` / `--yaml` | 保留快捷拼写，映射到同一个输出配置 | A 保留 |
| `--verbose/-v` | 保留布尔语义；不把 `-vv` 悄悄改为多个日志等级 | A 保留 |
| `--quiet/-q` | 关闭进度和普通提示；保留结果 stdout、warning、error；与 verbose 互斥 | A |
| `--color auto\|always\|never` | auto 按输出流 TTY、NO_COLOR、TERM 判断；结构化 stdout 永不加色 | A |
| `--progress auto\|plain\|none` | auto 在文本 TTY 使用动态进度，其他情况有界纯文本；独立于颜色 | A |
| `--non-interactive` | 禁止 Barn 提问及等待 sudo 密码；缺少确认时报错；交互 SSH 无远端命令时拒绝 | A |
| `--help/-h` | 人类可读帮助，退出 0，不加载运行服务 | A 保留 |
| `--version` | 继续与 version 共用版本结果模型 | A 保留 |

不增加全局 `--file/-f`：`logs -f` 已经是 follow，而且 status/stop/ssh 不读清单。
不增加全局 `--yes`、`--force`、`--timeout` 或 `--repo`：避免无效参数看起来适用于所有命令。
`--home` 可在将来完成状态根依赖注入后再考虑，本轮继续使用 `BARN_HOME`。

CLI 显式值 > 对应环境变量 > 内置默认值。现有环境变量继续保留：

| 来源 | 决策规则 |
| --- | --- |
| `BARN_OUTPUT` | text/json/yaml；只有没有 CLI 格式覆盖时才验证 |
| `BARN_VERBOSE` | 无显式 verbose 时才使用；`--verbose=false` 可覆盖 |
| `BARN_HOME` | 继续经 `state.ResolveDataRoot` 规范化和检查；跨目录操作建议绝对路径 |
| `BARN_REPO` | 明确 `--repo` > `--mirror` > BARN_REPO > 官方默认 |
| `NO_COLOR`、`TERM`、`CI` | 保留现有终端/进度适配；显式 color/progress 优先 |
| `BARN_VMNET_ARCHIVE` 等组件覆盖 | 保持现有组件层用途，不引入通用自动环境绑定 |

`--json` 与 `--yaml` 同时为 true 是错误。显式 `--format` 与另一个启用的快捷格式不一致也是
错误，一致则接受。已有 `--json=false`、`--yaml=false` 的顺序行为保留在兼容测试中；
新文档用 `--format text` 表示回到文本。无效环境值不能阻断显式覆盖，也不能阻断文本帮助。

新展示参数的布尔冲突按最终启用值判断：`--quiet=false --verbose` 不冲突；quiet 生效时
关闭进度，即使同时给了 progress。不要对这些快捷格式直接套用仅检查 flag 是否 Changed 的
互斥规则，否则显式 false 会被错误地当作启用。现有业务 flags 的组合规则则先按基线保留。

### 5.2 局部参数的目标调整

| 命令范围 | 新规范 | 兼容约束 |
| --- | --- | --- |
| init | 继续 `--output/-o PATH`；可选增加 `--overwrite` 作为更清楚的同义名 | `--force` 仍只允许覆盖文件，`--yes` 不隐式覆盖 |
| recreate/destroy，包括 Mac | 增加 `--yes/-y` 表示确认 | 原 `--force` 仅在这些命令上保留为确认兼容名；不能套用到 mac stop |
| purge | 增加 `--dry-run`、`--yes/-y` | A 阶段旧调用保持语义并说明风险；B 阶段缺少确认不再直接执行 |
| network/hosts install/uninstall | 增加 `--dry-run/-d` | A 保留旧默认；B 明确区分执行与预览 |
| Linux recreate/destroy | B 阶段要求 node 或 `--all` 指定范围 | A 保留无参数全部节点；全局清理选项只配 --all，不配 node |
| ssh-config | `show/install/uninstall`，`--name` 仍是 Linux 命名参数 | 原无动作显示、--install、--remove 继续适配同一 service |
| Linux/Mac logs | `--tail N`、`--follow/-f` | Mac 的 --lines/-n 保留别名；A 中 Linux 默认完整读取不变，B 再统一默认 100 行 |
| image | 新增 `image update` | 根 update 为兼容入口；仍然只刷新目录 |
| image prune / mac image prune | 明确 `--dry-run` 与 `--yes`；默认预览 | 有意保留清理命令的预览默认，不将其改成意外删除 |

`--tail 0` 表示不显示历史内容；配合 follow 仅看新内容；`--tail -1` 表示全部；其他负数错误。
Mac 旧 `--lines` 继续要求正数。新旧尾行数参数同时出现则拒绝，避免优先级歧义。
为保持 `-n` 的旧行为，先只给新 `--tail` 长名称；不全局重排 `-n`。

`--dry-run` 与 `--yes` 互斥，不能把“想预览但自动注入了 yes”默默解释成执行。
未实现无副作用计划的命令不注册 dry-run；例如 provision 不能声称预知任意脚本会做什么。

### 5.3 参数约束和位置规则

解析遵循 `barn <command> [flags] [args]`，在远端分隔符之前允许 flags 与位置参数交错。
共享参数通过 helper 显式注册到需要的叶子命令，不用 persistent flags 向无关命令扩散。

- 普通命令使用 `NoArgs`、`ExactArgs`、`MaximumNArgs` 或明确的节点列表校验。
- `--all` 与显式名称互斥；批量名称去重、保持确定顺序；未知名称给出可用名称和 status 提示。
- `setup -f FILE` 与模板、模板覆盖选项的关系在业务动作之前验证。
- `image import --name/--boot/--source-user` 继续要求一起出现。
- `logs --source events NODE` 在打开日志、创建审计记录之前报 usage。
- `--repo` 和 `--mirror` 保持既有优先级，不为了“统一”突然改成互斥。
- Mac 创建参数仅适用于新机器；不把 `mac up --cpu` 解释成隐式 configure。
- 只有实际支持的命令暴露某个 flag；平台限制在帮助中标注，在纯验证阶段拒绝不支持的组合。

### 5.4 配置来源和选择范围

| 命令 | 清单与状态来源 |
| --- | --- |
| init | 模板直接生成，不读取已应用配置 |
| setup | 显式 file；否则模板参数或发现清单；无清单时生成 meta |
| validate | 显式 file > 当前目录发现；绝不回退已应用状态 |
| plan/up/reload/resize/recreate | 显式 file > barn.yml > barn.yaml > pigsty.yml > pigsty.yaml > 已应用规格 |
| start/stop/restart/status/destroy/purge/访问命令 | 只用已应用状态；切换目录不选出另一个部署 |
| mac 全部命令 | `$BARN_HOME/mac` 的具名机器，与 Linux 清单独立 |

已有但无效的显式或发现清单必须报错，不能回退状态掩盖配置错误。文本在 verbose 或回退时
说明来源，结构化结果保留/补充对应来源字段；不改 JSON 字段名。

Linux 无参数日常生命周期继续作用于全部适用节点；SSH、exec、普通 logs 缺省选控制节点。
Mac 单机命令继续使用现有默认机器决策，start/stop 才有显式 --all。
B 阶段仅收紧 Linux destroy/recreate 的隐式全选；Mac destroy 保持必填名字，不顺便新增全删。
因此 B 阶段的准确 Usage 是 `destroy <node...> | --all` 与 `recreate <node...> | --all`；
第 3 节保留了兼容期的简写，实施切换时帮助、schema、completion 和 next 建议必须一起更新。

## 6. 执行、确认和预览契约

`--yes` 只替代 Barn 的确认，不绕过权限、资源归属、规格漂移、签名校验或网络占用检查。
`--force` 继续表示命令特有的行为，例如 `mac stop --force` 立即断电；兼容确认别名单独声明。

目标行为如下；改变现有默认的行在 B 阶段交付：

| 操作 | 文本交互终端 | 非终端 / non-interactive / 结构化结果 | dry-run |
| --- | --- | --- | --- |
| 查询、validate、plan | 直接执行 | 直接执行 | 无需 |
| up/start/stop/restart/resize 等普通动作 | 执行；需要首次宿主准备时展示并确认 | 不隐式进入向导；缺少前提给出 setup --yes 配方 | 只对有真实 planner 的操作提供 |
| setup、network/hosts install/uninstall | 展示具体计划后确认 | 缺少 yes 返回 2；不能只显示计划后假装应用成功 | 返回计划，退出 0，不创建状态、密钥、审计或下载 |
| destroy/recreate | 展示节点、磁盘影响并输入确认词 | 要求 yes；旧 force 在兼容期等价 | 独立计划实现完成后提供 |
| purge（B） | 展示完整清理范围并确认 | 要求 yes | 返回完整清理范围，不能偷偷调用 mutator |
| image prune / mac image prune | 默认只预览；yes 才删除 | 默认只预览，结果明确 applied=false | 显式等价于预览 |
| Mac Apple 下载 | 展示来源、目标和预计大小后确认 | 需要下载时要求 yes；本地 IPSW 沿用既有规则 | 只有真正无下载的计划实现后提供 |
| init 覆盖 | 需要显式 overwrite 或旧 force | 相同 | `-o -` 已可预览内容 |

计划必须对应实际将执行的动作。`barn plan` 是期望状态比较，不能用它冒充
`destroy --dry-run`、`purge --dry-run` 或脚本执行计划。

确认应在配置、名称、选项校验之后，在第一项修改之前发生。执行时重新验证状态 revision、
文件身份及实际删除范围；如果范围变化，需要重新计划，不把旧 yes 当作扩大授权。
`--non-interactive` 下 sudo 使用非交互检查；`--yes` 不代表已取得 sudo 密码。

root、帮助、版本、补全不得触发目录创建、日志文件创建、网络访问、sudo 或后台服务。
dry-run 的只读保证包括不写 begin 审计事件；实际执行仍记录 operation ID 和恢复信息。

## 7. 输出、错误、远端执行与日志

### 7.1 三条输出路径

| 场景 | stdout | stderr |
| --- | --- | --- |
| 普通文本 | 主结果、表格、原始配置或远端 stdout | 进度、提示、警告、错误、远端 stderr |
| JSON/YAML 单次结果 | 恰好一个结果文档，包括可用的部分失败报告 | 人类诊断；不能混进结果流 |
| logs follow | 文本字节流，或 JSON 的 NDJSON / YAML 文档流 | 诊断和取消信息；不向 stdout 追加另一种顶层错误对象 |

保留当前各命令的 typed payload。**本轮不把所有结果强制包进新的 `data` envelope**，
否则现有读取 `.nodes`、`.results` 等字段的脚本都会失效。JSON 与 YAML 从同一结构编码，
保持现有字段名、数字类型、null/空集合规则和失败报告形状。

新增命令可直接复用对应旧命令的结果类型：`image update` 等价于旧 `update`，
`ssh-config show` 等价于旧 `ssh-config`。新元数据字段只有确有用途且保持兼容时才追加。
若将来需要统一结果 envelope，应作为单独、可选择版本的协议设计，而不是这次重构的附赠品。

文本表格固定常见列，详细内容放 verbose 或明确查询。推荐 status 默认列为
`NAME STATE IP CPU MEMORY IMAGE`，机器未就绪时提供简短原因；实际字段从现有报告生成。
窄终端缩减可选列，管道输出不含 ANSI 或动态光标；不通过是否连接 TTY 切换为 JSON。
`--quiet` 不得把 `status` 或 `init -o -` 的主结果吞掉。

生成内容继续保留各自约定：

- `init -o -`：文本模式为原始 YAML；结构化模式保留当前含 content 的结果对象。
- `ssh-config show`：文本模式为可用的 OpenSSH 片段；结构化模式使用既有结果模型。
- `completion bash`：文本模式为可 source 的脚本；JSON/YAML 保留 shell/script 对象。
- `mac password`：只有明确调用才输出或复制密码；通用诊断、schema、进度及 next 提示不记录密码。

### 7.2 帮助和可自描述能力

保留 `barn COMMAND --help` 无论输出环境如何都给人类文本帮助的契约。
为**显式 help 命令**增加机器可读说明：

```bash
barn help --format json
barn help image pull --format yaml
barn --json help image pull
```

以上形式均通过相同输出选项解析；`--json` 与 `--format json` 没有两套含义。
`--help` 是查询人类帮助的短路标志，`help ...` 则是可选择格式的说明命令。

schema 从实际 Cobra tree 导出 path、aliases、summary、usage、参数名称/类型/默认值/作用域、
examples 和子命令；再补充少量 Barn 特有元数据：

```json
{
  "schema_version": 1,
  "path": "barn image pull",
  "scope": "image-cache",
  "effect": "write-cache",
  "inventory": "none",
  "output": "result",
  "confirmation": "none"
}
```

这只是附加元数据片段，不替代自动导出的参数。复杂约束由一个命令级规则声明同时用于
校验和 schema；任意业务前置检查仍留在 service，不把 Cobra 树当作完整执行计划。
`status` 标明可能协调运行记录，`doctor` 标明探测进程，`mac open` 标明可能启动，
`mac password` 标明敏感输出，`repo build` 标明写目录文件。
不能把缺失元数据默认解释为“安全、只读、可并行”。

根文本帮助示意：

```text
Barn runs local Linux labs and macOS virtual machines.

Usage:
  barn <command> [flags]

Get started:
  barn up                  Create and start the Linux lab
  barn status              Show nodes and readiness
  barn ssh                 Connect to the control node
  barn mac up              Create and start a macOS VM

...按工作流分组的完整命令列表...

Global flags:
  --format text|json|yaml   Result format (default text)
  --json / --yaml           Format shortcuts
  -v, --verbose            Detailed diagnostics
  ...

Run 'barn <command> --help' for examples and local flags.
```

每个叶子帮助按“一句话用途 → Usage → 最多三个常用示例 → 参数 → 相关下一步”组织。
高级恢复参数仍可见，说明具体适用条件；不把全部设计历史塞进 Long。
无子命令的 namespace 仍显示帮助并退出 2，保留既有 root/mac/ssh-config 例外。

### 7.3 错误与退出码

沿用当前退出码，不换成另一套通用 CLI 编号：

| 码 | 含义 |
| --- | --- |
| 0 | 成功，或明确的预览完成 |
| 1 | 运行失败 |
| 2 | 命令、参数、选择范围错误，或缺少要求的非交互确认 |
| 3 | 宿主/平台能力不足 |
| 4 | 状态或配置冲突 |
| 5 | 批量操作部分失败 |
| 6 | 资源不足或资源冲突 |
| 7 | 数据、身份、签名或完整性问题 |
| 130 | Barn 操作取消 |

SSH/exec 的远端程序退出码直接透传，因此可能与 Barn 分类码重叠；这是已有契约。
需要分辨时检查结构化结果中的远端 exit_code 与失败信息，不能把远端 7 误报为镜像校验失败。

根统一打印一次错误和 next 建议。usage 错误只给一句错误及当前命令的帮助入口，
不倾倒整页 usage；运行错误不附加无关参数帮助。不按 `err.Error()` 文本猜分类。
保留部分成功结果和 operation ID；不要因为返回 err 就丢弃已经发生的结果。

解析失败时尽量按明确的格式请求输出错误。若格式本身冲突或不合法，则 stderr 文本、stdout
为空、退出 2；若已确定 JSON/YAML，则 stdout 是一个通用失败文档。
错误渲染只需要读取展示配置，不初始化 service、状态目录、日志文件或特权会话。

### 7.4 远端参数和终端契约

```bash
barn ssh meta
barn ssh meta -- 'df -h /data; id'
barn exec meta -- printf '%s\n' 'a b' '' '--json'
barn exec meta -- sh -c 'uptime; id'
barn --format json exec meta -- sh -c 'exit 17'
```

继续保留以下规则：

- 第一个 `--` 前只允许 Barn flags 和至多一个节点/机器名；之后不再解析 Barn 参数。
- `exec` 必须有远端命令；首参数不得为空，后续空参数、带空格参数和第二个 `--` 完整保留。
- `ssh` 使用 OpenSSH 的 shell 字符串语义；`exec` 多参数通过现有 shell quoting 保持 argv。
- 单字符串 exec 的旧 shell 简写保留，但新示例推荐 `sh -c`；不通过此次重构改变引用语义。
- 不使用整个命令的 `DisableFlagParsing`，否则本地 help、未知参数和补全边界都会变差。
- 远端 completion 在分隔符后停止，不连接客机做命令探测。
- 交互 SSH 的子进程继续拿到真实终端文件描述符；Ctrl-C 作用于前台会话，不能误取消 Barn 父进程。
- 保留已有“结构化交互 SSH 会话走 stderr、结束结果走 stdout”的特例。
  结构化模式禁止的是 Barn 的确认提示，不是用户明确请求的 SSH 会话；显式
  `--non-interactive` 才拒绝无远端命令的交互会话。
- 非交互 SSH/exec 的结构化捕获保留字节上限与 truncated 字段；普通文本保持直接流式输出。

follow 流启动后遇到取消仍退出 130，不追加一个破坏流格式的错误文档。
Mac follow 和 Linux tail 的实现必须处理文件增长、截断、轮转、超长行以及取消，
不能只在命令层注册参数后忽略它们。

## 8. Go/Cobra 实现架构

### 8.1 目标目录

```text
cmd/
  barn/main.go                 进程入口，唯一 os.Exit
  barn-hosts-helper/           原有独立 helper

internal/cli/
  root.go                     NewRoot、Execute、全局展示参数、集中注册
  command_setup.go            setup/init/validate 的声明
  command_lifecycle.go        Linux 生命周期声明
  command_access.go           ssh/exec/provision/logs/ssh-config/hosts
  command_image.go            image 与兼容 update
  command_network.go
  command_mac.go              Mac 树；拆出原有执行与渲染闭包
  command_repo.go
  command_misc.go             version/completion
  flags.go                    共用 flag 注册、格式规范化、纯参数检查
  help.go                     文本帮助与结构化说明
  completion.go               只读的本地补全
  compat.go                   旧命令和旧 flag 的显式适配
  errors.go                   failure → exit code，统一错误出口
  output.go                   单次结果与结构化流编码
  lifecycle_output.go         各领域结果的文本 presenter
  mac_output.go
  network_output.go
  progress.go / terminal.go   当前终端能力，独立于业务服务
  prompt.go / signal.go       确认、stdin 与 SSH 信号交接

internal/app/
  runtime.go                  每次执行的配置和实际依赖
  setup.go                    宿主准备及其计划/应用编排
  lifecycle.go                配置来源、节点选择、生命周期调用
  access.go                   SSH 连接、provision、日志读取编排
  integration.go              SSH 配置与 hosts 集成
  image.go / network.go       跨服务编排；很薄的调用可直接复用领域 service
  mac.go                      需要组合的 Mac 操作

internal/private/             原有 Linux VM 管理、恢复与事务
internal/macvm/               原有 Mac 管理
internal/image/               原有镜像 service、目录和缓存
internal/config/              原有 inventory 解析
internal/state/               原有状态格式和根路径检查
internal/failure/             原有错误身份
...                          其余平台、网络、磁盘等实现
```

这是责任划分，不要求为了填满目录而增加空包装。简单命令可以从 CLI 直接调用已经设计好的
领域 service；只有目前混在 main 里的跨组件编排需要 app 层。

搬迁时保留命令族平铺结构。`command_mac.go` 中操作和输出迁出后再判断文件大小，
不按每个叶子创建一个目录。对既有 outcome 和 formatter 先搬再拆，不一次重写所有结果类型。

### 8.2 命令执行路径

```text
main
  → 创建可交接的信号上下文
  → cli.Execute(ctx, args, streams, dependencies)
      → NewRoot（纯构造；每次一棵树）
      → Cobra/pflag 解析参数
      → 纯参数校验、展示配置规范化
      → 只读解析配置/状态与操作范围
      → 生成准确计划（适用命令）
      → dry-run 返回，或确认
      → 创建操作上下文、获取锁并核验状态
      → app / 领域 service 执行
      → 返回 typed report / error
      → 统一渲染、释放资源、映射退出码
  → os.Exit(code)
```

接口形状可采用：

```go
type IOStreams struct {
    In     io.Reader
    Out    io.Writer
    ErrOut io.Writer
}

func Execute(ctx context.Context, args []string, streams IOStreams, deps Dependencies) int
func NewRoot(streams IOStreams, deps Dependencies) *cobra.Command
```

`Dependencies` 放真实边界，例如运行器、提示器、环境读取和用到的服务；按需要增加小接口，
不为全部内部类型制造 interface。端点保持显式类型化 Request/Report，
不演进成 `map[string]func(any)` 的动态分发框架。

RunE 负责“读取本命令已解析选项 → 构造 request → 调用 → 收集 outcome”。
业务层不依赖 Cobra；主结果不在领域服务里直接 print；进度沿用 activity.Reporter。
SSH 和日志这样的流式操作可以接收专用流接口，不把它们强行转换成内存中的一次性结果。

现有 `os.Stdin` 读取、终端检测及环境读取要逐步收口到一次执行的上下文。
`cmd.SetIn()` 必须真正传到确认及 SSH，不能只是设置后仍然读取全局 stdin。
不用 `os.Setenv` 实现单次调用选项，避免并行测试和嵌入式调用互相污染。

### 8.3 Cobra 生命周期的注意事项

已核对当前锁定版本 `v1.10.2` 的实际执行顺序：

```text
ParseFlags → help/version 短路 → Args
           → PersistentPreRunE → PreRunE
           → ValidateRequiredFlags → ValidateFlagGroups → RunE
```

因此不能把依赖安装、目录创建、下载、操作审计或 sudo 放在 pre-run 中：
缺少 required flag 或互斥 flags 的错误可能还没验证。PreRunE 只做无副作用的规范化和检查，
真正的副作用从 RunE 中已经验证的应用请求开始。

子命令的 PersistentPreRunE 默认会覆盖父级 persistent hook；不要让某个命令族绕开统一初始化。
优先只保留根级纯展示准备和叶子的纯 PreRunE，或者用显式 helper 组合；不依赖隐蔽全局开关。

### 8.4 收敛展示参数预扫描

目标是 Cobra 单一语法来源，而不是武断地承诺“一行预扫描也不能有”：help/version 在 hook
之前短路，unknown-command/unknown-flag 也可能在展示配置初始化前失败。

实现顺序：

1. 全局展示 flags 真正绑定到本次调用的 Options，正常执行不再先移除 argv 中的 flag。
2. 正常执行、help 和 version 共用无副作用的 `ResolveOutputOptions`；只读取已解析 flag
   及被选中的环境默认值。
3. Execute 在失败后仍可访问本次命令树和已解析的展示状态，优先据此输出错误。
4. 对 Cobra 在解析前返回的错误保留一个严格限域的 presentation bootstrap：只确定格式，
   使用同一棵树的参数元数据识别选项值，不修改 argv、不执行命令、不另建命令树。
   遇到无法证明归属的未知参数值，采用确定的文本错误回退，而不是猜测远端参数。
5. 兼容测试覆盖全局参数前后位置、`--flag=value`、布尔 false、短参数组合，以及
   `-f --json` / `-o --yaml` 这类“长得像 flag 的选项值”；`--` 后永不扫描。
6. 在这些情况全部验证后移除 `prepareOutput` 的 argv 改写和 `commandFlagValues` 的二次构树。
   若某个失败路径暂时仍需旧兼容适配，应明确留在 compat，而不是宣称迁移已经完成。

现有 `TestCommandPackageHasNoSecondaryFlagParser` 通过禁止导入 pflag 来检查边界，
并不能证明没有手写扫描。搬迁后应改成关注真实行为的解析/边界测试；为检查 flag 元数据
或生成文档正常使用 pflag 类型，并不等于重新解析一遍命令。

### 8.5 补全与文档

补全继续使用 Cobra 的 `ValidArgsFunction` / flag completion：

- desired 命令从有效显式或发现清单补全，无清单才回退已应用状态；坏清单不能静默回退。
- applied 命令从已应用状态补全；Mac 只读机器记录；已经选中的名字不重复建议。
- 镜像从本地 active/embedded catalog 和 local aliases 补全，不请求网络。
- YAML 路径、IPSW、脚本和 repo 目录按实际类型补全；枚举参数禁止文件补全。
- `--share [name=]path[:ro]` 需要认识前缀和后缀，不能把完整语法当普通目录处理。
- 配置或状态损坏时 completion 返回空建议，不提示密码、不修复现场、不创建 BARN_HOME。

从可导入的 NewRoot 生成参考命令页、man page 和补全脚本；人类教程仍由文档维护。
平台相关的帮助生成使用显式 platform capability，不靠作者当前机器的隐藏状态决定网站内容。
生成物写到仓库外构建目录或已验证忽略的位置；发布包安装到 shell/man 的标准路径。
`help --format json` 和生成文档均从同一树读取，避免第三份手写 flag 目录成为运行时真相。

## 9. 兼容试用方式

试用二进制应使用同一个入口和依赖版本，名字可为 `barn-cli-next`，输出放在仓库外。
“试用 CLI”不等于新建另一套 VM 数据：现有 Linux 宿主网络和 SSH 集成有全局部分，
不能只改 BARN_HOME 就宣称两个真实实验环境完全隔离。

先用全新的临时 BARN_HOME 和固定 fixture 比较：

```bash
barn-cli-next --help
barn-cli-next help image pull --format json
barn-cli-next init full --image el9 -o -
barn-cli-next plan -f /path/to/fixture/barn.yml --format json
barn-cli-next image list --format yaml
barn-cli-next completion bash
```

只在专用验收宿主/明确的可丢弃部署上验证 up、recreate、destroy、purge 和宿主安装卸载。
用旧、新二进制**顺序**操作同一份受控状态做兼容验证，不让两个进程同时变更同一部署。
本轮纯 CLI 重构不改变状态格式，二进制回退继续读原格式；任何必要状态迁移应另立设计。

## 10. 兼容矩阵与行为切换

| 旧用法 | 推荐用法 | A 阶段 | B 阶段 |
| --- | --- | --- | --- |
| `barn --json status` | `barn status --format json` | 两者等价 | 持续支持快捷名 |
| `barn init -o lab.yml` | 原样 | 含义不变 | 不复用 -o 为格式参数 |
| `barn update` | `barn image update` | 两者共用实现 | 根 update 作为隐藏兼容入口保留 |
| `barn mac ls` | `barn mac status` | 保留 ls/list/st | 不删除低成本别名 |
| `barn mac image ls` | `barn mac image list` | 保留 ls | 同左 |
| `barn ssh-config --install` | `barn ssh-config install` | 兼容适配 | 旧旗标隐藏但仍可执行 |
| `barn ssh-config --remove` | `barn ssh-config uninstall` | 兼容适配 | 同左 |
| `barn mac ssh-config --install/--remove` | 对应子命令 | 同上 | 同上 |
| `barn destroy --force meta` | `barn destroy meta --yes` | 同一确认语义 | 原 force 只在对应命令作为兼容名保留 |
| `barn recreate --force meta` | `barn recreate meta --yes` | 同上 | 同上 |
| `barn mac stop dev --force` | 原样 | 立即断电 | 不解释为 yes |
| `barn destroy` / `barn recreate` | `barn destroy --all` / `barn recreate --all` | 原全选有效，增加 --all | 无名字且无 all 退出 2 |
| `barn destroy --force --purge` | `barn purge --yes` | 原整套清理有效 | 旧 destroy 形式要求 --all，范围不变 |
| `barn purge` | `barn purge --yes` 或 `--dry-run` | 旧行为保留，提供迁移提示 | 终端确认；非交互无 yes 退出 2 |
| 非终端 `network/hosts install` | `... --yes` 或 `... --dry-run` | 原预览行为保留 | 无 yes/dry-run 退出 2 |
| `barn mac logs -n 100` | `barn mac logs --tail 100` | 旧正数规则保留 | 同左 |
| `barn logs meta` 全部历史 | `barn logs meta --tail -1` | 无参数仍全部历史 | 默认 100 行，完整读取必须显式 |

原有短别名 `s/v/pl/rc/st/de/ex/l/sc/im/images/dt/n/net/ver` 与子命令短别名继续解析。
不重新分配旧字母，不新增大量难猜的缩写。新增 Linux ls/list 指向 status，
不能指向 destroy/purge，也不能让 alias 获得不同参数或输出。

兼容命令要单独创建 Cobra 节点并调用同一 service；一个 `*cobra.Command` 不能同时挂两个父节点。
同时出现旧动作 flag 和新动作子命令时直接报 usage，避免 `ssh-config uninstall --install`
这样的歧义。共享 `--name` 可以只 persistent 于 ssh-config 命名空间。

不要直接依赖 Cobra `Command.Deprecated` 打印迁移消息：锁定版本会经命令输出流打印，
可能污染结构化 stdout。兼容提示由 Barn 的 stderr renderer 发出，最多一次；quiet 抑制普通
弃用提示。Hidden 只影响发现性，不改变已知旧用法的执行结果。

B 阶段是明确的行为变化，应经历至少一个有迁移说明的试用/过渡版本，并在正式发布说明中
集中列出。每一项有独立测试和迁移命令，不通过一次“重构”提交默默启用。
根 update、短别名等低成本入口没有必要为了整洁设定强制删除日期。

## 11. 分阶段实施与验收

| 阶段 | 具体改动 | 可审查交付 | 完成条件 |
| --- | --- | --- | --- |
| P0：锁定基线 | 建命令 surface、flags、aliases、输出和退出码基线；记录平台差异 | 行为测试与小型 surface fixture | 旧源码基线通过；不把机读 payload 的差异混入帮助快照 |
| P1：提取可导入 CLI | cmd/barn 变薄；命令、输出、信号先搬入 internal/cli；测试随代码移动 | 只搬迁责任的提交 | 原命令/flags/错误码与状态格式等价；Makefile、CI、打包路径同步 |
| P2：兼容体验 A | format/color/progress/quiet/non-interactive；规范帮助；image update、SSH 子命令、名称别名、yes/all 兼容入口；格式优先级修复 | 可试用二进制与兼容矩阵 | 所有旧正常调用通过；新 flags 生效；错误和 help 不污染 stdout |
| P3：应用层整理 | 从 main/setup/Mac 闭包提取编排；注入 streams、prompter、runner；统一 outcome/error 出口 | 按命令族拆分的重构提交 | 空/坏参数不产生操作事件、目录、下载或特权请求；恢复契约继续通过 |
| P4：补齐计划与日志 | 为 purge、销毁/重建等实际需要的操作实现只读 planner；network/hosts dry-run；Mac follow、Linux tail | 真正可用的 plan/read 功能 | dry-run 无写；计划和执行范围一致；流式日志边界与取消验证通过 |
| P5：行为收敛 B | 启用确认、显式全选、非交互和日志默认值迁移 | 独立行为变更与迁移说明 | 交互/脚本矩阵通过；兼容入口和旧 force 范围明确 |
| P6：文档与发布 | 命令参考生成、补全/man 打包、双语参考与教程更新 | 发布候选与验收记录 | 源码门禁、跨平台检查、镜像目录发布门禁全部通过 |

P2 为首个值得用户试用的里程碑。P4 的新 dry-run 在纯 planner 完成前不暴露；
P2 不以“已注册 flag”为完成功能的标准。P3/P4 可按命令族递进，避免一次同时重写
Linux 生命周期、Mac VM、宿主网络和所有 JSON 类型。

P0 的 surface fixture 是命令契约证据，不是第二套运行时命令注册表；只保存必要轻量元数据。
如果 P1 与 P3 拆分反而制造大量中间导出 API，可按命令族合并，但每次提交只改变一种责任。

### 11.1 测试矩阵

| 维度 | 必须覆盖的真实行为 |
| --- | --- |
| 语法 | 全部规范命令和兼容入口；未知命令建议；参数前后位置；重复与未知名称；互斥、必填、枚举 |
| 参数冲突 | init -o 与 format；logs -f 与 inventory -f；Mac logs -n 与生命周期 -n；旧 force 与 yes |
| 格式来源 | CLI 覆盖有效/无效环境；显式 false；冲突格式；错误发生在格式前/后；help/version 短路 |
| stdout/stderr | 单结果可解码；失败保留部分结果；write error；弃用提示不混 stdout；quiet 保留主结果 |
| 无副作用 | NewRoot、help、version、completion 无写入、网络或外部进程；语法错误在服务调用前返回；dry-run 无写入/下载/特权变更，只允许计划所需且已声明的只读探测 |
| 配置选择 | cwd 有效/无效/缺席清单；显式 -f；applied 回退；state 损坏；Mac 不读 Linux 清单 |
| 选择范围 | Linux 缺省全部与显式子集；B 删除要求 all；Mac 默认单机与 start/stop all；空集合保持既有幂等语义 |
| 确认 | TTY/非 TTY、text/JSON/YAML、yes/force/dry-run、sudo 可用/不可用，计划与真实执行可区分 |
| 远端边界 | 空参数、空格、引号、shell 字符、第二个 --、远端 --help/--json、远端非零码和信号 |
| 日志 | snapshot/follow，tail=-1/0/N，Mac lines 兼容，长行/轮转/截断，NDJSON/YAML 流与取消 |
| 补全 | bash/zsh/fish/powershell 脚本生成；bash 实际补全；坏状态无写入；-- 后不补全 Barn |
| 平台 | Linux amd64/arm64、macOS amd64/arm64 构建；Mac 真机运行和不支持平台的明确错误 |
| 状态恢复 | 选择节点独立收敛、部分失败重试、锁、磁盘身份、签名目录和旧状态继续可读 |

单元测试使用注入的流和依赖；真实 PTY 用于验证提示、颜色、SSH Ctrl-C，避免以 bytes.Buffer
测试冒充真实终端验收。跨编译只能验证构建，不能代替 KVM/HVF/Apple Virtualization 的运行验收。

没有理由为纯搬文件新增镜像下载集成测试；但修改确认、选择范围、输出协议和远端引用的提交
必须有行为测试。保留已有测试价值，减少只检查实现细节或固定文件名的 AST 约束。

按代码改动运行现有 `go test ./...`、`go vet ./...` 和相关 race 检查，最终运行仓库的
`make check`，不要以本设计阶段的 focused test 代替实现后的验收。
搬出 cmd/barn 后同步 `.github/workflows/ci.yml` 的 `go test -race ./internal/macvm ./cmd/barn`
及相关测试引用，避免 CI 看起来绿但漏跑新 CLI 包。构建、覆盖率、快照和补全输出放仓库外
或事先验证已忽略的位置。

### 11.2 文档与正式发布门禁

实现时更新伴随站的以下中英文页面：

- `content/docs/reference/cli.md` / `cli.zh.md`：语法、参数、格式和退出码。
- `content/docs/reference/mac.md` / `mac.zh.md`：Mac 规范名称、日志和确认。
- `content/docs/reference/images.md` / `images.zh.md`：image update 与兼容入口。
- 对应入门、日常管理和排错教程；根 README 只保留简洁工作流。
- 行为收敛被接受后，在 `content/blog/design/` 记录决定及其实施状态，不把本 Proposal
  直接写成已发布 reference。

正式发布仍必须遵守本仓库 AGENTS 的镜像目录门禁：从官方仓库获取最新签名 catalog，
先验签再同步 `internal/image/default-catalog.json`，同时更新 EmbeddedManifestVersion、
内置目录时间及必要的 repo.yaml；对照 stable artifact 的路径、架构、摘要和大小，
运行镜像目录测试，并在全新临时 BARN_HOME 中用保留的 `barn update` 验证签名校验与激活。
这道门禁属于未来正式发布，本次设计阶段无需更新目录或下载 VM 镜像。

## 12. 本次调查的证据与验证范围

已完成：

- 核对 Barn 源码、当前帮助和相关测试；参考 Pig 的 root、命令族、utils 和 ANCS 实现。
- 在仓库外构建 Barn；以隔离 BARN_HOME 逐个检查 64 个帮助路径（含根，排除内建 help）。
  这组帮助调用没有创建该 BARN_HOME。
- 复现格式优先级、namespace 默认退出码、格式化错误及远端分隔符的现状。
- 运行以下既有测试选择，结果通过：

```bash
go test ./cmd/barn \
  -run 'Test(Cobra|RootHelp|CommandTree|EveryVisibleCommand|ConfigFlagScope|RemoteCobraBoundary|RemoteHelp|GeneratedBashCompletion|ExplicitHelp|PresentationFlags|OutputEnvironment|PrepareOutput|UsageExitStatus|StructuredUsageError|CancelledContext|MacHelp)' \
  -count=1
```

未在本轮执行 VM 创建/删除、宿主安装卸载、网络变更、镜像下载或正式发布。
上述通过只能证明调查基线的相关测试通过，不代表本提案已经实现或经过真机验收。

关键源码：

- [Barn 依赖](../go.mod)、[命令根](../cmd/barn/command_root.go)、
  [参数与补全辅助](../cmd/barn/cli.go)、[输出预处理](../cmd/barn/output.go)。
- [生命周期声明](../cmd/barn/command_lifecycle.go)、[Mac 声明与操作](../cmd/barn/command_mac.go)、
  [当前主入口及编排](../cmd/barn/main.go)、[宿主准备](../cmd/barn/setup.go)。
- [命令契约测试](../cmd/barn/cli_test.go)、[远端边界测试](../cmd/barn/remote_command_test.go)、
  [输出测试](../cmd/barn/output_test.go)、[信号测试](../cmd/barn/signal_test.go)。
- Pig 固定基线的 [root.go](https://github.com/pgsty/pig/blob/56c858ca76d76afd016fba0948805afb93e9efcc/cmd/root.go)、
  [utils.go](https://github.com/pgsty/pig/blob/56c858ca76d76afd016fba0948805afb93e9efcc/cmd/utils.go)、
  [ANCS help](https://github.com/pgsty/pig/blob/56c858ca76d76afd016fba0948805afb93e9efcc/internal/ancs/help.go)。
- Cobra 官方 [局部与 persistent flags](https://cobra.dev/docs/how-to-guides/working-with-flags/)、
  [shell completion](https://cobra.dev/docs/how-to-guides/shell-completion/)，以及
  [v1.10.2 command.go](https://github.com/spf13/cobra/blob/v1.10.2/command.go) 的实际 hook 顺序。
