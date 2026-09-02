import DOMPurify from "dompurify"

export type ComposerValue = { text: string; html: string }

export function plainTextComposerValue(value: string): ComposerValue {
  return { text: value, html: plainTextToHtml(value) }
}

export function htmlComposerValue(value: string): ComposerValue {
  const html = sanitizeComposerHtml(value || "")
  const text = stripHtml(html)
  return { text, html: html || plainTextToHtml(text) }
}

export function plainTextToHtml(value: string) {
  const normalized = value.replace(/\r\n/g, "\n")
  if (!normalized.trim()) return ""
  return sanitizeComposerHtml(
    normalized
      .split(/\n{2,}/)
      .map((paragraph) => `<p>${plainTextToHtmlFragment(paragraph) || "<br>"}</p>`)
      .join("")
  )
}

export function plainTextToHtmlFragment(value: string) {
  return value
    .split("\n")
    .map((line) => escapeHtml(line))
    .join("<br>")
}

export function escapeHtml(value: string) {
  return value
    .replace(/&/g, "&amp;")
    .replace(/</g, "&lt;")
    .replace(/>/g, "&gt;")
    .replace(/"/g, "&quot;")
    .replace(/'/g, "&#39;")
}

export function buildMailFrameSrcDoc(bodyHtml: string, bodyText: string) {
  const rawBody = bodyHtml.trim() ? bodyHtml : `<pre>${escapeHtml(bodyText || "")}</pre>`
  const sanitized = DOMPurify.sanitize(rawBody, {
    ADD_ATTR: [
      "style",
      "type",
      "align",
      "valign",
      "bgcolor",
      "border",
      "cellpadding",
      "cellspacing",
      "width",
      "height",
    ],
    ADD_TAGS: ["html", "head", "body", "style", "center", "font"],
    WHOLE_DOCUMENT: /<html[\s>]/i.test(rawBody) || /<body[\s>]/i.test(rawBody),
  })
  if (/<html[\s>]/i.test(sanitized) || /<body[\s>]/i.test(sanitized)) {
    const hasHead = /<head[\s>]/i.test(sanitized)
    const withBase = hasHead
      ? sanitized.replace(
          /<head([^>]*)>/i,
          `<head$1><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1"><base target="_blank">${mailFrameBaseStyle()}`
        )
      : sanitized.replace(
          /<html([^>]*)>/i,
          `<html$1><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1"><base target="_blank">${mailFrameBaseStyle()}</head>`
        )
    return /<!doctype/i.test(withBase) ? withBase : `<!doctype html>${withBase}`
  }
  return `<!doctype html>
<html>
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<base target="_blank">
${mailFrameBaseStyle()}
</head>
<body>${sanitized}</body>
</html>`
}

function mailFrameBaseStyle() {
  return `<style>
  html, body { margin: 0; padding: 0; background: #fff; color: #111827; }
  body {
    box-sizing: border-box;
    overflow-wrap: anywhere;
    -webkit-text-size-adjust: 100%;
    font-family: Arial, "Helvetica Neue", Helvetica, sans-serif;
    font-size: 14px;
    line-height: 1.5;
  }
  *, *::before, *::after { box-sizing: border-box; }
  img { max-width: 100%; height: auto; }
  table { max-width: 100%; }
  pre { white-space: pre-wrap; word-break: break-word; font-family: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace; }
  a { color: #2563eb; }
</style>`
}

export function sanitizeComposerHtml(value: string) {
  return DOMPurify.sanitize(value || "")
}

export function htmlContainsMeaningfulContent(html: string) {
  return (
    /<(img|hr|table|ul|ol|li|blockquote|pre|div)[\s>]/i.test(html) ||
    stripHtml(html).trim().length > 0
  )
}

function stripHtml(html: string) {
  const div = document.createElement("div")
  div.innerHTML = DOMPurify.sanitize(html)
  return div.textContent || div.innerText || ""
}
