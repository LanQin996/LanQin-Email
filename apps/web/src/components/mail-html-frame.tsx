import * as React from "react"
import { buildMailFrameSrcDoc } from "@/components/mail-content"
import { translateUiText, useLanguage } from "@/lib/language"

/**
 * Renders untrusted mail HTML inside a sandboxed iframe.
 *
 * Both the webmail reader and the admin message viewer must use this. Sanitising
 * with DOMPurify alone is not enough: its default allow-list keeps `style`
 * attributes and `<style>` blocks, so a message could position an invisible overlay
 * over the surrounding application chrome. Rendering into an iframe without
 * `allow-scripts` confines both scripts and CSS to the frame.
 */
export function MailHtmlFrame({
  bodyHtml,
  bodyText,
  className,
  minHeight = 180,
}: {
  bodyHtml?: string
  bodyText?: string
  className?: string
  minHeight?: number
}) {
  const [language] = useLanguage()
  const iframeRef = React.useRef<HTMLIFrameElement>(null)
  const [height, setHeight] = React.useState(260)
  const srcDoc = React.useMemo(
    () => buildMailFrameSrcDoc(bodyHtml || "", bodyText || "", language),
    [bodyHtml, bodyText, language]
  )

  const resize = React.useCallback(() => {
    const frame = iframeRef.current
    const doc = frame?.contentDocument
    if (!frame || !doc) return
    const body = doc.body
    const html = doc.documentElement
    // Measure without the previous viewport height, so collapsing a quote can
    // shrink the frame again instead of leaving a large empty area.
    frame.style.height = "0px"
    const nextHeight = Math.max(
      minHeight,
      Math.ceil(
        Math.max(
          body?.scrollHeight || 0,
          body?.offsetHeight || 0,
          html?.scrollHeight || 0,
          html?.offsetHeight || 0
        )
      )
    )
    frame.style.height = `${nextHeight}px`
    setHeight(nextHeight)
  }, [minHeight])

  React.useEffect(() => {
    setHeight(260)
    const frame = iframeRef.current
    if (!frame) return
    let observer: ResizeObserver | undefined
    let observedDocument: Document | null = null
    const timers = [
      window.setTimeout(resize, 0),
      window.setTimeout(resize, 120),
      window.setTimeout(resize, 600),
    ]
    const attach = () => {
      const doc = frame.contentDocument
      if (!doc) return
      observedDocument?.removeEventListener("toggle", resize, true)
      observedDocument = doc
      doc.addEventListener("toggle", resize, true)
      observer?.disconnect()
      // Links open outside the frame; without noreferrer the target would learn the
      // mailbox URL.
      doc.querySelectorAll("a[href]").forEach((link) => {
        link.setAttribute("target", "_blank")
        link.setAttribute("rel", "noopener noreferrer")
      })
      resize()
      if ("ResizeObserver" in window) {
        observer = new ResizeObserver(resize)
        observer.observe(doc.documentElement)
        if (doc.body) observer.observe(doc.body)
      }
      doc
        .querySelectorAll("img")
        .forEach((img) => img.addEventListener("load", resize, { once: true }))
    }
    frame.addEventListener("load", attach)
    return () => {
      frame.removeEventListener("load", attach)
      observer?.disconnect()
      observedDocument?.removeEventListener("toggle", resize, true)
      timers.forEach((timer) => window.clearTimeout(timer))
    }
  }, [resize, srcDoc])

  return (
    <iframe
      ref={iframeRef}
      data-lanqin-i18n-ignore
      title={translateUiText("邮件正文", language)}
      className={className || "block w-full border-0 bg-white"}
      sandbox="allow-same-origin allow-popups allow-popups-to-escape-sandbox"
      referrerPolicy="no-referrer"
      srcDoc={srcDoc}
      style={{ height }}
    />
  )
}
