import * as React from "react"
import { buildMailFrameSrcDoc } from "@/components/mail-content"

/**
 * Renders untrusted mail HTML inside a sandboxed iframe.
 *
 * Both the webmail reader and the admin message viewer must use this. Sanitising
 * with DOMPurify alone is not enough: its default allow-list keeps `style`
 * attributes and `<style>` blocks, so a message could position an invisible overlay
 * over the surrounding application chrome. Rendering into an iframe without
 * `allow-scripts` confines both scripts and CSS to the frame.
 */
export function MailHtmlFrame({ bodyHtml, bodyText, className, minHeight = 180 }: { bodyHtml?: string; bodyText?: string; className?: string; minHeight?: number }) {
  const iframeRef = React.useRef<HTMLIFrameElement>(null)
  const [height, setHeight] = React.useState(260)
  const srcDoc = React.useMemo(() => buildMailFrameSrcDoc(bodyHtml || "", bodyText || ""), [bodyHtml, bodyText])

  const resize = React.useCallback(() => {
    const doc = iframeRef.current?.contentDocument
    if (!doc) return
    const body = doc.body
    const html = doc.documentElement
    setHeight(Math.max(minHeight, Math.ceil(Math.max(body?.scrollHeight || 0, body?.offsetHeight || 0, html?.scrollHeight || 0, html?.offsetHeight || 0))))
  }, [minHeight])

  React.useEffect(() => {
    setHeight(260)
    const frame = iframeRef.current
    if (!frame) return
    let observer: ResizeObserver | undefined
    const timers = [window.setTimeout(resize, 0), window.setTimeout(resize, 120), window.setTimeout(resize, 600)]
    const attach = () => {
      const doc = frame.contentDocument
      if (!doc) return
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
      doc.querySelectorAll("img").forEach((img) => img.addEventListener("load", resize, { once: true }))
    }
    frame.addEventListener("load", attach)
    return () => {
      frame.removeEventListener("load", attach)
      observer?.disconnect()
      timers.forEach((timer) => window.clearTimeout(timer))
    }
  }, [resize, srcDoc])

  return (
    <iframe
      ref={iframeRef}
      title="邮件正文"
      className={className || "block w-full border-0 bg-white"}
      sandbox="allow-same-origin allow-popups allow-popups-to-escape-sandbox"
      referrerPolicy="no-referrer"
      srcDoc={srcDoc}
      style={{ height }}
    />
  )
}
