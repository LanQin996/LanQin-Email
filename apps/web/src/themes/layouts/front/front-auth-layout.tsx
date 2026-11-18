import { LayoutBoundary } from "@/components/layout-boundary"
import { FrontAuthFrame, type FrontAuthSlots } from "./front-layout"
import { getFrontLayoutTemplate } from "./registry"

export function FrontAuthLayout({ layoutId, ...slots }: FrontAuthSlots & { layoutId: string }) {
  const template = getFrontLayoutTemplate(layoutId)
  const Auth = template.auth ?? FrontAuthFrame
  const selected = <Auth layoutId={template.auth ? template.id : "classic"} {...slots} />

  // Shipped themes share one responsive frame so changing CSS never resets the form.
  if (Auth === FrontAuthFrame) return selected
  return (
    <LayoutBoundary
      layoutId={template.id}
      fallback={<FrontAuthFrame layoutId="classic" {...slots} />}
    >
      {selected}
    </LayoutBoundary>
  )
}
