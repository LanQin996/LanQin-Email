# 维护窗口的队列与部署资产保全（实验性）

维护计划可显式启用加密保全：在应用写入者停止后，把当前 Postfix spool 和固定摘要的部署资产保存为本地 age 加密证据，再验证实际密文、全部 payload 和当前源快照。强制重建会替换原容器及其未持久化队列；允许非空队列继续时，保留的是经过验证的加密隔离副本，**不会自动把其中邮件投递到新服务，也不证明邮件已重新投递。** 未启用时仍要求队列可证为空。

此能力接入 [维护协调器](backup-maintenance.md)，不是日常 [数据归档](backup.md) 的扩展范围，不自动上传云端，也不赋予 Web API Docker socket、服务控制或任意宿主机路径权限。Issue #34 仍未完成。

## 显式配置

在已经审查的维护计划中添加可选字段；以下只是字段示意，公钥、绝对路径和摘要必须替换为真实值：

```json
{
  "preservation": {
    "recipient": "age1replace-with-a-real-x25519-public-key",
    "identityEnv": "LANQIN_MAINTENANCE_PRESERVATION_IDENTITY",
    "quarantineQueues": true,
    "assets": [
      { "path": "/private/deployment/tls/fullchain.pem", "sha256": "<trusted-64-hex>" },
      { "path": "/private/deployment/tls/privkey.pem", "sha256": "<trusted-64-hex>" }
    ]
  }
}
```

`identityEnv` 只保存维护进程环境变量的名字。实际执行须通过该受保护的环境变量提供匹配 `recipient` 的 age X25519 私钥；不会把私钥值写入计划或传给 Docker/Compose 子进程。`maintain-plan` 的结构检查不等于已经取得私钥并完成保全。

`quarantineQueues: true` 是允许非空队列在加密保全后退出在线投递的显式确认。省略或设为 `false` 时，即使已配置保全，仍要求队列可证为空；也可以在这个模式下保全部署资产和空 spool。

自动收集维护计划、当前容器/镜像及环境摘要绑定、计划列出的 Compose 文件、当前环境文件，以及配置了 `nextEnvironmentFile` 时的新环境副本。`assets` 只补充明确选择的部署文件，例如 TLS 证书和服务私钥；不扫描宿主机、不递归收集目录，不自动寻找其他密钥或配置。

额外资产最多 24 个，每个文件最多 8 MiB。要求绝对、固定摘要、非链接的普通文件及可信属主/父目录；不能位于 live targets、workspace、stateDir 或恢复暂存树中，不能重复选择自动包含的文件。资产内容若含标准 `AGE-SECRET-KEY-` 标记会拒绝，恢复私钥应独立保管；TLS/服务私钥可以作为显式加密资产，但不因此被自动应用。

## 捕获与收据

生产维护流程在安装及环境切换前执行保全检查；强制重建和显式恢复路径会再次检查当前队列与资产。捕获期间验证全部应用服务停止、容器清单、镜像及环境绑定，并在验证密文后再次读取源快照。变化或验证失败时拒绝继续，保留原容器和所有证据。

每个 Postfix 容器的 `/var/spool/postfix` 通过受限 `docker cp` tar 捕获，最多 64 MiB；检查标准 spool 布局、普通文件/目录、重复或越界名称、截断、链接/特殊类型及尾随数据，不在宿主机展开。单次全部 spool 捕获最多 256 MiB；**spool 加资产的总 payload 仍最多 256 MiB、160 个条目**，实际剩余空间须留给计划和配置。未知布局或超限不降级为跳过队列。

每份 bundle 使用独立 `maintenance-preservation` scope，绑定计划 SHA-256 和本次 operation ID。加密 manifest 按输入顺序记录名称、来源说明、字节数和每项 SHA-256；资产名仅为 `assets/0000.bin` 形式，spool 名仅为 `spools/<container-id>.tar`。来源说明仅是加密元数据，不作为恢复路径、服务命令或重放指令。

私有 `stateDir` 保存 `preservation-<manifest-sha256>.age` 和同名 `.json` 收据。收据含计划、操作、manifest/密文摘要及密文字节数；无资产正文、来源路径或私钥。发布不覆盖已有文件，密文先完整回读解密、核对预期 manifest 及每个 payload，再发布收据；中断后遗留的无收据密文也必须通过同样验证才能采用，不能只看文件名或格式头。

**保留同一操作的全部 bundle 和收据，不只保留最后一份。** 原环境切换前、切换后或后续恢复可能产生不同的当前快照；后期 bundle 中当前 `.env` 可能已经是新环境，不能据此假定它包含原环境。按各份 manifest 的来源、字节摘要和计划绑定选择所需证据。每个 stateDir 最多保留 16 份密文，达到上限会拒绝新快照，不自动删除旧保全。

本地收据应通过独立可信渠道保管，密文及收据也应由操作员另行安全复制。把两者一起放在同一个不可信存储后重新取回，不等于获得来源认证；age 加密也不是发送方签名。

## 验证与隔离恢复

使用受保护、匹配公钥的独立私钥文件，以及可信收据中的 `cipherSHA256`：

```text
./lanqin-backup preservation-verify --in /private/preservation-<manifest-sha256>.age --identity-file /offline/preservation-key.txt --expected-sha256 <trusted-cipher-sha256>
./lanqin-backup preservation-restore --in /private/preservation-<manifest-sha256>.age --identity-file /offline/preservation-key.txt --expected-sha256 <trusted-cipher-sha256> --out /private/new-preservation-evidence
```

`preservation-verify` 流式检查同一条密文的可信 SHA-256、age 最终认证、严格 manifest/tar、全部 payload 摘要和资源上限，不写明文文件。两条命令只输出计数、操作/计划绑定及验证状态，不打印来源说明、资产名或正文。

`preservation-restore` 的输出必须不存在，父目录须安全且已存在。Unix 下新目录为当前执行 UID 所有且仅属主可访问，文件为普通 `0600`；拒绝路径组件中的链接。成功输出 `manifest.json`、按 manifest 顺序编号的 `payloads/0000.bin` 等 opaque 文件，最后发布 `READY`。失败仅清理本次创建、身份仍匹配的输出；进程被强制中止可能留下没有 `READY` 的私有不完整目录，不能当作验证完成。

Windows 上须先给输出父目录设置私有 ACL；Unix 的 `0700/0600` 模式不能替代 Windows ACL，当前工具不检查或重写该 ACL。

即使 manifest 说明了原 `.env` 或 spool 来源，恢复也不会将文件写回原路径、重放归档 UID/GID、展开 spool tar、执行程序、调用 Docker，或启动任何服务。`READY` 仅表示隔离证据验证完成。后续恢复证书、环境或队列需要操作员另行审查和受控操作；当前没有自动 Postfix 队列导入/去重/重投递能力。

## 本轮验证边界

本轮最终 `go test ./... -count=1 -timeout=10m` 及全量 vet 通过；包含实际 age 加解密、完整 payload 校验、非空队列与显式确认、密文/收据篡改、冷复用/中断采用、变化快照、资源限制及 CLI 隔离恢复。固定 Compose/环境文件的 age 私钥标记会在任何控制器预检前拒绝，覆盖跨 64 KiB 边界；私钥变量与 Docker 白名单变量同名时也不会继承给子进程。

隔离 QEMU/Alpine Linux VM 实际通过 maintenance、preservation、`cmd/backup` 全包；私钥检查前移后再执行最新 maintenance 全包通过，所有 VM 已退出。合成队列和模拟 Compose controller 不等于真实 Docker 的 `cp` 输出、SMTP/IMAP 收发或队列重放验收。

Unix 私有属主、目录 fsync 与替换目录保护属于 Linux 验证范围；Windows 的文件测试使用宿主继承 ACL，不代表 Unix 语义或目录 fsync 已验证。即使 RAM 文件系统上的 fsync 返回成功，也不能证明块设备或断电耐久性。最新实际执行范围见 [进度记录](backup-progress.md)。
