import * as React from "react"
import { useQuery } from "@tanstack/react-query"
import { useSearchParams } from "react-router-dom"
import {
  ChevronRight,
  CircleCheck,
  CirclePause,
  Cloud,
  Container,
  FolderOpen,
  HardDrive,
  Info,
  Link2,
  MapPin,
  MoreHorizontal,
  Network,
  Pencil,
  Plus,
  RefreshCcw,
  ShieldCheck,
  Trash2,
  type LucideIcon,
} from "lucide-react"
import { api, type BackupDestination, type BackupDestinationPayload } from "@/lib/api"
import { uiMessage, uiText, useLanguage } from "@/lib/language"
import { errorMessage } from "@/lib/ui-errors"
import { cn, formatDateTime } from "@/lib/utils"
import { BackupUploadPanel } from "@/components/backup-upload-panel"
import { BackupSchedulePanel } from "@/components/backup-schedule-panel"
import { BackupDriveAuthorization } from "@/components/backup-drive-authorization"
import { useMe } from "@/hooks/use-me"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu"
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
  type: "s3" as "s3" | "webdav" | "drive",
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

function storageTypeName(type: BackupDestination["type"]) {
  return type === "s3" ? "S3" : type === "drive" ? "Google Drive" : "WebDAV"
}

function formFor(item?: BackupDestination) {
  return {
    ...emptyForm,
    ...(item && { name: item.name, type: item.type, disabled: item.disabled }),
    ...(item?.s3 || {}),
    url: item?.webdav?.url || "",
  }
}

function DestinationField({
  label,
  value,
  wide,
  icon: Icon,
}: {
  label: string
  value: string
  wide?: boolean
  icon: LucideIcon
}) {
  return (
    <div className={cn("min-w-0", wide && "sm:col-span-2")}>
      <dt className="flex items-center gap-1.5 text-xs text-muted-foreground">
        <Icon aria-hidden="true" className="h-3.5 w-3.5 shrink-0" />
        {label}
      </dt>
      <dd className="mt-1.5 break-words text-sm font-medium [overflow-wrap:anywhere]">{value}</dd>
    </div>
  )
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
  const [selectedId, setSelectedId] = React.useState("")
  const busy = React.useRef(false)
  const [searchParams, setSearchParams] = useSearchParams()
  const oauthResult = searchParams.get("driveOAuth")
  const [driveOutcome, setDriveOutcome] = React.useState<"success" | "failed" | null>(null)
  const refetchDestinations = destinations.refetch
  React.useEffect(() => {
    if (!allowed || (oauthResult !== "success" && oauthResult !== "failed")) return
    setDriveOutcome(oauthResult)
    const next = new URLSearchParams(searchParams)
    next.delete("driveOAuth")
    setSearchParams(next, { replace: true })
    void refetchDestinations()
  }, [allowed, oauthResult, searchParams, setSearchParams, refetchDestinations])
  const data = destinations.data
  const selected = data?.items.find((item) => item.id === selectedId) || data?.items[0]

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

  async function perform(action: () => Promise<unknown>, selectNew = false) {
    if (busy.current) return
    busy.current = true
    setPending(true)
    setError("")
    const previousIds = new Set(data?.items.map((item) => item.id))
    try {
      await action()
      closeEditor()
      setDeletion(null)
      const refreshed = await destinations.refetch()
      if (selectNew) {
        const added = refreshed.data?.items.find((item) => !previousIds.has(item.id))
        if (added) setSelectedId(added.id)
      }
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
        : form.type === "webdav"
          ? { webdav: { url: form.url, username: form.username, password: form.password } }
          : { drive: {} }),
    }
    void perform(
      () =>
        editor.id
          ? api.updateBackupDestination(editor.id, payload)
          : api.createBackupDestination(payload),
      !editor.id
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
        ...(item.type === "drive" && { drive: {} }),
      })
    )
  }

  if (!allowed) return <div role="alert">{uiText("只有超级管理员可以管理备份地点")}</div>

  const editable =
    !!data?.encryptionConfigured && !destinations.isError && !pending && !destinations.isFetching
  return (
    <div className="space-y-4" data-lanqin-i18n-ignore>
      {driveOutcome && (
        <div
          role={driveOutcome === "success" ? "status" : "alert"}
          className={cn(
            "rounded-lg border p-4 text-sm",
            driveOutcome === "failed" && "text-destructive"
          )}
        >
          {uiText(
            driveOutcome === "success"
              ? "Google Drive 授权成功，备份目录已绑定"
              : "Google Drive 授权未完成，请重新授权"
          )}
        </div>
      )}
      <section className="min-w-0 overflow-hidden rounded-xl border bg-background">
        <header className="flex flex-wrap items-center justify-between gap-3 border-b px-4 py-3 sm:px-5">
          <div className="flex items-center gap-2.5">
            <span className="text-sm font-medium">{uiText("存储地点")}</span>
            <Badge
              variant="secondary"
              className="tabular-nums"
              aria-label={uiText("共 {0} 个地点", [data?.items.length || 0])}
            >
              {data?.items.length || 0}
            </Badge>
          </div>
          <div className="flex items-center gap-2">
            <Button
              variant="ghost"
              size="icon"
              aria-label={uiText("刷新")}
              title={uiText("刷新")}
              disabled={pending || destinations.isFetching}
              onClick={() => {
                setError("")
                void destinations.refetch()
              }}
            >
              <RefreshCcw className={cn("h-4 w-4", destinations.isFetching && "animate-spin")} />
            </Button>
            <Button size="sm" disabled={!editable} onClick={() => openEditor()}>
              <Plus className="h-4 w-4" />
              {uiText("新增地点")}
            </Button>
          </div>
        </header>
        <div className="empty:hidden">
          {destinations.isPending && (
            <div role="status" className="px-5 py-8 text-sm text-muted-foreground">
              {uiText("加载中...")}
            </div>
          )}
          {destinations.isError && (
            <div role="alert" className="px-5 py-4 text-sm text-destructive">
              {uiText(errorMessage(destinations.error))}
            </div>
          )}
          {data && !data.encryptionConfigured && (
            <div role="alert" className="px-5 py-4 text-sm text-destructive">
              {uiText("请在服务器设置 LANQIN_BACKUP_SECRET_KEY：Base64 编码的 32 字节随机密钥")}
            </div>
          )}
          {error && !editor && !deletion && (
            <div role="alert" className="px-5 py-4 text-sm text-destructive">
              {uiText(error)}
            </div>
          )}
        </div>
        {data && !destinations.isError && (
          <div className="grid min-w-0 lg:grid-cols-[16rem_minmax(0,1fr)]">
            <section
              className="hidden min-w-0 flex-col border-r bg-muted/25 p-3 lg:flex"
              aria-label={uiText("选择备份地点")}
            >
              <div className="max-h-[calc(100svh-16rem)] space-y-1 overflow-y-auto" role="list">
                {data.items.map((item) => (
                  <div key={item.id} role="listitem">
                    <Button
                      variant="ghost"
                      disabled={pending}
                      aria-pressed={selected?.id === item.id}
                      aria-controls="backup-destination-details"
                      className={cn(
                        "h-auto w-full justify-start gap-3 whitespace-normal rounded-lg border border-transparent px-3 py-3 text-left shadow-none",
                        selected?.id === item.id
                          ? "border-border bg-background hover:bg-background"
                          : "hover:bg-background/70"
                      )}
                      onClick={() => setSelectedId(item.id)}
                    >
                      <span
                        className={cn(
                          "flex h-9 w-9 shrink-0 items-center justify-center rounded-lg border",
                          selected?.id === item.id
                            ? "border-foreground bg-foreground text-background"
                            : "bg-background text-muted-foreground"
                        )}
                      >
                        {item.type === "s3" ? (
                          <Cloud aria-hidden="true" />
                        ) : (
                          <HardDrive aria-hidden="true" />
                        )}
                      </span>
                      <span className="min-w-0 flex-1">
                        <span className="block truncate text-sm font-medium">{item.name}</span>
                        <span className="mt-1 flex items-center gap-1.5 text-xs font-normal text-muted-foreground">
                          <span>{storageTypeName(item.type)}</span>
                          <span aria-hidden>·</span>
                          <span className="inline-flex items-center gap-1">
                            {item.disabled ? (
                              <CirclePause aria-hidden="true" className="!h-3 !w-3" />
                            ) : (
                              <CircleCheck aria-hidden="true" className="!h-3 !w-3" />
                            )}
                            {uiText(item.disabled ? "已停用" : "已启用")}
                          </span>
                        </span>
                      </span>
                      {selected?.id === item.id && (
                        <ChevronRight className="text-muted-foreground" />
                      )}
                    </Button>
                  </div>
                ))}
              </div>
              {data.items.length === 0 && (
                <div className="px-3 py-6 text-sm text-muted-foreground">
                  {uiText("暂无备份地点")}
                </div>
              )}
            </section>
            <div className="min-w-0 border-b p-4 lg:hidden">
              <Label className="sr-only" htmlFor="backup-selected">
                {uiText("选择备份地点")}
              </Label>
              <Select
                value={selected?.id || ""}
                disabled={pending || !data.items.length}
                onValueChange={setSelectedId}
              >
                <SelectTrigger id="backup-selected">
                  <SelectValue placeholder={uiText("暂无备份地点")} />
                </SelectTrigger>
                <SelectContent data-lanqin-i18n-ignore>
                  {data.items.map((item) => (
                    <SelectItem key={item.id} value={item.id}>
                      {item.name}
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
            </div>
            <section
              id="backup-destination-details"
              className="min-w-0 p-5 sm:p-7"
              aria-label={uiText("连接参数")}
            >
              {selected ? (
                <div className="space-y-6">
                  <div className="flex flex-col justify-between gap-4 sm:flex-row sm:items-start">
                    <div className="flex min-w-0 items-center gap-3">
                      <span className="flex h-12 w-12 shrink-0 items-center justify-center rounded-xl border bg-muted/40 text-foreground">
                        {selected.type === "s3" ? (
                          <Cloud aria-hidden="true" className="h-6 w-6 stroke-[1.5]" />
                        ) : (
                          <HardDrive aria-hidden="true" className="h-6 w-6 stroke-[1.5]" />
                        )}
                      </span>
                      <div className="min-w-0 space-y-1.5">
                        <h2 className="break-words text-xl font-semibold tracking-tight [overflow-wrap:anywhere]">
                          {selected.name}
                        </h2>
                        <Badge variant="secondary" className="font-normal">
                          {storageTypeName(selected.type)}
                        </Badge>
                      </div>
                    </div>
                    <div className="flex shrink-0 items-center gap-1.5">
                      <Button
                        variant="outline"
                        size="sm"
                        disabled={!editable}
                        aria-label={uiText(uiMessage("编辑备份地点 {0}", [selected.name]))}
                        onClick={() => openEditor(selected)}
                      >
                        <Pencil className="h-4 w-4" />
                        {uiText("编辑")}
                      </Button>
                      <DropdownMenu>
                        <DropdownMenuTrigger asChild>
                          <Button
                            variant="ghost"
                            size="icon"
                            disabled={!editable}
                            aria-label={uiText("备份地点 {0} 的操作", [selected.name])}
                          >
                            <MoreHorizontal />
                          </Button>
                        </DropdownMenuTrigger>
                        <DropdownMenuContent align="end" data-lanqin-i18n-ignore>
                          <DropdownMenuItem
                            className="text-destructive focus:text-destructive"
                            onSelect={() => {
                              setDeletion({ item: selected, revision: data.revision })
                              setError("")
                            }}
                          >
                            <Trash2 className="mr-2 h-4 w-4" />
                            {uiText(uiMessage("删除备份地点 {0}", [selected.name]))}
                          </DropdownMenuItem>
                        </DropdownMenuContent>
                      </DropdownMenu>
                    </div>
                  </div>
                  <div className="flex items-center justify-between gap-4 border-y py-4">
                    <Label htmlFor="backup-destination-enabled" className="text-sm font-normal">
                      {uiText("启用地点")}
                    </Label>
                    <Switch
                      id="backup-destination-enabled"
                      checked={!selected.disabled}
                      disabled={!editable}
                      onCheckedChange={() => toggle(selected)}
                    />
                  </div>
                  <dl className="grid gap-x-6 gap-y-5 sm:grid-cols-2">
                    {selected.type === "s3" && selected.s3 ? (
                      <>
                        <DestinationField
                          icon={Link2}
                          label={uiText("S3 端点")}
                          value={selected.s3.endpoint}
                          wide
                        />
                        <DestinationField
                          icon={Container}
                          label={uiText("存储桶")}
                          value={selected.s3.bucket}
                        />
                        <DestinationField
                          icon={MapPin}
                          label={uiText("区域")}
                          value={selected.s3.region}
                        />
                        <DestinationField
                          icon={FolderOpen}
                          label={uiText("对象前缀")}
                          value={selected.s3.prefix || uiText("未设置")}
                        />
                        <DestinationField
                          icon={Network}
                          label={uiText("寻址方式")}
                          value={uiText(selected.s3.pathStyle ? "路径式寻址" : "虚拟主机式寻址")}
                        />
                      </>
                    ) : selected.type === "drive" ? (
                      <DestinationField
                        icon={FolderOpen}
                        label={uiText("Google Drive 文件夹 ID")}
                        value={selected.drive?.folderId || uiText("授权后自动创建")}
                        wide
                      />
                    ) : (
                      <DestinationField
                        icon={Link2}
                        label={uiText("WebDAV 目录地址")}
                        value={selected.webdav?.url || ""}
                        wide
                      />
                    )}
                  </dl>
                  <dl>
                    <DestinationField
                      icon={CircleCheck}
                      label={uiText("最近成功上传")}
                      value={
                        selected.lastSuccessAt
                          ? formatDateTime(selected.lastSuccessAt)
                          : uiText("暂无成功记录")
                      }
                    />
                  </dl>
                  <div className="flex items-center gap-2 rounded-lg bg-muted/40 px-3 py-3 text-sm">
                    <ShieldCheck className="h-4 w-4 shrink-0 text-muted-foreground" />
                    {uiText(selected.credentialsSet ? "凭据已加密保存" : "未配置")}
                  </div>
                  {selected.type === "drive" && (
                    <BackupDriveAuthorization
                      key={selected.id}
                      userId={me.data?.user.id || ""}
                      destination={selected}
                      revision={data.revision}
                      editable={editable}
                    />
                  )}
                  <div
                    role="status"
                    className="flex items-start gap-2.5 border-t pt-5 text-sm text-muted-foreground"
                  >
                    <Info className="mt-0.5 h-4 w-4 shrink-0" />
                    {uiText("可新建不停服在线备份或上传已有归档；生产覆盖恢复尚未接入")}
                  </div>
                  <BackupSchedulePanel
                    key={selected.id}
                    userId={me.data?.user.id || ""}
                    destination={selected}
                    destinationsRevision={data.revision}
                    editable={editable}
                  />
                </div>
              ) : (
                <div className="flex min-h-72 flex-col items-center justify-center gap-4 text-muted-foreground">
                  <Cloud className="h-8 w-8 stroke-1" />
                  <div className="text-sm">{uiText("请选择或新增备份地点")}</div>
                </div>
              )}
            </section>
          </div>
        )}
      </section>

      {data && !destinations.isError && (
        <BackupUploadPanel
          userId={me.data?.user.id || ""}
          destinations={data.items}
          revision={data.revision}
          preferredId={selected?.id}
          canUpload={editable}
        />
      )}

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
                onValueChange={(type: "s3" | "webdav" | "drive") =>
                  setForm({ ...emptyForm, name: form.name, type })
                }
              >
                <SelectTrigger id="backup-type">
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  <SelectItem value="s3">{"S3"}</SelectItem>
                  <SelectItem value="webdav">{"WebDAV"}</SelectItem>
                  <SelectItem value="drive">{"Google Drive"}</SelectItem>
                </SelectContent>
              </Select>
            </div>
            {editor?.id && form.type !== "drive" && (
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
            ) : form.type === "webdav" ? (
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
            ) : (
              <div role="status" className="rounded-md border p-3 text-sm">
                {uiText("保存地点后通过 Google OAuth 授权；无需填写网盘密码、令牌或文件夹 ID")}
              </div>
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
