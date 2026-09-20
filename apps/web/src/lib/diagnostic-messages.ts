import { uiText } from "./language"

/** Translate known diagnostic shapes; domain names remain literal parameters. */
export function dnsCheckMessage(message: string, ok: boolean): string {
  const exact = [
    "未找到 MX 记录",
    "MX 指向正确",
    "MX 未指向当前邮件主机",
    "DKIM 公钥匹配",
    "DKIM 记录存在，但公钥与当前域名配置不匹配",
    "未找到 DKIM 记录",
    "DMARC 记录存在",
    "未找到 DMARC 记录",
  ]
  if (exact.includes(message)) return uiText(message)
  const match = message.match(
    /^(未找到)?(域名 SPF|HELO 主机 (.+) 的 SPF)(记录|存在多条记录，会导致 SPF 校验错误|记录有效)$/
  )
  if (match) {
    const label = match[3] ? uiText("HELO 主机 {0} 的 SPF", [match[3]]) : uiText("域名 SPF")
    if (match[1]) return uiText("未找到 {0} 记录", [label])
    if (match[4] === "记录有效") return uiText("{0} 记录有效", [label])
    return uiText("{0} 存在多条记录，会导致 SPF 校验错误", [label])
  }
  return uiText(ok ? "DNS 检测通过" : "DNS 检测未通过")
}

export function dnsStatusLabel(status: string): string {
  if (status === "ok") return uiText("DNS 正常")
  if (status === "error") return uiText("DNS 检测未通过")
  return uiText("未检测")
}
