import { type UiText, uiText, useLanguage as useUiLanguage } from "@/lib/language"
import * as React from "react"
import { Button } from "@/components/ui/button"
import {
  Dialog,
  DialogContent,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog"

type ConfirmDialogProps = {
  open: boolean
  title: UiText
  description?: UiText
  confirmText?: UiText
  cancelText?: UiText
  destructive?: boolean
  pending?: boolean
  onOpenChange: (open: boolean) => void
  onConfirm: () => void
}

export function ConfirmDialog({
  open,
  title,
  description,
  confirmText = "确认",
  cancelText = "取消",
  destructive = false,
  pending = false,
  onOpenChange,
  onConfirm,
}: ConfirmDialogProps) {
  useUiLanguage()

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent aria-describedby={undefined}>
        <DialogHeader>
          <DialogTitle>{uiText(title)}</DialogTitle>
        </DialogHeader>
        {description && (
          <div className="whitespace-pre-line text-sm text-muted-foreground">
            {uiText(description)}
          </div>
        )}
        <DialogFooter>
          <Button
            type="button"
            variant="outline"
            onClick={() => onOpenChange(false)}
            disabled={pending}
          >
            {uiText(cancelText)}
          </Button>
          <Button
            type="button"
            variant={destructive ? "destructive" : "default"}
            onClick={onConfirm}
            disabled={pending}
          >
            {pending ? uiText("处理中...") : uiText(confirmText)}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}
