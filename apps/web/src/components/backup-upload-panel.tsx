import * as React from "react"
import { useQuery, useQueryClient } from "@tanstack/react-query"
import { Clock3, Download, Plus, RefreshCcw, Upload } from "lucide-react"
import { api, type BackupDestination, type BackupUploadJob } from "@/lib/api"
import { uiText, useLanguage } from "@/lib/language"
import { errorMessage } from "@/lib/ui-errors"
import { formatBytes, formatDateTime } from "@/lib/utils"
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
import { Label } from "@/components/ui/label"
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select"

function active(job?: BackupUploadJob) {
  return job?.status === "queued" || job?.status === "running"
}

function uploadReady(destination: BackupDestination) {
  return !destination.disabled && (destination.type !== "drive" || destination.credentialsSet)
}

function jobStatusText(job: BackupUploadJob) {
  if (job.status === "running") {
    if (job.phase === "collecting") return uiText("正在采集数据")
    if (job.phase === "encrypting") return uiText("正在加密归档")
    if (job.phase === "uploading") return uiText("上传中")
    if (job.kind === "snapshot") return uiText("正在执行备份")
  }
  if (job.kind === "snapshot") {
    if (job.status === "succeeded") return uiText("备份完成")
    if (job.status === "failed") return uiText("备份失败")
  }
  return statusText(job.status)
}

function capabilityText(reason?: string) {
  if (reason === "sqlite_required") return uiText("在线备份目前仅支持 SQLite")
  if (reason === "workspace_unavailable")
    return uiText("私有备份工作区不可用，请联系管理员检查配置")
  if (reason === "configuration_required")
    return uiText("请先在服务器配置备份公钥、私有工作区和归档目录")
  return uiText("在线备份暂不可用，请检查服务器配置")
}

function statusText(status: string) {
  switch (status) {
    case "queued":
      return uiText("排队中")
    case "running":
      return uiText("上传中")
    case "succeeded":
      return uiText("上传完成")
    case "partial":
      return uiText("部分成功")
    case "failed":
      return uiText("上传失败")
    case "cancelled":
      return uiText("已取消")
    case "interrupted":
      return uiText("任务中断，远端结果可能不确定")
    default:
      return uiText("操作未完成，请稍后重试")
  }
}

function resultError(code?: string) {
  switch (code) {
    case "reauthorization_required":
      return uiText("Google Drive 授权已失效，请重新授权")
    case "snapshot_failed":
    case "collection_failed":
      return uiText("数据采集未完成，未生成可上传归档")
    case "encryption_failed":
      return uiText("归档加密未完成，请检查服务器备份配置")
    case "configuration_changed":
    case "recipient_changed":
    case "capture_configuration_changed":
      return uiText("备份配置已变化，请重新创建任务")
    case "snapshot_cleanup_failed":
      return uiText("私有暂存清理失败，请联系管理员检查")
    case "artifact_publication_failed":
      return uiText("加密归档发布失败，未开始上传")
    case "capture_interrupted":
      return uiText("任务中断，远端结果可能不确定")
    case "destination_changed":
      return uiText("地点配置已变化，需重新提交任务")
    case "access_denied":
      return uiText("远端拒绝访问")
    case "endpoint_blocked":
      return uiText("存储地址被安全策略拒绝")
    case "tls_error":
      return uiText("存储服务器证书校验失败")
    case "timeout":
      return uiText("上传超时")
    case "artifact_unavailable":
      return uiText("加密归档不可用或校验失败")
    case "server_shutdown":
    case "lease_expired":
    case "lease_lost":
    case "heartbeat_failed":
      return uiText("任务中断，远端结果可能不确定")
    case "user_cancelled":
    case "cancelled":
      return uiText("已取消")
    default:
      return uiText("上传未完成，请检查地点配置后重试")
  }
}

export function BackupUploadPanel({
  userId,
  destinations,
  revision,
  preferredId,
  canUpload,
}: {
  userId: string
  destinations: BackupDestination[]
  revision: string
  preferredId?: string
  canUpload: boolean
}) {
  useLanguage()
  const client = useQueryClient()
  const capabilities = useQuery({
    queryKey: ["admin", "backup-capabilities", userId],
    queryFn: api.backupCapabilities,
    enabled: !!userId,
    retry: false,
  })
  const [cursor, setCursor] = React.useState("")
  const artifacts = useQuery({
    queryKey: ["admin", "backup-artifacts", userId],
    queryFn: api.backupArtifacts,
    enabled: !!userId,
    retry: false,
  })
  const jobs = useQuery({
    queryKey: ["admin", "backup-jobs", userId, cursor],
    queryFn: () => api.backupUploadJobs(cursor),
    enabled: !!userId,
    retry: false,
    refetchInterval: (q) => (q.state.data?.items.some(active) ? 2000 : false),
  })
  const [detailId, setDetailId] = React.useState("")
  const detail = useQuery({
    queryKey: ["admin", "backup-job", userId, detailId],
    queryFn: () => api.backupUploadJob(detailId),
    enabled: !!userId && !!detailId,
    retry: false,
    refetchInterval: (q) => (active(q.state.data) ? 2000 : false),
  })
  const [upload, setUpload] = React.useState<{
    requestId: string
    revision: string
    kind: "upload" | "snapshot"
    recipient: string
  } | null>(null)
  const [artifactId, setArtifactId] = React.useState("")
  const [targetIds, setTargetIds] = React.useState<string[]>([])
  const [pending, setPending] = React.useState(false)
  const [error, setError] = React.useState("")
  const busy = React.useRef(false)
  const completion = `${detail.data?.id}:${detail.data?.status}|${jobs.data?.items
    .filter((j) => !active(j))
    .map((j) => `${j.id}:${j.status}`)
    .join(",")}`
  React.useEffect(() => {
    void client.invalidateQueries({ queryKey: ["admin", "backup-destinations", userId] })
    void client.invalidateQueries({ queryKey: ["admin", "backup-artifacts", userId] })
  }, [client, userId, completion])

  function openUpload(kind: "upload" | "snapshot" = "upload") {
    const nonce = crypto.getRandomValues(new Uint8Array(16))
    setUpload({
      requestId: Array.from(nonce, (b) => b.toString(16).padStart(2, "0")).join(""),
      revision,
      kind,
      recipient: capabilities.data?.recipient || "",
    })
    setArtifactId(artifacts.data?.items[0]?.id || "")
    const preferred =
      destinations.find((d) => d.id === preferredId && uploadReady(d)) ||
      destinations.find(uploadReady)
    setTargetIds(preferred ? [preferred.id] : [])
    setError("")
  }

  async function submit(event: React.FormEvent<HTMLFormElement>) {
    event.preventDefault()
    if (!upload || busy.current) return
    if (
      (upload.kind === "upload" && !artifactId) ||
      targetIds.length < 1 ||
      targetIds.length > 16
    ) {
      setError(
        upload.kind === "snapshot" ? "请选择 1 到 16 个备份地点" : "请选择归档及 1 到 16 个地点"
      )
      return
    }
    busy.current = true
    setPending(true)
    setError("")
    try {
      const payload = {
        destinationIds: targetIds,
        requestId: upload.requestId,
        destinationsRevision: upload.revision,
      }
      const job =
        upload.kind === "snapshot"
          ? await api.createBackupSnapshot({ ...payload, expectedRecipient: upload.recipient })
          : await api.createBackupUpload({ ...payload, artifactId })
      setUpload(null)
      setCursor("")
      setDetailId(job.id)
      client.setQueryData(["admin", "backup-job", userId, job.id], job)
      await client.invalidateQueries({ queryKey: ["admin", "backup-jobs", userId] })
    } catch (e) {
      setError(errorMessage(e))
    } finally {
      busy.current = false
      setPending(false)
    }
  }

  async function cancelJob(job: BackupUploadJob) {
    if (busy.current) return
    busy.current = true
    setPending(true)
    setError("")
    try {
      await api.cancelBackupUpload(job.id)
      await Promise.all([detail.refetch(), jobs.refetch()])
    } catch (e) {
      setError(errorMessage(e))
    } finally {
      busy.current = false
      setPending(false)
    }
  }

  return (
    <section className="min-w-0 rounded-xl border bg-background" data-lanqin-i18n-ignore>
      <header className="flex flex-wrap items-center justify-between gap-3 border-b px-4 py-3 sm:px-5">
        <div className="flex items-center gap-2 text-sm font-medium">
          <Clock3 className="h-4 w-4" aria-hidden="true" />
          {uiText("备份与上传任务")}
        </div>
        <div className="flex items-center gap-2">
          <Button
            variant="ghost"
            size="icon"
            aria-label={uiText("刷新上传任务")}
            disabled={pending || jobs.isFetching || artifacts.isFetching}
            onClick={() => {
              setError("")
              void jobs.refetch()
              void artifacts.refetch()
              void capabilities.refetch()
            }}
          >
            <RefreshCcw className="h-4 w-4" />
          </Button>
          <Button
            size="sm"
            variant="outline"
            onClick={() => openUpload("upload")}
            disabled={
              !canUpload ||
              pending ||
              !artifacts.data?.available ||
              !artifacts.data.items.length ||
              !destinations.some(uploadReady)
            }
          >
            <Upload className="h-4 w-4" />
            {uiText("上传归档")}
          </Button>
          <Button
            size="sm"
            onClick={() => openUpload("snapshot")}
            disabled={
              !canUpload ||
              pending ||
              !capabilities.data?.onlineBackupAvailable ||
              !capabilities.data.recipient ||
              !destinations.some(uploadReady)
            }
          >
            <Plus className="h-4 w-4" />
            {uiText("新建备份")}
          </Button>
        </div>
      </header>
      <div className="space-y-3 p-4 sm:p-5">
        {capabilities.isPending && (
          <div role="status" className="text-sm text-muted-foreground">
            {uiText("加载中...")}
          </div>
        )}
        {capabilities.isError && (
          <div role="alert" className="text-sm text-destructive">
            {uiText(errorMessage(capabilities.error))}
          </div>
        )}
        {capabilities.data && !capabilities.data.onlineBackupAvailable && (
          <div role="status" className="text-sm text-muted-foreground">
            {capabilityText(capabilities.data.reason)}
          </div>
        )}
        {!destinations.some((d) => !d.disabled) && (
          <div role="status" className="text-sm text-muted-foreground">
            {uiText("请先启用至少一个备份地点")}
          </div>
        )}
        {artifacts.isPending && (
          <div role="status" className="text-sm text-muted-foreground">
            {uiText("加载中...")}
          </div>
        )}
        {artifacts.isError && (
          <div role="alert" className="text-sm text-destructive">
            {uiText(errorMessage(artifacts.error))}
          </div>
        )}
        {artifacts.data && !artifacts.data.available && (
          <div role="status" className="text-sm text-muted-foreground">
            {uiText("请先配置 LANQIN_BACKUP_ARTIFACT_DIR 并由宿主机发布加密归档")}
          </div>
        )}
        {artifacts.data?.available && !artifacts.data.items.length && (
          <div role="status" className="text-sm text-muted-foreground">
            {uiText("暂无已发布的加密归档")}
          </div>
        )}
        {jobs.isError && (
          <div role="alert" className="text-sm text-destructive">
            {uiText(errorMessage(jobs.error))}
          </div>
        )}
        {error && !upload && !detailId && (
          <div role="alert" className="text-sm text-destructive">
            {uiText(error)}
          </div>
        )}
        {jobs.isPending && <div role="status">{uiText("加载中...")}</div>}
        {jobs.data && !jobs.data.items.length && (
          <div className="text-sm text-muted-foreground">{uiText("暂无上传任务")}</div>
        )}
        {jobs.data?.items.map((job) => (
          <Button
            key={job.id}
            variant="ghost"
            className="h-auto w-full justify-between gap-4 whitespace-normal border p-3 text-left shadow-none"
            aria-label={uiText("查看上传任务 {0}", [job.id])}
            onClick={() => {
              setDetailId(job.id)
              setError("")
            }}
          >
            <span className="min-w-0 space-y-1">
              <span className="block truncate text-sm">{job.destinationNames.join(" / ")}</span>
              <span className="block text-xs font-normal text-muted-foreground">
                {uiText(job.kind === "snapshot" ? "在线备份" : "归档上传")}
              </span>
              <span className="block text-xs font-normal text-muted-foreground">
                {formatDateTime(job.createdAt)}
              </span>
            </span>
            <Badge
              variant={
                job.status === "failed" || job.status === "interrupted"
                  ? "destructive"
                  : "secondary"
              }
              className="max-w-48 shrink-0 whitespace-normal"
            >
              {jobStatusText(job)}
            </Badge>
          </Button>
        ))}
        {(cursor || jobs.data?.nextCursor) && (
          <div className="flex flex-wrap gap-2">
            <Button
              variant="outline"
              disabled={!cursor || jobs.isFetching}
              onClick={() => setCursor("")}
            >
              {uiText("最新任务")}
            </Button>
            <Button
              variant="outline"
              disabled={!jobs.data?.nextCursor || jobs.isFetching}
              onClick={() => setCursor(jobs.data?.nextCursor || "")}
            >
              {uiText("更早任务")}
            </Button>
          </div>
        )}
      </div>
      <Dialog
        open={!!upload}
        onOpenChange={(open) => {
          if (!open && !pending) {
            setUpload(null)
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
              <DialogTitle>
                {uiText(upload?.kind === "snapshot" ? "新建在线备份" : "上传加密归档")}
              </DialogTitle>
            </DialogHeader>
            <div role="status" className="rounded-md border p-3 text-sm">
              {uiText(
                upload?.kind === "snapshot"
                  ? "后台不停服采集 SQLite、邮件、附件与 DKIM，加密后上传；未索引邮件隔离保存，不是生产覆盖恢复"
                  : "仅上传已发布的归档，不会生成新的整站快照"
              )}
            </div>
            {upload?.kind === "snapshot" && (
              <div className="space-y-2">
                <Label>{uiText("加密公钥")}</Label>
                <div className="break-all rounded-md border p-3 font-mono text-xs">
                  {upload.recipient}
                </div>
              </div>
            )}
            {upload?.kind === "upload" && (
              <div className="space-y-2">
                <Label htmlFor="backup-artifact">{uiText("选择加密归档")}</Label>
                <Select value={artifactId} onValueChange={setArtifactId} disabled={pending}>
                  <SelectTrigger id="backup-artifact">
                    <SelectValue />
                  </SelectTrigger>
                  <SelectContent data-lanqin-i18n-ignore>
                    {artifacts.data?.items.map((item) => (
                      <SelectItem key={item.id} value={item.id}>
                        {formatDateTime(item.publishedAt)} · {formatBytes(item.bytes)} ·{" "}
                        {item.id.slice(-8)}
                      </SelectItem>
                    ))}
                  </SelectContent>
                </Select>
              </div>
            )}
            <div className="space-y-3">
              <div className="text-sm font-medium">{uiText("选择上传地点（最多 16 个）")}</div>
              {destinations
                .filter((d) => !d.disabled)
                .map((d) => (
                  <div key={d.id} className="flex items-center gap-2">
                    <Checkbox
                      id={`backup-upload-${d.id}`}
                      checked={targetIds.includes(d.id)}
                      disabled={
                        pending ||
                        !uploadReady(d) ||
                        (targetIds.length >= 16 && !targetIds.includes(d.id))
                      }
                      onCheckedChange={(checked) =>
                        setTargetIds((ids) =>
                          checked ? [...ids, d.id] : ids.filter((id) => id !== d.id)
                        )
                      }
                    />
                    <Label className="break-words" htmlFor={`backup-upload-${d.id}`}>
                      {d.name}
                      {d.type === "drive" && !d.credentialsSet && (
                        <span className="ml-2 text-xs text-muted-foreground">
                          {uiText("Google Drive 需要授权")}
                        </span>
                      )}
                    </Label>
                  </div>
                ))}
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
                  setUpload(null)
                  setError("")
                }}
              >
                {uiText("取消")}
              </Button>
              <Button
                type="submit"
                disabled={
                  pending ||
                  (upload?.kind === "upload" && !artifactId) ||
                  !targetIds.length ||
                  (upload?.kind === "snapshot" && !capabilities.data?.onlineBackupAvailable)
                }
              >
                {uiText(
                  pending
                    ? "处理中..."
                    : upload?.kind === "snapshot"
                      ? "创建备份任务"
                      : "加入上传队列"
                )}
              </Button>
            </DialogFooter>
          </form>
        </DialogContent>
      </Dialog>
      <Dialog
        open={!!detailId}
        onOpenChange={(open) => {
          if (!open && !pending) {
            setDetailId("")
            setError("")
          }
        }}
      >
        <DialogContent
          className="max-h-[90svh] overflow-y-auto sm:max-w-2xl"
          aria-describedby={undefined}
          data-lanqin-i18n-ignore
        >
          <DialogHeader>
            <DialogTitle>
              {uiText(detail.data?.kind === "snapshot" ? "备份任务" : "上传任务")}
            </DialogTitle>
          </DialogHeader>
          {detail.isPending && <div role="status">{uiText("加载中...")}</div>}
          {detail.isError && (
            <div role="alert" className="text-sm text-destructive">
              {uiText(errorMessage(detail.error))}
            </div>
          )}
          {detail.data && (
            <div className="min-w-0 space-y-4">
              <Badge variant="secondary">{jobStatusText(detail.data)}</Badge>
              <div className="space-y-1">
                <Label>{uiText("任务 ID")}</Label>
                <div className="break-all font-mono text-xs">{detail.data.id}</div>
              </div>
              {detail.data.artifact.sha256 && (
                <div className="space-y-1">
                  <Label>{uiText("归档 SHA-256")}</Label>
                  <div className="break-all font-mono text-xs">{detail.data.artifact.sha256}</div>
                </div>
              )}
              {detail.data.artifact.id && (
                <div className="space-y-1">
                  <Label>{uiText("归档发布时间")}</Label>
                  <div className="text-sm">{formatDateTime(detail.data.artifact.publishedAt)}</div>
                </div>
              )}
              {detail.data.recipient && (
                <div className="space-y-1">
                  <Label>{uiText("加密公钥")}</Label>
                  <div className="break-all font-mono text-xs">{detail.data.recipient}</div>
                </div>
              )}
              {detail.data.summary && (
                <div className="space-y-2 rounded-md border p-3 text-sm">
                  <div>
                    {uiText("已采集邮件 {0} 封、草稿 {1} 封、附件 {2} 个", [
                      detail.data.summary.messages,
                      detail.data.summary.drafts,
                      detail.data.summary.attachments,
                    ])}
                  </div>
                  <div>
                    {uiText("未索引候选 {0} 封（隔离保存）；外部缓存记录 {1} 条", [
                      detail.data.summary.unindexed,
                      detail.data.summary.externalCachedMessagesOnly,
                    ])}
                  </div>
                </div>
              )}
              {detail.data.errorCode && (
                <div role="status" className="text-sm text-muted-foreground">
                  {resultError(detail.data.errorCode)}
                </div>
              )}
              {detail.data.locations.map((location) => {
                const result = detail.data?.results.find((r) => r.destination_id === location.id)
                return (
                  <div key={location.id} className="space-y-2 rounded-md border p-3">
                    <div className="flex flex-wrap items-center justify-between gap-2">
                      <span className="break-words font-medium">{location.name}</span>
                      <Badge variant="outline">
                        {result ? statusText(result.state) : jobStatusText(detail.data!)}
                      </Badge>
                    </div>
                    <div className="break-all text-xs text-muted-foreground">
                      {location.endpoint}
                      {location.bucket ? ` / ${location.bucket}` : ""}
                      {location.folderId
                        ? `${location.endpoint ? " / " : ""}${location.folderId}`
                        : ""}
                    </div>
                    {result?.object_key && (
                      <div className="break-all font-mono text-xs">{result.object_key}</div>
                    )}
                    {result?.error_code && (
                      <div className="text-sm text-destructive">
                        {resultError(result.error_code)}
                      </div>
                    )}
                  </div>
                )
              })}
              <div role="status" className="text-sm text-muted-foreground">
                {uiText("上传完成表示远端已接受文件，尚未进行下载解密验证；请独立保存报告")}
              </div>
            </div>
          )}
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
                setDetailId("")
                setError("")
              }}
            >
              {uiText("关闭")}
            </Button>
            {detail.data && (
              <Button variant="outline" asChild>
                <a
                  href={`/api/admin/backups/jobs/${encodeURIComponent(detail.data.id)}?download=1`}
                  download
                >
                  <Download className="h-4 w-4" />
                  {uiText("保存上传报告")}
                </a>
              </Button>
            )}
            {detail.data && active(detail.data) && (
              <Button
                variant="destructive"
                disabled={pending || detail.data.cancelRequested}
                onClick={() => {
                  if (detail.data) void cancelJob(detail.data)
                }}
              >
                {uiText(
                  detail.data.cancelRequested
                    ? "正在取消..."
                    : detail.data.kind === "snapshot"
                      ? "取消备份"
                      : "取消上传"
                )}
              </Button>
            )}
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </section>
  )
}
