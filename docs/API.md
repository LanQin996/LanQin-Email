# LanQin Email API

LanQin Email exposes integration-oriented APIs under `/api/open`.

这些接口用于外部系统集成，统一放在 `/api/open` 下。它们不是匿名公开接口，只接受 API Token，不接受浏览器登录 Session Cookie。

## Base URL

All API endpoints are relative to your LanQin Email instance:

所有接口地址都相对于你的 LanQin Email 实例：

```
https://your-instance.example.com
```

## HTTP Status Codes

The API uses standard HTTP status codes:

接口使用标准 HTTP 状态码：

| Code 状态码 | Meaning 含义 |
|------|---------|
| `200 OK` | Request succeeded / 请求成功 |
| `201 Created` | Resource created successfully / 资源创建成功 |
| `400 Bad Request` | Invalid request parameters or validation error / 请求参数无效或校验失败 |
| `401 Unauthorized` | Missing or invalid API token / 缺少或无效的 API Token |
| `403 Forbidden` | Token lacks required permissions / Token 缺少所需权限 |
| `404 Not Found` | Resource does not exist / 资源不存在 |
| `429 Too Many Requests` | Rate limit exceeded / 超过频率限制 |
| `500 Internal Server Error` | Server error / 服务器错误 |

Validation failures — including a duplicate domain name — return `400 Bad Request`, not `409`.

校验失败（包括域名重复）会返回 `400 Bad Request`，而不是 `409`。

## Error Responses

All API errors return JSON with this structure:

所有接口的错误都以如下 JSON 结构返回：

```json
{
  "error": "error message"
}
```

**Important**: The API uses `DisallowUnknownFields()` for JSON parsing. Sending fields not defined in the request schema will result in a `400 Bad Request` error.

**重要提示**：接口在解析 JSON 时启用了 `DisallowUnknownFields()`。如果请求体中包含 schema 未定义的字段，将返回 `400 Bad Request` 错误。

Examples:

示例：

```json
{
  "error": "invalid token"
}
```

```json
{
  "error": "domain not found"
}
```

```json
{
  "error": "localPart is required"
}
```

## Authentication

Open API requests must use a Bearer API Token:

Open API 请求必须使用 Bearer API Token：

```http
Authorization: Bearer lq_xxx
```

Create tokens in **Profile / API Token**. The plain token is shown only once after creation, so store it securely and revoke it if it may have leaked.

请在 **个人中心 / API Token** 中创建 Token。明文 Token 只会在创建后显示一次，请安全保存；如果怀疑泄露，应立即撤销并重新创建。

Created token example:

创建后的 Token 示例：

```json
{
  "token": "lq_xxx"
}
```

Tokens created without a custom expiration default to 90 days. You can disable or revoke tokens from the same profile page.

如果没有自定义到期时间，Token 默认 90 天后过期。你可以在同一个个人中心页面中禁用或撤销 Token。

## Permissions

All Open API endpoints require an API token with appropriate permissions and role requirements:

所有 Open API 接口都需要具备相应权限和角色的 API Token：

| Endpoint 接口 | Required Permission 所需权限 | Required Role 所需角色 |
|----------|-------------------|---------------|
| `GET /api/open/domains` | any of `admin.domains.view`, `admin.dns.view`, `admin.mailboxes.view`, `admin.aliases.view`, `admin.settings.view`, `admin.templates.view` | admin |
| `POST /api/open/domains` | `admin.domains.create` | admin |
| `GET /api/open/domains/{id}` | any of `admin.domains.view`, `admin.dns.view`, `admin.mailboxes.view`, `admin.aliases.view`, `admin.settings.view`, `admin.templates.view` | admin |
| `POST /api/open/domains/{id}` | `admin.domains.update` | admin |
| `DELETE /api/open/domains/{id}` | `admin.domains.delete` | admin |
| `GET /api/open/mailboxes` | `admin.mailboxes.view` or `admin.messages.view` | admin |
| `POST /api/open/mailboxes` | `admin.mailboxes.create` | admin |
| `GET /api/open/mailboxes/{id}` | `admin.mailboxes.view` or `admin.messages.view` | admin |
| `POST /api/open/mailboxes/{id}` | `admin.mailboxes.update` | admin |
| `DELETE /api/open/mailboxes/{id}` | `admin.mailboxes.delete` | admin |
| `POST /api/open/send` | `mail.messages.send` | user or admin |
| `GET /api/open/send/{id}` | `mail.messages.read` | user or admin |
| `GET /api/open/mailboxes/{id}/messages` | `mail.messages.read` | user or admin |

**Notes:**
- Admin endpoints check for `requireAdminAccess` (role must be `admin`).
- Mail sending/reading endpoints work for regular users but only for mailboxes they own.
- Users can only read messages from their own active mailboxes.

**说明：**
- 域名和邮箱管理接口会检查 `requireAdminAccess`（角色必须为 `admin`）。
- 发信/读信接口对普通用户也可用，但只能操作自己拥有的邮箱。
- 用户只能读取自己拥有的 active 邮箱中的邮件。

## Domains

### List domains

```http
GET /api/open/domains
Authorization: Bearer lq_xxx
```

**Status:** `200 OK`

**Response:**

```json
{
  "items": [
    {
      "id": "dom_xxx",
      "name": "example.com",
      "status": "active",
      "dkimSelector": "lanqin",
      "dkimPublicKey": "v=DKIM1; k=rsa; p=MIIBIjANBgkq...",
      "dnsStatus": "unchecked",
      "dnsCheckedAt": null,
      "createdAt": "2026-06-29T00:00:00Z"
    }
  ]
}
```

**Field descriptions:**
- `status`: `active` or `disabled`
- `dnsStatus`: `unchecked` (initial), `ok` (all DNS records verified), or `error` (verification failed)
- `dnsCheckedAt`: Timestamp of last DNS check (nullable)
- `dkimPublicKey`: Public key for DKIM signing (omitted in some contexts)

**字段说明：**
- `status`：`active` 或 `disabled`
- `dnsStatus`：`unchecked`（初始）、`ok`（所有 DNS 记录校验通过）或 `error`（校验失败）
- `dnsCheckedAt`：上次 DNS 检查的时间戳（可为 null）
- `dkimPublicKey`：用于 DKIM 签名的公钥（部分场景下会省略）

**Note:** This endpoint returns all domains without pagination.

**注意：** 该接口一次性返回所有域名，不分页。

### Create domain

```http
POST /api/open/domains
Authorization: Bearer lq_xxx
Content-Type: application/json

{
  "name": "example.com"
}
```

**Status:** `201 Created`

**Response:**

```json
{
  "id": "dom_xxx",
  "name": "example.com",
  "status": "active",
  "dkimSelector": "lanqin",
  "dkimPublicKey": "v=DKIM1; k=rsa; p=MIIBIjANBgkq...",
  "dnsStatus": "unchecked",
  "dnsCheckedAt": null,
  "createdAt": "2026-06-29T00:00:00Z"
}
```

**Notes:**
- Domain name is automatically normalized to lowercase
- DKIM keys are generated automatically
- Initial `dnsStatus` is `unchecked`

**说明：**
- 域名会自动规范化为小写
- DKIM 密钥会自动生成
- 初始 `dnsStatus` 为 `unchecked`

### Get domain

```http
GET /api/open/domains/{id}
Authorization: Bearer lq_xxx
```

**Status:** `200 OK` or `404 Not Found`

**Response:** Same as domain object in list response.

**响应：** 与列表接口中的 domain 对象结构相同。

### Update domain status

```http
POST /api/open/domains/{id}
Authorization: Bearer lq_xxx
Content-Type: application/json

{
  "status": "active"
}
```

**Status:** `200 OK` or `404 Not Found`

**Request body:**
- `status`: Must be `active` or `disabled`

**请求体：**
- `status`：必须为 `active` 或 `disabled`

**Response:** Updated domain object.

**响应：** 更新后的 domain 对象。

**Note:** This endpoint uses `POST` (not `PATCH`/`PUT`) for simplicity in client implementations.

**注意：** 该接口使用 `POST`（而非 `PATCH`/`PUT`），以简化客户端实现。

### Delete domain

```http
DELETE /api/open/domains/{id}
Authorization: Bearer lq_xxx
```

**Status:** `200 OK`, `404 Not Found`, or `400 Bad Request`

**Response:**

```json
{
  "ok": true
}
```

**Error cases:**
- `400`: Domain still has mailboxes (must delete mailboxes first)
- `404`: Domain not found

**错误情况：**
- `400`：域名下仍有邮箱（需先删除邮箱）
- `404`：域名不存在

## Mailboxes

### List mailboxes

```http
GET /api/open/mailboxes
Authorization: Bearer lq_xxx
```

**Status:** `200 OK`

**Response:**

```json
{
  "items": [
    {
      "id": "mbx_xxx",
      "userId": "usr_xxx",
      "userEmail": "alice@example.com",
      "domainId": "dom_xxx",
      "localPart": "alice",
      "address": "alice@example.com",
      "displayName": "Alice",
      "quotaMb": 1024,
      "status": "active",
      "createdAt": "2026-06-29T00:00:00Z"
    }
  ]
}
```

**Note:** This endpoint returns all mailboxes without pagination.

**注意：** 该接口一次性返回所有邮箱，不分页。

### Create mailbox

```http
POST /api/open/mailboxes
Authorization: Bearer lq_xxx
Content-Type: application/json

{
  "domainId": "dom_xxx",
  "localPart": "alice",
  "displayName": "Alice",
  "password": "Password123!",
  "quotaMb": 1024,
  "ownerEmail": "alice@example.com"
}
```

**Status:** `201 Created`

**Request fields:**

| Field 字段 | Required 必填 | Description 说明 |
|-------|----------|-------------|
| `domainId` | Yes | ID of an existing domain / 已存在域名的 ID |
| `localPart` | Yes | Local part of the address. Normalized to lowercase; only `a-z 0-9 . _ % + -` are kept, other characters stripped / 地址本地部分。会规范化为小写，仅保留 `a-z 0-9 . _ % + -`，其余字符会被移除 |
| `password` | Yes | At least 8 characters. Used as the mailbox password / 至少 8 位，用作邮箱密码 |
| `displayName` | No | Defaults to the mailbox address if omitted / 省略时默认使用邮箱地址 |
| `quotaMb` | No | Mailbox quota in MB / 邮箱配额（MB） |
| `ownerEmail` | No | Owner's email. See owner resolution below / 拥有者邮箱，见下方拥有者解析规则 |
| `userId` | No | Bind to an existing user by ID. Takes precedence over `ownerEmail` / 绑定到已有用户的 ID，优先级高于 `ownerEmail` |

**Owner resolution:**
- If `userId` is provided, the mailbox is bound to that existing user (must be an active user).
- Otherwise, if `ownerEmail` is provided, LanQin Email looks up an active user with that email.
- If `ownerEmail` is omitted, the mailbox address is used as the owner email.
- If no active user with that email exists, a new user is created automatically.

**拥有者解析规则：**
- 如果传了 `userId`，邮箱会绑定到该已有用户（必须是启用状态的用户）。
- 否则，如果传了 `ownerEmail`，系统会查找该邮箱对应的启用用户。
- 如果省略 `ownerEmail`，则使用邮箱地址作为拥有者邮箱。
- 如果不存在对应的启用用户，系统会自动创建一个新用户。

**Response:** Created mailbox object (same shape as list response).

**响应：** 创建后的 mailbox 对象（结构与列表接口相同）。

### Get mailbox

```http
GET /api/open/mailboxes/{id}
Authorization: Bearer lq_xxx
```

**Status:** `200 OK` or `404 Not Found`

**Response:** Mailbox object (same shape as list response).

**响应：** mailbox 对象（结构与列表接口相同）。

### Update mailbox

```http
POST /api/open/mailboxes/{id}
Authorization: Bearer lq_xxx
Content-Type: application/json

{
  "displayName": "Alice Work",
  "quotaMb": 2048,
  "status": "active",
  "userId": "usr_xxx"
}
```

**Status:** `200 OK` or `404 Not Found`

All fields are optional. Omitted (or empty / non-positive) fields keep their current value. `status` can be `active` or `disabled`. When `userId` is provided, the target user must exist and be active.

所有字段均可选。省略（或为空 / 非正数）的字段会保留原值。`status` 可为 `active` 或 `disabled`。如果传了 `userId`，目标用户必须存在且处于启用状态。

**Response:** Updated mailbox object.

**响应：** 更新后的 mailbox 对象。

### Delete mailbox

```http
DELETE /api/open/mailboxes/{id}
Authorization: Bearer lq_xxx
```

**Status:** `200 OK`, `404 Not Found`, or `400 Bad Request`

**Response:**

```json
{
  "ok": true
}
```

**Notes:**
- Deleting a mailbox also deletes all of its messages.
- If the token owner is deleting their own mailbox, it cannot be their last remaining mailbox (returns `400`).

**说明：**
- 删除邮箱会同时删除该邮箱下的所有邮件。
- 如果 Token 拥有者删除的是自己的邮箱，则不能删除最后一个邮箱（会返回 `400`）。

## Send Mail

```http
POST /api/open/send
Authorization: Bearer lq_xxx
Content-Type: application/json

{
  "mailboxId": "mbx_xxx",
  "to": ["bob@example.com"],
  "cc": [],
  "bcc": [],
  "subject": "Hello",
  "text": "Plain text body",
  "html": "<p>HTML body</p>",
  "attachments": [
    {
      "filename": "report.pdf",
      "contentType": "application/pdf",
      "contentBase64": "JVBERi0xLjQK..."
    }
  ]
}
```

**Status:** `201 Created`

**Request fields:**

| Field 字段 | Required 必填 | Description 说明 |
|-------|----------|-------------|
| `mailboxId` | Yes | Sending mailbox ID (must be owned by the token user) / 发信邮箱 ID（必须属于 Token 拥有者） |
| `to` | Yes | Recipient addresses (at least one recipient across to/cc/bcc) / 收件人地址（to/cc/bcc 至少需有一个收件人） |
| `cc` | No | CC addresses / 抄送地址 |
| `bcc` | No | BCC addresses / 密送地址 |
| `subject` | No | Message subject / 邮件主题 |
| `text` | No | Plain text body / 纯文本正文 |
| `html` | No | HTML body / HTML 正文 |
| `attachments` | No | List of attachments (see below) / 附件列表（见下方） |

**Attachment fields:**

| Field 字段 | Description 说明 |
|-------|-------------|
| `filename` | Attachment file name / 附件文件名 |
| `contentType` | MIME type, e.g. `application/pdf` / MIME 类型，如 `application/pdf` |
| `contentBase64` | Base64-encoded file content / Base64 编码的文件内容 |

Total attachment size is limited by the sender's permission group (`maxAttachmentMb`, default 25 MB). Exceeding it returns `400`.

附件总大小受发信人所在权限组限制（`maxAttachmentMb`，默认 25 MB）。超出会返回 `400`。

**Response:**

```json
{
  "id": "mail_xxx",
  "queueId": "snd_xxx",
  "status": "queued",
  "messageId": "mail_xxx",
  "rfcMessageId": "<msg_xxx@example.com>",
  "mailboxId": "mbx_xxx",
  "mailboxAddress": "alice@example.com",
  "subject": "Hello",
  "recipients": ["bob@example.com"],
  "attemptCount": 0,
  "maxAttempts": 5,
  "createdAt": "2026-06-29T00:00:00Z"
}
```

**Response fields:**

| Field 字段 | Description 说明 |
|-------|-------------|
| `id` | Send identifier; use it with `GET /api/open/send/{id}` / 发信标识，可配合 `GET /api/open/send/{id}` 使用 |
| `queueId` | SMTP queue item id. Omitted when the message was only `accepted` / SMTP 队列项 ID；仅 `accepted` 时不返回 |
| `status` | Delivery status, see values below / 投递状态，见下方取值 |
| `messageId` | Internal stored message id / 内部存储的消息 ID |
| `rfcMessageId` | RFC 5322 `Message-ID` header / RFC 5322 的 `Message-ID` 头 |
| `mailboxAddress` | Sending mailbox address / 发信邮箱地址 |
| `recipients` | Deduplicated recipients (to + cc + bcc) / 去重后的收件人（to + cc + bcc） |
| `attemptCount` / `maxAttempts` | Delivery attempt counters (queue only) / 投递尝试次数（仅队列项返回） |
| `nextAttemptAt` / `lastError` | Next retry time / last delivery error (present when applicable) / 下次重试时间 / 最近一次投递错误（在适用时返回） |
| `updatedAt` / `deliveredAt` | Update / delivery timestamps (present when applicable) / 更新 / 投递时间戳（在适用时返回） |

When SMTP delivery is not configured, the message can be stored as accepted without a queue item:

如果没有配置 SMTP 投递，邮件可能只会进入 `accepted` 状态，不会产生 `queueId`。

Current status values:

当前状态取值：

- `accepted`: message was accepted and stored, but no SMTP queue item exists.
- `queued`: queued for SMTP delivery.
- `sending`: currently being delivered.
- `delivered`: SMTP delivery succeeded.
- `failed`: delivery failed and may be retried.
- `canceled`: delivery was canceled.

<br>

- `accepted`：邮件已被接受并存储，但没有 SMTP 队列项。
- `queued`：已进入 SMTP 投递队列。
- `sending`：正在投递中。
- `delivered`：SMTP 投递成功。
- `failed`：投递失败，可能会重试。
- `canceled`：投递已取消。

**Error cases:**

| Status 状态码 | Cause 原因 |
|--------|-------|
| `400` | No recipients / invalid MIME / attachment too large / 无收件人、MIME 无效或附件过大 |
| `403` | Sender address is not authorized / 发信地址未被授权 |
| `404` | Mailbox not found or not owned by the token user / 邮箱不存在或不属于 Token 拥有者 |
| `429` | SMTP send rate limit exceeded / 超过 SMTP 发信频率限制 |
| `507` | Mailbox quota exceeded / 邮箱配额已满 |

Bounce, complaint, rejection, and provider-specific delivery events require future webhook or delivery-event integration.

退信、投诉、拒收等更细状态需要后续接入投递事件或 webhook 后才能完整提供。

## Send Status

```http
GET /api/open/send/{id}
Authorization: Bearer lq_xxx
```

**Status:** `200 OK` or `404 Not Found`

`id` can be the value returned by `POST /api/open/send`. If a queue item exists, it can also be the queue id.

`id` 可以使用发信接口返回的 `id`；如果存在队列项，也可以使用 `queueId`。

**Response:** Same shape as the `POST /api/open/send` response. Only messages belonging to the token user's mailboxes are returned; otherwise `404`.

**响应：** 结构与 `POST /api/open/send` 的响应相同。只会返回属于 Token 拥有者邮箱的邮件，否则返回 `404`。

## Received Messages

```http
GET /api/open/mailboxes/{id}/messages?folder=Inbox&limit=30&cursor=0&q=keyword
Authorization: Bearer lq_xxx
```

**Status:** `200 OK` or `404 Not Found`

Query parameters:

查询参数：

- `folder`: folder name. Defaults to `Inbox`; use `all` for all folders.
- `limit`: page size, defaults to `30`, maximum `100`.
- `cursor`: numeric offset. Pass back the `nextCursor` value from the previous response to fetch the next page.
- `q`: optional search keyword. Matches subject, from, to, snippet, and body text.

<br>

- `folder`：文件夹名称。默认为 `Inbox`；使用 `all` 表示所有文件夹。
- `limit`：每页数量，默认 `30`，最大 `100`。
- `cursor`：数字偏移量。把上一次响应中的 `nextCursor` 传回即可获取下一页。
- `q`：可选搜索关键词。会匹配主题、发件人、收件人、摘要和正文。

Response:

响应：

```json
{
  "items": [
    {
      "id": "mail_xxx",
      "mailboxId": "mbx_xxx",
      "folder": "Inbox",
      "messageId": "<message@example.com>",
      "subject": "Hello",
      "from": "sender@example.com",
      "to": ["alice@example.com"],
      "receivedAt": "2026-06-29T00:00:00Z",
      "snippet": "Preview text",
      "isRead": false,
      "hasAttachments": false
    }
  ],
  "nextCursor": ""
}
```

`nextCursor` is empty when there are no more pages. Otherwise it contains the offset to pass as `cursor` for the next request.

当没有更多分页时，`nextCursor` 为空字符串；否则它是下次请求应作为 `cursor` 传入的偏移量。

Users can only read messages from their own active mailboxes.

用户只能读取自己拥有的 active 邮箱。
