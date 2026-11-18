import type { ComponentType } from "react"
import {
  ClassicFrontDesktop,
  ClassicFrontReader,
  FrontAuthFrame,
  WorkbenchFrontDesktop,
  WorkbenchFrontReader,
  type FrontAuthSlots,
  type FrontDesktopSlots,
  type FrontReaderSlots,
} from "./front-layout"

export type FrontLayoutTemplate = {
  id: string
  desktop: ComponentType<FrontDesktopSlots>
  reader: ComponentType<FrontReaderSlots>
  auth?: ComponentType<FrontAuthSlots & { layoutId: string }>
}

export const frontTemplateLayouts = {
  classic: {
    id: "classic",
    desktop: ClassicFrontDesktop,
    reader: ClassicFrontReader,
    auth: FrontAuthFrame,
  },
  workbench: {
    id: "workbench",
    desktop: WorkbenchFrontDesktop,
    reader: WorkbenchFrontReader,
    auth: FrontAuthFrame,
  },
} satisfies Record<string, FrontLayoutTemplate>

export function getFrontLayoutTemplate(id: string): FrontLayoutTemplate {
  const registry: Readonly<Record<string, FrontLayoutTemplate>> = frontTemplateLayouts
  return Object.prototype.hasOwnProperty.call(registry, id)
    ? registry[id]
    : frontTemplateLayouts.classic
}
