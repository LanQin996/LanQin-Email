import * as React from "react"
import { useQuery } from "@tanstack/react-query"
import { KeyRound } from "lucide-react"
import { api, type BackupDestination } from "@/lib/api"
import { uiText, useLanguage } from "@/lib/language"
import { errorMessage } from "@/lib/ui-errors"
import { Button } from "@/components/ui/button"
import { Badge } from "@/components/ui/badge"

export function BackupDriveAuthorization({
  destination,
  userId,
  revision,
  editable,
}: {
  destination: BackupDestination
  userId: string
  revision: string
  editable: boolean
}) {
  useLanguage()
  const capabilities = useQuery({
    queryKey: ["admin", "backup-capabilities", userId],
    queryFn: api.backupCapabilities,
    enabled: !!userId,
    retry: false,
  })
  const [pending, setPending] = React.useState(false)
  const [error, setError] = React.useState("")
  const busy = React.useRef(false)

  async function authorize() {
    if (busy.current || capabilities.isError || !capabilities.data?.driveOAuthAvailable) return
    busy.current = true
    setPending(true)
    setError("")
    try {
      const result = await api.authorizeBackupDrive(destination.id, revision)
      const url = new URL(result.authorizationUrl)
      if (
        url.protocol !== "https:" ||
        url.host !== "accounts.google.com" ||
        url.pathname !== "/o/oauth2/v2/auth" ||
        url.username ||
        url.password ||
        url.hash
      ) {
        setError("Google Drive 授权地址无效，请联系管理员")
        return
      }
      window.location.assign(url.href)
    } catch (cause) {
      setError(errorMessage(cause))
    } finally {
      busy.current = false
      setPending(false)
    }
  }

  return (
    <section className="space-y-3 rounded-lg border p-4" data-lanqin-i18n-ignore>
      <div className="flex flex-wrap items-center justify-between gap-3">
        <Badge variant={destination.credentialsSet ? "secondary" : "outline"}>
          {uiText(destination.credentialsSet ? "Google Drive 已授权" : "Google Drive 需要授权")}
        </Badge>
        <Button
          type="button"
          variant="outline"
          size="sm"
          disabled={
            !editable ||
            destination.disabled ||
            pending ||
            capabilities.isError ||
            !capabilities.data?.driveOAuthAvailable
          }
          onClick={() => void authorize()}
        >
          <KeyRound className="h-4 w-4" />
          {uiText(
            pending
              ? "正在准备授权..."
              : destination.credentialsSet
                ? "重新授权 Google Drive"
                : "授权 Google Drive"
          )}
        </Button>
      </div>
      {capabilities.isPending && (
        <div role="status" className="text-sm text-muted-foreground">
          {uiText("加载中...")}
        </div>
      )}
      {capabilities.isError && (
        <div className="space-y-2">
          <div role="alert" className="text-sm text-destructive">
            {uiText(errorMessage(capabilities.error))}
          </div>
          <Button
            type="button"
            size="sm"
            variant="outline"
            disabled={capabilities.isFetching}
            onClick={() => void capabilities.refetch()}
          >
            {uiText("重试")}
          </Button>
        </div>
      )}
      {capabilities.data && !capabilities.data.driveOAuthAvailable && (
        <div role="status" className="text-sm text-muted-foreground">
          {uiText("服务器尚未配置 Google Drive OAuth 客户端")}
        </div>
      )}
      <div role="status" className="text-sm text-muted-foreground">
        {uiText("每次授权会创建独立备份文件夹，不删除旧目录；仅申请应用文件权限")}
      </div>
      {destination.disabled && (
        <div role="status" className="text-sm text-muted-foreground">
          {uiText("请先启用此地点再进行 Google Drive 授权")}
        </div>
      )}
      {error && (
        <div role="alert" className="text-sm text-destructive">
          {uiText(error)}
        </div>
      )}
    </section>
  )
}
