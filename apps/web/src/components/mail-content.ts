import DOMPurify from "dompurify"
import { translateUiText, type Language } from "@/lib/language"

export type ComposerValue = { text: string; html: string }

export function withPrefix(subject: string, prefix: "Re:" | "Fwd:") {
  const pattern =
    prefix === "Re:"
      ? /^(?:(?:re|回复|答复|回覆)\s*[:：]\s*)+/i
      : /^(?:(?:fwd?|转发|轉寄|轉發)\s*[:：]\s*)+/i
  return `${prefix} ${subject.trim().replace(pattern, "")}`
}

export function quotedComposerValue(headers: string, body: string): ComposerValue {
  const quote = body
    .replace(/\r\n/g, "\n")
    .split("\n")
    .map((line) => `> ${line}`)
    .join("\n")
  return {
    text: `\n\n${headers}\n\n${quote}`,
    html: `<p><br></p><blockquote>${plainTextToHtml(headers)}${plainTextToHtml(body)}</blockquote>`,
  }
}

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

export function buildMailFrameSrcDoc(
  bodyHtml: string,
  bodyText: string,
  language: Language = "zh-CN"
) {
  const rawBody = bodyHtml.trim() ? bodyHtml : `<pre>${escapeHtml(bodyText || "")}</pre>`
  const sanitized = foldMailQuotes(
    DOMPurify.sanitize(rawBody, {
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
    }),
    language
  )
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

// Only fold our own recognizable reply header, not arbitrary blockquotes (which
// may be the actual message). Legacy replies used plain paragraphs with `>`.
function foldMailQuotes(html: string, language: Language) {
  const wholeDocument = /<html[\s>]|<body[\s>]/i.test(html)
  const doc = new DOMParser().parseFromString(html, "text/html")
  if (!wholeDocument) doc.body.innerHTML = html
  const header =
    /^----- 原始邮件 -----\nFrom: [^\n]*\nTo: [^\n]*\nDate: [^\n]*\nSubject: [^\n]*(?:\n|$)/
  const fold = (quote: Element) => {
    const details = doc.createElement("details")
    details.className = "mail-quoted-history"
    const summary = doc.createElement("summary")
    summary.textContent = translateUiText("显示 / 隐藏原邮件", language)
    quote.replaceWith(details)
    details.append(summary, quote)
  }
  for (const quote of Array.from(doc.body.querySelectorAll("blockquote"))) {
    if (!quote.parentElement?.closest("blockquote") && header.test(stripHtml(quote.innerHTML))) {
      fold(quote)
    }
  }
  for (const element of Array.from(doc.body.children)) {
    if (!["P", "PRE"].includes(element.tagName)) continue
    const text = stripHtml(element.innerHTML)
    const marker = text.indexOf("----- 原始邮件 -----")
    if (marker < 0 || (marker > 0 && text[marker - 1] !== "\n")) continue
    const tail = text.slice(marker)
    if (!header.test(tail)) continue
    // Only recognize the old plain-text format, never discard rich HTML.
    const siblings: Element[] = []
    let next = element.nextElementSibling
    while (
      next &&
      next.tagName === "P" &&
      /^(?:>[^\n]*(?:\n|$))+$/.test(stripHtml(next.innerHTML))
    ) {
      siblings.push(next)
      next = next.nextElementSibling
    }
    if ([element, ...siblings].some((node) => node.querySelector(":not(br)"))) continue
    const history = [tail, ...siblings.map((node) => stripHtml(node.innerHTML))].join("\n\n")
    const quote = doc.createElement("blockquote")
    quote.innerHTML = plainTextToHtml(history.replace(/^> ?/gm, ""))
    if (marker > 0) {
      element.innerHTML = plainTextToHtmlFragment(text.slice(0, marker))
      element.after(quote)
    } else {
      element.replaceWith(quote)
    }
    siblings.forEach((node) => node.remove())
    fold(quote)
  }
  return wholeDocument ? doc.documentElement.outerHTML : doc.body.innerHTML
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
  blockquote { margin: 16px 0; padding-left: 16px; border-left: 3px solid #d1d5db; color: #6b7280; }
  .mail-quoted-history { margin-top: 20px; }
  .mail-quoted-history > summary { cursor: pointer; color: #6b7280; font-size: 13px; }
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
  div.querySelectorAll("br").forEach((node) => node.replaceWith("\n"))
  div
    .querySelectorAll("p, div, blockquote, pre, li, tr, h1, h2, h3, h4, h5, h6")
    .forEach((node) => {
      node.append("\n")
    })
  return (div.textContent || "").trim()
}
