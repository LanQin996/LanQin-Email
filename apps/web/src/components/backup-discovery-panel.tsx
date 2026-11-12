import * as React from "react"
import { useQuery } from "@tanstack/react-query"
import { Search } from "lucide-react"
import { api, type BackupDestination, type BackupUploadJob } from "@/lib/api"
import { uiText, useLanguage } from "@/lib/language"
import { errorMessage } from "@/lib/ui-errors"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select"

export function BackupDiscoveryPanel({
  userId,
  destinations,
  revision,
  preferredId,
  canDiscover,
  onCreated,
}: {
  userId: string
  destinations: BackupDestination[]
  revision: string
  preferredId?: string
  canDiscover: boolean
  onCreated: (job: BackupUploadJob) => void
}) {
  useLanguage()
  const id = React.useId()
  const capabilities = useQuery({
    queryKey: ["admin", "backup-capabilities", userId],
    queryFn: api.backupCapabilities,
    enabled: !!userId,
    retry: false,
  })
  const [destinationId, setDestinationId] = React.useState(preferredId || "")
  const [sourceFolderId, setSourceFolderId] = React.useState("")
  const [pending, setPending] = React.useState(false)
  const [error, setError] = React.useState("")
  const busy = React.useRef(false)
  const request = React.useRef<{ fingerprint: string; id: string } | null>(null)
  const choices = destinations.filter((d) => !d.disabled && d.credentialsSet)
  const selected =
    choices.find((d) => d.id === destinationId) ||
    choices.find((d) => d.id === preferredId) ||
    choices[0]
  const available = canDiscover && !!capabilities.data?.discoveryAvailable && !!selected

  async function submit(event: React.FormEvent<HTMLFormElement>) {
    event.preventDefault()
    if (!available || !selected || busy.current) return
    const folder = selected.type === "drive" ? sourceFolderId.trim() : ""
    const fingerprint = JSON.stringify([selected.id, revision, folder])
    if (request.current?.fingerprint !== fingerprint) {
      request.current = {
        fingerprint,
        id: Array.from(crypto.getRandomValues(new Uint8Array(16)), (b) =>
          b.toString(16).padStart(2, "0")
        ).join(""),
      }
    }
    busy.current = true
    setPending(true)
    setError("")
    try {
      const job = await api.createBackupDiscovery({
        destinationId: selected.id,
        destinationsRevision: revision,
        requestId: request.current.id,
        ...(folder && { sourceFolderId: folder }),
      })
      request.current = null
      onCreated(job)
    } catch (e) {
      setError(errorMessage(e))
    } finally {
      busy.current = false
      setPending(false)
    }
  }

  return (
    <section className="min-w-0 space-y-4 rounded-xl border p-4 sm:p-5" data-lanqin-i18n-ignore>
      <h3 className="text-sm font-medium">{uiText("从云端发现备份")}</h3>
      <form onSubmit={submit} className="space-y-4">
        <div className="grid min-w-0 gap-4 sm:grid-cols-2">
          <div className="min-w-0 space-y-2">
            <Label htmlFor={`${id}-destination`}>{uiText("备份地点")}</Label>
            <Select
              value={selected?.id || ""}
              disabled={pending || !canDiscover || !choices.length}
              onValueChange={(value) => {
                setDestinationId(value)
                setSourceFolderId("")
                setError("")
              }}
            >
              <SelectTrigger id={`${id}-destination`} className="min-w-0">
                <SelectValue placeholder={uiText("选择备份地点")} />
              </SelectTrigger>
              <SelectContent data-lanqin-i18n-ignore>
                {choices.map((d) => (
                  <SelectItem key={d.id} value={d.id}>
                    {d.name}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
          </div>
          {selected?.type === "drive" && (
            <div className="min-w-0 space-y-2">
              <Label htmlFor={`${id}-folder`}>{uiText("原 Google Drive 文件夹 ID（可选）")}</Label>
              <Input
                id={`${id}-folder`}
                value={sourceFolderId}
                onChange={(event) => setSourceFolderId(event.target.value)}
                maxLength={256}
                pattern="[A-Za-z0-9_-]*"
                autoComplete="off"
                disabled={pending || !canDiscover}
              />
            </div>
          )}
        </div>
        {selected?.type === "drive" && (
          <div role="note" className="rounded-md border p-3 text-sm">
            {uiText(
              "留空使用当前已授权文件夹；原文件夹必须对当前 Google 授权可见，不会更改地点配置"
            )}
          </div>
        )}
        <div role="note" className="rounded-md border p-3 text-sm">
          {uiText("不依赖本地上传历史，仅验证云端恢复索引；列表不保证完整、最新或数据仍可恢复")}
        </div>
        {capabilities.isPending && <div role="status">{uiText("加载中...")}</div>}
        {capabilities.isError && (
          <div role="alert" className="text-sm text-destructive">
            {uiText(errorMessage(capabilities.error))}
          </div>
        )}
        {!capabilities.isPending &&
          !capabilities.isError &&
          !capabilities.data?.discoveryAvailable && (
            <div role="status" className="text-sm text-muted-foreground">
              {uiText("云端发现不可用，请检查备份主密钥和服务器网络策略")}
            </div>
          )}
        {!choices.length && (
          <div role="status" className="text-sm text-muted-foreground">
            {uiText("请先启用并授权一个备份地点")}
          </div>
        )}
        {error && (
          <div role="alert" className="text-sm text-destructive">
            {uiText(error)}
          </div>
        )}
        <Button type="submit" disabled={!available || pending} className="w-full sm:w-auto">
          <Search className="h-4 w-4" />
          {uiText(pending ? "处理中..." : "开始发现")}
        </Button>
      </form>
    </section>
  )
}
