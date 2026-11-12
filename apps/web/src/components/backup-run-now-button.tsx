import * as React from "react"
import { useQuery, useQueryClient } from "@tanstack/react-query"
import { Play } from "lucide-react"
import { api, type BackupDestination, type BackupUploadJob } from "@/lib/api"
import { uiText, useLanguage } from "@/lib/language"
import { errorMessage } from "@/lib/ui-errors"
import { Button } from "@/components/ui/button"

export function BackupRunNowButton({
  userId,
  destination,
  revision,
  editable,
  onCreated,
}: {
  userId: string
  destination: BackupDestination
  revision: string
  editable: boolean
  onCreated: (job: BackupUploadJob) => void
}) {
  useLanguage()
  const client = useQueryClient()
  const capabilities = useQuery({
    queryKey: ["admin", "backup-capabilities", userId],
    queryFn: api.backupCapabilities,
    enabled: !!userId,
    retry: false,
  })
  const [pending, setPending] = React.useState(false)
  const [error, setError] = React.useState("")
  const busy = React.useRef(false)
  const attempt = React.useRef<{ requestId: string; revision: string; recipient: string }>()
  const available =
    editable &&
    !destination.disabled &&
    (destination.type !== "drive" || destination.credentialsSet) &&
    !!capabilities.data?.onlineBackupAvailable &&
    !!capabilities.data.recipient?.startsWith("password:")
  async function execute() {
    if (!available || busy.current) return
    busy.current = true
    setPending(true)
    setError("")
    const binding = { revision, recipient: capabilities.data!.recipient! }
    if (
      !attempt.current ||
      attempt.current.revision !== binding.revision ||
      attempt.current.recipient !== binding.recipient
    ) {
      attempt.current = { ...binding, requestId: crypto.randomUUID() }
    }
    try {
      const job = await api.createBackupSnapshot({
        destinationIds: [destination.id],
        destinationsRevision: attempt.current.revision,
        expectedRecipient: attempt.current.recipient,
        requestId: attempt.current.requestId,
      })
      attempt.current = undefined
      client.setQueryData(["admin", "backup-job", userId, job.id], job)
      await client.invalidateQueries({ queryKey: ["admin", "backup-jobs", userId] })
      onCreated(job)
    } catch (cause) {
      // Retain the same nonce after an ambiguous response so another click can
      // only return the original admission, never silently create duplicate work.
      setError(errorMessage(cause))
      void capabilities.refetch()
    } finally {
      busy.current = false
      setPending(false)
    }
  }
  return (
    <div className="space-y-2" data-lanqin-i18n-ignore>
      <Button
        type="button"
        disabled={!available || pending || capabilities.isFetching}
        onClick={() => void execute()}
      >
        <Play className="h-4 w-4" aria-hidden="true" />
        {uiText(pending ? "正在提交备份..." : "立即执行")}
      </Button>
      {error && (
        <div role="alert" className="text-sm text-destructive">
          {uiText(error)}
        </div>
      )}
    </div>
  )
}
