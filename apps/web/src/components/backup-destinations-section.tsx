import * as React from "react"
import { useQuery } from "@tanstack/react-query"
import { Pencil, Plus, RefreshCcw, Trash2 } from "lucide-react"
import { api, type BackupDestination, type BackupDestinationPayload } from "@/lib/api"
import { uiMessage, uiText, useLanguage } from "@/lib/language"
import { errorMessage } from "@/lib/ui-errors"
import { useMe } from "@/hooks/use-me"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card"
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
import { Switch } from "@/components/ui/switch"

const emptyForm = {
  name: "",
  type: "s3" as "s3" | "webdav",
  disabled: false,
  endpoint: "",
  region: "auto",
  bucket: "",
  prefix: "",
  pathStyle: true,
  accessKeyId: "",
  secretAccessKey: "",
  sessionToken: "",
  clearSessionToken: false,
  url: "",
  username: "",
  password: "",
}

function formFor(item?: BackupDestination) {
  return {
    ...emptyForm,
    ...(item && { name: item.name, type: item.type, disabled: item.disabled }),
    ...(item?.s3 || {}),
    url: item?.webdav?.url || "",
  }
}

export function BackupDestinationsSection() {
  useLanguage()
  const me = useMe()
  const allowed = me.data?.user.role === "admin"
  const destinations = useQuery({
    queryKey: ["admin", "backup-destinations", me.data?.user.id],
    queryFn: api.backupDestinations,
    enabled: allowed,
    retry: false,
  })
  const [editor, setEditor] = React.useState<{ id?: string; revision: string } | null>(null)
  const [deletion, setDeletion] = React.useState<{
    item: BackupDestination
    revision: string
  } | null>(null)
  const [form, setForm] = React.useState(emptyForm)
  const [pending, setPending] = React.useState(false)
  const [error, setError] = React.useState("")
  const busy = React.useRef(false)
  const data = destinations.data

  function closeEditor() {
    setEditor(null)
    setForm({ ...emptyForm }) // No secrets in query/mutation caches or persistent browser storage.
    setError("")
  }

  function openEditor(item?: BackupDestination) {
    if (!data || pending) return
    setForm(formFor(item))
    setEditor({ id: item?.id, revision: data.revision })
    setError("")
  }

  async function perform(action: () => Promise<unknown>) {
    if (busy.current) return
    busy.current = true
    setPending(true)
    setError("")
    try {
      await action()
      closeEditor()
      setDeletion(null)
      await destinations.refetch()
    } catch (e) {
      setError(errorMessage(e)) // Safe source text is translated when rendered.
      void destinations.refetch()
    } finally {
      busy.current = false
      setPending(false)
    }
  }

  function submit(event: React.FormEvent<HTMLFormElement>) {
    event.preventDefault()
    if (!editor) return
    const payload: BackupDestinationPayload = {
      revision: editor.revision,
      name: form.name,
      type: form.type,
      disabled: form.disabled,
      ...(form.type === "s3"
        ? {
            s3: {
              endpoint: form.endpoint,
              region: form.region,
              bucket: form.bucket,
              prefix: form.prefix,
              pathStyle: form.pathStyle,
              accessKeyId: form.accessKeyId,
              secretAccessKey: form.secretAccessKey,
              sessionToken: form.sessionToken,
              clearSessionToken: form.clearSessionToken,
            },
          }
        : { webdav: { url: form.url, username: form.username, password: form.password } }),
    }
    void perform(() =>
      editor.id
        ? api.updateBackupDestination(editor.id, payload)
        : api.createBackupDestination(payload)
    )
  }

  function toggle(item: BackupDestination) {
    if (!data) return
    void perform(() =>
      api.updateBackupDestination(item.id, {
        revision: data.revision,
        name: item.name,
        type: item.type,
        disabled: !item.disabled,
        s3: item.s3,
        webdav: item.webdav,
      })
    )
  }

  if (!allowed) return <div role="alert">{uiText("只有超级管理员可以管理备份地点")}</div>

  const editable =
    !!data?.encryptionConfigured && !destinations.isError && !pending && !destinations.isFetching
  return (
    <div className="space-y-4" data-lanqin-i18n-ignore>
      <Card>
        <CardHeader className="flex flex-row flex-wrap items-center justify-between gap-2">
          <CardTitle>{uiText("备份地点")}</CardTitle>
          <div className="flex flex-wrap gap-2">
            <Button
              variant="outline"
              disabled={pending || destinations.isFetching}
              onClick={() => {
                setError("")
                void destinations.refetch()
              }}
            >
              <RefreshCcw className="h-4 w-4" />
              {uiText("刷新")}
            </Button>
            <Button disabled={!editable} onClick={() => openEditor()}>
              <Plus className="h-4 w-4" />
              {uiText("新增地点")}
            </Button>
          </div>
        </CardHeader>
        <CardContent className="space-y-4">
          <div role="status" className="rounded-md border p-3 text-sm">
            {uiText("当前仅管理备份地点，后台备份任务和定时计划尚未接入")}
          </div>
          {destinations.isPending && <div role="status">{uiText("加载中...")}</div>}
          {destinations.isError && (
            <div role="alert" className="text-sm text-destructive">
              {uiText(errorMessage(destinations.error))}
            </div>
          )}
          {data && !data.encryptionConfigured && (
            <div role="alert" className="text-sm text-destructive">
              {uiText("请在服务器设置 LANQIN_BACKUP_SECRET_KEY：Base64 编码的 32 字节随机密钥")}
            </div>
          )}
          {error && !editor && !deletion && (
            <div role="alert" className="text-sm text-destructive">
              {uiText(error)}
            </div>
          )}
          {data && !destinations.isError && data.items.length === 0 && (
            <div className="text-sm text-muted-foreground">{uiText("暂无备份地点")}</div>
          )}
          {data && !destinations.isError && (
            <div className="grid gap-4 xl:grid-cols-2">
              {data.items.map((item) => (
                <Card key={item.id}>
                  <CardHeader className="gap-2">
                    <div className="flex items-center justify-between gap-2">
                      <CardTitle className="break-all text-base">{item.name}</CardTitle>
                      <Badge variant="secondary">{item.type === "s3" ? "S3" : "WebDAV"}</Badge>
                    </div>
                  </CardHeader>
                  <CardContent className="space-y-3">
                    <div className="flex flex-wrap gap-2">
                      <Badge variant={item.disabled ? "outline" : "secondary"}>
                        {uiText(item.disabled ? "已停用" : "已启用")}
                      </Badge>
                      <Badge variant="outline">{uiText("凭据已加密保存")}</Badge>
                    </div>
                    <div className="break-all text-sm text-muted-foreground">
                      {item.s3?.endpoint || item.webdav?.url}
                    </div>
                    {item.s3 && (
                      <div className="break-all text-sm">
                        {item.s3.bucket}
                        {item.s3.prefix ? ` / ${item.s3.prefix}` : ""}
                      </div>
                    )}
                    <div className="flex flex-wrap gap-2">
                      <Button
                        variant="outline"
                        disabled={!editable}
                        aria-label={uiText(uiMessage("编辑备份地点 {0}", [item.name]))}
                        onClick={() => openEditor(item)}
                      >
                        <Pencil className="h-4 w-4" />
                        {uiText("编辑")}
                      </Button>
                      <Button variant="outline" disabled={!editable} onClick={() => toggle(item)}>
                        {uiText(item.disabled ? "启用" : "停用")}
                      </Button>
                      <Button
                        variant="ghost"
                        className="text-destructive"
                        disabled={!editable}
                        aria-label={uiText(uiMessage("删除备份地点 {0}", [item.name]))}
                        onClick={() => {
                          setDeletion({ item, revision: data.revision })
                          setError("")
                        }}
                      >
                        <Trash2 className="h-4 w-4" />
                        {uiText("删除")}
                      </Button>
                    </div>
                  </CardContent>
                </Card>
              ))}
            </div>
          )}
        </CardContent>
      </Card>

      <Dialog
        open={!!editor}
        onOpenChange={(open) => {
          if (!open && !pending) closeEditor()
        }}
      >
        <DialogContent
          className="max-h-[90svh] overflow-y-auto"
          aria-describedby={undefined}
          data-lanqin-i18n-ignore
        >
          <form onSubmit={submit} className="space-y-4">
            <DialogHeader>
              <DialogTitle>{uiText(editor?.id ? "编辑备份地点" : "新增地点")}</DialogTitle>
            </DialogHeader>
            <div className="space-y-2">
              <Label htmlFor="backup-name">{uiText("名称")}</Label>
              <Input
                id="backup-name"
                value={form.name}
                maxLength={128}
                required
                disabled={pending}
                onChange={(e) => setForm({ ...form, name: e.target.value })}
              />
            </div>
            <div className="space-y-2">
              <Label htmlFor="backup-type">{uiText("存储类型")}</Label>
              <Select
                value={form.type}
                disabled={pending || !!editor?.id}
                onValueChange={(type: "s3" | "webdav") =>
                  setForm({ ...emptyForm, name: form.name, type })
                }
              >
                <SelectTrigger id="backup-type">
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  <SelectItem value="s3">{"S3"}</SelectItem>
                  <SelectItem value="webdav">{"WebDAV"}</SelectItem>
                </SelectContent>
              </Select>
            </div>
            {editor?.id && (
              <div role="status" className="rounded-md border p-3 text-sm">
                {uiText("凭据留空以保留；更换地址需重新填写，更新密钥对会清除旧会话令牌")}
              </div>
            )}
            {form.type === "s3" ? (
              <>
                <div className="space-y-2">
                  <Label htmlFor="backup-endpoint">{uiText("S3 端点")}</Label>
                  <Input
                    id="backup-endpoint"
                    value={form.endpoint}
                    type="url"
                    required
                    disabled={pending}
                    onChange={(e) => setForm({ ...form, endpoint: e.target.value })}
                  />
                </div>
                <div className="grid gap-4 sm:grid-cols-2">
                  <div className="space-y-2">
                    <Label htmlFor="backup-region">{uiText("区域")}</Label>
                    <Input
                      id="backup-region"
                      value={form.region}
                      required
                      disabled={pending}
                      onChange={(e) => setForm({ ...form, region: e.target.value })}
                    />
                  </div>
                  <div className="space-y-2">
                    <Label htmlFor="backup-bucket">{uiText("存储桶")}</Label>
                    <Input
                      id="backup-bucket"
                      value={form.bucket}
                      required
                      disabled={pending}
                      onChange={(e) => setForm({ ...form, bucket: e.target.value })}
                    />
                  </div>
                </div>
                <div className="space-y-2">
                  <Label htmlFor="backup-prefix">{uiText("对象前缀")}</Label>
                  <Input
                    id="backup-prefix"
                    value={form.prefix}
                    disabled={pending}
                    onChange={(e) => setForm({ ...form, prefix: e.target.value })}
                  />
                </div>
                <div className="flex items-center gap-3">
                  <Switch
                    id="backup-path-style"
                    checked={form.pathStyle}
                    disabled={pending}
                    onCheckedChange={(pathStyle) => setForm({ ...form, pathStyle })}
                  />
                  <Label htmlFor="backup-path-style">{uiText("路径式寻址")}</Label>
                </div>
                <div className="space-y-2">
                  <Label htmlFor="backup-access-key">{uiText("访问密钥 ID")}</Label>
                  <PasswordInput
                    id="backup-access-key"
                    value={form.accessKeyId}
                    maxLength={8192}
                    autoComplete="new-password"
                    required={!editor?.id}
                    disabled={pending}
                    onChange={(e) => setForm({ ...form, accessKeyId: e.target.value })}
                  />
                </div>
                <div className="space-y-2">
                  <Label htmlFor="backup-secret-key">{uiText("访问密钥 Secret")}</Label>
                  <PasswordInput
                    id="backup-secret-key"
                    value={form.secretAccessKey}
                    maxLength={8192}
                    autoComplete="new-password"
                    required={!editor?.id}
                    disabled={pending}
                    onChange={(e) => setForm({ ...form, secretAccessKey: e.target.value })}
                  />
                </div>
                <div className="space-y-2">
                  <Label htmlFor="backup-session-token">{uiText("会话令牌（可选）")}</Label>
                  <PasswordInput
                    id="backup-session-token"
                    value={form.sessionToken}
                    maxLength={8192}
                    autoComplete="new-password"
                    disabled={pending || form.clearSessionToken}
                    onChange={(e) => setForm({ ...form, sessionToken: e.target.value })}
                  />
                </div>
                {editor?.id && (
                  <div className="flex items-center gap-3">
                    <Switch
                      id="backup-clear-token"
                      checked={form.clearSessionToken}
                      disabled={pending}
                      onCheckedChange={(clearSessionToken) =>
                        setForm({ ...form, clearSessionToken, sessionToken: "" })
                      }
                    />
                    <Label htmlFor="backup-clear-token">{uiText("清除已保存的会话令牌")}</Label>
                  </div>
                )}
              </>
            ) : (
              <>
                <div className="space-y-2">
                  <Label htmlFor="backup-dav-url">{uiText("WebDAV 目录地址")}</Label>
                  <Input
                    id="backup-dav-url"
                    value={form.url}
                    type="url"
                    required
                    disabled={pending}
                    onChange={(e) => setForm({ ...form, url: e.target.value })}
                  />
                </div>
                <div className="space-y-2">
                  <Label htmlFor="backup-username">{uiText("用户名")}</Label>
                  <PasswordInput
                    id="backup-username"
                    value={form.username}
                    maxLength={8192}
                    autoComplete="new-password"
                    required={!editor?.id}
                    disabled={pending}
                    onChange={(e) => setForm({ ...form, username: e.target.value })}
                  />
                </div>
                <div className="space-y-2">
                  <Label htmlFor="backup-password">{uiText("密码")}</Label>
                  <PasswordInput
                    id="backup-password"
                    value={form.password}
                    maxLength={8192}
                    autoComplete="new-password"
                    required={!editor?.id}
                    disabled={pending}
                    onChange={(e) => setForm({ ...form, password: e.target.value })}
                  />
                </div>
              </>
            )}
            {error && (
              <div role="alert" className="text-sm text-destructive">
                {uiText(error)}
              </div>
            )}
            <DialogFooter>
              <Button type="button" variant="outline" disabled={pending} onClick={closeEditor}>
                {uiText("取消")}
              </Button>
              <Button type="submit" disabled={pending}>
                {uiText(pending ? "处理中..." : "保存")}
              </Button>
            </DialogFooter>
          </form>
        </DialogContent>
      </Dialog>
      <Dialog
        open={!!deletion}
        onOpenChange={(open) => {
          if (!open && !pending) {
            setDeletion(null)
            setError("")
          }
        }}
      >
        <DialogContent aria-describedby={undefined} data-lanqin-i18n-ignore>
          <DialogHeader>
            <DialogTitle>
              {uiText(uiMessage("删除备份地点 {0}", [deletion?.item.name || ""]))}
            </DialogTitle>
          </DialogHeader>
          <div>{uiText("仅删除地点配置，不会删除远端备份")}</div>
          {error && (
            <div role="alert" className="text-sm text-destructive">
              {uiText(error)}
            </div>
          )}
          <DialogFooter>
            <Button
              variant="outline"
              disabled={pending}
              onClick={() => {
                setDeletion(null)
                setError("")
              }}
            >
              {uiText("取消")}
            </Button>
            <Button
              variant="destructive"
              disabled={pending}
              onClick={() => {
                if (deletion)
                  void perform(() =>
                    api.deleteBackupDestination(deletion.item.id, deletion.revision)
                  )
              }}
            >
              {uiText(pending ? "处理中..." : "删除")}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </div>
  )
}
