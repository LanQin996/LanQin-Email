# 维护窗口恢复与回滚（实验性 CLI）

约束已确认：**备份不停服；覆盖恢复允许维护窗口，保留旧数据再切换。** 本工具不停止、启动或检查全部邮件服务，不控制 Docker。`--confirm-maintenance` 是操作者停写声明，不是自动证明。不要对运行中的目录使用它。

已在获准的 Linux `/tmp` 独立目录完成合成数据的真实复制、UID/GID、目录 fsync、不覆盖重命名、错误回滚和公开 CLI 演练，**没有访问或改动线上 data/mail/dkim，也没有停服务**。这不等于真实邮件服务器收发恢复、真实云账号或所有部署拓扑已验收；生产使用前仍须隔离环境演练。

## 1. 取回并验证

按 [交付状态与下载说明](backup-progress.md) 使用 `fetch`，或从自己的云服务取回密文。使用独立可信 SHA-256 收据和离线恢复私钥执行：

```text
./lanqin-backup restore --in /staging/download.age --identity-file /offline/recovery-key.txt --expected-sha256 <trusted-64-hex> --out /staging/recovered
```

成功容器包含 `READY`、`snapshot/data`、`snapshot/mail`、`snapshot/dkim`。当前维护安装只支持带完整 `data/backup-online.json` 的 `online-data` 归档；旧离线归档没有可靠路径映射，不猜测安装。原始解密暂存区保持不变。

不能把 READY 当作可启动服务的许可：它证明归档解密/校验暂存完成，而非 UID/GID、目标路径和所有旧业务状态已经安全。

## 2. 维护与预检

由操作者先正常停止所有相关写入者：API、SMTP/投递、Dovecot IMAP/POP3/LMTP、DKIM 同步、后台任务及共享目录的其他实例，确认没有遗留旧数据库或目录句柄。挂载这些路径的容器必须在切换后重新创建/重挂载，不能让旧进程继续持有旧 bind mount。

准备独立、当前安装操作者所有且私有的工作区和 journal 目录（Unix `0700`）。源暂存、准备区、journal、三个目标根不能相同或互为祖先。目标是现存真实目录，不接受符号链接、文件系统根或挂载点；目标父目录须由安装操作者控制且不可被组/其他用户写入。不同目标可在不同文件系统，工具会先复制到各自同父目录的 sibling，再在该文件系统内重命名。需要 Linux `RENAME_NOREPLACE`；不支持时失败，不退化为覆盖式 rename。

六项 UID/GID 必须按**目标服务的实际运行身份及容器用户映射**显式设置，不能信归档 UID/GID，也不要照搬示例数字。新树默认目录 `0700`、文件 `0600`，不会重放可执行位/特殊权限。若多个服务以不同身份共享 SQLite 等文件，须在隔离验收中核对必要的受控共享访问；私有默认权限不是所有拓扑可直接运行的承诺，不应简单放开为全局可读写。

检查磁盘峰值：原目标、同父目录新树、旧 rollback 树、原始暂存及准备树会同时存在。工具不自动清除旧数据。

## 3. 安装

以下是 Linux 示例；`DATA_UID` 等六个变量必须先按目标部署设置，空值会被拒绝。host target 和 runtime root 不是同一概念：例如宿主机 `/srv/lanqin/data` 可能挂载为容器 `/data`，数据库中应写 `/data/...`，不能写宿主机路径。

```sh
./lanqin-backup install \
  --staged /staging/recovered \
  --workspace /private-restore-work \
  --runtime-data-root /data --runtime-mail-root /var/mail/vhosts \
  --data-target /srv/lanqin/data \
  --mail-target /srv/lanqin/mail \
  --dkim-target /srv/lanqin/dkim \
  --journal-dir /private-restore-journals \
  --data-uid "$DATA_UID" --data-gid "$DATA_GID" \
  --mail-uid "$MAIL_UID" --mail-gid "$MAIL_GID" \
  --dkim-uid "$DKIM_UID" --dkim-gid "$DKIM_GID" \
  --confirm-maintenance
```

安装步骤：

1. 重新核对整个在线清单、文件摘要/大小/身份、范围、私有权限及 SQLite 完整性；拒绝链接、额外文件、未知触发器和不支持状态。
2. 在新私有准备树内重映射附件和邮件的绝对路径；从清单回填空原文路径的重建草稿，保留未索引候选隔离区，恢复记录的 INTERNALDATE。原暂存区不改。
3. 只修改准备副本：撤销会话/登录挑战，禁用 API Token 并保留元数据；取消旧发送和定时发送，本地投递设为 `restore_paused`；未完成 Webhook/Telegram 推送保留载荷但延后到 9999 年；再次移除备份计划/租约/任务/OAuth state。计数和说明保存到 `restore-install-plan.json`。不自动重发任何旧载荷。
4. 按目标的显式身份复制到同父目录新树，设置私有权限；完成复制后才触碰旧树。旧目录改名为 `<target>.rollback-<transaction>`，新树逐一发布。三次切换不是全局原子，所以必须维护停写。
5. 每步使用私有 durable journal 和事务标记；全部目录发布、journal committed 并同步后才清除启动阻止标记。部分失败尽力按已验证身份回滚；未知/篡改状态 fail closed，保留 journal 供人工，不盲删任何旧树或失败树。

输出包含事务、journal、原 live target 参数、实际旧树路径及审查计划位置。失败或中断也可能已有 journal/目录变更；保留回执，不要因非零退出码就盲目重跑。暂存清理/标准输出失败可能发生在切换完成之后，同样应先检查 journal。

解密暂存的 `data/backup-online.json` 和准备树的 `.lanqin-restore-not-activated`、事务中的 `.lanqin-restore-incomplete` 都会阻止**本版本 API**启动。成功安装保留原清单为 `data/restore-source-report.json`，并保留 `data/restore-install-plan.json`，避免准备区清理后丢失暂停状态报告。该保护不能替代停写，也不控制 Postfix/Dovecot 或旧 API。

## 4. 审查后由操作者启动

工具永远不启动服务。启动前阅读安装计划，核对数据库/附件/原文、DKIM 公私钥和读者权限、目录挂载、部署环境和外部主密钥。`.env`、证书、Postfix spool 等未包含的资产要单独恢复。

会话需要重新登录，API Token 已禁用；用户密码和授权配置来自快照，因此应审查账户、共享、转发、SSO 和外部账号。外部账户同步未由安装器暂停，须在目标部署核对/禁用后再逐项恢复。不要直接把所有旧发送任务改回排队，可能重复向外发信；通知和本地投递也要显式审查后处理。未索引候选不自动导入。

由操作者重新挂载并启动受控服务，再检查健康、登录、文件夹、附件读取、收信/发信和 DKIM。在真实收发验收完成前，不应视为生产恢复交付。

## 5. 回滚

回滚也必须再次停止全部写入者，并重新提供**原 live target 参数**，不是 `.rollback-*` sibling 路径：

```sh
./lanqin-backup rollback \
  --journal /private-restore-journals/restore-TRANSACTION.journal \
  --data-target /srv/lanqin/data \
  --mail-target /srv/lanqin/mail \
  --dkim-target /srv/lanqin/dkim \
  --confirm-maintenance
```

journal 不是访问任意路径的授权：工具核对显式目标、事务、所有根的标记和同父目录派生名，预检所有根后才移动。新树被隔离为 `<target>.failed-<transaction>`，旧树恢复到原位置；不会删除新旧数据，重复已完成回滚为安全 no-op。

若新系统已使用后再回滚，回滚前的新邮件仍在保留的 failed 树中，**不会自动合并进旧库**；需另行核对和恢复。手工删除 journal、标记或任一旧树可能破坏可验证回滚能力，不要把这种情况当作强制覆盖重试的理由。

## 本轮验证（2026-10-31）

- Windows 全量 Go 测试及针对性 install/CLI 测试通过；Windows 上公开安装/回滚入口直接拒绝，单元测试模拟 owner 和目录 fsync，未提供生产平台绕过开关。
- 获用户明确授权后，在前述 Linux 服务器新建的独立 `/tmp/lanqin-restore-tests.*` 范围执行整个 install 包测试。真实 ownership、目录 fsync、`RENAME_NOREPLACE`、挂载点拒绝、断裂 journal、发布失败自动回滚及激活标记异常回滚通过；没有访问线上 data/mail/dkim 或调用服务管理命令。
- 另运行真实 Linux CLI 子进程：合成 WAL SQLite/Maildir → create-online → restore → install → rollback → 重复 rollback。验证 runtime 路径、原文与 INTERNALDATE、实际数值 UID/GID、旧发送暂停且 payload 保留、旧树与失败树保留、source 未改及 workspace 清空。未启动邮件服务。
- 最后在 Linux 验证导入副本自身会再次排除备份任务/计划/OAuth state，不仅依赖生产者的清理标志。合成测试结束后已清理该独立测试目录和临时程序。
- 真实用户邮件收发、Postfix 队列恢复、真实云账号授权和完整部署生命周期尚未验收。此验证不授权也未执行生产恢复；自动远端保留策略仍未实现。
