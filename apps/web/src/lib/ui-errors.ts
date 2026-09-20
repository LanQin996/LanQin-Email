import { exactTranslations } from "@/lib/language"

const knownErrors: Record<string, string> = {
  unauthorized: "登录会话已失效，请重新登录",
  forbidden: "无权执行此操作",
  "permission required": "无权执行此操作",
  "external imap is disabled": "外部 IMAP 未启用",
  "invalid oauth state": "授权状态无效或已过期，请重试",
  "missing oauth code": "授权失败",
  "oauth exchange failed": "授权失败",
  "oauth account email does not match requested external email": "授权邮箱与填写的邮箱不一致",
  "delivery queue item is no longer eligible": "队列任务状态已变化，请刷新后重试",
  "failed to list folders": "无法读取文件夹，请稍后重试",
  "failed to load remote messages": "无法读取远端邮件，请稍后重试",
  "failed to load remote message": "无法读取远端邮件，请稍后重试",
  "failed to update remote message": "无法更新远端邮件，请稍后重试",
}

/** Keep a safe source message in query state; translate only when rendering it. */
export function errorMessage(error: unknown, status?: number): string {
  const message = error instanceof Error ? error.message : typeof error === "string" ? error : ""
  if (Object.prototype.hasOwnProperty.call(knownErrors, message)) return knownErrors[message]
  if (Object.prototype.hasOwnProperty.call(exactTranslations, message)) return message
  if (status === 401) return "登录会话已失效，请重新登录"
  if (status === 403) return "无权执行此操作"
  if (status === 404) return "请求的内容不存在或无权访问"
  if (status === 409) return "数据已变化，请刷新后重试"
  if (status === 429) return "请求过于频繁，请稍后重试"
  if (status === 400 || status === 422) return "请检查输入内容后重试"
  return "操作未完成，请稍后重试"
}

export class ApiError extends Error {
  constructor(
    readonly status: number,
    message: unknown
  ) {
    super(errorMessage(message, status))
    this.name = "ApiError"
  }
}
