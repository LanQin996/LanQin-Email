import * as React from "react"
import { useQuery, useQueryClient } from "@tanstack/react-query"
import { CalendarClock, RefreshCcw } from "lucide-react"
import {
  api,
  type BackupDestination,
  type BackupSchedulePeriod,
  type BackupScheduleUnit,
} from "@/lib/api"
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
import { Checkbox } from "@/components/ui/checkbox"
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select"

const weekdayNames = ["星期日", "星期一", "星期二", "星期三", "星期四", "星期五", "星期六"]

function periodName(period: BackupSchedulePeriod, unit: BackupScheduleUnit = "minutes") {
  if (period === "hourly") return uiText("每小时")
  if (period === "weekly") return uiText("每周")
  if (period === "monthly") return uiText("每月")
  if (period === "interval")
    return uiText(unit === "days" ? "每 N 天" : unit === "hours" ? "每 N 小时" : "每 N 分钟")
  return uiText("每天")
}

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
      return uiText("备份加密设置已变化，请重新确认计划")
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
    period: BackupSchedulePeriod
    minute: number
    weekdays: number[]
    dayOfMonth: number
    interval: number
    unit: BackupScheduleUnit
  } | null>(null)
  const [pending, setPending] = React.useState(false)
  const [error, setError] = React.useState("")
  const busy = React.useRef(false)
  const canEnable =
    !destination.disabled &&
    (destination.type !== "drive" || destination.credentialsSet) &&
    !!capabilities.data?.onlineBackupAvailable &&
    !!capabilities.data.recipient?.startsWith("password:")

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
      period: schedule?.period || "daily",
      minute: schedule?.minute ?? 0,
      weekdays: schedule?.weekdays?.length ? [...schedule.weekdays] : [1],
      dayOfMonth: schedule?.dayOfMonth || 1,
      interval: schedule?.interval || 5,
      unit: schedule?.unit || "minutes",
    })
  }

  async function submit(event: React.FormEvent<HTMLFormElement>) {
    event.preventDefault()
    if (!editor || busy.current) return
    const fields = new FormData(event.currentTarget)
    const time = String(fields.get("time") || "")
    const timezone = String(fields.get("timezone") || "").trim()
    const calendar = ["daily", "weekly", "monthly"].includes(editor.period)
    if ((calendar && !/^(?:[01]\d|2[0-3]):[0-5]\d$/.test(time)) || !timezone) {
      setError("请填写执行时间和 IANA 时区")
      return
    }
    const bounds =
      editor.unit === "minutes" ? [5, 527040] : editor.unit === "hours" ? [1, 8760] : [1, 366]
    if (
      (editor.period === "hourly" &&
        (!Number.isInteger(editor.minute) || editor.minute < 0 || editor.minute > 59)) ||
      (editor.period === "weekly" && !editor.weekdays.length) ||
      (editor.period === "monthly" &&
        (!Number.isInteger(editor.dayOfMonth) ||
          editor.dayOfMonth < 1 ||
          editor.dayOfMonth > 31)) ||
      (editor.period === "interval" &&
        (!Number.isInteger(editor.interval) ||
          editor.interval < bounds[0] ||
          editor.interval > bounds[1]))
    ) {
      setError("请检查执行周期参数；分钟间隔至少 5 分钟")
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
        period: editor.period,
        ...(calendar && { time }),
        ...(editor.period === "hourly" && { minute: editor.minute }),
        ...(editor.period === "weekly" && { weekdays: editor.weekdays }),
        ...(editor.period === "monthly" && { dayOfMonth: editor.dayOfMonth }),
        ...(editor.period === "interval" && { interval: editor.interval, unit: editor.unit }),
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
          {uiText("备份计划")}
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
            <dt className="text-muted-foreground">{uiText("执行周期")}</dt>
            <dd className="mt-1 break-words">
              {periodName(schedule.period || "daily", schedule.unit)}
              {schedule.period === "interval"
                ? ` · ${schedule.interval}`
                : schedule.period === "hourly"
                  ? ` · ${String(schedule.minute ?? 0).padStart(2, "0")} ${uiText("分钟")}`
                  : ` · ${schedule.time}`}
              {schedule.period === "weekly" &&
                ` · ${(schedule.weekdays || []).map((day) => uiText(weekdayNames[day])).join("、")}`}
              {schedule.period === "monthly" &&
                ` · ${uiText("每月第 {0} 日", [schedule.dayOfMonth || 1])}`}
              {` · ${schedule.timezone}`}
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
              <DialogTitle>{uiText("设置备份计划")}</DialogTitle>
            </DialogHeader>
            <div className="break-words text-sm font-medium">{destination.name}</div>
            <div className="flex items-center justify-between gap-3">
              <Label htmlFor="backup-plan-enabled">{uiText("启用计划")}</Label>
              <Switch
                id="backup-plan-enabled"
                checked={editor?.enabled || false}
                disabled={pending || (!editor?.enabled && !canEnable)}
                onCheckedChange={(enabled) => setEditor((value) => value && { ...value, enabled })}
              />
            </div>
            <div className="space-y-2">
              <Label htmlFor="backup-plan-period">{uiText("执行周期")}</Label>
              <Select
                value={
                  editor?.period === "interval"
                    ? `interval-${editor.unit}`
                    : editor?.period || "daily"
                }
                disabled={pending}
                onValueChange={(selected) =>
                  setEditor((value) => {
                    if (!value) return value
                    if (selected.startsWith("interval-")) {
                      const unit = selected.slice(9) as BackupScheduleUnit
                      return {
                        ...value,
                        period: "interval",
                        unit,
                        interval:
                          unit === "minutes" ? Math.max(5, Math.min(value.interval, 527040)) : 1,
                      }
                    }
                    return { ...value, period: selected as BackupSchedulePeriod }
                  })
                }
              >
                <SelectTrigger id="backup-plan-period">
                  <SelectValue />
                </SelectTrigger>
                <SelectContent data-lanqin-i18n-ignore>
                  <SelectItem value="daily">{uiText("每天")}</SelectItem>
                  <SelectItem value="hourly">{uiText("每小时")}</SelectItem>
                  <SelectItem value="weekly">{uiText("每周")}</SelectItem>
                  <SelectItem value="monthly">{uiText("每月")}</SelectItem>
                  <SelectItem value="interval-minutes">{uiText("每 N 分钟")}</SelectItem>
                  <SelectItem value="interval-hours">{uiText("每 N 小时")}</SelectItem>
                  <SelectItem value="interval-days">{uiText("每 N 天")}</SelectItem>
                </SelectContent>
              </Select>
            </div>
            {editor?.period === "weekly" && (
              <div className="space-y-2">
                <Label>{uiText("执行星期")}</Label>
                <div className="grid grid-cols-2 gap-3 sm:grid-cols-4">
                  {weekdayNames.map((name, day) => (
                    <div key={day} className="flex items-center gap-2">
                      <Checkbox
                        id={`backup-weekday-${day}`}
                        checked={!!editor.weekdays.includes(day)}
                        disabled={pending}
                        onCheckedChange={(checked) =>
                          setEditor(
                            (value) =>
                              value && {
                                ...value,
                                weekdays:
                                  checked === true
                                    ? [...new Set([...value.weekdays, day])].sort()
                                    : value.weekdays.filter((item) => item !== day),
                              }
                          )
                        }
                      />
                      <Label htmlFor={`backup-weekday-${day}`}>{uiText(name)}</Label>
                    </div>
                  ))}
                </div>
              </div>
            )}
            {editor?.period === "monthly" && (
              <div className="space-y-2">
                <Label htmlFor="backup-plan-day">{uiText("每月日期")}</Label>
                <Input
                  id="backup-plan-day"
                  type="number"
                  min={1}
                  max={31}
                  step={1}
                  required
                  disabled={pending}
                  value={editor.dayOfMonth}
                  onChange={(event) => {
                    const dayOfMonth = Number(event.currentTarget.value)
                    setEditor((value) => value && { ...value, dayOfMonth })
                  }}
                />
                <div role="note" className="text-sm text-muted-foreground">
                  {uiText("该月没有所选日期时，在当月最后一天执行")}
                </div>
              </div>
            )}
            {editor?.period === "hourly" && (
              <div className="space-y-2">
                <Label htmlFor="backup-plan-minute">{uiText("每小时的第几分钟")}</Label>
                <Input
                  id="backup-plan-minute"
                  type="number"
                  min={0}
                  max={59}
                  step={1}
                  required
                  disabled={pending}
                  value={editor.minute}
                  onChange={(event) => {
                    const minute = Number(event.currentTarget.value)
                    setEditor((value) => value && { ...value, minute })
                  }}
                />
              </div>
            )}
            {editor?.period === "interval" && (
              <div className="space-y-2">
                <Label htmlFor="backup-plan-interval">{uiText("执行间隔")}</Label>
                <Input
                  id="backup-plan-interval"
                  type="number"
                  min={editor.unit === "minutes" ? 5 : 1}
                  max={editor.unit === "minutes" ? 527040 : editor.unit === "hours" ? 8760 : 366}
                  step={1}
                  required
                  disabled={pending}
                  value={editor.interval}
                  onChange={(event) => {
                    const interval = Number(event.currentTarget.value)
                    setEditor((value) => value && { ...value, interval })
                  }}
                />
                <div role="note" className="text-sm text-muted-foreground">
                  {uiText(
                    "间隔从保存时开始计算，按固定时长执行；分钟间隔至少 5 分钟，不提供秒级邮件备份"
                  )}
                </div>
              </div>
            )}
            <div className="grid gap-4 sm:grid-cols-2">
              {editor && ["daily", "weekly", "monthly"].includes(editor.period) && (
                <div className="space-y-2">
                  <Label htmlFor="backup-plan-time">{uiText("执行时间")}</Label>
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
              )}
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
                <Label>{uiText("备份加密方式")}</Label>
                <div className="break-all rounded-md border p-3 font-mono text-xs">
                  {uiText("管理员设置的备份密码")}
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
