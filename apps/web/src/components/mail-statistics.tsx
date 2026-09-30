import { useState } from "react"
import { Archive, HardDrive, Inbox, Paperclip, Send } from "lucide-react"
import type { MailStats } from "@/lib/api"
import { uiText, useLanguage } from "@/lib/language"
import { cn, formatBytes } from "@/lib/utils"
import { Button } from "@/components/ui/button"
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card"
import { Badge } from "@/components/ui/badge"

export function MailStatistics({
  stats,
  folderLabel,
}: {
  stats: MailStats
  folderLabel: (folder: string) => string
}) {
  const [language] = useLanguage()
  const [metric, setMetric] = useState<"count" | "bytes">("count")
  const number = (value: number) => new Intl.NumberFormat(language).format(value)
  const percent = (value: number) =>
    new Intl.NumberFormat(language, { style: "percent", maximumFractionDigits: 1 }).format(value)
  const folders = [...stats.byFolder].sort((a, b) => b[metric] - a[metric])
  const total = stats.totalMessages
  const read = Math.max(0, total - stats.unreadMessages)
  const readRatio = total > 0 ? read / total : 0
  const quotaRatio = stats.quotaBytes > 0 ? stats.storageBytes / stats.quotaBytes : 0
  const max = Math.max(1, ...folders.map((folder) => folder[metric]))
  const countRole = (role: string) =>
    stats.byFolder
      .filter((folder) => folder.role === role)
      .reduce((sum, folder) => sum + folder.count, 0)
  const metrics = [
    { label: uiText("收件箱"), value: number(countRole("inbox")), icon: Inbox },
    { label: uiText("已发送"), value: number(countRole("sent")), icon: Send },
    { label: uiText("草稿箱"), value: number(countRole("drafts")), icon: Archive },
    {
      label: uiText("平均邮件大小"),
      value: formatBytes(total > 0 ? stats.storageBytes / total : 0),
      icon: Paperclip,
    },
  ]
  return (
    <div className="space-y-4" data-lanqin-i18n-ignore>
      <div className="grid gap-3 sm:grid-cols-2 xl:grid-cols-4">
        {metrics.map(({ label, value, icon: Icon }) => (
          <Card key={label} className="shadow-none">
            <CardContent className="flex items-center gap-3 p-4">
              <div className="rounded-lg bg-muted p-2.5">
                <Icon className="h-4 w-4 text-muted-foreground" aria-hidden />
              </div>
              <div className="min-w-0 flex-1">
                <div className="text-sm text-muted-foreground">{label}</div>
                <div className="mt-1 text-xl font-semibold tabular-nums">{value}</div>
              </div>
            </CardContent>
          </Card>
        ))}
      </div>
      <div className="grid gap-4 xl:grid-cols-3">
        <Card className="shadow-none xl:col-span-2">
          <CardHeader className="flex-row flex-wrap items-center justify-between gap-3 space-y-0">
            <CardTitle>{uiText("文件夹分布")}</CardTitle>
            <div className="flex gap-1" role="group" aria-label={uiText("统计指标")}>
              <Button
                size="sm"
                variant={metric === "count" ? "secondary" : "ghost"}
                aria-pressed={metric === "count"}
                onClick={() => setMetric("count")}
              >
                {uiText("邮件数量")}
              </Button>
              <Button
                size="sm"
                variant={metric === "bytes" ? "secondary" : "ghost"}
                aria-pressed={metric === "bytes"}
                onClick={() => setMetric("bytes")}
              >
                {uiText("存储用量")}
              </Button>
            </div>
          </CardHeader>
          <CardContent className="space-y-5">
            {folders.length === 0 ? (
              <div className="py-12 text-center text-sm text-muted-foreground">
                {uiText("暂无邮件数据")}
              </div>
            ) : (
              folders.map((folder, index) => (
                <div key={`${folder.role}:${folder.folder}:${index}`} className="space-y-2">
                  <div className="flex items-center justify-between gap-3 text-sm">
                    <span className="min-w-0 truncate font-medium">
                      {folderLabel(folder.folder)}
                    </span>
                    <span className="shrink-0 tabular-nums">
                      {metric === "count"
                        ? uiText("{0} 封", [number(folder.count)])
                        : formatBytes(folder.bytes)}
                    </span>
                  </div>
                  <div className="h-2 overflow-hidden rounded-full bg-muted" aria-hidden>
                    <div
                      className="h-full rounded-full bg-foreground/80 transition-[width] motion-reduce:transition-none"
                      style={{ width: `${(folder[metric] / max) * 100}%` }}
                    />
                  </div>
                  <div className="flex justify-between gap-3 text-xs text-muted-foreground">
                    <span>{uiText("未读 {0}", [number(folder.unread)])}</span>
                    <span>
                      {metric === "count"
                        ? formatBytes(folder.bytes)
                        : uiText("{0} 封", [number(folder.count)])}
                    </span>
                  </div>
                </div>
              ))
            )}
          </CardContent>
        </Card>
        <Card className="shadow-none">
          <CardHeader>
            <CardTitle>{uiText("阅读状态")}</CardTitle>
          </CardHeader>
          <CardContent>
            <div className="relative mx-auto mb-6 h-44 w-44">
              <svg viewBox="0 0 120 120" className="h-full w-full -rotate-90" aria-hidden>
                <circle
                  cx="60"
                  cy="60"
                  r="50"
                  fill="none"
                  stroke="currentColor"
                  strokeWidth="10"
                  className="text-muted"
                />
                <circle
                  cx="60"
                  cy="60"
                  r="50"
                  fill="none"
                  stroke="currentColor"
                  strokeWidth="10"
                  pathLength="100"
                  strokeDasharray={`${readRatio * 100} 100`}
                  className="text-foreground"
                />
              </svg>
              <div className="absolute inset-0 flex flex-col items-center justify-center">
                <span className="text-3xl font-semibold tabular-nums">{percent(readRatio)}</span>
                <span className="mt-1 text-sm text-muted-foreground">{uiText("已读比例")}</span>
              </div>
            </div>
            <div className="flex items-center justify-between border-t py-3 text-sm">
              <span>{uiText("已读")}</span>
              <span className="font-medium tabular-nums">{number(read)}</span>
            </div>
            <div className="flex items-center justify-between border-t py-3 text-sm">
              <span>{uiText("未读")}</span>
              <Badge variant="secondary">{number(stats.unreadMessages)}</Badge>
            </div>
            {total === 0 && (
              <div className="mt-2 text-sm text-muted-foreground">{uiText("暂无邮件数据")}</div>
            )}
          </CardContent>
        </Card>
      </div>
      <Card className="shadow-none">
        <CardHeader className="flex-row items-center justify-between space-y-0">
          <CardTitle>{uiText("存储用量")}</CardTitle>
          <HardDrive className="h-5 w-5 text-muted-foreground" aria-hidden />
        </CardHeader>
        <CardContent className="space-y-4">
          <div className="flex flex-wrap items-end justify-between gap-2">
            <div className="text-2xl font-semibold tabular-nums">
              {formatBytes(stats.storageBytes)}
              {stats.quotaBytes > 0 && (
                <span className="ml-2 text-sm font-normal text-muted-foreground">
                  / {formatBytes(stats.quotaBytes)}
                </span>
              )}
            </div>
            <Badge variant={quotaRatio >= 0.9 ? "destructive" : "secondary"}>
              {stats.quotaBytes > 0 ? percent(quotaRatio) : uiText("未设置容量上限")}
            </Badge>
          </div>
          {stats.quotaBytes > 0 && (
            <>
              <div className="h-2.5 overflow-hidden rounded-full bg-muted" aria-hidden>
                <div
                  className={cn(
                    "h-full rounded-full bg-foreground",
                    quotaRatio >= 0.9 && "bg-destructive"
                  )}
                  style={{ width: `${Math.min(100, Math.max(0, quotaRatio * 100))}%` }}
                />
              </div>
              <div className="flex flex-wrap justify-between gap-2 text-sm text-muted-foreground">
                <span>
                  {uiText("剩余容量：{0}", [
                    formatBytes(Math.max(0, stats.quotaBytes - stats.storageBytes)),
                  ])}
                </span>
                <span className={cn(quotaRatio >= 0.9 && "text-destructive")}>
                  {quotaRatio >= 1
                    ? uiText("存储容量已用尽")
                    : quotaRatio >= 0.9
                      ? uiText("存储容量即将用尽")
                      : uiText("存储容量使用正常")}
                </span>
              </div>
            </>
          )}
          <div className="flex flex-wrap items-center justify-between gap-2 border-t pt-4 text-sm">
            <span className="text-muted-foreground">{uiText("附件")}</span>
            <span className="tabular-nums">
              {uiText("{0} 个附件 · {1}", [
                number(stats.attachmentCount),
                formatBytes(stats.attachmentBytes),
              ])}
            </span>
          </div>
        </CardContent>
      </Card>
    </div>
  )
}
