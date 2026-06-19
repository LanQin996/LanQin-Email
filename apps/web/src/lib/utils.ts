import { type ClassValue, clsx } from "clsx"
import { twMerge } from "tailwind-merge"

export function cn(...inputs: ClassValue[]) {
  return twMerge(clsx(inputs))
}

export function formatDate(value: string) {
  const date = new Date(value)
  if (Number.isNaN(date.getTime())) return ""
  return new Intl.DateTimeFormat("zh-CN", { month: "2-digit", day: "2-digit", hour: "2-digit", minute: "2-digit" }).format(date)
}

export function formatDateTime(value: string) {
  const date = new Date(value)
  if (Number.isNaN(date.getTime())) return ""
  return new Intl.DateTimeFormat("zh-CN", { year: "numeric", month: "2-digit", day: "2-digit", hour: "2-digit", minute: "2-digit" }).format(date)
}

export function formatBytes(bytes: number) {
  if (!bytes) return "0 B"
  const units = ["B", "KB", "MB", "GB"]
  let size = bytes
  let idx = 0
  while (size >= 1024 && idx < units.length - 1) { size /= 1024; idx++ }
  return `${size.toFixed(idx === 0 ? 0 : 1)} ${units[idx]}`
}

/**
 * Normalize charset aliases to canonical names supported by the browser's TextDecoder.
 * Only contains aliases that differ from their canonical name — standard names like
 * "gb18030", "big5", "euc-kr" are passed through directly to TextDecoder.
 */
function normalizeCharset(charset: string): string {
  const c = charset.toLowerCase().trim()
  const aliases: Record<string, string> = {
    "gb2312": "gbk",
    "x-gbk": "gbk",
    "euc-cn": "gbk",
    "hz-gb-2312": "gbk",
    "shift-jis": "shift_jis",
    "sjis": "shift_jis",
    "windows-31j": "shift_jis",
    "ks_c_5601-1987": "euc-kr",
    "ksc5601": "euc-kr",
    "windows-949": "euc-kr",
    "iso-8859-1": "windows-1252",
  }
  const mapped = aliases[c]
  if (mapped) return mapped
  // cpXXX / cpXXX windows code pages: cp936→windows-936, cp943→windows-943, etc.
  if (/^cp\d+$/.test(c)) return "windows-" + c.slice(2)
  return c
}

/**
 * Decode RFC 2047 encoded words in mail headers (e.g. =?UTF-8?B?5byA5ZSu?=).
 * Handles Base64 (B) and Quoted-Printable (Q) encoding.
 * Supports non-UTF-8 charsets (e.g. GBK, GB2312, Shift_JIS) via charset alias normalization.
 * Per RFC 2047 §6.2, linear whitespace between adjacent encoded words is stripped.
 * Returns the original string unchanged if no encoded words are found or on error.
 */
export function decodeMimeHeader(value: string): string {
  if (!value || !value.includes("=?")) return value
  // RFC 2047 §6.2: ignore whitespace between adjacent encoded words.
  const collapsed = value.replace(/(\?=)\s+(=\?)/g, "$1$2")
  return collapsed.replace(/=\?([^?]+)\?([bBqQ])\?([^?]*)\?=/g, (_match, charset, encoding, encoded) => {
    try {
      const lowerEncoding = String(encoding).toLowerCase()
      let decoded: string
      if (lowerEncoding === "b") {
        const sanitized = encoded.replace(/\s+/g, "")
        const padded = sanitized + "=".repeat((4 - (sanitized.length % 4)) % 4)
        decoded = atob(padded)
      } else {
        decoded = encoded.replace(/_/g, " ").replace(/=([0-9a-fA-F]{2})/g, (_m: string, hex: string) => String.fromCharCode(parseInt(hex, 16)))
      }
      const bytes = new Uint8Array(Array.from(decoded, (ch) => ch.charCodeAt(0)))
      const normalized = normalizeCharset(charset) || "utf-8"
      const decoder = new TextDecoder(normalized)
      return decoder.decode(bytes)
    } catch {
      return _match
    }
  })
}
