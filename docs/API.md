# LanQin Email API

LanQin Email exposes integration-oriented APIs under `/api/open`.

这些接口用于外部系统集成，统一放在 `/api/open` 下。它们不是匿名公开接口，只接受 API Token，不接受浏览器登录 Session Cookie。

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

- Domain APIs require admin access and domain permissions.
- Mailbox management APIs require admin access and mailbox permissions.
- Sending mail requires `mail.send`.
- Reading mailbox messages and send status requires `mail.read`.

- 域名接口需要管理员访问权限和域名相关权限。
- 邮箱管理接口需要管理员访问权限和邮箱相关权限。
- 发送邮件需要 `mail.send`。
- 读取邮箱邮件和发信状态需要 `mail.read`。

## Domains

### List domains

```http
GET /api/open/domains
Authorization: Bearer lq_xxx
```

Response:

```json
{
  "items": [
    {
      "id": "dom_xxx",
      "name": "example.com",
      "status": "active",
      "dkimSelector": "lanqin",
      "dkimPublicKey": "...",
      "dnsStatus": "unchecked",
      "createdAt": "2026-06-29T00:00:00Z"
    }
  ]
}
```

### Create domain

```http
POST /api/open/domains
Authorization: Bearer lq_xxx
Content-Type: application/json

{
  "name": "example.com"
}
```

### Get domain

```http
GET /api/open/domains/{id}
Authorization: Bearer lq_xxx
```

### Update domain status

```http
POST /api/open/domains/{id}
Authorization: Bearer lq_xxx
Content-Type: application/json

{
  "status": "active"
}
```

`status` can be `active` or `disabled`.

### Delete domain

```http
DELETE /api/open/domains/{id}
Authorization: Bearer lq_xxx
```

Domains that still have mailboxes cannot be deleted.

## Mailboxes

### List mailboxes

```http
GET /api/open/mailboxes
Authorization: Bearer lq_xxx
```

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

`ownerEmail` is optional. If omitted, the mailbox address is used as the owner email. If an active user with that email does not exist, LanQin Email creates one.

也可以传 `userId` 绑定到已有用户。`password` 至少 8 位，并会用于邮箱密码。

### Get mailbox

```http
GET /api/open/mailboxes/{id}
Authorization: Bearer lq_xxx
```

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

All fields are optional. `status` can be `active` or `disabled`.

### Delete mailbox

```http
DELETE /api/open/mailboxes/{id}
Authorization: Bearer lq_xxx
```

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
  "html": "<p>HTML body</p>"
}
```

Response:

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
  "createdAt": "2026-06-29T00:00:00Z"
}
```

When SMTP delivery is not configured, the message can be stored as accepted without a queue item:

如果没有配置 SMTP 投递，邮件可能只会进入 `accepted` 状态，不会产生 `queueId`。

Current status values:

- `accepted`: message was accepted and stored, but no SMTP queue item exists.
- `queued`: queued for SMTP delivery.
- `sending`: currently being delivered.
- `delivered`: SMTP delivery succeeded.
- `failed`: delivery failed and may be retried.
- `canceled`: delivery was canceled.

Bounce, complaint, rejection, and provider-specific delivery events require future webhook or delivery-event integration.

退信、投诉、拒收等更细状态需要后续接入投递事件或 webhook 后才能完整提供。

## Send Status

```http
GET /api/open/send/{id}
Authorization: Bearer lq_xxx
```

`id` can be the value returned by `POST /api/open/send`. If a queue item exists, it can also be the queue id.

`id` 可以使用发信接口返回的 `id`；如果存在队列项，也可以使用 `queueId`。

## Received Messages

```http
GET /api/open/mailboxes/{id}/messages?folder=Inbox&limit=30&cursor=0&q=keyword
Authorization: Bearer lq_xxx
```

Query parameters:

- `folder`: folder name. Defaults to `Inbox`; use `all` for all folders.
- `limit`: page size, maximum `100`.
- `cursor`: numeric cursor returned as `nextCursor`.
- `q`: optional search keyword.

Response:

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

Users can only read messages from their own active mailboxes.

用户只能读取自己拥有的 active 邮箱。
