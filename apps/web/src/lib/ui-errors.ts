import { exactTranslations } from "@/lib/language"

const knownErrors: Record<string, string> = {
  backup_encryption_invalid: "备份加密设置无效，请检查密码与加密方式",
  backup_encryption_conflict: "备份加密设置已变化，请关闭窗口重新打开后重试",
  backup_encryption_unavailable: "无法读取或保存备份加密设置，请检查服务器加密配置",
  backup_encryption_current_password_invalid: "当前备份密码错误",
  backup_maintenance_unavailable: "主机覆盖恢复暂不可用，请联系管理员检查维护配置",
  backup_maintenance_conflict: "覆盖恢复计划或请求已变化，请刷新计划后重试",
  backup_maintenance_not_found: "尚未找到此覆盖恢复请求，请保留请求编号并稍后查询",
  backup_resume_unavailable: "此任务无法继续，请检查归档、地点配置和任务状态",
  backup_restore_busy: "其他备份或恢复任务正在执行，请稍后重试",
  backup_restore_configuration_unavailable: "私有恢复工作区不可用或配置已变化",
  backup_restore_source_unavailable: "此备份副本不可恢复，地点配置、收据或保留状态已变化",
  backup_restore_reauthentication_failed: "管理员密码或两步验证码错误",
  backup_drive_authorization_failed: "Google Drive 授权未完成，请重新授权",
  backup_drive_client_invalid: "Google Drive 客户端配置无效，请检查客户端 ID 和密钥",
  backup_drive_client_conflict: "Google Drive 客户端配置已变化，请关闭窗口重新打开后重试",
  backup_drive_client_unavailable: "无法读取或保存 Google Drive 客户端配置，请检查服务器加密设置",
  reauthorization_required: "Google Drive 授权已失效，请重新授权",
  backup_key_unavailable: "备份加密密钥缺失或不匹配，请检查服务器配置",
  backup_destination_invalid: "备份地点配置无效，请检查名称、HTTPS 地址和存储参数",
  backup_credentials_required: "请完整填写凭据；更换地址或密钥对时需重新填写",
  backup_revision_conflict: "备份配置已变化，请关闭弹窗并刷新后重试",
  backup_destination_limit: "备份地点数量或配置总大小已达到上限",
  backup_destination_not_found: "备份地点已不存在，请刷新后重试",
  backup_store_unavailable: "无法读取或保存备份配置，请稍后重试",
  backup_execution_unavailable: "服务端尚未配置可验证的加密归档目录",
  backup_queue_full: "上传队列已满，请稍后重试",
  "resource is a unified collection target; change or disable collection first":
    "请先更换或关闭引用此目标的统一收件设置",
  "collection target must be an active local mailbox without a forwarding alias":
    "请选择启用的站内邮箱，目标不能配置别名转发",
  "collection target cannot be a forwarding alias": "统一收件目标不能设置别名转发",
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
