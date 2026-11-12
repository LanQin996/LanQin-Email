import type { BackupMaintenanceStatus } from "./api-types"

// Stable opaque ID shared with the host handoff protocol; it is not authorization.
export async function backupMaintenanceRequestId(actorId: string, nonce: string) {
  const digest = await crypto.subtle.digest(
    "SHA-256",
    new TextEncoder().encode(`${actorId}:${nonce}`)
  )
  return Array.from(new Uint8Array(digest).slice(0, 16), (byte) =>
    byte.toString(16).padStart(2, "0")
  ).join("")
}

export function backupMaintenanceFinished(status?: BackupMaintenanceStatus) {
  return !!status && ["succeeded", "failed", "needsRecovery"].includes(status.state)
}

export const backupMaintenanceMilestones = [
  "准备恢复文件",
  "准备恢复数据",
  "执行恢复预检查",
  "停止邮件服务",
  "切换并校验数据",
  "启动并检查服务",
  "恢复完成",
] as const

const maintenanceSteps: Record<string, { label: string; milestone: number }> = {
  queued: { label: "等待执行覆盖恢复", milestone: -1 },
  "checking-source": { label: "检查已验证的备份", milestone: 0 },
  copying: { label: "复制恢复文件到维护工作区", milestone: 0 },
  "checking-copy": { label: "校验维护工作区文件", milestone: 0 },
  importing: { label: "导入恢复数据", milestone: 1 },
  preflight: { label: "执行恢复预检查", milestone: 2 },
  stopping: { label: "停止邮件服务", milestone: 3 },
  installing: { label: "替换数据库及邮件文件", milestone: 4 },
  validating: { label: "校验切换后的数据", milestone: 4 },
  committing: { label: "确认恢复数据切换", milestone: 4 },
  starting: { label: "启动邮件服务", milestone: 5 },
  "health-check": { label: "检查邮件服务健康状态", milestone: 5 },
  "rolling-back": { label: "回退尚未提交的数据切换", milestone: -1 },
  "recovering-original": { label: "恢复原有邮件服务", milestone: -1 },
}

export function backupMaintenanceStep(status?: BackupMaintenanceStatus) {
  if (status?.state === "succeeded") return { label: "恢复完成", milestone: 6 }
  const step = status?.progress?.step
  if (step && Object.prototype.hasOwnProperty.call(maintenanceSteps, step)) {
    return maintenanceSteps[step]
  }
  switch (status?.phase) {
    case "queued":
      return maintenanceSteps.queued
    case "preparing":
      return { label: "正在准备恢复文件", milestone: -1 }
    case "importing":
      return { label: "导入恢复数据", milestone: -1 }
    case "maintenance":
      return { label: "正在执行维护恢复", milestone: -1 }
    default:
      return { label: "等待状态更新", milestone: -1 }
  }
}

export function backupMaintenanceElapsed(start: string | undefined, end: number) {
  const began = start ? Date.parse(start) : NaN
  if (!Number.isFinite(began) || !Number.isFinite(end)) return undefined
  const seconds = Math.max(0, Math.floor((end - began) / 1000))
  const hours = Math.floor(seconds / 3600)
  const minutes = Math.floor((seconds % 3600) / 60)
  return `${String(hours).padStart(2, "0")}:${String(minutes).padStart(2, "0")}:${String(seconds % 60).padStart(2, "0")}`
}
