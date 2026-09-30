import { useState } from "react"
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"
import { api } from "@/lib/api"
import { uiText, useLanguage } from "@/lib/language"
import { errorMessage } from "@/lib/ui-errors"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import { Switch } from "@/components/ui/switch"
import { Badge } from "@/components/ui/badge"
import {
  Dialog,
  DialogContent,
  DialogHeader,
  DialogTitle,
  DialogTrigger,
  DialogFooter,
} from "@/components/ui/dialog"
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select"

type Collection = { enabled: boolean; targetMailboxId: string }
type Target = { id: string; address: string }

export function DomainCollectionDialog({
  domainId,
  domainName,
}: {
  domainId: string
  domainName: string
}) {
  useLanguage()
  const [open, setOpen] = useState(false)
  const settings = useQuery({
    queryKey: ["domain-collection", domainId],
    queryFn: () => api.domainCollection(domainId),
    enabled: open,
    refetchOnWindowFocus: false,
  })
  const targets = useQuery({
    queryKey: ["domain-collection-targets"],
    queryFn: api.domainCollectionTargets,
    enabled: open,
    refetchOnWindowFocus: false,
  })
  return (
    <Dialog open={open} onOpenChange={setOpen}>
      <DialogTrigger asChild>
        <Button size="sm" variant="outline">
          {uiText("统一收件")}
        </Button>
      </DialogTrigger>
      <DialogContent
        aria-describedby={undefined}
        data-lanqin-i18n-ignore
        className="max-h-[90dvh] overflow-y-auto"
      >
        <DialogHeader>
          <DialogTitle>{uiText("统一收件：{0}", [domainName])}</DialogTitle>
        </DialogHeader>
        {settings.isError || targets.isError ? (
          <div className="space-y-3">
            <div role="alert" className="text-sm text-destructive">
              {uiText("无法加载统一收件设置")}
            </div>
            <Button
              variant="outline"
              disabled={settings.isFetching || targets.isFetching}
              onClick={() => {
                void settings.refetch()
                void targets.refetch()
              }}
            >
              {uiText("重试")}
            </Button>
          </div>
        ) : !settings.data || !targets.data || settings.isFetching || targets.isFetching ? (
          <div role="status" className="py-6 text-sm text-muted-foreground">
            {uiText("加载中...")}
          </div>
        ) : (
          <CollectionForm
            key={domainId}
            domainId={domainId}
            initial={settings.data}
            targets={targets.data.items}
            onSaved={() => setOpen(false)}
          />
        )}
      </DialogContent>
    </Dialog>
  )
}

function CollectionForm({
  domainId,
  initial,
  targets,
  onSaved,
}: {
  domainId: string
  initial: Collection
  targets: Target[]
  onSaved: () => void
}) {
  useLanguage()
  const qc = useQueryClient()
  const [enabled, setEnabled] = useState(initial.enabled)
  const [targetMailboxId, setTargetMailboxId] = useState(initial.targetMailboxId)
  const [search, setSearch] = useState("")
  const selected = targets.find((target) => target.id === targetMailboxId)
  const filtered = targets.filter(
    (target) =>
      target.id === targetMailboxId ||
      target.address.toLowerCase().includes(search.trim().toLowerCase())
  )
  const save = useMutation({
    mutationFn: () =>
      api.setDomainCollection(domainId, {
        enabled,
        targetMailboxId: enabled ? targetMailboxId : "",
      }),
    onSuccess: (data) => {
      qc.setQueryData(["domain-collection", domainId], data)
      void qc.invalidateQueries({ queryKey: ["domain-collection-targets"] })
      onSaved()
    },
  })
  const jobs = useQuery({ queryKey: ["local-delivery-jobs"], queryFn: api.localDeliveryJobs })
  const failedJobs = (jobs.data?.items || []).filter(
    (job) => job.targetMailboxId === targetMailboxId && job.status !== "delivered"
  )
  return (
    <form
      className="space-y-5"
      onSubmit={(event) => {
        event.preventDefault()
        save.mutate()
      }}
    >
      <div className="flex items-center justify-between gap-3">
        <Label htmlFor="collection-enabled">{uiText("启用统一收件")}</Label>
        <Switch
          id="collection-enabled"
          checked={enabled}
          onCheckedChange={setEnabled}
          disabled={save.isPending}
        />
      </div>
      <Badge variant="secondary">{uiText("保留原邮箱邮件")}</Badge>
      <div className="space-y-2">
        <Label htmlFor="collection-search">{uiText("搜索站内邮箱")}</Label>
        <Input
          id="collection-search"
          value={search}
          onChange={(event) => setSearch(event.target.value)}
          disabled={!enabled || save.isPending}
        />
        <Label htmlFor="collection-target">{uiText("统一收件目标")}</Label>
        <Select
          value={targetMailboxId}
          onValueChange={setTargetMailboxId}
          disabled={!enabled || save.isPending}
        >
          <SelectTrigger id="collection-target">
            <SelectValue placeholder={uiText("选择站内邮箱")} />
          </SelectTrigger>
          <SelectContent data-lanqin-i18n-ignore>
            {filtered.map((target) => (
              <SelectItem key={target.id} value={target.id}>
                {target.address}
              </SelectItem>
            ))}
            {filtered.length === 0 && (
              <div className="p-3 text-sm text-muted-foreground">
                {uiText("没有可用的站内邮箱")}
              </div>
            )}
          </SelectContent>
        </Select>
        {enabled && targetMailboxId && !selected && (
          <div role="alert" className="text-sm text-destructive">
            {uiText("原目标不可用，请重新选择或关闭统一收件")}
          </div>
        )}
      </div>
      {save.isError && (
        <div role="alert" className="text-sm text-destructive">
          {uiText(errorMessage(save.error))}
        </div>
      )}
      {jobs.isError && (
        <div role="alert" className="text-sm text-destructive">
          {uiText("无法加载本地投递状态")}
        </div>
      )}
      {failedJobs.length > 0 && (
        <section className="space-y-2 rounded-lg border p-3">
          <h3 className="text-sm font-medium">{uiText("本地投递任务")}</h3>
          {failedJobs.slice(0, 5).map((job) => (
            <div key={job.id} className="flex flex-wrap justify-between gap-2 text-sm">
              <span>{job.status === "failed" ? uiText("投递失败") : uiText("等待投递重试")}</span>
              <span>{uiText("已尝试 {0} 次", [job.attempts])}</span>
            </div>
          ))}
        </section>
      )}
      <DialogFooter>
        <Button type="submit" disabled={save.isPending || (enabled && !selected)}>
          {save.isPending ? uiText("保存中...") : uiText("保存")}
        </Button>
      </DialogFooter>
    </form>
  )
}
