# 多地点上传（Issue #34，M2 首批）

在 [快照 CLI](backup.md) 的基础上，`upload` 将**已经完成的同一份密文**上传到多个 S3 / WebDAV / Google Drive 实例。它不会访问应用数据库、生成在线快照、停止服务、使用恢复私钥或删除远端内容。[管理端](backup-admin.md) 已接通新建快照、上传任务、每日计划、Drive OAuth 及默认关闭的 [远端保留策略](backup-retention.md)；独立 CLI upload 不触发该策略。`fetch` 支持显式下载并校验密文；[认证恢复索引](backup-discovery.md) 已接入 CLI 和 Web 后台独立发现，可在没有本地历史时选择受信收据，再进入 [Web 恢复向导](backup-web-restore.md)；不自动维护切换。

## 配置

参考 [多地点配置样例](backup-destinations.example.json)。样例包含 3 个 S3、2 个 WebDAV，各自拥有独立 ID、名称、连接配置和环境变量引用；`.invalid` 地址必须替换为实际地址。相同类型、endpoint、bucket 都可以重复，只有 `id` 必须唯一。当前单次配置上限为 64 个地点，不是 S3 只能添加一个。

- CLI 配置只保存 `access_key_id_env`、`secret_access_key_env`、可选的 `session_token_env` 或 `username_env` / `password_env`，这些字段的值是环境变量名称，**不是凭据本身**。通过受保护的运行环境提供对应变量，不能把真实凭据写入此样例、命令行或仓库。管理端加密凭据库与此 CLI 配置目前独立，尚未互相导出或自动对接；本 CLI 不读取应用凭据库，也不替操作者加密外部环境配置。
- S3 使用官方 AWS SDK for Go v2，逐地点显式提供凭据；不使用默认 AWS 凭据链、共享配置文件或实例元数据凭据。分别设置 HTTPS `endpoint`、`region`、`bucket`、可选 `prefix` 和 `path_style`。只支持普通桶，不支持 ARN / Access Point / S3 Express 特殊端点。
- `prefix` 不带开头/结尾的 `/`，只允许 ASCII 字母、数字、`_`、`-`、`.` 及目录分隔符，禁止 `.` / `..` 段。桶必须预先存在。
- WebDAV 的 `url` 是**已存在的集合目录**，支持 Basic Authentication，通过 HTTPS 传输。不会自动创建目录、协商其他认证方式或跟随重定向。需要应用密码的供应商应由操作者提供应用密码。
- `disabled: true` 表示上传时跳过该地点；与管理端独立的每日计划开关不同。显式 `fetch` 可读取停用地点以便恢复，但不启用它或修改计划。
- 配置文件必须是单个 version 1 JSON 值，最多 1 MiB，拒绝未知字段。完整配置结构及选择的 ID 会在任何上传前校验；单个地点缺少运行时凭据仅使该地点失败，不影响其他有效地点。

## 手动执行

先按照离线快照文档构建 CLI 并运行 `create`，独立可信地保存它输出的 SHA-256 收据。然后在已配置凭据环境、且暂存目录权限合适的进程中执行：

```text
./tmp/lanqin-backup upload --config /secure/config/destinations.json --in /backups/sample.age --expected-sha256 <trusted-64-hex-receipt> --spool-dir /secure/spool
```

默认对所有未停用的地点上传。只选其中几个地点：

```text
./tmp/lanqin-backup upload --config /secure/config/destinations.json --in /backups/sample.age --expected-sha256 <trusted-64-hex-receipt> --destinations r2,b2 --concurrency 2 --attempts 3 --timeout 20m --spool-dir /secure/spool
```

`--spool-dir` 必须存在；不指定时使用系统临时目录。上传前将输入复制到一个私有临时密文文件，对**实际复制的数据**校验收据；各地点及重试通过独立读取器读取同一副本，不会共享文件偏移或因原文件变化而上传不同内容。暂存区需有至少一份密文的可用空间，完成或失败后只移除本次创建的暂存文件，不删除源备份。Linux 文件权限为 `0600`；Windows 仍需操作者配置合适的目录 ACL。

仅接受 regular file 和二进制 age 格式前缀，拒绝符号链接、明显的明文或不匹配的收据。前缀检查不是 age 认证，也不证明文件内含有效的 LanQin 快照；完整格式及加密认证仍在 `restore` 时检查。

以下限制描述未指定检查点参数的旧 CLI 模式；管理端默认及 CLI 显式 `--resume-dir/--resume-id` 使用 [分片/继续协议](backup-resume.md)，对象发现和完成验证有所不同：

- 每次 CLI 调用默认并发 2 个地点，可设置 1..8；多个独立进程之间目前没有全局调度或限流。
- 单地点最多 3 次尝试，只对网络错误、超时、限流和部分服务端错误进行有界退避重试。认证失败、重定向、已存在对象等不自动重试。取消操作会中止运行请求并跳过待执行任务。
- 每次尝试默认超时 20 分钟，最多 1 小时。默认密文上限 1 GiB，`--max-bytes` 最多 5 GiB；此旧模式是完整 PUT，不能用于超过该限制的归档。分片模式同样保留明确的总大小上限。
- 每次尝试生成独立随机对象名 `lanqin-<128-bit-random>.age`，并发送 `If-None-Match: *`，不降级成覆盖式上传。S3 额外使用 `Content-MD5` 做传输校验；安全收据始终使用 SHA-256，而非 MD5。

## 结果、失败与恢复

标准输出为 JSON，结果顺序与配置一致：

```json
{
  "sha256": "<trusted-ciphertext-sha256>",
  "bytes": 12345,
  "destinations": [
    {
      "destination_id": "r2",
      "state": "succeeded",
      "attempts": 1,
      "object_key": "lanqin/lanqin-<random>.age",
      "completed_at": "2026-10-29T00:00:00Z"
    },
    {
      "destination_id": "dav1",
      "state": "failed",
      "attempts": 1,
      "error_code": "access_denied"
    }
  ]
}
```

- 状态为 `succeeded`、`failed`、`cancelled` 或 `skipped`。只有成功结果包含最终 `object_key` 和 UTC `completed_at`；它们目前不写入应用数据库，不代表已完成持久化“最近成功”记录。
- 任一启用地点失败/取消，CLI 返回非零退出码，同时仍输出其他地点的结果。配置、收据或本地暂存预检失败时不发起上传，也不输出成功报告。全部停用时直接报错。
- 旧 CLI 模式成功表示远端确认 PUT 完成（S3 为 200，WebDAV 为 200/201/204），未独立回读；检查点模式成功还要求完整密文回读匹配。两者都不等于解密/恢复演练；S3 的 202 异步接受不能当成完成。原始服务端错误正文、认证头和 SDK 错误不会回显，只输出稳定分类码。
- 旧 CLI 模式网络失败会换新名字；检查点模式保留原对象/会话进行核对，未知结果仍可能留下临时内容。都不能把云端所有 `.age` 文件当成已确认备份或盲目清理。管理端有受控保留策略和明确取消时的已知 S3 会话清理，详见对应说明；独立 CLI 信号中止保留检查点。
- 将原始可信收据及成功报告在云端之外独立可信地保存。可显式配置原索引主密钥发布 [认证恢复索引](backup-discovery.md)，之后 CLI 或管理端发现不依赖旧任务历史；索引失败与密文成功独立报告，不能把无认证的云端 metadata 当作原始可信 SHA。Web 自动维护切换仍待实现。

## 网络与验证边界

默认只允许公网 HTTPS，并验证服务器证书。部署者可为明确的内网 NAS/MinIO 授予下面的精确网络白名单，Web 请求和地点 JSON 不能扩大策略；没有关闭 TLS 校验或全局允许私网的开关。不读取 HTTP(S) 代理环境变量，不跟随重定向，凭据仅发给该地点配置对应的 endpoint / S3 bucket 主机。每次建连解析全部 DNS 答案并校验，再直接连接已验证 IP，避免二次解析切换目标。

### 明确授权内网 NAS

API 从部署环境 `LANQIN_BACKUP_NETWORK_ALLOWLIST_JSON` 读取策略，默认空值保持公网策略。格式为一个不超过 64 KiB 的 version 1 JSON，最多 64 个精确 host/port 目标、每个最多 16 个私网 IP：

```json
{
  "version": 1,
  "targets": [
    { "host": "nas.example.internal", "port": 443, "ips": ["192.168.1.10"] }
  ]
}
```

- 只允许 RFC1918 IPv4 或 IPv6 ULA，目标按精确主机名/端口匹配。若用 IP 字面量作 `host`，它必须与 `ips` 一致。CIDR、通配符、重复目标、IPv4-mapped IPv6、zone ID、loopback、link-local/云元数据、CGNAT 和其他特殊用途地址均拒绝。
- 对命中白名单的 host/port，所有 DNS 答案都必须是该列表中的私网 IP；多出一个公网或未授权私网答案也拒绝，不先尝试“其中一个看似安全”的地址。其他 host/port 继续使用默认公网策略。
- NAS 仍需使用 `https://`，证书必须匹配配置 hostname/IP 并受进程所在系统信任。私有 CA 由部署者安全安装至该环境信任库；不能靠白名单允许任意自签名证书、忽略主机名或跟随重定向。仅代理可达的服务仍不支持。
- 内网 S3/MinIO 必须使用 `path_style: true`，避免 bucket 虚拟主机产生不同主机名及 DNS 边界。Google Drive/OAuth 始终使用固定 Google HTTPS 公网来源，不采用内网白名单。
- 策略为当前进程的部署配置，不保存到地点凭据库、任务记录或对外 JSON，不允许通过 Web 设置。非空策略格式错误时，备份操作返回安全失败；邮件应用本身可继续运行。撤销或变更须更新配置并重建/重启持有旧配置的 API/CLI 进程；后续任务/新连接按生效策略核验。

独立 CLI 不隐式读取这个部署变量。需显式通过 `--network-policy-env <环境变量名>` 选择同形状的策略值，例如在受保护环境设置 `BACKUP_NETWORK_POLICY` 后：

```text
./tmp/lanqin-backup upload --config /secure/config/destinations.json --in /backups/sample.age --expected-sha256 <trusted-64-hex> --spool-dir /secure/spool --network-policy-env BACKUP_NETWORK_POLICY
./tmp/lanqin-backup fetch --config /secure/config/destinations.json --destination nas --object-key lanqin-<32-hex>.age --expected-sha256 <trusted-64-hex> --out /private/download.age --network-policy-env BACKUP_NETWORK_POLICY
```

策略 JSON 不写入地点配置文件；省略标志时即使环境中存在策略变量，CLI 也保持公网策略。无效/缺失的显式变量会在网络操作前失败，不回显 JSON 中的内部地址或误填值。

### 2026-11-04：真实自建存储服务验证

第 24 轮在无 NIC 的隔离 Linux VM 中运行真实 MinIO 与 WsgiDAV/Cheroot，通过当前生产 HTTP API 创建独立 S3/WebDAV 地点，手动上传同一在线快照、完整密文回读并发布认证索引。两个独立每日计划实际在 `05:16 UTC` 分别产生成功任务和次日游标；各地点认证发现两项备份，两种存储都实际下载并完成 age/SQLite ready 暂存。随后 WebDAV 恢复任务经 root helper 完成真实邮件维护与回滚；详情、版本/摘要与复现步骤见 [真实存储演练](backup-real-storage-validation.md)。

使用明确测试 CA 的 HTTPS 和固定主机/端口/IP 私网策略，没有关闭生产证书校验、注入 uploader 或直接写任务数据库。这证明自建 S3-compatible/WebDAV 软件联调，不是 Google Drive、商业 AWS/R2/B2 或第三方云账户验收；本轮只有 1 个 S3 + 1 个 WebDAV，没有完成 3S3/2DAV/Drive 的完整真实共存样例、真实大文件续传/撤销授权或保留清理矩阵。客户端是 HTTP 工具，不是浏览器；本轮未修改生产代码或重新运行 Go 测试。

### 此前的隔离协议测试

自动测试使用合成 age 密文和本机 TLS 测试服务，执行真实 SDK 签名请求及 WebDAV PUT，覆盖多个同类地点、独立凭据签名验证、失败隔离、独立读偏移、重试、取消、收据预检、DNS 和重定向保护。另有合成 SQLite / 邮件快照从导出、加密、上传到取回密文并验证暂存恢复的闭环测试。**这些是隔离协议测试，不是已连接 R2、B2、AWS 或具体 WebDAV 供应商的生产联调证明。**

2026-10-29 本地验证记录：`go vet ./...`、`go test ./... -count=1`、`go mod verify` 通过；新增存储和 CLI 测试重复运行 3 次通过；Linux amd64 / arm64、`CGO_ENABLED=0` 构建通过。Windows CLI 子进程演练验证 keygen / create / 五地点缺失凭据的独立失败报告 / restore，没有向云端发送请求。原始 `gofmt -l .` 仍列出 56 个既有 CRLF 文件，换行归一化后无真实格式差异；未格式化这些无关文件。未进行 Linux 运行恢复、真实云供应商、管理界面或跨数据库备份验证。

实现参考：[AWS SDK v2 端点配置](https://docs.aws.amazon.com/sdk-for-go/v2/developer-guide/configure-endpoints.html)、[S3 PutObject 条件写入](https://docs.aws.amazon.com/AmazonS3/latest/API/API_PutObject.html)、[WebDAV RFC 4918](https://www.rfc-editor.org/rfc/rfc4918)。
