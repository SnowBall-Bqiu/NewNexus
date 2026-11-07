import { t } from '@/lib/i18n';
// Focus management, dismissal and mounting lifecycle are owned by Base UI.
import type * as React from "react"
import { Dialog as DialogPrimitive } from "@base-ui/react/dialog"
import { X } from "lucide-react"
import { cn } from "@/lib/utils"
import { Button } from "./button"

const Dialog = DialogPrimitive.Root
const DialogTrigger = DialogPrimitive.Trigger
const DialogClose = DialogPrimitive.Close
const DialogPortal = DialogPrimitive.Portal

function DialogOverlay({ className, ...props }: DialogPrimitive.Backdrop.Props) {
  return <DialogPrimitive.Backdrop data-slot="dialog-overlay" className={cn("fixed inset-0 z-50 bg-black/35 transition-opacity duration-[250ms] ease-[var(--ease-out)] data-starting-style:opacity-0 data-ending-style:opacity-0 motion-reduce:transition-none", className)} {...props} />
}
function DialogContent({ className, children, showCloseButton = true, closeLabel = t("common.close"), ...props }: DialogPrimitive.Popup.Props & { showCloseButton?: boolean; closeLabel?: string }) {
  return <DialogPortal><DialogOverlay /><DialogPrimitive.Popup data-slot="dialog-content" className={cn("fixed left-1/2 top-1/2 z-50 grid max-h-[calc(100dvh-2rem)] w-[calc(100%-2rem)] max-w-lg -translate-x-1/2 -translate-y-1/2 gap-5 overflow-y-auto rounded-lg border border-border bg-popover p-6 text-popover-foreground shadow-xl outline-none transition-[opacity,scale] duration-[250ms] ease-[var(--ease-out)] data-starting-style:scale-96 data-starting-style:opacity-0 data-ending-style:scale-96 data-ending-style:opacity-0 motion-reduce:transition-none", className)} {...props}>{children}{showCloseButton && <DialogPrimitive.Close render={<Button variant="ghost" size="icon-sm" className="absolute right-3 top-3" />}><X /><span className="sr-only">{closeLabel}</span></DialogPrimitive.Close>}</DialogPrimitive.Popup></DialogPortal>
}
function DialogHeader({ className, ...props }: React.ComponentProps<"div">) { return <div data-slot="dialog-header" className={cn("flex flex-col gap-2 pr-6", className)} {...props} /> }
function DialogFooter({ className, ...props }: React.ComponentProps<"div">) { return <div data-slot="dialog-footer" className={cn("flex flex-col-reverse gap-2 sm:flex-row sm:justify-end", className)} {...props} /> }
function DialogTitle({ className, ...props }: DialogPrimitive.Title.Props) { return <DialogPrimitive.Title data-slot="dialog-title" className={cn("text-base font-semibold leading-6", className)} {...props} /> }
function DialogDescription({ className, ...props }: DialogPrimitive.Description.Props) { return <DialogPrimitive.Description data-slot="dialog-description" className={cn("text-sm leading-6 text-muted-foreground", className)} {...props} /> }
export { Dialog, DialogTrigger, DialogClose, DialogPortal, DialogOverlay, DialogContent, DialogHeader, DialogFooter, DialogTitle, DialogDescription }
