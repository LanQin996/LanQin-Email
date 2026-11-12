import * as React from "react"
import { type BackupDestination, api } from "@/lib/api"
import { uiText, useLanguage } from "@/lib/language"
import { errorMessage } from "@/lib/ui-errors"
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

export function BackupRetentionPanel({
  destination,
  revision,
  editable,
  onSaved,
}: {
  destination: BackupDestination
  revision: string
  editable: boolean
  onSaved: () => Promise<unknown>
}) {
  useLanguage()
  const [editor, setEditor] = React.useState<{ revision: string; count: string } | null>(null)
  const [confirmed, setConfirmed] = React.useState(false)
  const [pending, setPending] = React.useState(false)
  const [error, setError] = React.useState("")
  const busy = React.useRef(false)
  const id = React.useId()

  async function submit(event: React.FormEvent<HTMLFormElement>) {
    event.preventDefault()
    if (!editor || busy.current) return
    const value = String(new FormData(event.currentTarget).get("keepLast") || "")
    if (!/^\d+$/.test(value) || Number(value) > 100 || (Number(value) > 0 && !confirmed)) {
      setError("请填写 0 至 100，并确认远端清理")
      return
    }
    busy.current = true
    setPending(true)
    setError("")
    try {
      await api.updateBackupRetention(destination.id, {
        revision: editor.revision,
        keepLast: Number(value),
        confirm: confirmed,
      })
      setEditor(null)
      await onSaved()
    } catch (e) {
      setError(errorMessage(e))
      await onSaved()
    } finally {
      busy.current = false
      setPending(false)
    }
  }

  return (
    <section className="min-w-0 rounded-lg border p-4" data-lanqin-i18n-ignore>
      <header className="flex flex-wrap items-center justify-between gap-3">
        <h3 className="text-sm font-medium">{uiText("远端保留策略")}</h3>
        <div className="flex flex-wrap items-center gap-2">
          <Badge variant="secondary">
            {destination.keepLast
              ? uiText("保留最近 {0} 份", [destination.keepLast])
              : uiText("自动清理已关闭")}
          </Badge>
          <Button
            variant="outline"
            size="sm"
            disabled={!editable || pending}
            onClick={() => {
              setEditor({ revision, count: String(destination.keepLast || 0) })
              setConfirmed(false)
              setError("")
            }}
          >
            {uiText("设置保留策略")}
          </Button>
        </div>
      </header>
      <Dialog
        open={!!editor}
        onOpenChange={(open) => {
          if (!open && !pending) setEditor(null)
        }}
      >
        <DialogContent
          className="max-h-[90svh] overflow-y-auto"
          aria-describedby={undefined}
          data-lanqin-i18n-ignore
        >
          <form onSubmit={submit} className="space-y-4">
            <DialogHeader>
              <DialogTitle>{uiText("设置保留策略")}</DialogTitle>
            </DialogHeader>
            <div className="break-words text-sm font-medium">{destination.name}</div>
            <div className="space-y-2">
              <Label htmlFor={id}>{uiText("保留份数（0 关闭，1 至 100）")}</Label>
              <Input
                id={id}
                name="keepLast"
                type="number"
                min={0}
                max={100}
                step={1}
                required
                disabled={pending}
                value={editor?.count || ""}
                onChange={(e) => {
                  const count = e.currentTarget.value
                  setEditor((value) => value && { ...value, count })
                  setConfirmed(false)
                }}
              />
            </div>
            <div role="note" className="rounded-md border p-3 text-sm">
              {uiText(
                "新备份成功并回读校验后，才清理此地点已确认的旧备份；失败时保留现有版本，不清理其他文件"
              )}
            </div>
            {Number(editor?.count) > 0 && (
              <div className="flex items-start gap-3">
                <Checkbox
                  id={`${id}-confirm`}
                  checked={confirmed}
                  disabled={pending}
                  onCheckedChange={(value) => setConfirmed(value === true)}
                />
                <Label htmlFor={`${id}-confirm`} className="leading-normal">
                  {uiText("我确认自动删除超出保留份数的远端旧备份，此操作不可撤销")}
                </Label>
              </div>
            )}
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
                onClick={() => setEditor(null)}
              >
                {uiText("取消")}
              </Button>
              <Button type="submit" disabled={pending || (Number(editor?.count) > 0 && !confirmed)}>
                {uiText(pending ? "处理中..." : "保存")}
              </Button>
            </DialogFooter>
          </form>
        </DialogContent>
      </Dialog>
    </section>
  )
}
