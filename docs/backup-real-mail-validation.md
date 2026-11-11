# 隔离的真实邮件备份、覆盖恢复与回滚演练

2026-11-04，第 23 轮全新环境演练通过了 **all-in-one、SQLite、CLI** 的真实闭环。使用 WHPX 加速的 QEMU/Linux 客体、Docker 29.9.2、Compose 5.6.1 和新的 8 GiB ext4 虚拟磁盘；客体无 NIC、宿主文件共享或宿主端口转发，邮件与账户均为合成样本。测试完成后应用容器及拥有的 Docker daemon 停止，VM 正常退出。

这项结果不覆盖真实云服务、stack、PostgreSQL/MySQL Docker 协调、浏览器点击到主机 helper 的邮件流程、非空 Postfix 队列重导入或生产部署。ext4 上的正常同步和退出也不是断电耐久试验。Issue #34 仍未完成，其他边界见 [交付状态](backup-progress.md)。

## 实际验证内容

1. 全新数据库通过当前 API 建立合成管理员和邮箱。SMTP 25 实际投递固定 MIME 与 296 字节二进制附件；API 授权附件下载和使用明确测试 CA 的 IMAPS 读取核对正文、Message-ID 和附件字节。
2. 当前容器中的 `create-online` 在服务继续运行时采集 SQLite/Maildir，加密 age 归档并核对实际密文摘要。快照后再实际投递另一封邮件。
3. 当前宿主 CLI 解密到隔离暂存，通过可信报告收据建立新的维护计划。计划显式启用 `sqliteGroupRead`，读取组是实际 Postfix 主 GID 103；没有在安装后手工改权限来补救。
4. 正式 `maintain` 完成停止、安装、离线验证、提交、容器重建和健康检查，返回 `healthy`、`committed=true`、`needsRecovery=false`。恢复后的原始 MIME SHA-256、附件和实际 IMAP INTERNALDATE 与基线一致，旧定时发送为 `cancelled`，快照后邮件没有合并进新的活动收件箱。
5. 恢复后再投递一封真实 SMTP 邮件，并经 API 附件下载和 IMAPS 验证可读。
6. 显式 `maintain-recover --operation rollback` 完成，`rollbackCompleted=true`、`originalServicesRestored=true`、`needsRecovery=false`。原树中的快照后邮件重新可读，恢复后新接受的邮件保留在 failed/new Maildir 中，没有被回滚删除或自动合并。

IMAP UIDVALIDITY 在覆盖恢复时实际改变，回滚后恢复原值；不承诺 UID 稳定。原邮件的 INTERNALDATE 在本次演练中始终为 `04-Nov-2026 04:38:39 +0000`，没有错误地以其较早的邮件 `Date` 头替代。Linux install 与 maintenance 最新全包测试也在该客体运行并通过，包括组读策略、类型/链接/权限漂移和读取者主组预检。

## 环境问题与实际修复

真实空 spool 中的 `postfix/restart` 是空普通文件，原校验器误拒。现在仅兼容这个固定空标记；非空、链接、目录、其他未知项及真实排队文件仍不能绕过空队列检查。

私有 `0700/0600` 数据树使不同 UID 的 Postfix 在 SQLite 查询时失败，服务进程存在并不代表能收信。修复采用显式、计划/journal 绑定的 [SQLite 服务组读取](backup-maintenance.md#sqlite-服务组读取)，不把数据库设为世界可读、不提升 Postfix 为 root。另一次机制测试实际证明 API 写事务后 WAL/SHM 保持批准组与 `0640`，Postfix 没有写权限，无关 UID 1002 无法读取；正式第 23 轮随后从空磁盘验证完整策略路径。

此前 TCG 下同一容器约 266.68 秒才通过角色检查，240 秒限制不足以判断为永久服务故障。离线客体的不可达 nameserver 也曾使 Rspamd 无法初始化；最终演练使用明确 WHPX 和仅客体内的 loopback 解析器声明，不提供外网解析、不跳过生产健康检查。这些环境诊断与真实 SQLite 权限缺陷分别记录，早期中断或失败收据没有追认为成功。

## 在独立测试环境复现

先准备隔离的 Linux Docker 环境、当前构建的 all-in-one 镜像、合成账户和独立绑定目录。按 [维护计划](backup-maintenance.md) 填写真实路径、镜像、六项 UID/GID 及固定摘要；SQLite 读取组必须核对目标 Postfix 主 GID。不得把下面的占位路径指向现有生产部署。

公开 CLI 的关键步骤为：

```text
./lanqin-backup keygen --identity-file /private/recovery.key
./lanqin-backup create-online --database /data/lanqin.db --data-dir /data --maildir-root /var/mail/vhosts --workspace /private/collect --recipient <generated-public-recipient> --out /private/online.age
./lanqin-backup restore --in /private/online.age --identity-file /private/recovery.key --expected-sha256 <trusted-ciphertext-sha256> --out /private/recovered
./lanqin-backup maintain-plan --config /private/maintenance-plan.json
./lanqin-backup maintain --config /private/maintenance-plan.json --confirm-maintenance
./lanqin-backup maintain-recover --config /private/maintenance-plan.json --operation rollback --confirm-maintenance
```

`create-online` 的路径属于执行采集所在的 namespace，维护计划则须写真实宿主绑定源与容器 runtime roots。每一步须使用实际输出的可信摘要；仅有 `READY`、健康检查或 CLI 退出码，不能替代 SMTP 接收、授权附件下载和 IMAP 内容/INTERNALDATE 校验。应在快照后、恢复后各接受一封不同邮件，并验证回滚后的旧树和 failed 树，才能证明新数据没有被悄悄丢弃。

## 本次工作区证据与本地重放材料

本次使用已校验的较旧公开发布镜像作运行依赖基础，再覆盖当前工作树 API、备份 CLI、前端构建及部署配置。没有把旧基础镜像当作当前源码。第 23 轮冻结的身份为：

| 材料 | SHA-256 / 镜像 ID |
| --- | --- |
| 公开基础 docker-load 归档 | `6fc54bbca5a3ae12549e4a7c3f892baf9bb2494b55d2a37305fa119c7fac8ff6` |
| 当前覆盖构建 context tar | `1f7065112b42180af363015cdad15015c30e6348b7535de258752c0d7e459293` |
| 当前宿主维护 CLI | `a4c506653a30fcb842e46127aa1f19e4f133f3080e875b86ce0c81f136d77d9c` |
| 实际派生镜像 | `sha256:029b0112d330ad2651d4263282e2dd461f4a7c7da56d3bec8f16801fc093834c` |

本机工作区为 `E:\Projet\LanQin-Email`，其下的证据和输入集中在：

```text
tmp/issue34-docker-vm/run23/execution.json
tmp/issue34-docker-vm/run23/serial.log
tmp/issue34-docker-vm/run23/mail-evidence/result.json
tmp/issue34-docker-vm/run23/mail-evidence/commands.log
tmp/issue34-runtime-context.json
tmp/issue34-build-runtime-context.py
tmp/issue34-docker-vm-run.py
tmp/issue34-real-mail-tested-task.sh
tmp/issue34-real-mail-integration.py
tmp/issue34-real-mail-client.py
```

这些是当前工作区的 ignored `tmp` 材料，不随仓库 clone 分发，也不是已经接入的 CI 邮件集成任务。拥有相同工作区材料及已核验的 QEMU/Alpine/APK 缓存时，可先运行 `python tmp/issue34-build-runtime-context.py`，再运行 `python tmp/issue34-docker-vm-run.py --run runFreshCopy --accel whpx` 并附加输入参数：`--image` 指定基础归档，`--input` 逐个指定冻结的 context/CLI/客户端/演练脚本及两份 Linux 测试二进制，`--task` 指定 `tmp/issue34-real-mail-tested-task.sh`。run 名须为以 `run` 开头的全新字母数字名称，`runFreshCopy` 也只是待确认不存在的示例；必须使用空虚拟磁盘，不重放已存在的维护 journal。准确输入列表和参数以本次 `execution.json` 为证。

结果文件中 `success:true`、两份 Linux 测试结果 0、客体结果 0 和完成标记相互对应，QEMU 收据记录正常退出。本文不包含恢复私钥、TLS 私钥、数据库正文或邮件原文；这些材料没有作为诊断日志导出。
