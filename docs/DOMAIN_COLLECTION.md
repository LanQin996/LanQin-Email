# 站内跨域统一收件

管理员在「域名管理 → 统一收件」选择本服务器的有效邮箱。默认关闭，固定保留原邮箱；不存在的地址进入统一目标。目标允许跨域，但不能是站外地址、停用邮箱、停用用户/域名下的邮箱，或具有转出别名的地址。修改目标、开启和关闭均记录操作者和前后目标 ID。

生成的副本与未知地址兜底不再次执行目标域名规则。正常直接投递目标域名仍执行该域名规则。同一次发送按最终邮箱去重，支持别名和加号地址。只影响新收件，不回填历史，不接管外部 IMAP 同步。

## 接口与权限

管理接口位于 `/api/admin`；相同能力提供于 `/api/open/v1`（兼容 `/api/open`）。必须为管理员且具有对应域名权限；Open API 还需要 `domains:read` / `domains:write` scope。

| 方法 | 相对路径 | 内容 |
| --- | --- | --- |
| GET | `/domains/{id}/collection` | `{ "enabled": false, "targetMailboxId": "" }`，未配置即关闭 |
| POST | `/domains/{id}/collection` | `{ "enabled": true, "targetMailboxId": "mbx_..." }`；关闭时传 `enabled:false` |
| GET | `/domain-collection-targets` | `{ "items": [{ "id": "mbx_...", "address": "archive@local.test" }] }`，全部有效站内邮箱 |
| GET | `/local-delivery-jobs` | 最近 100 个开发模式本地任务的状态、尝试次数、原因代码；不返回邮件内容 |

读操作需要域名查看权限，修改需要域名更新权限。非法目标返回 400；引用中的邮箱删除、停用或转移，以及目标用户/域名停用、删除返回 409；必须先更换或解除配置。管理端及 Open API 采用同一检查。域名原有状态接口不承载此配置。

数据库 `domain_collections` 按来源域名保存唯一目标 ID；`domain_collection_audit` 保存变更审计。禁用删除配置行，不删除已投递邮件。外部数据库升级至 schema v12；SQLite 启动时增量创建相应表。

## 投递与故障

- **生产 SMTP**：Postfix `recipient_bcc_maps` 对原始信封收件人添加隐藏副本，`virtual_alias_maps` 处理地址别名和未知地址兜底。BCC 和别名扩展不重新提交 SMTP，因此不级联。原来的 To/Cc 邮件头不增加可见目标。
- `virtual_alias_domains` 显式留空，域名归属由 `virtual_mailbox_domains` 判断；地址别名和统一收件仍由 `virtual_alias_maps` 处理。统一收件 SQL 仅匹配具有非空本地部分的完整地址，纯域名及 `@域名` 查询不返回目标，避免 Postfix 将邮箱域误判为别名域。
- `enable_original_recipient=no` 使 Postfix 按最终地址去重；SQL 全地址查询将加号地址归一到邮箱，避免同一邮箱被不同扩展重复投递。这也意味着不再依赖 Postfix 自动增加的 X-Original-To 头来区分别名，加号地址不再用于 Dovecot 的明细子文件夹投递。未配置域名的未知收件地址没有被重写，仍走原有无人收件路径。
- SMTP 已配置时 Go 不提前本地入箱。邮件服务投递后由 Maildir 同步导入，导入不生成统一副本；各收件邮箱继续执行各自过滤规则。SMTP 阶段拒收不会留下 Go 提前生成的副本。
- **没有 SMTP 的开发模式**：先解析原始收件人，按邮箱 ID 去重后创建持久化任务。每个目标独立处理，满额或临时故障不会阻止其他目标。租约 5 分钟；每 5 秒扫描；最多尝试 8 次，退避从 60 秒开始，最高 64 分钟。以内部发送记录和目标邮箱唯一约束防重，保存已创建邮件 ID，恢复时不重复插入，Maildir 文件名也固定。
- 本地任务最终成功或失败即移除正文和附件快照；元数据保留 30 天，失败原因仅包含安全代码。原件与副本应用同样的独立任务机制。
- 生产重试和最终状态由 Postfix 队列及邮件日志负责，**不是上述本地任务列表**。自动 BCC 使用 `NOTIFY=NONE`，目标失败不会向原始发件人暴露隐藏收件地址；应通过 Postfix 日志监控 deferred/bounced/expired，并按部署日志保留策略留档。未知地址的正常兜底收件仍遵循原始投递的 DSN 行为。

## 部署与回滚

1. 备份数据库和邮件服务配置。先部署新 API，等待兼容迁移成功；所有规则保持关闭。不要让引用新表的 Postfix 映射先于迁移上线。
2. 更新 Postfix 镜像/配置，包含 `sqlite-collection-bcc.cf`、三种数据库对应的 `configure-db.sh` 映射和 `main.cf`。使用现有部署环境文件，不覆盖 `deploy/.env`；确认数据库映射查询成功后 reload。
3. 在隔离 Postfix/Dovecot 环境验证三个数据库：同/跨域、未知地址、共同目标、直接目标、别名、加号、多收件人，以及链路与环路；通过 Maildir 和 IMAP 核实实际数量与邮件头，制造配额/临时故障，核实独立重试和日志。逐域开启。
4. 功能回滚先关闭统一收件规则，检查并处理已有队列，再撤回邮件服务的映射配置；保留已升级 API、v12 表及已投递邮件。旧 API 的 schema 校验不支持直接降级到 v11，不应在运行中的新库上启动旧二进制或删除表。

SQLite 路由/SQL 映射单元测试与浏览器模拟响应测试不能替代真实 SMTP、Dovecot、IMAP 和 PostgreSQL/MySQL 验证。上线前必须执行上述隔离验证。

## 已部署实例的域名分类修复

旧配置未显式设置 `virtual_alias_domains`，Postfix 默认继承 `virtual_alias_maps`；统一收件 SQL 又会对纯域名查询返回目标，导致开启规则的邮箱域被误判为别名域。同域原邮箱和统一目标均可能退信，日志可能包含 `User unknown in virtual alias table` 或域名同时出现在 `virtual_alias_domains` 和 `virtual_mailbox_domains` 的警告。

一体化 Docker 实例应更新为包含本次修复的镜像，并按现有部署流程重建 `lanqin-email` 服务，使 `main.cf`、SQLite 映射及数据库配置脚本一起更新。仅重启旧镜像不会获得修复。更新前记录镜像版本并备份配置；沿用现有 `.env` 和数据挂载。

需要先恢复旧实例的域名分类时，可在服务器的 `deploy` 目录执行以下命令；这只修正当前容器配置，后续仍需更新镜像以持久保留修复：

```text
docker compose exec lanqin-email postconf -e "virtual_alias_domains ="
docker compose exec lanqin-email postfix check
docker compose exec lanqin-email postfix reload
docker compose exec lanqin-email postconf virtual_alias_domains virtual_mailbox_domains virtual_alias_maps
```

确认 `virtual_alias_domains` 为空，并保持原有 `virtual_mailbox_domains`、`virtual_alias_maps`。使用合成邮件验证真实邮箱和不存在地址均进入预期邮箱，检查原件、副本数量、别名及加号地址，再分别验证关闭统一收件后无人收件开启和关闭的行为。已经退信的邮件需要发送方重新发送；仍在队列中的邮件应先检查状态，不能假定配置修复会恢复所有历史邮件。

若需回滚镜像，先关闭受影响域名的统一收件规则，再恢复记录的版本并核实正常收件；保留数据库、邮件目录和显式留空的域名分类设置，避免重新触发同一故障。
