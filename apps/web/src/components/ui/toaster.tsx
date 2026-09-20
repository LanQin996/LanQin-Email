"use client"

import { uiText, useLanguage, type UiMessage } from "@/lib/language"
import type { ReactNode } from "react"

import { useToast } from "@/hooks/use-toast"
import {
  Toast,
  ToastClose,
  ToastDescription,
  ToastProvider,
  ToastTitle,
  ToastViewport,
} from "@/components/ui/toast"

export function Toaster() {
  useLanguage()
  const { toasts } = useToast()

  return (
    <ToastProvider label={uiText("通知")}>
      {toasts.map(function ({ id, title, description, action, ...props }) {
        return (
          <Toast key={id} {...props}>
            <div className="grid gap-1">
              {title && <ToastTitle>{localizeNotice(title)}</ToastTitle>}
              {description && <ToastDescription>{localizeNotice(description)}</ToastDescription>}
            </div>
            {action}
            <ToastClose />
          </Toast>
        )
      })}
      <ToastViewport />
    </ToastProvider>
  )
}

function localizeNotice(value: ReactNode | UiMessage): ReactNode {
  if (typeof value === "string") return uiText(value)
  if (
    value &&
    typeof value === "object" &&
    "key" in value &&
    !("$$typeof" in value) &&
    typeof value.key === "string"
  )
    return uiText(value as UiMessage)
  return value as ReactNode
}
