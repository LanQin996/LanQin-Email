import DOMPurify from "dompurify"
import { formatDate, formatDateTime } from "@/lib/utils"
import { escapeHtml } from "@/components/mail-content"

export type ScheduleDraft = {
  title: string
  start: string
  durationMinutes: number
  reminderMinutes: number
  repeat: "none" | "daily" | "weekly" | "monthly" | "yearly"
  allDay: boolean
  customDuration: boolean
  customReminder: boolean
  lunar: boolean
  location: string
  description: string
}

export function defaultScheduledSendValue() {
  const date = new Date(Date.now() + 30 * 60 * 1000)
  const minute = date.getMinutes()
  date.setMinutes(minute + (5 - (minute % 5 || 5)))
  return toDateTimeLocalValue(date)
}

export function scheduledSendPresets() {
  return [
    { label: "30 分钟后", value: toDateTimeLocalValue(new Date(Date.now() + 30 * 60 * 1000)) },
    { label: "2 小时后", value: toDateTimeLocalValue(new Date(Date.now() + 2 * 60 * 60 * 1000)) },
    { label: "明早 9 点", value: toDateTimeLocalValue(nextMorningAtNine()) },
    { label: "下周一 9 点", value: toDateTimeLocalValue(nextMondayAtNine()) },
  ]
}

function nextMorningAtNine() {
  const date = new Date()
  date.setDate(date.getDate() + 1)
  date.setHours(9, 0, 0, 0)
  return date
}

function nextMondayAtNine() {
  const date = new Date()
  const day = date.getDay()
  const daysUntilMonday = (8 - day) % 7 || 7
  date.setDate(date.getDate() + daysUntilMonday)
  date.setHours(9, 0, 0, 0)
  return date
}

export function defaultScheduleStartValue() {
  const date = new Date()
  date.setMinutes(date.getMinutes() + (60 - (date.getMinutes() % 60 || 60)))
  return toDateTimeLocalValue(date)
}

export function toDateTimeLocalValue(date: Date) {
  const pad = (value: number) => String(value).padStart(2, "0")
  return `${date.getFullYear()}-${pad(date.getMonth() + 1)}-${pad(date.getDate())}T${pad(date.getHours())}:${pad(date.getMinutes())}`
}

export function normalizeSchedule(schedule: ScheduleDraft): ScheduleDraft {
  return { ...schedule, title: schedule.title.trim(), location: schedule.location.trim(), description: schedule.description.trim() }
}

export function scheduleToHtml(schedule: ScheduleDraft) {
  const start = parseScheduleStart(schedule)
  const end = schedule.allDay ? new Date(start.getTime() + 24 * 60 * 60 * 1000) : new Date(start.getTime() + schedule.durationMinutes * 60 * 1000)
  const rows = [
    ["时间", schedule.allDay ? formatDate(start.toISOString()) : `${formatDateTime(start.toISOString())} - ${formatTimeOnly(end)}`],
    ["持续", schedule.allDay ? "全天" : durationLabel(schedule.durationMinutes)],
    ["提醒", reminderLabel(schedule.reminderMinutes)],
    ["重复", repeatLabel(schedule.repeat)],
    schedule.location ? ["位置", schedule.location] : undefined,
    schedule.description ? ["描述", schedule.description] : undefined,
  ].filter(Boolean) as string[][]
  return DOMPurify.sanitize(`
    <div style="border:1px solid #d4d4d8;border-radius:8px;padding:14px 16px;margin:16px 0;background:#fafafa;">
      <div style="font-weight:600;font-size:16px;margin-bottom:10px;">${escapeHtml(schedule.title)}</div>
      ${rows.map(([label, value]) => `<div style="margin:6px 0;"><span style="color:#71717a;">${label}：</span>${escapeHtml(value)}</div>`).join("")}
    </div>
  `)
}

export function scheduleToFile(schedule: ScheduleDraft) {
  const ics = scheduleToIcs(schedule)
  const filename = `${safeFilename(schedule.title || "schedule")}.ics`
  return new File([ics], filename, { type: "text/calendar;charset=utf-8" })
}

function scheduleToIcs(schedule: ScheduleDraft) {
  const start = parseScheduleStart(schedule)
  const end = schedule.allDay ? new Date(start.getTime() + 24 * 60 * 60 * 1000) : new Date(start.getTime() + schedule.durationMinutes * 60 * 1000)
  const uid = `${Date.now()}-${Math.random().toString(36).slice(2)}@lanqin-email`
  const lines = [
    "BEGIN:VCALENDAR",
    "VERSION:2.0",
    "PRODID:-//LanQin Email//Webmail//CN",
    "CALSCALE:GREGORIAN",
    "METHOD:PUBLISH",
    "BEGIN:VEVENT",
    `UID:${uid}`,
    `DTSTAMP:${toIcsDateTime(new Date())}`,
    schedule.allDay ? `DTSTART;VALUE=DATE:${toIcsDate(start)}` : `DTSTART:${toIcsDateTime(start)}`,
    schedule.allDay ? `DTEND;VALUE=DATE:${toIcsDate(end)}` : `DTEND:${toIcsDateTime(end)}`,
    `SUMMARY:${escapeIcs(schedule.title)}`,
    schedule.location ? `LOCATION:${escapeIcs(schedule.location)}` : "",
    schedule.description ? `DESCRIPTION:${escapeIcs(schedule.description)}` : "",
    schedule.repeat !== "none" ? `RRULE:FREQ=${schedule.repeat.toUpperCase()}` : "",
  ].filter(Boolean)
  if (schedule.reminderMinutes > 0) {
    lines.push("BEGIN:VALARM", `TRIGGER:-PT${schedule.reminderMinutes}M`, "ACTION:DISPLAY", `DESCRIPTION:${escapeIcs(schedule.title)}`, "END:VALARM")
  }
  lines.push("END:VEVENT", "END:VCALENDAR")
  return `${lines.join("\r\n")}\r\n`
}

export function parseScheduleStart(schedule: ScheduleDraft) {
  const value = schedule.allDay ? `${schedule.start.slice(0, 10)}T00:00` : schedule.start
  const date = new Date(value)
  return Number.isNaN(date.getTime()) ? new Date() : date
}

function toIcsDateTime(date: Date) {
  const pad = (value: number) => String(value).padStart(2, "0")
  return `${date.getUTCFullYear()}${pad(date.getUTCMonth() + 1)}${pad(date.getUTCDate())}T${pad(date.getUTCHours())}${pad(date.getUTCMinutes())}${pad(date.getUTCSeconds())}Z`
}

function toIcsDate(date: Date) {
  const pad = (value: number) => String(value).padStart(2, "0")
  return `${date.getFullYear()}${pad(date.getMonth() + 1)}${pad(date.getDate())}`
}

export function formatTimeOnly(date: Date) {
  return date.toLocaleTimeString("zh-CN", { hour: "2-digit", minute: "2-digit" })
}

export function durationLabel(minutes: number) {
  if (minutes % 1440 === 0) return `${minutes / 1440}天`
  if (minutes % 60 === 0) return `${minutes / 60}小时`
  return `${minutes}分钟`
}

export function reminderLabel(minutes: number) {
  if (minutes <= 0) return "准时"
  return `${durationLabel(minutes)}前`
}

export function repeatLabel(repeat: ScheduleDraft["repeat"]) {
  return ({ none: "永不", daily: "每天", weekly: "每周", monthly: "每月", yearly: "每年" } as Record<ScheduleDraft["repeat"], string>)[repeat]
}

function safeFilename(value: string) {
  return value.trim().replace(/[\\/:*?"<>|]+/g, "-").replace(/\s+/g, "-").slice(0, 64) || "schedule"
}

function escapeIcs(value: string) {
  return value.replace(/\\/g, "\\\\").replace(/\n/g, "\\n").replace(/,/g, "\\,").replace(/;/g, "\\;")
}
