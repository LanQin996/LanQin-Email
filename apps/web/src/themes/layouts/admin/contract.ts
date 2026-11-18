import type { ComponentType, ReactNode } from "react"

export type AdminLayoutLink = Readonly<{
  key: string
  href: string
  label: string
  icon: ReactNode
}>

export type AdminShellSlots = Readonly<{
  brand: ReactNode
  brandLabel: string
  sidebarLanguage: ReactNode
  compactLanguage: ReactNode
  account: ReactNode
  accountLabel: string
  signOut: ReactNode
  navigationLabel: string
  openNavigationLabel: string
  currentLabel: string
  activeKey: string
  links: readonly AdminLayoutLink[]
  children: ReactNode
}>

export type AdminOverviewSlots = Readonly<{
  metrics?: ReactNode
  status: ReactNode
  setup: ReactNode
  domains: ReactNode
  runtime: ReactNode
}>

export type AdminLayoutTemplate = Readonly<{
  version: 1
  Shell: ComponentType<AdminShellSlots>
  Overview: ComponentType<AdminOverviewSlots>
}>
