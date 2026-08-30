import type { MailLabel, MailRuleAction, MailRuleCondition } from "@/lib/api"

export type RuleCreatePayload = {
  mailboxId: string
  name: string
  matchMode: "all" | "any"
  conditions: MailRuleCondition[]
  actions: MailRuleAction[]
  applyToExisting: boolean
  stopProcessing: boolean
  enabled: boolean
}

export type RuleConditionField = NonNullable<MailRuleCondition["field"]>
export type RuleConditionOperator = NonNullable<MailRuleCondition["operator"]>

export const conditionFieldLabels: Record<RuleConditionField, string> = {
  all: "所有邮件",
  from: "发件人地址",
  to: "收件人地址",
  cc: "抄送地址",
  subject: "邮件主题",
  body: "邮件正文",
  attachment: "附件名称",
  size: "邮件大小",
  date: "收信日期",
}

export const conditionOperatorLabels: Record<RuleConditionOperator, string> = {
  contains: "包含",
  "not-contains": "不包含",
  equals: "等于",
  "not-equals": "不等于",
  "starts-with": "开头是",
  "ends-with": "结尾是",
  gt: "大于",
  gte: "大于等于",
  lt: "小于",
  lte: "小于等于",
  before: "早于",
  after: "晚于",
  on: "当天",
}

const textConditionOperators: RuleConditionOperator[] = ["contains", "not-contains", "equals", "not-equals", "starts-with", "ends-with"]
const sizeConditionOperators: RuleConditionOperator[] = ["gt", "gte", "lt", "lte", "equals", "not-equals"]
const dateConditionOperators: RuleConditionOperator[] = ["before", "after", "on", "equals", "not-equals"]

export const conditionFields = Object.keys(conditionFieldLabels) as RuleConditionField[]
export const commonRuleFolders = ["Inbox", "Archive", "Spam", "Trash"]
export const ruleActionLabels: Record<MailRuleAction["type"], string> = {
  archive: "移入归档",
  trash: "移入回收站",
  star: "添加星标",
  "mark-read": "标记已读",
  label: "添加标签",
  move: "移动到",
  forward: "自动转发到",
  telegram: "发送 Telegram 通知",
}

export function normalizeDraftAction(action: MailRuleAction, labels: MailLabel[]): MailRuleAction {
  if (action.type === "label") {
    const value = action.value || labels[0]?.name || ""
    return { type: "label", value, labelId: labels.find((label) => label.name === value)?.id || action.labelId || "" }
  }
  if (action.type === "move") return { type: "move", value: action.value || "Archive" }
  if (action.type === "forward") return { type: "forward", value: (action.value || "").trim() }
  return { type: action.type }
}

export function isForwardEmail(value?: string) {
  return /^[^\s@]+@[^\s@]+$/.test((value || "").trim())
}

export function conditionOperatorsForField(field?: MailRuleCondition["field"]) {
  if (field === "all") return ["equals"] as RuleConditionOperator[]
  if (field === "size") return sizeConditionOperators
  if (field === "date") return dateConditionOperators
  return textConditionOperators
}

export function defaultConditionOperator(field?: MailRuleCondition["field"]): RuleConditionOperator {
  if (field === "all") return "equals"
  if (field === "size") return "gte"
  if (field === "date") return "on"
  return "contains"
}

export function conditionPlaceholder(field?: MailRuleCondition["field"]) {
  if (field === "size") return "例如 10mb"
  if (field === "date") return "选择日期"
  if (field === "attachment") return "输入附件名或扩展名"
  return "输入值"
}

export function conditionSummary(conditions: MailRuleCondition[] = [], fromContains = "", subjectContains = "") {
  const legacyConditions = [
    fromContains ? { field: "from", operator: "contains", value: fromContains } as MailRuleCondition : undefined,
    subjectContains ? { field: "subject", operator: "contains", value: subjectContains } as MailRuleCondition : undefined,
  ].filter(Boolean) as MailRuleCondition[]
  const items = conditions.length > 0 ? conditions : legacyConditions
  return items.map(conditionItemSummary).join("；") || "无条件"
}

function conditionItemSummary(item: MailRuleCondition): string {
  if (item.conditions?.length) {
    const mode = item.matchMode === "any" ? "任一" : "全部"
    return `${mode}(${item.conditions.map(conditionItemSummary).join("；")})`
  }
  const field = item.field || "from"
  if (field === "all") return "所有邮件"
  const operator = item.operator || defaultConditionOperator(field)
  return `${conditionFieldLabels[field]} ${conditionOperatorLabels[operator]} ${item.value || ""}`
}

export function actionSummary(action: MailRuleAction) {
  if (action.type === "label") return `${ruleActionLabels[action.type]}${action.value ? `：${action.value}` : ""}`
  if (action.type === "move") return `${ruleActionLabels[action.type]}：${folderLabel(action.value || "Archive")}`
  if (action.type === "forward") return `${ruleActionLabels[action.type]}：${action.value || ""}`
  return ruleActionLabels[action.type]
}

function folderLabel(folder: string) {
  return ({ Inbox: "收件箱", Sent: "已发送", Drafts: "草稿箱", Archive: "归档", Spam: "垃圾邮件", Trash: "回收站" } as Record<string, string>)[folder] || folder
}
