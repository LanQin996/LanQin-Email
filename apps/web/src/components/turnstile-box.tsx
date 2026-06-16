import * as React from "react"

declare global {
  interface Window {
    turnstile?: {
      render: (container: HTMLElement, options: { sitekey: string; callback: (token: string) => void; "expired-callback": () => void; "error-callback": () => void }) => string
      remove: (widgetId: string) => void
    }
  }
}

export function TurnstileBox({ siteKey, onToken }: { siteKey: string; onToken: (token: string) => void }) {
  const ref = React.useRef<HTMLDivElement | null>(null)
  React.useEffect(() => {
    if (!siteKey || !ref.current) return
    let cancelled = false
    let widgetId = ""
    function render() {
      if (cancelled || !ref.current || !window.turnstile) return
      ref.current.innerHTML = ""
      widgetId = window.turnstile.render(ref.current, {
        sitekey: siteKey,
        callback: onToken,
        "expired-callback": () => onToken(""),
        "error-callback": () => onToken(""),
      })
    }
    if (window.turnstile) {
      render()
    } else {
      const existing = document.querySelector('script[src="https://challenges.cloudflare.com/turnstile/v0/api.js?render=explicit"]')
      if (existing) {
        existing.addEventListener("load", render, { once: true })
      } else {
        const script = document.createElement("script")
        script.src = "https://challenges.cloudflare.com/turnstile/v0/api.js?render=explicit"
        script.async = true
        script.defer = true
        script.addEventListener("load", render, { once: true })
        document.head.appendChild(script)
      }
    }
    return () => {
      cancelled = true
      onToken("")
      if (widgetId && window.turnstile) window.turnstile.remove(widgetId)
    }
  }, [siteKey, onToken])
  return <div className="flex justify-center rounded-md border p-2"><div ref={ref} /></div>
}
