import * as React from "react"
import { useQuery } from "@tanstack/react-query"
import { api, type BackupDestination, type BackupUploadJob } from "@/lib/api"
import { uiText, useLanguage } from "@/lib/language"
import { errorMessage } from "@/lib/ui-errors"
import { formatBytes, formatDateTime } from "@/lib/utils"
import { BackupRestoreDialog } from "@/components/backup-restore-dialog"
import { Button } from "@/components/ui/button"

export function BackupDiscoveryResults({
  userId,
  source,
  destinations,
  revision,
  restoreAvailable,
  restoreMaxBytes,
  onRestored,
}: {
  userId: string
  source: BackupUploadJob
  destinations: BackupDestination[]
  revision: string
  restoreAvailable: boolean
  restoreMaxBytes: number
  onRestored: (job: BackupUploadJob) => void
}) {
  useLanguage()
  const [cursors, setCursors] = React.useState([""])
  const cursor = cursors[cursors.length - 1]
  const ready =
    source.kind === "discover" && source.status === "succeeded" && !!source.discovery?.ready
  const entries = useQuery({
    queryKey: ["admin", "backup-discovery-entries", userId, source.id, cursor],
    queryFn: ({ signal }) => api.backupDiscoveryEntries(source.id, cursor, signal),
    enabled: ready,
    retry: false,
  })
  if (!ready) return null

  return (
    <section className="min-w-0 space-y-4" data-lanqin-i18n-ignore>
      <h4 className="text-sm font-medium">
        {uiText("已认证云端索引 {0} 份", [source.discovery?.count || 0])}
      </h4>
      {source.discovery?.sourceFolderId && (
        <dl className="min-w-0 text-sm">
          <dt>{uiText("本次读取的 Google Drive 文件夹")}</dt>
          <dd className="break-all font-mono text-xs">{source.discovery.sourceFolderId}</dd>
        </dl>
      )}
      <div role="note" className="rounded-md border p-3 text-sm">
        {uiText("索引认证不代表远端文件仍存在；恢复前仍会验证下载大小、摘要、密文认证和数据库")}
      </div>
      {entries.isPending && <div role="status">{uiText("加载中...")}</div>}
      {entries.isError && (
        <div className="space-y-2">
          <div role="alert" className="text-sm text-destructive">
            {uiText(errorMessage(entries.error))}
          </div>
          <Button
            variant="outline"
            disabled={entries.isFetching}
            onClick={() => void entries.refetch()}
          >
            {uiText("重试")}
          </Button>
        </div>
      )}
      {entries.data && !entries.data.items.length && (
        <div role="status" className="text-sm text-muted-foreground">
          {uiText("未发现可验证的云端备份索引")}
        </div>
      )}
      {entries.data?.items.map((entry) => (
        <div key={entry.id} className="min-w-0 space-y-3 rounded-md border p-3">
          <dl className="grid min-w-0 gap-3 text-sm sm:grid-cols-2">
            <div>
              <dt>{uiText("索引记录完成时间")}</dt>
              <dd>{formatDateTime(entry.completedAt)}</dd>
            </div>
            <div>
              <dt>{uiText("加密文件大小")}</dt>
              <dd>{formatBytes(entry.bytes)}</dd>
            </div>
            <div className="min-w-0 sm:col-span-2">
              <dt>{uiText("云端对象标识")}</dt>
              <dd className="break-all font-mono text-xs">{entry.objectKey}</dd>
            </div>
            <div className="min-w-0 sm:col-span-2">
              <dt>{uiText("归档 SHA-256")}</dt>
              <dd className="break-all font-mono text-xs">{entry.sha256}</dd>
            </div>
            <div className="min-w-0 sm:col-span-2">
              <dt>{uiText("原备份地点 ID")}</dt>
              <dd className="break-all font-mono text-xs">{entry.originalDestinationId}</dd>
            </div>
          </dl>
          <BackupRestoreDialog
            key={entry.id}
            source={source}
            discoveryEntry={entry}
            maxBytes={restoreMaxBytes}
            destinations={destinations}
            revision={revision}
            available={restoreAvailable}
            onCreated={onRestored}
          />
        </div>
      ))}
      {(cursors.length > 1 || entries.data?.nextCursor) && (
        <div className="flex flex-wrap gap-2">
          <Button
            variant="outline"
            disabled={cursors.length === 1 || entries.isFetching}
            onClick={() =>
              setCursors((current) => (current.length > 1 ? current.slice(0, -1) : current))
            }
          >
            {uiText("上一页")}
          </Button>
          <Button
            variant="outline"
            disabled={!entries.data?.nextCursor || entries.isFetching}
            onClick={() => {
              const next = entries.data?.nextCursor
              if (next)
                setCursors((current) =>
                  current[current.length - 1] === next ? current : [...current, next]
                )
            }}
          >
            {uiText("下一页")}
          </Button>
        </div>
      )}
    </section>
  )
}
