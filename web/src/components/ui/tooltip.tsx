import { Tooltip as TooltipPrimitive } from "@base-ui/react/tooltip"
import { cn } from "@/lib/utils"

function TooltipProvider({ delay = 250, ...props }: TooltipPrimitive.Provider.Props) { return <TooltipPrimitive.Provider delay={delay} {...props} /> }
const Tooltip = TooltipPrimitive.Root
const TooltipTrigger = TooltipPrimitive.Trigger
function TooltipContent({ className, side = "top", sideOffset = 6, align = "center", alignOffset = 0, ...props }: TooltipPrimitive.Popup.Props & Pick<TooltipPrimitive.Positioner.Props, "side" | "sideOffset" | "align" | "alignOffset">) {
  return <TooltipPrimitive.Portal><TooltipPrimitive.Positioner side={side} sideOffset={sideOffset} align={align} alignOffset={alignOffset} className="z-70"><TooltipPrimitive.Popup data-slot="tooltip-content" className={cn("max-w-xs rounded-md bg-foreground px-2.5 py-1.5 text-xs text-background shadow-md origin-(--transform-origin) transition-[opacity,scale] duration-[125ms] ease-[var(--ease-out)] data-starting-style:scale-97 data-ending-style:scale-97 data-starting-style:opacity-0 data-ending-style:opacity-0 motion-reduce:transition-none", className)} {...props} /></TooltipPrimitive.Positioner></TooltipPrimitive.Portal>
}
export { TooltipProvider, Tooltip, TooltipTrigger, TooltipContent }
