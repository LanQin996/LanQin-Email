import * as React from "react"
import { useQuery, useQueryClient } from "@tanstack/react-query"
import { Check, Copy, KeyRound, Pencil, RefreshCcw, ShieldCheck } from "lucide-react"
import { api, type BackupDriveClientConfiguration } from "@/lib/api"
import { uiText, useLanguage } from "@/lib/language"
import { ApiError, errorMessage } from "@/lib/ui-errors"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
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

const emptyConfiguration: BackupDriveClientConfiguration = {
  source: "none",
  configured: false,
  editable: false,
  clientId: "",
  clientSecretSet: false,
  revision: "",
  redirectUri: "",
}

export function BackupDriveClientSettings({ userId }: { userId: string }) {
  useLanguage()
  const queryClient = useQueryClient()
  const [open, setOpen] = React.useState(false)
  const [clientId, setClientId] = React.useState("")
  const [clientSecret, setClientSecret] = React.useState("")
  const [error, setError] = React.useState("")
  const [pending, setPending] = React.useState(false)
  const [copied, setCopied] = React.useState(false)
  const [editor, setEditor] = React.useState<BackupDriveClientConfiguration | null>(null)
  const busy = React.useRef(false)
  const formId = React.useId()
  const queryKey = ["admin", "backup-drive-client", userId]
  const configuration = useQuery({
    queryKey,
    queryFn: api.backupDriveClientConfiguration,
    enabled: !!userId,
    retry: false,
  })
  const value = configuration.data || emptyConfiguration
  const locked = value.source === "environment" && !value.editable
  const configured = value.configured && value.clientSecretSet

  function clearForm() {
    setClientId("")
    setClientSecret("")
    setError("")
    setCopied(false)
    setEditor(null)
  }

  function setDialog(next: boolean) {
    if (pending) return
    setOpen(next)
    if (!next) clearForm()
    else {
      setEditor(value)
      setClientId(value.clientId)
      setClientSecret("")
      setError("")
    }
  }

  async function submit(event: React.FormEvent<HTMLFormElement>) {
    event.preventDefault()
    if (busy.current || !editor?.editable || !clientId.trim() || !editor.revision) return
    busy.current = true
    setPending(true)
    setError("")
    const secret = clientSecret
    const payload = { revision: editor.revision, clientId: clientId.trim(), clientSecret: secret }
    setClientSecret("")
    try {
      await api.saveBackupDriveClientConfiguration(payload)
      await Promise.all([
        queryClient.invalidateQueries({ queryKey }),
        queryClient.invalidateQueries({ queryKey: ["admin", "backup-capabilities", userId] }),
        queryClient.invalidateQueries({ queryKey: ["admin", "backup-destinations", userId] }),
      ])
      setOpen(false)
      clearForm()
    } catch (cause) {
      setError(errorMessage(cause))
      if (cause instanceof ApiError && cause.status === 409) {
        await queryClient.invalidateQueries({ queryKey })
      }
    } finally {
      payload.clientSecret = ""
      setClientSecret("")
      busy.current = false
      setPending(false)
    }
  }

  async function copyRedirect() {
    if (!value.redirectUri) return
    try {
      await navigator.clipboard.writeText(value.redirectUri)
      setCopied(true)
    } catch {
      setError("无法复制回调地址，请手动选择并复制")
    }
  }

  return (
    <div className="space-y-3" data-lanqin-i18n-ignore>
      <div className="flex flex-wrap items-center justify-between gap-3 rounded-md border bg-muted/20 p-3">
        <div className="flex min-w-0 items-center gap-2">
          <ShieldCheck className="h-4 w-4 shrink-0 text-muted-foreground" aria-hidden="true" />
          <div className="min-w-0 space-y-1">
            <div className="flex flex-wrap items-center gap-2">
              <span className="text-sm font-medium">{uiText("Google OAuth 客户端")}</span>
              <Badge variant={configured ? "secondary" : "outline"}>
                {configuration.isPending
                  ? uiText("加载中...")
                  : configured
                    ? uiText("已配置")
                    : uiText("未配置")}
              </Badge>
              {value.source === "environment" && (
                <Badge variant="outline">{uiText("服务器环境变量")}</Badge>
              )}
            </div>
            {!configuration.isPending && locked && (
              <div
                role={value.configured ? "status" : "alert"}
                className="text-sm text-muted-foreground"
              >
                {value.configured
                  ? uiText("此 OAuth 客户端由服务器环境变量提供，只能在服务器配置中修改")
                  : uiText(
                      "服务器环境只配置了部分 OAuth 客户端参数；请在服务器同时配置 Client ID 和 Client Secret"
                    )}
              </div>
            )}
            {!configuration.isPending && value.source === "database" && configured && (
              <div className="break-all text-xs text-muted-foreground">{value.clientId}</div>
            )}
            {configuration.isError && (
              <div role="alert" className="text-sm text-destructive">
                {uiText(errorMessage(configuration.error))}
              </div>
            )}
          </div>
        </div>
        <div className="flex shrink-0 items-center gap-2">
          <Button
            type="button"
            size="sm"
            variant="outline"
            disabled={
              !value.editable || configuration.isPending || configuration.isError || pending
            }
            onClick={() => setDialog(true)}
          >
            {configured ? <Pencil className="h-4 w-4" /> : <KeyRound className="h-4 w-4" />}
            {uiText(configured ? "编辑 OAuth 客户端" : "配置 OAuth 客户端")}
          </Button>
          {configuration.isError && (
            <Button
              type="button"
              size="icon"
              variant="ghost"
              aria-label={uiText("重试")}
              title={uiText("重试")}
              disabled={configuration.isFetching}
              onClick={() => void configuration.refetch()}
            >
              <RefreshCcw className="h-4 w-4" />
            </Button>
          )}
        </div>
      </div>

      <Dialog open={open} onOpenChange={setDialog}>
        <DialogContent
          aria-describedby={undefined}
          data-lanqin-i18n-ignore
          className="max-h-[90svh] overflow-y-auto"
        >
          <DialogHeader>
            <DialogTitle>
              {uiText(configured ? "编辑 Google OAuth 客户端" : "配置 Google OAuth 客户端")}
            </DialogTitle>
          </DialogHeader>
          <form className="space-y-4" onSubmit={submit} autoComplete="off">
            <div className="space-y-2">
              <Label htmlFor={`${formId}-client-id`}>{uiText("客户端 ID")}</Label>
              <Input
                id={`${formId}-client-id`}
                value={clientId}
                required
                maxLength={512}
                autoComplete="off"
                spellCheck={false}
                disabled={pending}
                onChange={(event) => setClientId(event.target.value)}
              />
            </div>
            <div className="space-y-2">
              <Label htmlFor={`${formId}-client-secret`}>{uiText("客户端密钥")}</Label>
              <PasswordInput
                id={`${formId}-client-secret`}
                value={clientSecret}
                required={!editor?.clientSecretSet || clientId.trim() !== editor.clientId}
                maxLength={4096}
                autoComplete="new-password"
                disabled={pending}
                onChange={(event) => setClientSecret(event.target.value)}
              />
              {editor?.clientSecretSet && (
                <div className="text-sm text-muted-foreground">
                  {uiText("留空保留已保存密钥；更换客户端 ID 时必须填写新密钥")}
                </div>
              )}
            </div>
            <div className="space-y-2">
              <Label htmlFor={`${formId}-redirect`}>{uiText("已授权的重定向 URI")}</Label>
              <div className="flex min-w-0 flex-wrap items-center gap-2">
                <Input
                  id={`${formId}-redirect`}
                  value={value.redirectUri}
                  readOnly
                  className="min-w-0 flex-1 break-all font-mono text-xs"
                  aria-label={uiText("已授权的重定向 URI")}
                />
                <Button type="button" variant="outline" onClick={() => void copyRedirect()}>
                  {copied ? <Check className="h-4 w-4" /> : <Copy className="h-4 w-4" />}
                  {uiText(copied ? "已复制" : "复制回调地址")}
                </Button>
              </div>
            </div>
            <div role="note" className="rounded-md border p-3 text-sm">
              {uiText("此客户端用于全部 Google Drive 备份地点；保存更改后，请重新授权各地点")}
            </div>
            <div className="flex flex-wrap gap-2 text-sm">
              <a
                href="https://console.cloud.google.com/auth/clients"
                target="_blank"
                rel="noreferrer"
                className="underline underline-offset-4"
              >
                {uiText("打开 Google Cloud OAuth 客户端")}
              </a>
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
                onClick={() => setDialog(false)}
              >
                {uiText("取消")}
              </Button>
              <Button type="submit" disabled={pending || !clientId.trim() || !editor?.revision}>
                {uiText(pending ? "处理中..." : "保存 OAuth 客户端")}
              </Button>
            </DialogFooter>
          </form>
        </DialogContent>
      </Dialog>
    </div>
  )
}
