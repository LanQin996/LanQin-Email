import { createContext } from "react"
import { ClassicOverview, ClassicShell } from "./classic"
import type { AdminLayoutTemplate } from "./contract"
import { WorkbenchOverview, WorkbenchShell } from "./workbench"

export const AdminLayoutContext = createContext("classic")

export const adminLayouts = {
  classic: { version: 1, Shell: ClassicShell, Overview: ClassicOverview },
  workbench: { version: 1, Shell: WorkbenchShell, Overview: WorkbenchOverview },
} as const satisfies Record<string, AdminLayoutTemplate>

export function getAdminLayoutTemplate(id: string): AdminLayoutTemplate {
  return Object.prototype.hasOwnProperty.call(adminLayouts, id)
    ? adminLayouts[id as keyof typeof adminLayouts]
    : adminLayouts.classic
}
