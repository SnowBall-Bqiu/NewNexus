import { t } from '@/lib/i18n';
import type * as React from "react"
import { Dialog as SheetPrimitive } from "@base-ui/react/dialog"
import { X } from "lucide-react"
import { cn } from "@/lib/utils"
import { Button } from "./button"
import { DialogOverlay } from "./dialog"

const Sheet = SheetPrimitive.Root
const SheetTrigger = SheetPrimitive.Trigger
const SheetClose = SheetPrimitive.Close
const SheetPortal = SheetPrimitive.Portal
const sides = {
  right: "inset-y-0 right-0 h-dvh w-[min(92vw,480px)] border-l data-starting-style:translate-x-full data-ending-style:translate-x-full",
  left: "inset-y-0 left-0 h-dvh w-[min(85vw,280px)] border-r data-starting-style:-translate-x-full data-ending-style:-translate-x-full",
  top: "inset-x-0 top-0 max-h-[90dvh] border-b data-starting-style:-translate-y-full data-ending-style:-translate-y-full",
  bottom: "inset-x-0 bottom-0 max-h-[90dvh] border-t data-starting-style:translate-y-full data-ending-style:translate-y-full",
}
function SheetContent({ className, children, side = "right", showCloseButton = true, closeLabel = t("common.close"), ...props }: SheetPrimitive.Popup.Props & { side?: keyof typeof sides; showCloseButton?: boolean; closeLabel?: string }) {
  return <SheetPortal><DialogOverlay /><SheetPrimitive.Popup data-slot="sheet-content" data-side={side} className={cn("fixed z-50 flex flex-col gap-5 overflow-y-auto border-border bg-popover text-popover-foreground shadow-xl outline-none transition-transform duration-[var(--motion-normal,180ms)] ease-[var(--ease-drawer)] motion-reduce:transition-none", sides[side], className)} {...props}>{children}{showCloseButton && <SheetPrimitive.Close render={<Button variant="ghost" size="icon-sm" className="absolute right-3 top-3" />}><X /><span className="sr-only">{closeLabel}</span></SheetPrimitive.Close>}</SheetPrimitive.Popup></SheetPortal>
}
function SheetHeader({ className, ...props }: React.ComponentProps<"div">) { return <div data-slot="sheet-header" className={cn("flex flex-col gap-2 border-b px-6 py-5 pr-12", className)} {...props} /> }
function SheetFooter({ className, ...props }: React.ComponentProps<"div">) { return <div data-slot="sheet-footer" className={cn("mt-auto flex items-center justify-end gap-2 border-t p-5", className)} {...props} /> }
function SheetTitle({ className, ...props }: SheetPrimitive.Title.Props) { return <SheetPrimitive.Title data-slot="sheet-title" className={cn("text-base font-semibold", className)} {...props} /> }
function SheetDescription({ className, ...props }: SheetPrimitive.Description.Props) { return <SheetPrimitive.Description data-slot="sheet-description" className={cn("text-sm leading-6 text-muted-foreground", className)} {...props} /> }
export { Sheet, SheetTrigger, SheetClose, SheetPortal, SheetContent, SheetHeader, SheetFooter, SheetTitle, SheetDescription }
