import { Check, ChevronsUpDown, Languages } from "lucide-react"
import { languageOptions, uiText, useLanguage } from "@/lib/language"
import { Button } from "@/components/ui/button"
import { SidebarMenuButton } from "@/components/ui/sidebar"
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select"
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu"

export function LanguageSelector({
  variant = "compact",
}: {
  variant?: "compact" | "field" | "sidebar"
}) {
  const [language, setLanguage] = useLanguage()
  const currentLanguage = languageOptions.find((option) => option.value === language)

  if (variant === "field") {
    return (
      <Select
        value={language}
        onValueChange={(value) => {
          const option = languageOptions.find((item) => item.value === value)
          if (option) setLanguage(option.value)
        }}
      >
        <SelectTrigger aria-label={uiText("界面语言")}>
          <SelectValue />
        </SelectTrigger>
        <SelectContent>
          {languageOptions.map((option) => (
            <SelectItem key={option.value} value={option.value}>
              <span lang={option.htmlLang}>{option.label}</span>
            </SelectItem>
          ))}
        </SelectContent>
      </Select>
    )
  }

  return (
    <DropdownMenu>
      <DropdownMenuTrigger asChild>
        {variant === "sidebar" ? (
          <SidebarMenuButton
            aria-label={uiText("切换语言")}
            tooltip={uiText("切换语言")}
            className="h-10"
          >
            <Languages className="size-4 text-muted-foreground" aria-hidden />
            <span lang={currentLanguage?.htmlLang} className="flex-1 truncate">
              {currentLanguage?.label}
            </span>
            <ChevronsUpDown
              className="ml-auto size-4 text-muted-foreground group-data-[collapsible=icon]:hidden"
              aria-hidden
            />
          </SidebarMenuButton>
        ) : (
          <Button
            type="button"
            variant="ghost"
            size="sm"
            aria-label={uiText("切换语言")}
            title={uiText("切换语言")}
          >
            {currentLanguage?.shortLabel}
          </Button>
        )}
      </DropdownMenuTrigger>
      <DropdownMenuContent
        align={variant === "sidebar" ? "start" : "end"}
        side={variant === "sidebar" ? "top" : "bottom"}
        className="min-w-40"
      >
        {languageOptions.map((option) => (
          <DropdownMenuItem
            key={option.value}
            onSelect={() => setLanguage(option.value)}
            className="gap-2"
          >
            <span lang={option.htmlLang} className="flex-1">
              {option.label}
            </span>
            {option.value === language && <Check className="size-4" aria-hidden />}
          </DropdownMenuItem>
        ))}
      </DropdownMenuContent>
    </DropdownMenu>
  )
}
