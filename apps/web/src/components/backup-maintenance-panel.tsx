import * as React from "react"
import { useQuery, useQueryClient } from "@tanstack/react-query"
import { api, type BackupMaintenanceStatus, type BackupUploadJob } from "@/lib/api"
import { uiText, useLanguage } from "@/lib/language"
import { ApiError, errorMessage } from "@/lib/ui-errors"
import { backupMaintenanceRequestId } from "@/lib/backup-maintenance"
import { Badge } from "@/components/ui/badge"
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

function finished(status?: BackupMaintenanceStatus) {
  return !!status && ["succeeded", "failed", "needsRecovery"].includes(status.state)
}

function stateText(status?: BackupMaintenanceStatus) {
  switch (status?.state) {
    case "queued":
      return uiText("覆盖恢复已排队")
    case "preparing":
      return uiText("正在准备覆盖恢复")
    case "maintaining":
      return uiText("正在维护并覆盖恢复")
    case "succeeded":
      return uiText("覆盖恢复完成，请重新登录")
    case "failed":
      return uiText("覆盖恢复失败，请联系管理员检查")
    case "needsRecovery":
      return uiText("覆盖恢复需要人工恢复，请联系管理员")
    default:
      return uiText("覆盖恢复请求结果待确认")
  }
}

type MaintenanceAttempt = {
  requestId: string
  statusId: string
  sourceJobId: string
  planId: string
}
function pendingStorageKey(userId: string) {
  return `lanqin:backup-maintenance-request:${userId}`
}

function savedAttempt(userId: string): MaintenanceAttempt | null {
  try {
    const value = JSON.parse(window.sessionStorage.getItem(pendingStorageKey(userId)) || "null")
    if (
      value &&
      /^[a-f0-9]{32}$/.test(value.statusId) &&
      /^[A-Za-z0-9_-]{16,80}$/.test(value.requestId) &&
      typeof value.sourceJobId === "string" &&
      value.sourceJobId.length <= 128 &&
      /^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$/.test(value.planId)
    )
      return {
        requestId: value.requestId,
        statusId: value.statusId,
        sourceJobId: value.sourceJobId,
        planId: value.planId,
      }
  } catch {
    // A blocked or invalid storage entry must not prevent status queries in memory.
  }
  return null
}

export function BackupMaintenanceProgress({
  requestId,
  sourceJobId,
  status,
  disconnected,
  fetching,
  submitting,
  onRetry,
  onDismiss,
}: {
  requestId: string
  sourceJobId: string
  status?: BackupMaintenanceStatus
  disconnected: boolean
  fetching: boolean
  submitting: boolean
  onRetry: () => void
  onDismiss?: () => void
}) {
  useLanguage()
  return (
    <div className="space-y-3 rounded-md border p-3" aria-live="polite" data-lanqin-i18n-ignore>
      <Badge variant={status?.state === "needsRecovery" ? "destructive" : "secondary"}>
        {stateText(status)}
      </Badge>
      <dl className="grid gap-2 text-sm sm:grid-cols-2">
        <div className="min-w-0">
          <dt>{uiText("覆盖恢复请求编号")}</dt>
          <dd className="break-all font-mono text-xs">{requestId}</dd>
        </div>
        <div className="min-w-0">
          <dt>{uiText("恢复验证任务")}</dt>
          <dd className="break-all font-mono text-xs">{sourceJobId}</dd>
        </div>
      </dl>
      {status?.errorCode && (
        <div role="alert" className="text-sm text-destructive">
          {uiText("覆盖恢复未完成，请保留请求编号并联系管理员检查；不要重复提交")}
        </div>
      )}
      {disconnected && (
        <div role="status" className="text-sm">
          {uiText(
            "维护期间可能暂时断连或撤销会话。请求编号已保留，仅重试查询；服务恢复后请重新登录"
          )}
        </div>
      )}
      {!finished(status) && (
        <div role="note" className="text-sm">
          {uiText("关闭弹窗仍会查询此请求；请勿重复发起覆盖恢复")}
        </div>
      )}
      <Button type="button" variant="outline" disabled={submitting || fetching} onClick={onRetry}>
        {uiText(fetching ? "正在查询维护状态" : "重新查询维护状态")}
      </Button>
      {onDismiss && (status?.state === "succeeded" || status?.state === "failed") && (
        <Button type="button" variant="ghost" disabled={submitting} onClick={onDismiss}>
          {uiText("关闭此记录")}
        </Button>
      )}
    </div>
  )
}

export function BackupMaintenancePanel({
  userId,
  source,
}: {
  userId: string
  source?: BackupUploadJob
}) {
  useLanguage()
  const client = useQueryClient()
  const id = React.useId()
  const [open, setOpen] = React.useState(false)
  const [planId, setPlanId] = React.useState("")
  const [confirmed, setConfirmed] = React.useState(false)
  const [quarantineConfirmed, setQuarantineConfirmed] = React.useState(false)
  const [error, setError] = React.useState("")
  const [submitting, setSubmitting] = React.useState(false)
  const busy = React.useRef(false)
  const [attempt, setAttempt] = React.useState<MaintenanceAttempt | null>(() =>
    savedAttempt(userId)
  )
  React.useEffect(() => {
    try {
      if (attempt) window.sessionStorage.setItem(pendingStorageKey(userId), JSON.stringify(attempt))
      else window.sessionStorage.removeItem(pendingStorageKey(userId))
    } catch {
      // IDs remain in memory when browser storage is disabled. Never persist credentials.
    }
  }, [attempt, userId])
  React.useEffect(() => {
    setConfirmed(false)
    setQuarantineConfirmed(false)
  }, [source?.id])
  const ready =
    source?.kind === "restore" && source.status === "succeeded" && !!source.restore?.ready
  const plans = useQuery({
    queryKey: ["admin", "backup-maintenance-plans", userId],
    queryFn: api.backupMaintenancePlans,
    enabled: !!userId && ready,
    retry: false,
  })
  const choices = (plans.data?.available ? plans.data.items : []).filter(
    (plan) => plan.driver === source?.restore?.manifest?.driver
  )
  const selected = choices.find((plan) => plan.id === planId)
  const statusKey = ["admin", "backup-maintenance-status", userId, attempt?.requestId]
  const status = useQuery({
    queryKey: statusKey,
    queryFn: () => api.backupMaintenanceStatus(attempt!.statusId),
    enabled: !!userId && !!attempt && !submitting,
    retry: false,
    refetchInterval: (query) => (finished(query.state.data) ? false : 5000),
  })
  const tracking = !!attempt && !finished(status.data)

  async function submit(event: React.FormEvent<HTMLFormElement>) {
    event.preventDefault()
    if (
      !ready ||
      !source ||
      !selected ||
      !confirmed ||
      (selected.queueQuarantine && !quarantineConfirmed) ||
      tracking ||
      busy.current
    )
      return
    const form = event.currentTarget
    const fields = new FormData(form)
    const payload = {
      sourceJobId: source.id,
      planId: selected.id,
      templateSHA256: selected.templateSHA256,
      requestId: crypto.randomUUID(),
      password: String(fields.get("password") || ""),
      twoFactorCode: String(fields.get("twoFactorCode") || "").trim(),
      confirm: true,
    }
    if (!payload.password) return
    busy.current = true
    setSubmitting(true)
    setError("")
    setConfirmed(false)
    setQuarantineConfirmed(false)
    // Credentials are never put in query state or persistent browser storage.
    form.reset()
    try {
      // Match the server's opaque actor/nonce binding before sending the POST.
      const statusId = await backupMaintenanceRequestId(userId, payload.requestId)
      setAttempt({
        requestId: payload.requestId,
        statusId,
        sourceJobId: payload.sourceJobId,
        planId: payload.planId,
      })
      const result = await api.createBackupMaintenance(payload)
      client.setQueryData(["admin", "backup-maintenance-status", userId, payload.requestId], result)
    } catch (cause) {
      if (cause instanceof ApiError && [400, 401, 403, 404, 409, 429].includes(cause.status)) {
        setAttempt(null)
        setError(errorMessage(cause))
        if (cause.status === 409) void plans.refetch()
      } else {
        // A lost response is ambiguous. Only GET may follow; never repeat this POST.
        setError("覆盖恢复请求结果待确认")
      }
    } finally {
      payload.password = payload.twoFactorCode = ""
      fields.delete("password")
      fields.delete("twoFactorCode")
      busy.current = false
      setSubmitting(false)
    }
  }

  if (!ready && !attempt) return null

  const progress = attempt && (
    <BackupMaintenanceProgress
      requestId={attempt.statusId}
      sourceJobId={attempt.sourceJobId}
      status={status.data}
      disconnected={status.isError}
      fetching={status.isFetching}
      submitting={submitting}
      onRetry={() => void status.refetch()}
      onDismiss={() => {
        setAttempt(null)
        setOpen(false)
        setError("")
      }}
    />
  )

  return (
    <section className="space-y-3 rounded-md border p-3" data-lanqin-i18n-ignore>
      <div className="flex flex-wrap items-center justify-between gap-3">
        <Label>{uiText("覆盖恢复")}</Label>
        <Button
          variant="outline"
          disabled={!attempt && (!ready || !choices.length || plans.isFetching)}
          onClick={() => {
            if (!tracking) {
              setPlanId(choices[0]?.id || "")
              setConfirmed(false)
              setQuarantineConfirmed(false)
              setError("")
            }
            setOpen(true)
          }}
        >
          {uiText(attempt ? "查看覆盖恢复" : "覆盖恢复")}
        </Button>
      </div>
      {!attempt && ready && (
        <div className="break-all text-xs">
          {uiText("恢复验证任务")}: {source?.id}
        </div>
      )}
      {!attempt && plans.isPending && <div role="status">{uiText("正在读取主机恢复计划")}</div>}
      {!attempt && !plans.isPending && !choices.length && (
        <div role="status" className="text-sm">
          {uiText(
            plans.isError
              ? "无法读取主机恢复计划，请稍后刷新"
              : "主机尚未配置与此备份匹配的覆盖恢复计划"
          )}
          <Button type="button" variant="ghost" onClick={() => void plans.refetch()}>
            {uiText("刷新")}
          </Button>
        </div>
      )}
      {progress}
      <Dialog open={open} onOpenChange={setOpen}>
        <DialogContent
          aria-describedby={undefined}
          data-lanqin-i18n-ignore
          className="max-h-[90svh] overflow-y-auto"
        >
          <DialogHeader>
            <DialogTitle>{uiText("覆盖恢复")}</DialogTitle>
          </DialogHeader>
          {attempt ? (
            progress
          ) : (
            <form className="space-y-4" onSubmit={submit} autoComplete="off">
              <div className="space-y-2">
                <Label htmlFor={`${id}-plan`}>{uiText("主机已批准的恢复计划")}</Label>
                <Select
                  value={planId}
                  onValueChange={(value) => {
                    setPlanId(value)
                    setConfirmed(false)
                    setQuarantineConfirmed(false)
                  }}
                  disabled={submitting}
                >
                  <SelectTrigger id={`${id}-plan`}>
                    <SelectValue />
                  </SelectTrigger>
                  <SelectContent data-lanqin-i18n-ignore>
                    {choices.map((plan) => (
                      <SelectItem key={plan.id} value={plan.id}>
                        {plan.id}
                      </SelectItem>
                    ))}
                  </SelectContent>
                </Select>
              </div>
              <div className="space-y-2">
                <Label htmlFor={`${id}-password`}>{uiText("当前管理员密码")}</Label>
                <PasswordInput
                  id={`${id}-password`}
                  name="password"
                  required
                  maxLength={72}
                  autoComplete="off"
                  disabled={submitting}
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
                  disabled={submitting}
                />
              </div>
              <div role="note" className="rounded-md border p-3 text-sm">
                {uiText(
                  "此操作会停止邮件服务并以此备份覆盖当前数据。恢复后旧会话将撤销，需要重新登录"
                )}
              </div>
              {selected?.queueQuarantine && (
                <div className="flex items-start gap-3">
                  <Checkbox
                    id={`${id}-quarantine`}
                    checked={quarantineConfirmed}
                    disabled={submitting}
                    onCheckedChange={(value) => setQuarantineConfirmed(value === true)}
                  />
                  <Label htmlFor={`${id}-quarantine`} className="leading-normal">
                    {uiText(
                      "我同意加密保全并隔离尚未投递的队列邮件；它们不会自动重新投递，需要管理员另行处理"
                    )}
                  </Label>
                </div>
              )}
              <div className="flex items-start gap-3">
                <Checkbox
                  id={`${id}-confirm`}
                  checked={confirmed}
                  disabled={submitting}
                  onCheckedChange={(value) => setConfirmed(value === true)}
                />
                <Label htmlFor={`${id}-confirm`} className="leading-normal">
                  {uiText("我确认现在进入维护窗口，停止服务并覆盖当前邮件数据，且已准备重新登录")}
                </Label>
              </div>
              {error && (
                <div role="alert" className="text-sm text-destructive">
                  {uiText(error)}
                </div>
              )}
              <DialogFooter>
                <Button type="button" variant="outline" onClick={() => setOpen(false)}>
                  {uiText("取消")}
                </Button>
                <Button
                  type="submit"
                  variant="destructive"
                  disabled={
                    !selected ||
                    !confirmed ||
                    (selected.queueQuarantine && !quarantineConfirmed) ||
                    submitting
                  }
                >
                  {uiText(submitting ? "处理中..." : "停止服务并覆盖恢复")}
                </Button>
              </DialogFooter>
            </form>
          )}
        </DialogContent>
      </Dialog>
    </section>
  )
}
