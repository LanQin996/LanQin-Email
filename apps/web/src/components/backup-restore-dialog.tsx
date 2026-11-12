import * as React from "react"
import {
  api,
  type BackupDestination,
  type BackupDiscoveryEntry,
  type BackupUploadJob,
} from "@/lib/api"
import { uiText, useLanguage } from "@/lib/language"
import { errorMessage } from "@/lib/ui-errors"
import { formatBytes, formatDateTime } from "@/lib/utils"
import { Button } from "@/components/ui/button"
import { Checkbox } from "@/components/ui/checkbox"
import {
  Dialog,
  DialogContent,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import { PasswordInput } from "@/components/ui/password-input"
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select"

export function BackupRestoreDialog({
  source,
  destinations,
  revision,
  available,
  discoveryEntry,
  maxBytes,
  onCreated,
}: {
  source: BackupUploadJob
  destinations: BackupDestination[]
  revision: string
  available: boolean
  discoveryEntry?: BackupDiscoveryEntry
  maxBytes?: number
  onCreated: (job: BackupUploadJob) => void
}) {
  useLanguage()
  const choices =
    discoveryEntry && source.kind === "discover"
      ? source.locations
          .filter((location) =>
            destinations.some((d) => d.id === location.id && !d.disabled && d.credentialsSet)
          )
          .map((location) => ({ destination_id: location.id }))
      : source.results.filter(
          (result) =>
            result.state === "succeeded" &&
            !!result.object_key &&
            !source.pruned?.[result.destination_id] &&
            destinations.some(
              (d) => d.id === result.destination_id && !d.disabled && d.credentialsSet
            )
        )
  const exceedsLimit = !!discoveryEntry && !!maxBytes && discoveryEntry.bytes > maxBytes
  const unknownLimit = !!discoveryEntry && (!maxBytes || maxBytes <= 0)
  const [editor, setEditor] = React.useState<{
    revision: string
    destinationId: string
    requestId: string
    step: number
    encryptionMode: "password" | "key"
  } | null>(null)
  const [pending, setPending] = React.useState(false)
  const [confirmed, setConfirmed] = React.useState(false)
  const [error, setError] = React.useState("")
  const busy = React.useRef(false)
  const id = React.useId()

  async function submit(event: React.FormEvent<HTMLFormElement>) {
    event.preventDefault()
    if (!editor || busy.current || !confirmed || !available || exceedsLimit || unknownLimit) return
    const form = event.currentTarget
    const fields = new FormData(form)
    const payload = {
      sourceJobId: source.id,
      ...(discoveryEntry && { discoveryEntryId: discoveryEntry.id }),
      destinationId: editor.destinationId,
      expectedSHA256: discoveryEntry?.sha256 || source.artifact.sha256,
      destinationsRevision: editor.revision,
      requestId: editor.requestId,
      encryptionMode: editor.encryptionMode,
      identity: editor.encryptionMode === "key" ? String(fields.get("identity") || "").trim() : "",
      backupPassword:
        editor.encryptionMode === "password" ? String(fields.get("backupPassword") || "") : "",
      password: String(fields.get("password") || ""),
      twoFactorCode: String(fields.get("twoFactorCode") || "").trim(),
      confirm: true,
    }
    if (!(payload.identity || payload.backupPassword) || !payload.password) {
      setError("请填写备份密码或恢复私钥，以及当前管理员密码")
      payload.identity = payload.backupPassword = payload.password = payload.twoFactorCode = ""
      fields.delete("identity")
      fields.delete("backupPassword")
      fields.delete("password")
      fields.delete("twoFactorCode")
      return
    }
    busy.current = true
    setPending(true)
    setError("")
    setConfirmed(false)
    // Secrets never enter React Query, persistent browser storage or a report.
    // Clear controls as soon as the request has been constructed, also on errors.
    form.reset()
    try {
      const job = await api.createBackupRestore(payload)
      setEditor(null)
      setConfirmed(false)
      onCreated(job)
    } catch (e) {
      setError(errorMessage(e))
    } finally {
      payload.identity = payload.backupPassword = payload.password = payload.twoFactorCode = ""
      fields.delete("identity")
      fields.delete("backupPassword")
      fields.delete("password")
      fields.delete("twoFactorCode")
      busy.current = false
      setPending(false)
    }
  }

  if (
    source.kind === "restore" ||
    (source.kind === "discover" && (!discoveryEntry || !source.discovery?.ready)) ||
    (source.status !== "succeeded" && source.status !== "partial") ||
    !choices.length
  )
    return null

  return (
    <div className="space-y-3">
      <Button
        variant="outline"
        disabled={!available || exceedsLimit || unknownLimit || pending}
        onClick={() => {
          setEditor({
            revision,
            destinationId: choices[0].destination_id,
            requestId: Array.from(crypto.getRandomValues(new Uint8Array(16)), (b) =>
              b.toString(16).padStart(2, "0")
            ).join(""),
            step: 1,
            encryptionMode:
              source.recipient && !source.recipient.startsWith("password:") ? "key" : "password",
          })
          setError("")
          setConfirmed(false)
        }}
      >
        {uiText("恢复此备份")}
      </Button>
      {exceedsLimit && (
        <div role="status" className="text-sm text-muted-foreground">
          {uiText("此备份超过本实例恢复上限 {0}；请使用独立 CLI", [formatBytes(maxBytes!)])}
        </div>
      )}
      {unknownLimit && (
        <div role="status" className="text-sm text-muted-foreground">
          {uiText("无法确认本实例恢复大小上限，请刷新后重试")}
        </div>
      )}
      {!available && !exceedsLimit && !unknownLimit && (
        <div role="status" className="text-sm text-muted-foreground">
          {uiText("服务器尚未配置私有恢复工作区")}
        </div>
      )}
      <Dialog
        open={!!editor}
        onOpenChange={(open) => {
          if (!open && !pending) {
            setEditor(null)
            setError("")
            setConfirmed(false)
          }
        }}
      >
        <DialogContent
          className="max-h-[90svh] overflow-y-auto"
          aria-describedby={undefined}
          data-lanqin-i18n-ignore
        >
          <DialogHeader>
            <DialogTitle>
              {uiText(editor?.step === 1 ? "选择恢复副本" : "验证并暂存备份")}
            </DialogTitle>
          </DialogHeader>
          {editor?.step === 1 ? (
            <div className="space-y-4">
              <div className="space-y-2">
                <Label htmlFor={`${id}-destination`}>{uiText("备份地点")}</Label>
                <Select
                  value={editor.destinationId}
                  onValueChange={(destinationId) =>
                    setEditor((current) => current && { ...current, destinationId })
                  }
                >
                  <SelectTrigger id={`${id}-destination`}>
                    <SelectValue />
                  </SelectTrigger>
                  <SelectContent data-lanqin-i18n-ignore>
                    {choices.map((result) => (
                      <SelectItem key={result.destination_id} value={result.destination_id}>
                        {source.locations.find((l) => l.id === result.destination_id)?.name ||
                          result.destination_id}
                      </SelectItem>
                    ))}
                  </SelectContent>
                </Select>
              </div>
              <dl className="grid gap-3 text-sm sm:grid-cols-2">
                <div>
                  <dt>{uiText(discoveryEntry ? "索引记录完成时间" : "归档发布时间")}</dt>
                  <dd>
                    {formatDateTime(discoveryEntry?.completedAt || source.artifact.publishedAt)}
                  </dd>
                </div>
                <div>
                  <dt>{uiText("加密文件大小")}</dt>
                  <dd>{formatBytes(discoveryEntry?.bytes || source.artifact.bytes)}</dd>
                </div>
                <div className="min-w-0 sm:col-span-2">
                  <dt>{uiText("归档 SHA-256")}</dt>
                  <dd className="break-all font-mono text-xs">
                    {discoveryEntry?.sha256 || source.artifact.sha256}
                  </dd>
                </div>
              </dl>
              <div role="note" className="rounded-md border p-3 text-sm">
                {uiText(
                  discoveryEntry
                    ? "按已认证的云端索引下载，再验证密文、解密和数据库；索引不保证数据仍存在，失败不会覆盖当前邮件"
                    : "使用本实例保存的成功上传收据下载，解密后检查格式、完整性和数据库；失败不会覆盖当前邮件"
                )}
              </div>
              <DialogFooter>
                <Button variant="outline" onClick={() => setEditor(null)}>
                  {uiText("取消")}
                </Button>
                <Button onClick={() => setEditor((current) => current && { ...current, step: 2 })}>
                  {uiText("下一步")}
                </Button>
              </DialogFooter>
            </div>
          ) : (
            <form
              key={editor?.encryptionMode}
              className="space-y-4"
              onSubmit={submit}
              autoComplete="off"
            >
              <div className="space-y-2">
                <Label htmlFor={`${id}-encryption-mode`}>{uiText("备份的加密方式")}</Label>
                <Select
                  value={editor?.encryptionMode || "password"}
                  disabled={pending}
                  onValueChange={(value) => {
                    setEditor(
                      (current) =>
                        current && { ...current, encryptionMode: value as "password" | "key" }
                    )
                    setConfirmed(false)
                    setError("")
                  }}
                >
                  <SelectTrigger id={`${id}-encryption-mode`}>
                    <SelectValue />
                  </SelectTrigger>
                  <SelectContent data-lanqin-i18n-ignore>
                    <SelectItem value="password">{uiText("管理员自设备份密码")}</SelectItem>
                    <SelectItem value="key">{uiText("历史私钥加密备份")}</SelectItem>
                  </SelectContent>
                </Select>
              </div>
              <div className="space-y-2">
                <Label htmlFor={`${id}-decryption`}>
                  {uiText(
                    editor?.encryptionMode === "password"
                      ? "创建此备份时的备份密码"
                      : "历史备份的 age 恢复私钥"
                  )}
                </Label>
                <PasswordInput
                  id={`${id}-decryption`}
                  name={editor?.encryptionMode === "password" ? "backupPassword" : "identity"}
                  required
                  maxLength={editor?.encryptionMode === "password" ? 1024 : 256}
                  autoComplete="off"
                  spellCheck={false}
                  disabled={pending}
                />
              </div>
              <div className="space-y-2">
                <Label htmlFor={`${id}-password`}>{uiText("当前管理员密码")}</Label>
                <PasswordInput
                  id={`${id}-password`}
                  name="password"
                  required
                  maxLength={72}
                  autoComplete="off"
                  disabled={pending}
                />
              </div>
              <div className="space-y-2">
                <Label htmlFor={`${id}-totp`}>{uiText("两步验证码（已启用时必填）")}</Label>
                <Input
                  id={`${id}-totp`}
                  name="twoFactorCode"
                  inputMode="numeric"
                  pattern="[0-9]{6}"
                  maxLength={6}
                  autoComplete="off"
                  disabled={pending}
                />
              </div>
              <div role="note" className="rounded-md border p-3 text-sm">
                {uiText(
                  "备份密码或私钥仅在此次恢复任务的内存中使用，不写入配置或任务记录，也不提交给云存储。隔离暂存成功不等于生产覆盖恢复，切换仍需维护窗口"
                )}
              </div>
              <div className="flex items-start gap-3">
                <Checkbox
                  id={`${id}-confirm`}
                  checked={confirmed}
                  disabled={pending}
                  onCheckedChange={(checked) => setConfirmed(checked === true)}
                />
                <Label htmlFor={`${id}-confirm`} className="leading-normal">
                  {uiText(
                    "我确认将备份密码或恢复私钥提交给当前邮件服务器，用于下载、解密和隔离验证此次备份"
                  )}
                </Label>
              </div>
              {error && (
                <div role="alert" className="text-sm text-destructive">
                  {uiText(error)}
                </div>
              )}
              <DialogFooter>
                <Button
                  type="button"
                  variant="outline"
                  disabled={pending}
                  onClick={() => {
                    setEditor((current) => current && { ...current, step: 1 })
                    setError("")
                  }}
                >
                  {uiText("上一步")}
                </Button>
                <Button type="submit" disabled={pending || !confirmed}>
                  {uiText(pending ? "处理中..." : "开始恢复验证")}
                </Button>
              </DialogFooter>
            </form>
          )}
        </DialogContent>
      </Dialog>
    </div>
  )
}
