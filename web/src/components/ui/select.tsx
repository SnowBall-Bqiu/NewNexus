import { Select as SelectPrimitive } from "@base-ui/react/select"
import { Check, ChevronDown } from "lucide-react"
import { cn } from "@/lib/utils"

const Select = SelectPrimitive.Root
const SelectGroup = SelectPrimitive.Group
function SelectValue({ className, ...props }: SelectPrimitive.Value.Props) { return <SelectPrimitive.Value data-slot="select-value" className={cn("flex flex-1 text-left", className)} {...props} /> }
function SelectTrigger({ className, children, size = "default", ...props }: SelectPrimitive.Trigger.Props & { size?: "default" | "sm" }) {
  return <SelectPrimitive.Trigger data-slot="select-trigger" className={cn("inline-flex w-full items-center justify-between gap-2 rounded-md border border-input bg-background px-3 text-sm outline-none focus-visible:border-ring focus-visible:ring-2 focus-visible:ring-ring/25 disabled:cursor-not-allowed disabled:opacity-50 aria-invalid:border-destructive data-placeholder:text-muted-foreground", size === "sm" ? "h-8 text-xs" : "h-9", className)} {...props}>{children}<SelectPrimitive.Icon render={<ChevronDown className="size-4 shrink-0 text-muted-foreground" />} /></SelectPrimitive.Trigger>
}
type ContentProps = SelectPrimitive.Popup.Props & Pick<SelectPrimitive.Positioner.Props, "side" | "sideOffset" | "align" | "alignOffset" | "alignItemWithTrigger">
function SelectContent({ className, children, side = "bottom", align = "start", sideOffset = 5, alignOffset = 0, alignItemWithTrigger = false, ...props }: ContentProps) {
  return <SelectPrimitive.Portal><SelectPrimitive.Positioner className="isolate z-60" side={side} align={align} sideOffset={sideOffset} alignOffset={alignOffset} alignItemWithTrigger={alignItemWithTrigger}><SelectPrimitive.Popup data-slot="select-content" className={cn("max-h-(--available-height) min-w-(--anchor-width) origin-(--transform-origin) overflow-y-auto rounded-lg border border-border bg-popover p-1 text-popover-foreground shadow-lg outline-none transition-[opacity,scale] duration-[200ms] ease-[var(--ease-out)] data-starting-style:opacity-0 data-ending-style:opacity-0 data-starting-style:scale-95 data-ending-style:scale-95 motion-reduce:transition-none", className)} {...props}><SelectPrimitive.List>{children}</SelectPrimitive.List></SelectPrimitive.Popup></SelectPrimitive.Positioner></SelectPrimitive.Portal>
}
function SelectItem({ className, children, ...props }: SelectPrimitive.Item.Props) { return <SelectPrimitive.Item data-slot="select-item" className={cn("relative flex min-h-8 w-full cursor-default select-none items-center rounded-md py-1.5 pl-2 pr-8 text-sm outline-none data-highlighted:bg-accent data-highlighted:text-accent-foreground data-disabled:pointer-events-none data-disabled:opacity-50", className)} {...props}><SelectPrimitive.ItemText>{children}</SelectPrimitive.ItemText><SelectPrimitive.ItemIndicator className="absolute right-2"><Check className="size-3.5" /></SelectPrimitive.ItemIndicator></SelectPrimitive.Item> }
function SelectLabel({ className, ...props }: SelectPrimitive.GroupLabel.Props) { return <SelectPrimitive.GroupLabel className={cn("px-2 py-1.5 text-xs text-muted-foreground", className)} {...props} /> }
function SelectSeparator({ className, ...props }: SelectPrimitive.Separator.Props) { return <SelectPrimitive.Separator className={cn("-mx-1 my-1 h-px bg-border", className)} {...props} /> }
export { Select, SelectGroup, SelectValue, SelectTrigger, SelectContent, SelectItem, SelectLabel, SelectSeparator }
