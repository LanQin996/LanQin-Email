import * as React from "react"
import { Download, Eye, Paperclip } from "lucide-react"
import { Button } from "@/components/ui/button"
import { translateUiText, useLanguage } from "@/lib/language"
import { Dialog, DialogContent, DialogHeader, DialogTitle } from "@/components/ui/dialog"

const MAX_PREVIEW_BYTES = 25 * 1024 * 1024

export function MailAttachment({
  filename,
  contentType,
  sizeBytes,
  sizeLabel,
  href,
  allowed,
}: {
  filename: string
  contentType: string
  sizeBytes: number
  sizeLabel: string
  href: string
  allowed: boolean
}) {
  const [language] = useLanguage()
  const t = (value: string) => translateUiText(value, language)
  const [open, setOpen] = React.useState(false)
  const [url, setUrl] = React.useState("")
  const [error, setError] = React.useState("")
  const mime = contentType.split(";")[0].trim().toLowerCase()
  const isPdf = mime === "application/pdf" || /\.pdf$/i.test(filename)
  const imageMime = /^image\/(png|jpeg|gif|webp|bmp)$/.test(mime) ? mime : ""
  const previewable = isPdf || !!imageMime
  const tooLarge = sizeBytes > MAX_PREVIEW_BYTES

  React.useEffect(() => {
    if (!open || !allowed || !previewable || tooLarge) return
    const controller = new AbortController()
    let objectUrl = ""
    setUrl("")
    setError("")
    void (async () => {
      try {
        const response = await fetch(href, {
          credentials: "same-origin",
          signal: controller.signal,
        })
        if (!response.ok) throw new Error("附件加载失败，请检查访问权限或稍后重试。")
        if (Number(response.headers.get("Content-Length")) > MAX_PREVIEW_BYTES) {
          throw new Error("附件超过 25 MB，请下载后查看。")
        }
        const blob = await response.blob()
        if (blob.size > MAX_PREVIEW_BYTES) {
          throw new Error("附件超过 25 MB，请下载后查看。")
        }
        if (isPdf && (await blob.slice(0, 5).text()) !== "%PDF-") {
          throw new Error("文件不是有效的 PDF，请下载后检查。")
        }
        if (controller.signal.aborted) return
        // Force an allow-listed MIME type; never render attachment HTML or SVG.
        objectUrl = URL.createObjectURL(
          new Blob([blob], { type: isPdf ? "application/pdf" : imageMime })
        )
        setUrl(objectUrl)
      } catch (cause) {
        if (!controller.signal.aborted) {
          const knownErrors = [
            "附件加载失败，请检查访问权限或稍后重试。",
            "附件超过 25 MB，请下载后查看。",
            "文件不是有效的 PDF，请下载后检查。",
          ]
          setError(
            cause instanceof Error && knownErrors.includes(cause.message)
              ? cause.message
              : "附件加载失败，请稍后重试。"
          )
        }
      }
    })()
    return () => {
      controller.abort()
      if (objectUrl) URL.revokeObjectURL(objectUrl)
    }
  }, [open, allowed, previewable, tooLarge, href, isPdf, imageMime])

  return (
    <>
      <div
        data-lanqin-i18n-ignore
        className="flex flex-wrap items-center gap-3 rounded-md border p-3 text-sm"
      >
        <Paperclip className="h-4 w-4 shrink-0 text-muted-foreground" />
        <div className="min-w-0 flex-1">
          {allowed && previewable && !tooLarge ? (
            <Button
              type="button"
              variant="link"
              className="block h-auto max-w-full truncate p-0 text-left font-normal"
              title={filename}
              onClick={() => setOpen(true)}
            >
              {filename}
            </Button>
          ) : (
            <div className="truncate" title={filename}>
              {filename}
            </div>
          )}
          <div className="text-xs text-muted-foreground">
            {sizeLabel}
            {" · "}
            {t(
              !allowed
                ? "无附件访问权限"
                : tooLarge
                  ? "超过 25 MB，请下载查看"
                  : !previewable
                    ? "此格式暂不支持在线预览"
                    : "支持预览"
            )}
          </div>
        </div>
        {allowed && (
          <div className="flex shrink-0 gap-2">
            {previewable && !tooLarge && (
              <Button variant="outline" size="sm" onClick={() => setOpen(true)}>
                <Eye className="h-4 w-4" />
                {t("预览")}
              </Button>
            )}
            <Button variant="outline" size="sm" asChild>
              <a href={href} download={filename}>
                <Download className="h-4 w-4" />
                {t("下载")}
              </a>
            </Button>
          </div>
        )}
      </div>
      <Dialog open={open && allowed} onOpenChange={setOpen}>
        <DialogContent
          data-lanqin-i18n-ignore
          aria-describedby={undefined}
          closeLabel={t("关闭")}
          className="flex h-[85dvh] w-[95vw] max-w-5xl flex-col"
        >
          <DialogHeader className="min-w-0 pr-6">
            <DialogTitle className="break-all">{filename}</DialogTitle>
          </DialogHeader>
          <div className="min-h-0 flex-1 overflow-auto rounded-md border bg-muted/30">
            {error ? (
              <p role="alert" className="p-6 text-sm">
                {t(error)}
              </p>
            ) : !url ? (
              <p role="status" className="p-6 text-sm text-muted-foreground">
                {t("正在加载附件…")}
              </p>
            ) : isPdf ? (
              <iframe
                title={`${t("预览")} ${filename}`}
                src={url}
                className="h-full w-full border-0"
              />
            ) : (
              <img
                src={url}
                alt={filename}
                className="mx-auto max-h-full max-w-full object-contain"
                onError={() => setError("图片无法预览，请下载后查看。")}
              />
            )}
          </div>
          <div className="flex flex-wrap items-center justify-between gap-2">
            <p className="text-xs text-muted-foreground">
              {t("无法显示或浏览器不支持预览？可下载后查看。")}
            </p>
            <Button variant="outline" size="sm" asChild>
              <a href={href} download={filename}>
                {t("下载附件")}
              </a>
            </Button>
          </div>
        </DialogContent>
      </Dialog>
    </>
  )
}
