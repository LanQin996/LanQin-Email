# Issue #34 交付状态（2026-11-12）

**尚未完成整个 Issue，不能关闭。当前版本已按维护者明确要求部署到其既有服务器，供维护者自行实测。** 下表区分实现、隔离演练与实际部署观察，避免把阶段完成当作全部完成。

## 2026-11-12：覆盖恢复阶段与断连状态可见性

维护者发起的实际覆盖恢复已由主机 helper 记录为成功，约 77 秒完成，恢复后的应用容器健康。旧弹窗只展示粗粒度 `preparing/maintaining`，API 停服或会话撤销后仍保留最后一次“准备中”徽标，因此页面旧状态不等同主机仍在处理。

本轮补充 helper 持久状态的可选 `progress:{step,startedAt}`，只发布固定阶段及时间；复制校验、停服预检、安装、验证提交、服务启动和健康检查由实际执行路径上报，不推算文件百分比或剩余时间。旧 helper 与历史记录不带此字段时继续使用原 `phase`。状态查询继续要求当前超级管理员会话，不建立无认证状态接口，不回显日志、路径、邮件内容或凭据；断连提示区分最后已知状态和实时状态，维护过程仅重试同一 GET 请求，不重复发起覆盖恢复。

本轮验证：Windows `go test ./...` 全部通过（app 424.614 秒，其余包通过或使用缓存），全量 `go vet ./...` 通过；维护协调器 11 个成功/故障分支核对观察状态与已持久化 journal 一致，API 拒绝未知阶段及越界时间。格式检查仍列出 53 个既有 CRLF 文件，归一化后无实际差异；`git diff --check` 通过。本轮没有数据库结构变化，未重跑双库服务合同。

前端 14 项备份 UI 测试、shadcn、1914 项三语言词条/10 项 i18n 测试及全量 ESLint 通过；完整 `pnpm --dir apps/web run check` 仍在原 23 个未修改文件的 Prettier 阶段中断，清单见 [管理端验证记录](backup-admin.md)。单独 TypeScript/Vite 构建通过。实际 Edge/Chromium 浏览器加载正式源码组件与生产构建 CSS，使用合成 API 验证自动阶段变化、503 最后已知状态、401 重新登录提示、恢复连接后查到成功、终态停止轮询及简繁英切换；1280×900 和 390×844 视口无页面错误，测试没有发送 POST。这是模拟响应的实际浏览器验收，不是连接生产后端重新执行覆盖恢复。

已部署 `lanqin-email:restore-progress-20261112`，镜像 `sha256:cf06fe0bf2e7851b112d4501a65f7fde31ea9aa9449491b9f3828f08645df4f5`。API/CLI、host helper、公开 HTML 与 20 个 JS/CSS 资源摘要一致，readiness、Postfix、Dovecot、Rspamd 和 helper 健康；新 helper 能读取原成功请求，批准计划仍可查询，无会话的维护状态请求返回 401。升级前保留停服数据、配置、原镜像和队列材料，环境及密码保持不变，本轮未再次执行生产覆盖恢复。细阶段只在内存观察，原粗状态及终态继续持久化；维护断连期间无法提供实时查询，界面明确显示最后已知状态，重新登录后继续 GET。

## 2026-11-11：继承镜像健康检查的部署被维护预检查误拒绝

维护者覆盖恢复请求在停服前预检查失败，主机日志为 `preflight_failed`，无安装事务、未停服务、未切换数据，容器仍健康。根因为当前 Compose 继承镜像定义的 HEALTHCHECK，而预检查错误要求 Compose 本身必须重复声明。现允许继承所选镜像的健康检查，并比对实际容器使用的检查命令与 Compose 显式覆盖或镜像默认命令完全一致；显式禁用、NONE、两处均缺失或容器检查被改动继续拒绝，不删除健康检查要求。

新增 all-in-one/stack 共 14 个组合验证继承、显式覆盖及拒绝分支，Windows 定向通过；生产库派生的只读控制器在既有服务器实际完成完整 Preflight，包括固定 Compose/环境摘要、镜像、挂载、容器、配置及 Postfix 账户校验。诊断封锁全部 Stop/Recreate/Install/回滚动作，在新的私有状态目录运行；前后容器 ID 和原失败请求日志不变。Linux maintenance 全测试包亦通过。Windows `go test ./...` 通过（maintenance 30.485 秒，其余使用 Go 缓存），全量 vet 及差异检查通过，格式检查仍为 53 个既有 CRLF 文件、归一化无实际差异。本轮没有前端或数据库结构变化，不重跑前端及双库服务合同。

最终修复版本已部署，API/CLI、公开页面与全部静态资源摘要一致，邮件服务及受限 helper 健康检查通过；更新后的批准计划再次通过真实只读 Preflight。原环境、密码和失败请求不改动，未自动重试生产覆盖恢复。初始镜像保留了同元数据旧 API 文件，最终使用内容摘要命名的构建输入及镜像内校验修正；中间失败验证收据保留，不列为成功。维护者刷新后可关闭旧失败记录，再从原成功验证任务创建新覆盖恢复请求。

## 2026-11-11：Maildir 标记文件名导致恢复验证失败

维护者实测发现密码归档下载成功后恢复验证失败。只读元数据与独立临时目录重放确认：密码配置与提交密码一致、密文收据一致、解密和基础数据库检查成功；恢复准备的路径校验误拒绝标准 Maildir `:2,S` 标记文件名。正式修复只在 mail payload 的合法 Maildir 结构及字母标记后缀允许冒号，复用采集器同一解析器；data/DKIM、路径穿越、任意冒号/ADS、非法标记、链接和 Windows 不可本地化路径仍拒绝。数据库路径重映射明确区分 mail/data，主机 handoff 副本校验同步修正，避免只修第一个入口。

正式源码派生的 Linux 诊断程序已在服务器独立目录对同一归档完成密码解密、收据与报告验证、完整安装准备、认证 handoff 复制；全部 5,909 个冒号路径均属于标准 Maildir 标记，没有跳过 payload/数据库检查。五项新增 Linux 合成回归均通过，覆盖密码 age 往返、实际冒号文件、路径映射、主机复制与恶意路径拒绝。诊断不启动维护切换、不改活动邮件，密码仅经进程内存管道传递，临时明文副本已清理；失败历史记录保持原状态，需要维护者重新提交恢复任务。

最终 Windows `go test ./...` 通过（app 440.891 秒、CLI 5.765 秒、install 14.164 秒、handoff 0.259 秒、online 2.188 秒），全量 vet 通过；Linux 的 online、install、handoff 三个完整测试包亦实际通过。格式检查仍为原 53 个 CRLF 文件，归一化无实际差异，差异检查通过。本轮没有前端或数据库持久化结构变更，未重跑前端检查或双库服务合同；构建包内前端字节与上一部署完全一致。

修复已部署到维护者既有服务器，API/CLI、公开页面与全部静态资源摘要匹配当前包，readiness、邮件服务及受限 helper 健康检查通过；原环境及密码保持不变，升级前保留停服数据、配置、原镜像和队列材料。本轮未执行生产覆盖恢复或改写失败任务，实际管理员重新提交恢复后的页面验收仍由维护者完成。

## 2026-11-11：备份新密码改为 8–128 个字符

新设置或修改备份密码统一使用 8–128 个 Unicode 码点，Web、API 与 CLI 一致；中文和非 BMP 字符不按字节或 UTF-16 单元重复计数。保留精确密码值，不修剪、不归一化，仍拒绝全空白、控制字符及非法 Unicode。前端新密码控件不使用不等价的原生字符长度限制。旧策略下已保存的较短中文密码和较长密码继续可读、可执行备份、可验证旧密码后改密；历史归档恢复保留原长度兼容，不修改任何密码或重新加密历史文件。

针对性测试覆盖 ASCII、中文、emoji 的 7/8/128/129 字符、控制字符、空白、确认、历史 4 字中文和 1024 字节配置读取/改密，以及实际 8 字符 CLI 在线备份到独立目录恢复。Go 全量通过（app 462.510 秒、CLI 7.904 秒，其余未受影响包使用缓存），全量 vet 通过；独立 PostgreSQL/MySQL 配置及原生恢复合同分别通过（app 11.240/15.522 秒），临时数据库均停止。Go 格式检查保留原 53 个 CRLF 文件，归一化无实际格式差异。

Web 完整 check 的 shadcn、1878 项三语/10 项语言测试与全量 ESLint 通过，仍在原 23 个无关文件的 Prettier 检查处中断；修改文件格式、单独 TypeScript/Vite 构建与 10 组源码组件/密码规则测试通过。另用 React renderer 和合成 Query cache/FormData 验证三语首次及修改密码共六种 8 字符提交场景，固定密码模式与提交后清空均通过；替换了弹窗容器，不代表浏览器或真实后台端到端。

新长度策略已部署到维护者既有实例。API/CLI、公开页面及全部当前 JS/CSS 摘要与构建包一致，邮件服务、readiness 和受限 helper 检查通过；原环境配置及密码未修改，未执行生产备份或恢复。保留停服数据、原配置、镜像与队列材料供回退。

## 2026-11-06：后台新备份强制管理员自设密码

后台新建与计划备份不再使用环境公钥，也不提供公钥模式切换。无密码配置或历史 key 配置都返回 password 模式但未设置状态，保留原配置 revision 供管理员迁移；服务器不生成替代密码。立即执行、启用计划及排队采集均由后端验证密码配置，既有公钥计划到期停用。页面最上方提供密码设置，首次设置不索要不存在的旧密码；修改已设置密码仍需当前备份密码及确认新密码。历史归档不会改密，旧公钥归档继续用原私钥恢复，独立 CLI 显式公钥工具保持兼容。

本轮最终 `go test ./...` 通过（app 517.522 秒、CLI 6.427 秒，其余未受影响包使用 Go 测试缓存），全量 `go vet ./...` 通过。新增环境公钥与历史 key 配置不能绕过密码的实际 HTTP/队列/计划边界测试通过；隔离 PostgreSQL/MySQL 实际配置、计划及原生恢复合同分别通过（app 9.856/18.920 秒），自建数据库均已停止。双库原生流程仍含注入云传输和 Windows 模拟目录发布，不等同供应商或 Linux 维护验收。Go 格式检查仍存在原 53 个 CRLF 文件，换行归一化无实际差异，未进行全仓格式化。

完整前端检查的 shadcn、1878 项三语/10 个语言测试及全量 ESLint 通过，仍被原有 23 个无关 Prettier 文件阻断；改动文件格式检查、单独最终 TypeScript/Vite 构建及九组源码 SSR 测试通过。另用真实 React renderer 配合合成 Query cache/FormData 验证三语首次与变更密码共六种表单交互：无公钥选择、首次不需旧密码、固定提交 password 模式、提交后清空。弹窗容器被替换，此证据不是实际浏览器或真实 API 端到端。

强制密码版本已更新到维护者既有实例。API/CLI、公开入口及全部当前静态资源摘要与构建包一致，邮件服务及受限 helper 健康检查通过；原环境文件未改变，未替管理员设置密码、调用云端或执行恢复。升级前保留原配置、镜像、停服数据与队列材料。维护者刷新后在页首设密码，已有旧计划需重新确认；没有声称完成生产管理员浏览器操作验收。

## 2026-11-05：任务详情恢复按钮重复修复

任务详情中的恢复向导与云端发现结果曾使用相同的兄弟组件 key，切换任务时 React 会残留旧恢复按钮。现为两者增加不同前缀，同时保留按任务切换时清空表单状态。使用真实 React 18 renderer、实际任务组件及合成 Query cache 重现：旧版切换后按钮数为 1、3、5 并继续累积；修后连续八次切换均为 1，重复 key 警告为 0。弹窗容器在该组件回归中被替换为透明容器，不是浏览器或真实 API 验收。

本轮 shadcn、三语检查及全量 ESLint 通过；完整 check 仍在原有 23 个无关文件的格式阶段中断，单独最终 TypeScript/Vite 构建、修改文件格式及差异检查通过，六项既有组件 SSR 回归通过。修复只改两个前端 key；部署包内 API/CLI 摘要与上一版一致，未重复运行后端测试。

修复已部署到维护者既有实例，公开入口及全部当前静态资源与构建摘要一致；邮件服务与受限 helper 健康检查通过，未设置备份密码或执行覆盖恢复。真实管理员浏览器操作仍由维护者实测。

## 2026-11-05：执行周期扩展

计划后端已扩展为每日、每小时、每周、每月及固定间隔，兼容原有每日配置。每小时可选分钟，每周可选多个星期，每月可选 1–31 日（不足取月底），固定间隔可按分钟、小时或天设置，最短 5 分钟；不接受秒级周期、任意 cron 或脚本。固定间隔由服务端生成起点，按实际时长推进，日历周期保留 IANA 时区及夏令时处理。

新周期计算、无效参数拒绝、SQLite 持久化、多实例 CAS、同日多轮去重和错过多期只补一轮的定向测试通过（8.092 秒）。隔离 PostgreSQL/MySQL 的加密配置与周期合同均实际通过，连同原生数据库备份恢复合同 app 分别 7.224/11.843 秒，临时数据库已停止；Windows 合同仍有模拟目录发布及注入云端传输，不能替代 Linux 服务维护或供应商验收。地点“立即执行”复用现有快照 API，不要求先启用计划，也不改动计划游标。新增周期已部署供维护者实测，不能沿用旧每日演练作为新小时、周、月或间隔计划的真实供应商验收。

## 2026-11-05：管理员自设备份密码

新增密码模式与既有 age 公钥模式并存：管理员在后台输入并确认备份密码，标准 age scrypt 加密归档，自动任务需要的密码通过现有主密钥加密落库，不回显。更换模式或密码需原备份密码及最新 revision；旧归档不重新加密，仍需当时的历史密码或私钥。

本轮最终 `go test ./...`、`go vet ./...` 通过（app 373.967 秒、CLI 6.596 秒、archive 8.116 秒），包含真实密码归档、错误密码、旧公钥回归、原生格式及独立 CLI 在线备份到另一恢复目录。Go 格式脚本仍报告既有 53 个 CRLF 文件，换行归一化后无实际格式差异。前端三语 1871 项/10 个语言测试、shadcn、ESLint、改动文件格式、最终 TypeScript/Vite 构建及 6 项真实组件 SSR 测试通过；完整 check 仍在原 23 个无关 Prettier 文件中断，未声称完整通过。SSR 使用合成 Query cache，未新增实际浏览器交互验收。

密码、周期及立即执行入口已更新到维护者既有部署，升级前保留停服数据、原配置、镜像和队列材料；原环境配置保留，受限 helper 二进制和批准配置摘要同步更新。未替维护者设置备份密码、启用计划、调用云端或执行覆盖恢复。部署后的邮件容器健康与公开页面资源摘要验证分别记录，维护者仍需完成实际管理员页面操作验收。

跨服务器离线恢复密码归档需密文、该归档的历史密码及独立可信 SHA-256 收据，不依赖原应用主密钥；**自动发现并验证云端旧索引仍需原主密钥及云端访问权限**。备份密码不能替代地点凭据或索引信任，也不保证新实例自动列出全部旧备份。CLI 使用私有 `--password-file`，Web 明确区分管理员登录密码与备份密码，见 [管理说明](backup-admin.md)、[恢复指南](backup-restore.md) 与 [独立发现](backup-discovery.md)。

## 2026-11-04：Drive 客户端可在后台配置

Google Drive 地点授权区新增共享 OAuth 客户端配置弹窗。环境变量未管理客户端时，超级管理员可填写 Client ID 和 Client Secret，加密保存后即时用于授权、回调及刷新，不需要编辑 `.env` 或重启。读取接口不回显密钥；同 ID 留空保留已存密钥，换 ID 必须提供新密钥。任一环境参数非空时仍保持环境优先并锁定后台编辑，避免混用两种来源。配置变更会使旧授权流程和不匹配的刷新结果失效。

新增 CRUD、权限、匿名/API Token 拒绝、版本冲突、环境优先、密钥保留、配置轮换及数据库配置正向 TLS 流程定向测试通过（16.126 秒）；Go 全量通过（app 325.503 秒）及全量 vet 通过。前端 TypeScript/Vite 构建、改动文件格式/lint、54 项三语实际组件 SSR 断言通过；完整前端检查在 shadcn、1812 项翻译/10 个语言测试与全量 ESLint 后，仍被原 23 个无关文件的格式问题阻断。没有新增实际浏览器或 Google 真实授权验收。

已部署该入口并核对对外 HTML、入口和管理页脚本摘要与构建一致；原 `.env` 未更改，helper 批准配置已同步，邮件服务及 helper 基础健康检查通过。未替维护者填写 Google 凭据、请求 Google 授权或执行备份覆盖恢复。

## 2026-11-04：已部署，交由维护者实测

部署收据确认当前版本已替换既有应用镜像，页面和 `/readyz` 返回 200，邮件容器健康，Postfix、Dovecot、Rspamd 及受限主机 helper 的基础检查通过。升级前已保存停服数据副本、原配置、原镜像和队列材料；未配置云端账号、启用自动计划或在现有数据上执行覆盖恢复。恢复私钥未上传服务器，具体服务器地址、凭据和私有回退路径不写入仓库文档。

部署观察是当时的启动检查，不是持续健康监控，也不替代维护者的实际功能验收。按维护者要求暂不扩大测速或隔离测试；实测可先按 [简短清单](backup-user-test.md) 完成上传与隔离解密，覆盖恢复另行明确确认。下文早期“未部署”叙述仅记录对应阶段的历史状态。

## 2026-11-04：真实存储到 Web API 与主机维护闭环通过

第 24 轮全新无 NIC 的 WHPX/Linux ext4 VM 中，一处真实 MinIO S3-compatible 和一处 WsgiDAV/Cheroot 已通过当前生产 HTTP 路由创建地点、手动在线快照上传/完整回读、认证索引、两个独立地点的实际每日计划，以及两种存储各自的下载/age ready 恢复。两个计划实际在 `05:16 UTC` 产生不同成功任务，游标推进到次日；各地点发现两项认证备份。错误 age 私钥失败后，实际 live MIME/INTERNALDATE 未变。

随后真实 HTTP 管理员重新认证，经固定 Unix socket 的 root helper 完成正式维护，host 成功与 core `healthy/committed/needsRecovery=false` 相符。原 Cookie 为 401、旧恢复记录为 404，重新登录仍可查询同一持久 host ID。覆盖后原 MIME、附件和 INTERNALDATE 保持，快照后邮件不在新活动收件箱、旧定时发送取消；新的 SMTP 邮件经 API 附件及 TLS IMAP 读取成功。使用 helper 生成计划显式回滚后，旧邮件恢复，新接受邮件保留在 failed 树。

这次是实际自建存储软件、HTTP API、root helper、Docker 与邮件服务联调，客户端不是浏览器，也没有 Google Drive 或商业云账户。本轮只有 1 个 S3 + 1 个 WebDAV，不满足至少 3 个 S3 + 2 个 WebDAV + Drive 共存的完整真实样例；M2/M4 不能据此勾选完成。没有生产代码变更或新 Go 全量运行，未把此前检查冒称本轮重新执行。来源、复现步骤、输入摘要与剩余边界见 [真实存储演练](backup-real-storage-validation.md)。

## 2026-11-04：隔离的 all-in-one SQLite 真实邮件恢复与回滚通过

在全新独立 ext4 虚拟磁盘上的 WHPX/Linux VM 中，Docker 29.9.2、Compose 5.6.1 和当前工作树派生的 all-in-one 镜像已完成真实 SQLite CLI 闭环：SMTP 25 接收合成 MIME 与 296 字节二进制附件 → API 授权下载及明确测试 CA 验证的 IMAPS 读取 → 不停服在线 age 备份/隔离解密 → 正式维护计划停止、安装、提交、重建并达到 `healthy` → 恢复后实际新邮件收取 → 显式回滚。原文 MIME 摘要与实际 IMAP INTERNALDATE 精确保持，旧定时发送为 `cancelled`；回滚后旧的快照后邮件重新可读，新恢复期间接受的邮件保留在 failed 树。UIDVALIDITY 实际改变，不承诺 UID 稳定。详见 [真实邮件演练](backup-real-mail-validation.md)。

实际停止后的空 Postfix spool 含 `postfix/restart` 空普通文件，原校验器因此拒绝继续且恢复原容器，没有覆盖数据。现仅兼容该固定名称的空普通文件，非空、链接、目录及其他未知条目仍拒绝；修复后的 maintenance 全包（48.745 秒）、CLI 全包（1.568 秒）及全量 vet 已通过。

演练还定位并修复了私有 SQLite 文件使不同 UID 的 Postfix 无法查询的问题：正式计划显式启用 `sqliteGroupRead`，绑定实际 Postfix 主 GID，只给固定数据库及必要 sidecar 组读权限，不改成世界可读、不提升邮件进程权限。独立机制验证证明 API 写事务后 sidecar 保持批准组/权限，Postfix 无写权限、无关 UID 被拒；最后全新演练由正式安装器设置该策略，没有手工 chmod 绕过。此前 TCG 慢启动及离线客体不可达 nameserver 是测试环境问题；换用明确 WHPX 与客体 loopback 解析器声明后完成验证，没有跳过生产健康检查。早期中断/健康失败收据保留为失败证据，不追认为成功。

本次 VM 无 NIC、宿主文件系统共享或宿主端口转发，公开旧发布镜像只作为已校验基础，当前 API/CLI/前端/部署配置另有构建输入摘要。测试结束容器及拥有的 Docker daemon 已停止，VM 正常退出。此结果仅覆盖隔离的 all-in-one、SQLite、CLI 路径；真实云、stack、PostgreSQL/MySQL Docker 协调、Web 点击到 helper 的实际邮件流程、非空队列重导入及生产/断电耐久仍未验收，Issue 保持打开。

| 能力 | 实现状态 | 已验证 / 未验证 |
| --- | --- | --- |
| 多个 S3、WebDAV、Drive 地点 | 已实现，独立配置及加密凭据 | 多实例合成/HTTP 配置测试；真实 1 个 MinIO S3 + 1 个 WebDAV 的手动与定时闭环通过，完整 3S3/2DAV/Drive 真实共存样例未验收 |
| 不停服 SQLite、Maildir、附件、草稿、DKIM 采集 | 已实现，引用闭包而非整盘原子快照 | 真实文件/SQLite、并发变更、CLI 加解密；未做生产 Linux 恢复 |
| 手动任务、取消、历史、阶段、逐地点结果 | 已实现 | 真实 API/SQLite；成功云响应为注入 uploader，错误路径为本地模拟目标 |
| 每地点每日/每小时/每周/每月/固定间隔、IANA 时区及独立启停 | 已实现并部署供实测，兼容旧每日配置 | 新周期定向 SQLite/CAS/DST/月末、双库合同及 Go 全量回归通过；旧每日已有两个真实存储地点的实际 UTC 日程收据，新增周期的真实供应商执行未验收 |
| Drive OAuth、PKCE、一次性 state、刷新 | 已实现，申请 `drive.file` | 本地 TLS token/folder API、串绑/重放/刷新冲突/日志脱敏；真实 Google 同意未验收 |
| Drive 上传与文件校验 | 已实现，预分配 file ID、分块/session 查询和密文回读 | 本地 TLS/实际进程终止与重建会话、ID/目录/大小/摘要校验；真实 Google 待验收 |
| 云端密文下载 `fetch` 与 Web 下载 | 已实现 S3/WebDAV/Drive | MinIO S3/WebDAV 经实际 Web API 下载、age/SQLite ready 恢复通过；Drive 仅本地 TLS 等分层测试，真实 Google 待验收 |
| 无本地历史的云端恢复发现 | 已接通 S3/WebDAV/Drive 认证 sidecar、CLI 以及 Web 后台发现/加密分页/重新认证恢复 | 真实 MinIO/WebDAV 各两项认证发现及 Web 恢复、128 条合成分页、双库元数据契约与三语言 SSR 通过；真实 Google/浏览器待验收，见 [恢复索引](backup-discovery.md) |
| 错误密钥拒绝、非覆盖隔离恢复 | 已实现 | 全流认证、可信收据、越界/链接/资源限制测试 |
| Web 云端列表/恢复向导 | 已接通成功记录选副本、重新认证、后台下载/解密/隔离验证；SQLite 与原生任务可交接维护 CLI，也可向固定 root helper 自动提交批准计划；原生任务可直接导入独立空库 | MinIO/WebDAV 到真实 HTTP API/SQLite/age/主机维护邮件闭环通过；双库 HOST 与跨 UID 另有分层证据；浏览器、Google、stack/原生数据库 Docker 未验收，见 [Web 恢复](backup-web-restore.md) |
| 维护窗口 CLI 三目录覆盖、路径/UID/GID/INTERNALDATE 映射、旧发送暂停、回滚 | 已实现，见 [恢复指南](backup-restore.md) | 合成目录及隔离 all-in-one SQLite 的真实 Docker/SMTP/API 附件/IMAPS 维护与回滚通过；其他部署拓扑及生产恢复未验收 |
| 固定部署计划的维护协调 CLI | 已接通 `maintain-plan`、`maintain`、`maintain-status`、`maintain-recover`、停服证明、数据/环境事务、提交前校验、服务重建与显式恢复；默认要求 Postfix 队列可证为空，可显式启用加密保全 | 最新 Windows 全量、Linux install/maintenance 全包及全新 ext4 VM 的真实 all-in-one SQLite 维护/回滚邮件闭环通过；stack、原生数据库 Docker 协调及断电耐久未验收，见 [协调器](backup-maintenance.md) |
| Web 批准计划到主机 helper 自动交接 | 已接通 root-owned 固定配置、Unix peer UID、重新认证提交、独立源验证/安全复制、root 状态与人工 resolve；默认关闭，不给 API Docker socket | 真实 HTTP API 到 root socket/helper、all-in-one SQLite Docker 覆盖/收发/回滚通过；旧会话与记录失效后持久状态可读；126 项 SSR 非浏览器，其他部署组合未验收，见 [主机交接](backup-maintenance-host.md) |
| 维护窗口队列/显式部署资产加密保全 | 已接通独立 age 格式、当前源二次检查、私有收据、完整回读验证及隔离证据 CLI；非空队列须显式确认隔离，不自动应用资产或重放队列 | Windows 最终全量、实际 age 往返及隔离 Linux maintenance/preservation/CLI 全包通过；真实 Docker spool 捕获、队列导入/重投递及断电耐久未验收，见 [加密保全](backup-preservation.md) |
| 自动保留策略/远端清理 | 已实现，默认关闭、每地点保留 1..100 份 | 合成任务/真实 SQLite、HTTP 与本地 TLS；新旧密文回读和强 ETag 条件删除；真实供应商未验收，见 [保留策略](backup-retention.md) |
| 分片/跨进程断点续传 | 已接通 S3 multipart、Drive 分块及加密检查点；WebDAV 完整响应核对 | 真实进程被终止后由新进程继续，本地 TLS；WebDAV 不支持通用字节追加，未知会话/残留边界见 [续传说明](backup-resume.md) |
| PostgreSQL/MySQL 数据备份恢复 | 已接通实验性同引擎采集/加密/独立空库导入、Web 新建快照，以及 Linux 专用维护安装/激活/回滚 CLI | PostgreSQL 16.15 / MySQL 8.4.13 合成往返和 Web 入队快照通过；Windows 用真实 DB 验证激活核心及模拟目录；Linux 公开维护 CLI 子进程进入 CI 测试、尚无远端运行结果；见 [原生数据库](backup-native-databases.md) |

## 原 Issue 与 M0–M4 完成核对

依据原 [计划草案](https://github.com/LanQin996/LanQin-Email/issues/34#issuecomment-6490101468)、[多地点补充要求](https://github.com/LanQin996/LanQin-Email/issues/34#issuecomment-6490875106) 及后续确认的不停服备份要求，不能只按已通过的 SQLite 样例缩小交付范围：

| 要求 | 代码/实际证据 | 尚缺内容或验收 |
| --- | --- | --- |
| M0 设计、范围、威胁模型和密钥/格式 | 已有公钥备份/离线私钥、认证格式、可信收据/索引、权限与网络策略文档；一致性明确为应用引用闭包 | 当前日常数据归档不含 Postfix spool/全部部署资产，维护保全另有范围；不能宣称完整整站无损快照 |
| M1 SQLite 本地闭环与隔离邮件恢复 | 第 23 轮实际 CLI、Docker、SMTP/API 附件/IMAPS、覆盖及回滚通过；第 24 轮另通过实际 HTTP API 闭环 | 生产部署、非空队列自动重导入及完整部署资产应用未覆盖 |
| M2 WebDAV/S3/Drive 多目标上传下载、重试/续传/撤销 | 多实例与失败隔离、跨进程续传有合成/本地 TLS 回归；真实 MinIO/WebDAV 手动、日程、索引和两种下载通过 | Google OAuth/Drive 真实授权、刷新失效/撤销/重授权与大文件续传未验；3S3+2DAV+Drive 完整真实共存样例及商业云差异未验 |
| M3 受控执行器、UI、日程、保留、通知和审计 | 有界任务/租约、历史、取消、保留策略、续传审计和 root 状态；两实际日程及 API 重新认证覆盖恢复通过 | 独立连接测试 UI/API 和备份失败主动通知未实现；全面操作审计尚需核对，任务错误/历史不代替通知；保留清理真实供应商、浏览器/三语交互完整验收仍缺 |
| M4 PostgreSQL/MySQL 与 all-in-one/stack 矩阵 | 同引擎原生代码及实际双库 Windows 核心/空库导入合同通过；all-in-one SQLite Docker 已验 | stack、PostgreSQL/MySQL Linux Docker 维护/真实收发全矩阵未验，不能一并标为支持已交付 |
| 统一流程中的压缩 | 当前实际格式是 `age(tar(...))`，未启用压缩 | 压缩编解码与兼容性仍未实现；不能写成已完成“压缩加密” |
| 整站范围、密钥依赖与队列 | data/mail/dkim 数据归档；显式部署资产/队列有维护加密保全与隔离恢复，原发送状态暂停 | 完整日常范围、缺主密钥能力提示和非空队列导入/去重/重投递仍须继续；远程 IMAP 上游邮件不在本地归档内 |

以上保留实现、合成测试和真实服务验收的区别，不给任何里程碑补画完成勾选；原任务继续推进。

## 原生数据库快照与维护安装（2026-11-03）

在原先 [PostgreSQL/MySQL 数据采集及隔离导入](backup-native-databases.md) 基础上，接通在线捕获 worker 的原生版本归档、原生 staged 文件验证、数据库路径映射/旧任务暂停，以及 `install-native`、`activate-native`、`rollback-native`。新数据库未激活时 App 拒绝启动；激活记录与安装 plan、journal、数据库驱动及实际 runtime roots 绑定；切换和回滚都要求 Linux/显式维护确认，不启停服务、不改部署 DSN、不清除旧/失败目录。Web 云恢复仍只下载、解密、验证并隔离暂存，不通过 Web 覆盖生产。

原生 `import-native` 与 `install-native` 支持 `--web-job/--web-state-dsn-env/--web-restore-root`，从原实例加密任务记录取得已认证暂存，复制后重新核对报告和 payload；不需再次传私钥。导入要求独立空库、保留未激活 guard，安装必须显式维护确认。原状态库和新目标库的 DSN 环境变量名称/值不得相同，操作员须保证实际数据库隔离。

已在本机准备独立 PostgreSQL 16 / MySQL 8.4 实例，原元数据契约和原生数据/公开 CLI 往返均实际通过并关闭测试进程；历史记录中的“缺少 DSN”描述是当时状态，不代表本轮仍未执行。真实供应商、Linux 全站收发恢复和 Actions 容器编排仍未验收。

## 本轮续作：远端保留策略

本轮另新增默认关闭的按份数 [远端保留策略](backup-retention.md) 及三语言管理界面。上传收据先落库，新密文完整回读成功后，才对旧密文回读并携带强 ETag 条件删除；失败地点不清理，取消/租约/配置变更阻断后续删除。旧任务保留 `pending/deleted/missing` 状态和来源任务，清理错误独立报告，不覆盖上传结果。

**Google Drive/商业云实际账户、完整实例与部署矩阵、浏览器及生产验收仍未完成，Issue 继续保持未完成。** Web 自动交接预批准主机计划已在真实 MinIO/WebDAV、HTTP API、root helper 和 all-in-one SQLite 邮件服务路径通过；不是浏览器操作。独立认证索引不证明远端未删数据、列表完整或最新。本机隔离 PostgreSQL/MySQL 已用于真实数据库/逻辑数据和 Web 捕获测试，但 Linux 原生数据库与 stack Docker 邮件维护仍缺实际结果；本次 1S3+1DAV 也不能替代原要求的 3S3+2DAV+Drive 样例。

## 本轮续作：维护协调 CLI

新增 [部署计划与协调器](backup-maintenance.md) 及故意不能原样执行的 [计划模板](../deploy/maintenance-plan.example.json)。配置、六项身份、完整输入摘要和项目锁绑定明确目标；停服证明后才允许安装，首次服务启动前先持久提交。提交前失败可安全恢复旧树/旧容器；提交后可能已有新邮件，启动或健康失败只停止并保留数据，必须显式恢复，不自动把新数据覆盖回旧快照。

原生环境使用独立、固定摘要的完整 next env 与容器端数据库身份；在停服状态切换环境后，用无端口/无 worker 的只读 `verify-native` 验证实际容器数据库与安装 plan。SQLite 提交前检查完整 payload、INTERNALDATE、报告摘要及改写后的数据库 SHA，提交后恢复只校验当前数据与不可变 plan 配对，不重放旧归档。

所有强制重建路径都先检查 Postfix spool；默认要求可证为空，可显式启用 [队列与部署资产加密保全](backup-preservation.md)。保全当前 spool 和固定摘要资产后，仍须完整解密核对预期 manifest，并二次检查停止状态和源快照；验证失败保留停止的容器。准备失败且原数据未变或已验证回滚时只启动同一原容器。保全仅提供隔离证据，不重放队列或应用部署资产；Web 主机 helper 已实验性接通，自动队列恢复及真实部署验收仍未完成，不将此能力缩写成完整整站灾备支持。

本轮保全包、CLI 和维护 gate 已通过最终 Windows 全量及隔离 Linux 包级验证。启用保全时，固定 Compose/环境文件中的 age 恢复私钥标记会在任何控制器预检前拒绝，恢复私钥环境变量也不会传给 Docker。不同阶段可产生独立 bundle，须保留同一操作全部密文/收据，不能只用最后一份代替原环境或先前队列。

普通安装器仍要求恢复暂存树和私有 workspace 由**实际安装操作者的 UID** 所有，Unix 权限仅属主可访问。非 root API 创建的暂存不能直接交给不同 UID 的 root 安装器；路径映射不改变此检查。新主机 helper 对批准 API UID 的源树进行独立验证、受限复制和复制后复验，建立新的 root-owned 输入再调用原安装器；没有放宽 `materializePrivateInfo`，不以 root 可读取或更宽权限冒充可信输入。

## 本轮续作：Web 主机维护自动交接

新增 [主机 helper](backup-maintenance-host.md)：API 默认不联系任何 socket，部署者明确配置固定 Unix socket 与 root-owned 批准模板后，真实超级管理员可从成功 ready 的恢复任务再认证，提交计划 ID/模板摘要/任务引用。helper 再次核验当前管理员、加密任务、报告和批准源根，复制为 root-owned 独立输入，必要时导入新空库，再按固定协调计划停服、验证、切换和健康恢复；Web 不接收本地路径、命令、UID/DSN 或私钥。

helper 用持久 root 记录追踪状态；重启遇到未完成阶段一律隔离为 `needsRecovery`，不自动重放。相同请求与同一计划/恢复任务不得换 nonce 重投。API 失联后可用预计算 ID 查询，覆盖撤销会话后须重新登录。人工恢复须使用原生成计划，停 daemon 取得 host 锁后，再用 `maintenance-host-resolve` 验证精确状态解除隔离；不会通过 resolve 再次控制服务或仅凭 `READY` 判为安全。

## 本轮续作：认证云端恢复索引

新增 [恢复索引](backup-discovery.md)：成功密文完整回读后，发布独立用途 HKDF/HMAC 认证的不可覆盖 sidecar 并回读。原地点 ID 与物理地点绑定保留，跨新实例配置 ID 可以发现旧收据；Drive 媒体仍核对原 ID 与 folder。索引失败不丢失成功密文收据，任务为部分完成且不触发该地点旧备份清理。CLI 支持显式 `publish-index` 重试，以及 `discover` → 认证 bundle → `fetch` → 原有 age `restore`，不依赖旧 DB 历史。发现只证明观察到合法历史收据，不证明远端未删数据、列表完整或最新。

管理端现可从地点直接创建 `kind=discover` 任务，复用共享队列、租约、取消及中断处理。128 条结果按 32 条独立加密分页，只有成功且 ready 后可读取；恢复只接受条目 ID，并把源任务、所选页、地点与租约作 CAS 绑定。Drive 可明确读取当前授权可见的原文件夹，下载使用认证原地点 ID/folder 与当前凭据。发现不污染最近上传成功、不触发清理，运行态分页不进入导出或安装副本。

内网 NAS/MinIO 精确白名单已接通部署变量 `LANQIN_BACKUP_NETWORK_ALLOWLIST_JSON`，仅支持明确 host/port 与 RFC1918/ULA IP，默认仍只允许公网 HTTPS。上传、下载、保留和 S3 会话清理使用当前运行期策略，Drive 不采纳该策略；CLI 必须显式 `--network-policy-env`。不放宽 TLS、重定向或云元数据地址规则，详见 [网络策略](backup-storage.md#明确授权内网-nas)。精确绑定、全 DNS 答案、撤销、默认拒绝及本地 TLS 协议回归通过；测试网络适配将合成内网 IP 转接到 loopback fixture，未用真实 NAS 联调。

## 本轮续作：分片与跨进程继续

新增 [加密检查点与继续上传](backup-resume.md)：应用检查点与进度受原租约 CAS 保护；CLI 文件检查点及 namespace 索引加密，使用 OS 独占锁。S3 原 upload ID 查询部件、条件完成并回读，Drive 原 file ID/session/服务端偏移续传，WebDAV 核对同名完整对象而不覆盖部分内容。明确取消尝试关闭已知 S3 分片会话，退出保留状态；成功地点不重传，已成功副本不删除。上传运行态在导出和安装副本中被再次排除。

## 本轮续作：Web 恢复验证与维护交接

新增 [Web 恢复向导](backup-web-restore.md)：从已确认上传结果选择副本，管理员密码/TOTP 重新认证、限流，私钥只传给当前实例内存队列；与备份/保留清理共用租约。后台下载、同流可信摘要与 age 认证、数据库格式和完整在线清单验证，失败不触碰 live；成功仅发布隔离 ready。SQLite 维护安装使用 `--web-state-db`，原生空库导入及安装使用 `--web-state-dsn-env`；都与 `--web-job/--web-restore-root` 绑定认证任务，并在独立副本重新核对报告与 payload。没有给 API Docker socket、服务管理权限或任意本地路径参数。

## 备份计划的行为

- 每地点选择执行周期及 IANA 时区；每日、每周及每月设置 `HH:MM`，每小时设置分钟，固定间隔设置数量及单位。开启或编辑后从下一次未来触发开始；固定间隔首次在保存时刻加上间隔后触发。计划关闭不影响手动备份，也不取消已经入队的任务；需要取消时在任务列表明确取消。
- 固定一个 30 秒扫描 worker，只入队，不在定时器里执行采集/网络。使用同一个容量 16 的后台队列，满队列不消费触发点，后续扫描重试。
- 计划游标、任务及队列在同一 DB 事务 CAS 提交；请求 ID 按计划代、地点和触发点的 UTC 时间确定，多实例并发不重复创建，同日多轮不会被按日去重吞掉。错过多期只补最近一轮，不回放整个历史。
- 日历周期在 DST 重复小时取最早实际时间；缺失时间取当天第一个不早于目标的实际分钟，相同实际触发点去重。每月日期不足取月底；完全不存在的本地日期跳过。固定间隔不受 DST 影响，`days` 为每 24 小时而非下一个本地日历日。计划变更生成新代，明确改变的计划不是旧计划重复任务。
- 地点删除、停用、凭据授权代或公钥变化，到期时禁用旧计划并记录安全错误，不自动改为新的上传目标。正常 Drive 短期令牌刷新不改变授权代，因此不会误禁用计划。
- 在线导出的私有数据库副本排除备份计划、任务/租约及未完成 OAuth state，源库不变。安装时原生目标库的可重放发送/本地投递会被暂停或取消，session/challenge 撤销、token/外部账户禁用、待通知保持；服务恢复后由操作员显式复核。

## Google Drive 的部署要求

HTTPS 站点根仍由 `LANQIN_PUBLIC_BASE_URL` 指定。未配置环境变量时，超级管理员可直接在备份地点的 Google Drive 授权区点击「配置 OAuth 客户端」，填写 Client ID 和 Client Secret；保存后即时用于授权、回调及令牌刷新，不需要重启或修改 `.env`。客户端配置适用于全部 Drive 地点，修改 ID 或密钥后须重新授权受影响地点。在自己的 Google OAuth Web 客户端注册界面显示的回调地址：

```text
https://your-mail.example/api/admin/backups/drive/oauth/callback
```

实例还需原有 `LANQIN_BACKUP_SECRET_KEY` 加密凭据库；后台填写的客户端 ID 和密钥也进入加密配置，读取接口仅返回 ID、是否已有密钥及配置版本，不回显密钥。编辑同一 ID 时密钥留空表示保留，换 ID 时必须填写新密钥。客户端配置本身是凭据，不属于待重放任务；备份恢复仍需要原服务器加密主密钥解读它。

环境变量 `LANQIN_BACKUP_DRIVE_CLIENT_ID`、`LANQIN_BACKUP_DRIVE_CLIENT_SECRET` 继续优先。只要其中任一项非空，后台入口就只读；不能把一半环境配置与数据库中另一半拼接。完整环境配置可继续沿用，部分缺失需在服务器补齐或移除后使用后台配置。更改客户端会令未完成的旧授权流程失效，旧令牌不会被混用到新客户端。

界面不接收网盘密码或 refresh token；客户端尚不可用时授权按钮关闭。新建地点后点击授权，由 Google 同意页完成；每次授权创建独立应用备份文件夹，重新授权不删除旧目录，也不自动导入旧账户内容。

授权采用管理员/地点/配置代绑定、十分钟单次 state、PKCE S256 和离线授权；令牌进入加密凭据库，授权码、state、session URL 和 provider body 不返回日志/错误。每个 Drive 上传任务前刷新一次令牌，刷新 CAS 合并无关地点更新，不用新授权的 token 配旧文件夹。

**反向代理也必须对该 callback 去掉查询参数日志。** 应用已脱敏自己的请求日志，但无法控制 Nginx/CDN 等外层记录 `$request` / `$request_uri`。callback 日志宜只记录 `$uri`；不得把完整 code/state URL 收集进访问日志或错误上报。

Drive 使用固定 Google HTTPS origins、证书和公网地址验证，拒绝代理环境和重定向。检查点模式将 session URL 加密持久化，严格限制固定 origin/path，不向 UI/报告/日志回显；原 CLI 非检查点模式仍保留旧行为。继续能力与失败边界见 [续传说明](backup-resume.md)；上传/密文回读成功仍不是解密恢复验收。[Google 上传文档](https://developers.google.com/workspace/drive/api/guides/manage-uploads)、[OAuth 服务端流程](https://developers.google.com/identity/protocols/oauth2/web-server)。

## 云端下载到隔离恢复

从独立可信位置保管的上传报告取得地点、object key（Drive 为文件 ID）及密文 SHA-256，配置文件仍只放凭据环境变量引用：

```text
./tmp/lanqin-backup fetch --config /secure/destinations.json --destination r2 --object-key lanqin/lanqin-0123456789abcdef0123456789abcdef.age --expected-sha256 <trusted-64-hex> --out /staging/download.age
./tmp/lanqin-backup restore --in /staging/download.age --identity-file /offline/recovery-key.txt --expected-sha256 <trusted-64-hex> --out /staging/new-recovery
```

输出必须不存在。默认密文上限 1 GiB，可显式调至单 PUT 上限 5 GiB；下载有时限，不自动重试，失败只清除本次临时密文。停用地点可以显式读取，不会被自动启用。S3/WebDAV 只接受本应用生成的对象名及选定前缀；Drive 先验证父目录、地点和可信摘要元数据，再下载媒体。不会遍历或读取不相关云文件，不使用默认 AWS 凭据链。解密后可按恢复指南使用明确维护确认的 `install/rollback`；不是 Web 一键覆盖，也不自动启动服务。

独立 CLI 的 Drive 配置为 `type: "drive"`、`drive: {"folder_id":"<bound-folder>","access_token_env":"BACKUP_DRIVE_ACCESS"}`。该短期令牌须由操作者显式提供；CLI fetch 不读取应用加密凭据库，也不自动启动 OAuth 流程。不要把 bearer token 放入命令行或配置 JSON。

`fetch` 校验同一条下载流的可信 SHA-256 和 age 格式头，不持有恢复私钥、不做完整解密认证；第二条 `restore` 才校验 age 全流认证。文件和收据若都来自同一不可信存储，不构成来源认证。下载/暂存成功不表示已覆盖生产，旧发送任务、绝对路径、权限、IMAP 元数据、未索引候选与回滚还需人工/后续受控恢复流程处理。

## 本轮验证记录

- 2026-11-04 第 24 轮没有生产源码变更或新 Go 全量运行。新空磁盘的真实 MinIO/WsgiDAV、HTTP API、两处实际 UTC 日程、认证发现、两种下载/age 暂存、错误私钥/live 不变、root helper 正式维护、旧 Cookie 401/旧任务 404、重新登录后的持久状态、邮件内容/附件/INTERNALDATE、新 SMTP 与正式回滚均通过，收据 `success:true`，客体结果 0、VM 正常退出。它证明自建存储软件联调，不证明 Google/商业云账号、浏览器或全部实例/部署组合。来源与证据见 [真实存储演练](backup-real-storage-validation.md)；下文 Go/SSR 检查保持原执行时间和范围。
- 2026-11-04 SQLite 服务组策略整合后的 Windows `go test ./... -count=1 -timeout=10m` exit 0：app 404.350 秒、install 19.930 秒、maintenance 100.265 秒、CLI 1.821 秒、storage 13.929 秒、maintenancehost 1.448 秒。之后主 GID 预检的小修通过定向测试（0.547 秒）和下述最新 Linux 全包；最新全量 vet 通过，gofmt 仍为 53 个既有 CRLF 文件、归一化后无实际差异。
- 全新真实 Docker 第 23 轮同时执行最新 Linux install、maintenance 全包，均 exit 0，并完成上述 all-in-one SQLite 的实际备份、维护覆盖、新邮件收取及回滚闭环。结果收据为 `success:true`、维护 `healthy/committed/needsRecovery=false`、回滚 `rollbackCompleted/originalServicesRestored=true` 且 `needsRecovery=false`；进程正常退出。公开旧基础镜像与当前构建覆盖的来源、摘要和精确命令分别保留，未把旧镜像当成当前源码，也未从中断 journal 推断成功。具体证据和复现前提见 [真实邮件演练](backup-real-mail-validation.md)。
- 本轮再次尝试浏览器检查时，Computer Use 因无法可靠确认 URL 而停止，没有新增浏览器断言；临时 Vite 进程已停止、对应端口已不监听。下文 126 项 SSR 是此前合成组件验证，不升级为浏览器交互证据。
- 本轮 Web helper 整合 Windows 最终 `go test ./... -count=1 -timeout=10m` exit 0：app 423.088 秒、maintenance 58.794 秒、handoff 0.649 秒、maintenancehost 2.382 秒、`cmd/backup` 2.935 秒、storage 15.040 秒、install 19.191 秒。全量后的 resolve 私有阶段放弃补丁通过 host 2.072 秒/CLI 1.971 秒定向复验；最新全量 `go vet ./...` 通过，gofmt 仍为原 53 个 CRLF 文件、换行归一化无实际差异。这是全量与后续受影响包分别验证，不将旧全量当作最后补丁已运行。
- 本轮隔离 Linux VM 第八轮 maintenancehost 全包 3.50 秒、CLI 全包 4.77 秒通过，实际执行 root Unix socket、拒绝非批准 UID 1001 客户端、客户端拒绝非 root 假 helper，以及取消时等待 HTTP handler/worker 清理后再释放主机锁。第九轮 handoff 全包 0.68 秒通过，包含 UID 1001 源树到 root-owned 副本；修正合成私有祖先目录的 `0700` 夹具后复验，未放宽生产权限检查。第十轮最新 resolve/host 全包 3.59 秒通过，用真实协调器 `Run` 和模拟服务动作生成合法终态日志，覆盖精确哈希、无 journal 可放弃分支及未知阶段拒绝，未重新执行真实服务控制。全部 VM 已终止。
- 本轮真实 PostgreSQL/MySQL HOST 正向合同分别通过（19.449/13.764 秒），测试数据库已停止。测试验证原生认证恢复任务、当前管理员身份、独立复制、复制后 payload 校验、独立空库导入及未激活 guard，包含篡改在建表前拒绝、非空库拒绝和源暂存保留；这是 Windows 本机数据库合同，不是 Linux 原生数据库/Docker/真实 SMTP/IMAP 协调。各层成功不等同一次 Web 点击到真实邮件部署覆盖恢复已完成。
- 保全集成最终 `go test ./... -count=1 -timeout=10m` 全量 exit 0：app 413.484 秒、maintenance 96.129 秒、preservation 0.371 秒、storage 16.090 秒、`cmd/backup` 2.418 秒、install 20.297 秒；全量 `go vet ./...` exit 0。额外资产、显式隔离确认、错钥/篡改、冷复用/中断采用、变化快照、资源限制和实际 age/opaque 隔离恢复均进入测试。
- 本轮 Linux VM 第五轮实际执行 maintenance、preservation、`cmd/backup` 全包，均 exit 0（分别 35.13、1.06、4.50 秒）。之后把自动 Compose/环境文件的 age 私钥拒绝前移，再重编并实际执行第六轮 maintenance 全包，exit 0，包含跨 64 KiB 标记和控制器预检前拒绝矩阵。第六轮未采集整包 wallclock，不以逐用例耗时合计冒充包时长。Unix 私有权限、恢复目录被替换时不误删、Docker 白名单环境变量别名不泄露私钥均实际通过；所有 VM 已自然退出。
- 首轮全量曾因模拟 Drive 重复索引的过期 `Size` 失败：RFC3339Nano 时间精度改变 JSON 长度，夹具仍沿用旧大小。已用固定不同精度时间稳定重现并修正夹具大小，增加大小不符拒绝和保留最早认证时间断言；定向连续 20 次及上述最终全量通过，未放宽生产校验。
- 上一轮维护协调整合的全量曾通过：app 包 421.709 秒、maintenance 包 47.532 秒、install 包 22.384 秒。
- 上一轮镜像身份/运行渲染补丁的受影响包复验也通过：maintenance 包 28.115 秒、`cmd/backup` 包 1.627 秒、install 包 14.474 秒；该记录保留为之前阶段的验证。
- 新增实际 Linux 运行验证：官方 QEMU 11.1 与 Alpine virt 内核，在无 NIC、无宿主挂载、1536 MiB RAM 的临时 VM 运行交叉编译测试程序。maintenance、install 和 `cmd/backup` 全包均 exit 0，无 FAIL/panic；最新 CLI 全包实际耗时 3.95 秒。`TestMaintenanceLinuxRealSQLiteActionsNeverReplayCommittedData` 单用例也通过（0.92 秒）。两处合成测试夹具（第二个 stateDir、上传检查点根）原先缺少显式 `0700`，分别修正权限后重跑受影响包；未放宽实现的私有属主/权限检查。修后 Windows CLI 全包亦通过（1.454 秒），全量 vet 通过；未重复其他已通过包或整个仓库全量。
- 上述 VM 执行了真实 Linux UID/属主检查、目录 rename 与 fsync 调用；guest 使用 RAM rootfs/tmpfs，没有块设备，不能证明块设备刷新或断电后的数据耐久性。Compose controller/runner 仍为模拟，没有 Docker daemon、真实 SMTP/IMAP 收发，也没有 Linux 原生 PostgreSQL/MySQL 服务或完整原生维护协调。临时 VM 已自然退出，未挂载或修改宿主部署数据。
- 两个独立 PostgreSQL 16.15、MySQL 8.4.13 原生/元数据合同脚本均 exit 0，包含发现队列、认证加密分页和原生运行态页排除；源记录保留。这是 Windows 本机隔离数据库与公开导入 CLI 验证，不是 Docker 部署或 Linux 维护协调端到端。新实例无上传历史/无本地 catalog 的三存储发现→密码重认证→实际 age/SQLite/安装器暂存验证，以及草稿/二进制附件闭环已通过；App 网络为注入，协议另用本地 TLS，不当作真实供应商端到端。
- Linux 公开 Web 导入/维护 CLI 生命周期已加入测试分支，尚无 CI 运行结果；Windows 不绕过生产平台限制。PostgreSQL/MySQL 公开 Web `import-native`、容器到宿主路径映射及既有原生合同仍通过，临时数据库均已停止。
- 之前修正的索引失败继续能力已包含在本轮全量中：所有目标密文都成功时不再提供无效的继续上传；mixed 任务只继续未成功的密文，保留原索引失败与 partial 状态。真实 HTTP 409/202、DTO、检查点与旧成功收据保全回归通过。
- 可选 Compose 备份挂载及镜像中的 `lanqin-backup` 已接通；固定 `compose-go v2.4.9` 使用独立合成环境文件检查 12 个默认/构建/数据库组合通过，未读取或覆盖 `deploy/.env`。CI YAML 的 actionlint 与新增 Bash 片段的 shfmt 检查通过；没有本机 Docker CLI/镜像构建或远端 Actions 运行结果。
- 本轮 Web helper UI 的完整 `pnpm --dir apps/web run check` 中 shadcn、i18n（当时 1791 项、10 个测试）与全量 ESLint 通过，仍被原 23 个文件的 Prettier 阶段阻断；单独构建、改动文件格式/lint 均通过，没有全仓格式化。随后补上仅成功/失败终态可关闭本地记录的入口，未知结果或需人工恢复时仍保留编号；最新 i18n 1792 项/10 个测试、shadcn、改动文件格式/lint 及 TypeScript/Vite 构建均通过。维护表单/计划加载/断连状态/未知结果/重新登录提示和终态收起等三语言实际 React SSR 共 126 断言通过，使用合成 Query cache，不是实际浏览器或真实 API/停服交互；此前独立发现的 60 项 SSR 仅为上一阶段证据。失败文件清单见 [管理端验证记录](backup-admin.md)。Computer Use 曾因无法可靠识别 Windows 浏览器 URL 而停止；390px/Portal/键盘等实际浏览器验收仍缺失。已停止的 task-owned 预览源码可逆归档为 `.tsx.txt`。
- 本地真实 API/SQLite/生产前端保存每日 `23:58 Asia/Shanghai` 并显示正确下次触发；验证 Drive 地点无需手填 token，缺 OAuth 客户端时按钮禁用并明确提示。时间控件提交改为读取命名 FormData 字段，修复原生时间值更新时保存旧值的问题。浏览器未进行真实 Google 同意。
- 真实本地 TLS 测试覆盖 Drive 多目标隔离、回调到新目录绑定、刷新令牌轮换/CAS、错误授权不阻断 S3、恶意 session URL/元数据/校验值拒绝；日志测试证明 callback code/state/provider error 不入应用请求日志，handler 仍收到真实参数。
- 修复并回归了初次指纹检查与令牌刷新之间的重新授权竞态：worker 将排队指纹传给刷新函数，新授权不得与旧文件夹混用，测试确认不匹配时零 token 网络请求。
- 三种存储真实本地 TLS 下载到实际 age 解密/SQLite/邮件暂存往返通过；S3 大于 64 KiB 的归档未被错误响应上限截断。错误摘要、越界对象、Drive 父目录或归属错误、取消、超限和已有输出均拒绝。
- 假时钟驱动真实持久化队列验证每日去重、满队列不消费、错过多天只补一轮、DST、配置换钥及多实例 CAS。PostgreSQL/MySQL 元数据合同本轮已用独立实例实际运行通过，早期“无 DSN 跳过”记录不再代表当前状态。
- Linux `CGO_ENABLED=0` API/backup CLI 构建，以及 `cmd/backup`、maintenance 的 `go test -c` 交叉编译均 exit 0；编译证据与上文的 VM 实际执行范围分别记录，不能替代 Docker/邮件服务协调验收。gofmt 脚本仍列出 53 个既有 CRLF 文件，换行归一化后无实际格式差异；`git diff --check` 通过。临时 UI 服务已关闭，未提交、推送或部署，未改动线上服务。此记录不构成真实供应商或生产恢复验收，Issue 保持打开。
