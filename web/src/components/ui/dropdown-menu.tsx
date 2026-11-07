import type * as React from "react"
import { Menu as MenuPrimitive } from "@base-ui/react/menu"
import { Check, ChevronRight } from "lucide-react"
import { cn } from "@/lib/utils"

const DropdownMenu = MenuPrimitive.Root
const DropdownMenuTrigger = MenuPrimitive.Trigger
const DropdownMenuPortal = MenuPrimitive.Portal
const DropdownMenuGroup = MenuPrimitive.Group
const DropdownMenuSub = MenuPrimitive.SubmenuRoot
const DropdownMenuRadioGroup = MenuPrimitive.RadioGroup

type ContentProps = MenuPrimitive.Popup.Props & Pick<MenuPrimitive.Positioner.Props, "side" | "sideOffset" | "align" | "alignOffset">
function DropdownMenuContent({ className, side = "bottom", align = "end", sideOffset = 6, alignOffset = 0, ...props }: ContentProps) {
  return <MenuPrimitive.Portal><MenuPrimitive.Positioner side={side} align={align} sideOffset={sideOffset} alignOffset={alignOffset} className="isolate z-60 outline-none"><MenuPrimitive.Popup data-slot="dropdown-menu-content" className={cn("z-60 max-h-(--available-height) min-w-40 origin-(--transform-origin) overflow-y-auto rounded-lg border border-border bg-popover p-1 text-popover-foreground shadow-lg outline-none transition-[opacity,scale] duration-[200ms] ease-[var(--ease-out)] data-starting-style:scale-95 data-starting-style:opacity-0 data-ending-style:scale-95 data-ending-style:opacity-0 motion-reduce:transition-none", className)} {...props} /></MenuPrimitive.Positioner></MenuPrimitive.Portal>
}
const itemClass = "relative flex min-h-8 cursor-default select-none items-center gap-2 rounded-md px-2 py-1.5 text-[13px] outline-none data-highlighted:bg-accent data-highlighted:text-accent-foreground data-disabled:pointer-events-none data-disabled:opacity-45 [&_svg]:size-4 [&_svg]:shrink-0"
function DropdownMenuItem({ className, inset, variant = "default", ...props }: MenuPrimitive.Item.Props & { inset?: boolean; variant?: "default" | "destructive" }) { return <MenuPrimitive.Item data-slot="dropdown-menu-item" className={cn(itemClass, inset && "pl-8", variant === "destructive" && "text-destructive data-highlighted:bg-destructive/10 data-highlighted:text-destructive", className)} {...props} /> }
function DropdownMenuLabel({ className, inset, ...props }: MenuPrimitive.GroupLabel.Props & { inset?: boolean }) { return <MenuPrimitive.GroupLabel data-slot="dropdown-menu-label" className={cn("px-2 py-1.5 text-xs font-medium text-muted-foreground", inset && "pl-8", className)} {...props} /> }
function DropdownMenuCheckboxItem({ className, children, ...props }: MenuPrimitive.CheckboxItem.Props) { return <MenuPrimitive.CheckboxItem data-slot="dropdown-menu-checkbox-item" className={cn(itemClass, "pr-8", className)} {...props}>{children}<MenuPrimitive.CheckboxItemIndicator className="absolute right-2"><Check className="size-3.5" /></MenuPrimitive.CheckboxItemIndicator></MenuPrimitive.CheckboxItem> }
function DropdownMenuRadioItem({ className, children, ...props }: MenuPrimitive.RadioItem.Props) { return <MenuPrimitive.RadioItem data-slot="dropdown-menu-radio-item" className={cn(itemClass, "pr-8", className)} {...props}>{children}<MenuPrimitive.RadioItemIndicator className="absolute right-2"><Check className="size-3.5" /></MenuPrimitive.RadioItemIndicator></MenuPrimitive.RadioItem> }
function DropdownMenuSeparator({ className, ...props }: MenuPrimitive.Separator.Props) { return <MenuPrimitive.Separator className={cn("-mx-1 my-1 h-px bg-border", className)} {...props} /> }
function DropdownMenuShortcut({ className, ...props }: React.ComponentProps<"span">) { return <span className={cn("ml-auto text-xs text-muted-foreground", className)} {...props} /> }
function DropdownMenuSubTrigger({ className, children, ...props }: MenuPrimitive.SubmenuTrigger.Props) { return <MenuPrimitive.SubmenuTrigger className={cn(itemClass, "data-popup-open:bg-accent", className)} {...props}>{children}<ChevronRight className="ml-auto size-3.5" /></MenuPrimitive.SubmenuTrigger> }
function DropdownMenuSubContent({ side = "right", align = "start", ...props }: ContentProps) { return <DropdownMenuContent side={side} align={align} {...props} /> }
export { DropdownMenu, DropdownMenuTrigger, DropdownMenuPortal, DropdownMenuContent, DropdownMenuGroup, DropdownMenuLabel, DropdownMenuItem, DropdownMenuCheckboxItem, DropdownMenuRadioGroup, DropdownMenuRadioItem, DropdownMenuSeparator, DropdownMenuShortcut, DropdownMenuSub, DropdownMenuSubTrigger, DropdownMenuSubContent }
