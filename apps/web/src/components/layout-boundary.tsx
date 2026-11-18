import * as React from "react"
import { CLASSIC_LAYOUT } from "@/themes/layouts/catalog"

type BoundaryProps = { layoutId: string; fallback: React.ReactNode; children: React.ReactNode }

class SafeLayout extends React.Component<Omit<BoundaryProps, "layoutId">, { failed: boolean }> {
  state = { failed: false }

  static getDerivedStateFromError() {
    return { failed: true }
  }

  render() {
    return this.state.failed ? this.props.fallback : this.props.children
  }
}

export function LayoutBoundary({ layoutId, fallback, children }: BoundaryProps) {
  if (layoutId === CLASSIC_LAYOUT) return children
  return (
    <SafeLayout key={layoutId} fallback={fallback}>
      {children}
    </SafeLayout>
  )
}
