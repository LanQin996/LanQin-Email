import * as React from "react"
import { useQuery, useQueryClient } from "@tanstack/react-query"
import { CalendarClock, RefreshCcw } from "lucide-react"
import { api, type BackupDestination } from "@/lib/api"
import { uiText, useLanguage } from "@/lib/language"
import { errorMessage } from "@/lib/ui-errors"
import { formatDateTime } from "@/lib/utils"
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
import { Switch } from "@/components/ui/switch"

function localTimezone() {
  try {
    return Intl.DateTimeFormat().resolvedOptions().timeZone || "UTC"
  } catch {
    return "UTC"
  }
}

function scheduleError(code: string) {
  switch (code) {
    case "capture_configuration_changed":
      return uiText("计划公钥已变化，请重新确认计划")
    case "destination_changed":
      return uiText("计划地点不可用，请检查地点配置")
    case "queue_full":
      return uiText("备份队列已满，计划将稍后重试")
    default:
      return uiText("计划暂未执行，请检查配置后重新保存")
  }
}

export function BackupSchedulePanel({
  userId,
  destination,
  destinationsRevision,
  editable,
}: {
  userId: string
  destination: BackupDestination
  destinationsRevision: string
  editable: boolean
}) {
  useLanguage()
  const client = useQueryClient()
  const schedules = useQuery({
    queryKey: ["admin", "backup-schedules", userId],
    queryFn: api.backupSchedules,
    enabled: !!userId,
    retry: false,
    refetchInterval: 30000,
  })
  const capabilities = useQuery({
    queryKey: ["admin", "backup-capabilities", userId],
    queryFn: api.backupCapabilities,
    enabled: !!userId,
    retry: false,
  })
  const schedule = schedules.data?.items.find((item) => item.destinationId === destination.id)
  const [editor, setEditor] = React.useState<{
    revision: string
    destinationsRevision: string
    recipient: string
    enabled: boolean
    time: string
    timezone: string
  } | null>(null)
  const [pending, setPending] = React.useState(false)
  const [error, setError] = React.useState("")
  const busy = React.useRef(false)
  const canEnable =
    !destination.disabled &&
    (destination.type !== "drive" || destination.credentialsSet) &&
    !!capabilities.data?.onlineBackupAvailable &&
    !!capabilities.data.recipient

  function openEditor() {
    if (!schedules.data || pending) return
    setError("")
    setEditor({
      revision: schedules.data.revision,
      destinationsRevision,
      recipient: capabilities.data?.recipient || "",
      enabled: schedule?.enabled || false,
      time: schedule?.time || "03:00",
      timezone: schedule?.timezone || localTimezone(),
    })
  }

  async function submit(event: React.FormEvent<HTMLFormElement>) {
    event.preventDefault()
    if (!editor || busy.current) return
    const fields = new FormData(event.currentTarget)
    const time = String(fields.get("time") || "")
    const timezone = String(fields.get("timezone") || "").trim()
    if (!/^(?:[01]\d|2[0-3]):[0-5]\d$/.test(time) || !timezone) {
      setError("请填写每日时间和 IANA 时区")
      return
    }
    if (editor.enabled && (!canEnable || !editor.recipient)) {
      setError("启用计划需要可用的备份配置和已启用地点")
      return
    }
    busy.current = true
    setPending(true)
    setError("")
    try {
      await api.updateBackupSchedule(destination.id, {
        revision: editor.revision,
        destinationsRevision: editor.destinationsRevision,
        expectedRecipient: editor.recipient,
        enabled: editor.enabled,
        time,
        timezone,
      })
      setEditor(null)
      await client.invalidateQueries({ queryKey: ["admin", "backup-schedules", userId] })
    } catch (e) {
      setError(errorMessage(e))
      void schedules.refetch()
      void capabilities.refetch()
    } finally {
      busy.current = false
      setPending(false)
    }
  }

  return (
    <section className="min-w-0 rounded-lg border p-4" data-lanqin-i18n-ignore>
      <header className="flex flex-wrap items-center justify-between gap-3">
        <h3 className="flex items-center gap-2 text-sm font-medium">
          <CalendarClock className="h-4 w-4" aria-hidden="true" />
          {uiText("每日备份计划")}
        </h3>
        <div className="flex flex-wrap items-center gap-2">
          {schedules.data && !schedules.isError && (
            <Badge variant="secondary">
              {uiText(schedule?.enabled ? "计划已启用" : "未启用计划")}
            </Badge>
          )}
          <Button
            type="button"
            size="icon"
            variant="ghost"
            aria-label={uiText("刷新备份计划")}
            disabled={pending || schedules.isFetching}
            onClick={() => {
              void schedules.refetch()
              void capabilities.refetch()
            }}
          >
            <RefreshCcw className="h-4 w-4" />
          </Button>
          <Button
            type="button"
            variant="outline"
            size="sm"
            disabled={!editable || pending || !schedules.data || schedules.isError}
            onClick={openEditor}
          >
            {uiText("设置计划")}
          </Button>
        </div>
      </header>
      {schedules.isPending && (
        <div role="status" className="mt-3 text-sm">
          {uiText("加载中...")}
        </div>
      )}
      {schedules.isError && (
        <div role="alert" className="mt-3 text-sm text-destructive">
          {uiText(errorMessage(schedules.error))}
        </div>
      )}
      {schedule && !schedules.isError && (
        <dl className="mt-4 grid min-w-0 gap-3 text-sm sm:grid-cols-2">
          <div className="min-w-0">
            <dt className="text-muted-foreground">{uiText("每日执行时间")}</dt>
            <dd className="mt-1 break-words">
              {schedule.time} · {schedule.timezone}
            </dd>
          </div>
          <div className="min-w-0">
            <dt className="text-muted-foreground">{uiText("下次触发")}</dt>
            <dd className="mt-1 break-words">
              {schedule.enabled && schedule.nextRunAt
                ? formatDateTime(schedule.nextRunAt)
                : uiText("未安排")}
            </dd>
          </div>
          {schedule.lastJobId && (
            <div className="min-w-0 sm:col-span-2">
              <dt className="text-muted-foreground">{uiText("最近入队任务")}</dt>
              <dd className="mt-1 break-all font-mono text-xs">{schedule.lastJobId}</dd>
            </div>
          )}
        </dl>
      )}
      {schedule?.lastErrorCode && (
        <div role="status" className="mt-3 text-sm text-muted-foreground">
          {scheduleError(schedule.lastErrorCode)}
        </div>
      )}
      {destination.disabled && (
        <div role="status" className="mt-3 text-sm text-muted-foreground">
          {uiText("地点已停用，定时计划不会执行")}
        </div>
      )}
      <Dialog
        open={!!editor}
        onOpenChange={(open) => {
          if (!open && !pending) {
            setEditor(null)
            setError("")
          }
        }}
      >
        <DialogContent
          className="max-h-[90svh] overflow-y-auto"
          aria-describedby={undefined}
          data-lanqin-i18n-ignore
        >
          <form onSubmit={submit} className="space-y-4">
            <DialogHeader>
              <DialogTitle>{uiText("设置每日备份计划")}</DialogTitle>
            </DialogHeader>
            <div className="break-words text-sm font-medium">{destination.name}</div>
            <div className="flex items-center justify-between gap-3">
              <Label htmlFor="backup-plan-enabled">{uiText("启用每日计划")}</Label>
              <Switch
                id="backup-plan-enabled"
                checked={editor?.enabled || false}
                disabled={pending || (!editor?.enabled && !canEnable)}
                onCheckedChange={(enabled) => setEditor((value) => value && { ...value, enabled })}
              />
            </div>
            <div className="grid gap-4 sm:grid-cols-2">
              <div className="space-y-2">
                <Label htmlFor="backup-plan-time">{uiText("每日执行时间")}</Label>
                <Input
                  id="backup-plan-time"
                  name="time"
                  type="time"
                  step={60}
                  required
                  disabled={pending}
                  value={editor?.time || ""}
                  onChange={(e) => {
                    const time = e.currentTarget.value
                    setEditor((value) => value && { ...value, time })
                  }}
                />
              </div>
              <div className="space-y-2">
                <Label htmlFor="backup-plan-timezone">{uiText("IANA 时区")}</Label>
                <Input
                  id="backup-plan-timezone"
                  name="timezone"
                  required
                  maxLength={64}
                  disabled={pending}
                  value={editor?.timezone || ""}
                  onChange={(e) => {
                    const timezone = e.currentTarget.value
                    setEditor((value) => value && { ...value, timezone })
                  }}
                />
              </div>
            </div>
            {editor?.enabled && (
              <div className="space-y-2">
                <Label>{uiText("加密公钥")}</Label>
                <div className="break-all rounded-md border p-3 font-mono text-xs">
                  {editor.recipient}
                </div>
              </div>
            )}
            {!canEnable && (
              <div role="status" className="rounded-md border p-3 text-sm">
                {uiText("启用计划需要可用的备份配置和已启用地点")}
              </div>
            )}
            <div role="status" className="rounded-md border p-3 text-sm">
              {uiText("关闭计划不影响手动备份；修改计划后以下次触发时间为准")}
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
                  setEditor(null)
                  setError("")
                }}
              >
                {uiText("取消")}
              </Button>
              <Button type="submit" disabled={pending || (!!editor?.enabled && !canEnable)}>
                {uiText(pending ? "处理中..." : "保存")}
              </Button>
            </DialogFooter>
          </form>
        </DialogContent>
      </Dialog>
    </section>
  )
}
