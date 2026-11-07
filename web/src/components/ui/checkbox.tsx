import { Checkbox as CheckboxPrimitive } from "@base-ui/react/checkbox"
import { Check, Minus } from "lucide-react"
import { cn } from "@/lib/utils"

type CheckboxProps = Omit<CheckboxPrimitive.Root.Props, "checked"> & { checked?: boolean | "indeterminate" }

function Checkbox({ className, checked, indeterminate, ...props }: CheckboxProps) {
  const mixed = checked === "indeterminate" || indeterminate
  return (
    <CheckboxPrimitive.Root data-slot="checkbox" checked={checked === "indeterminate" ? false : checked} indeterminate={mixed} className={cn("peer relative inline-flex size-4 shrink-0 items-center justify-center rounded-[4px] border border-input bg-background outline-none focus-visible:border-ring focus-visible:ring-2 focus-visible:ring-ring/30 disabled:cursor-not-allowed disabled:opacity-50 aria-invalid:border-destructive data-checked:border-primary data-checked:bg-primary data-checked:text-primary-foreground data-indeterminate:border-primary data-indeterminate:bg-primary data-indeterminate:text-primary-foreground", className)} {...props}>
      <CheckboxPrimitive.Indicator data-slot="checkbox-indicator" className="grid place-items-center text-current">
        {mixed ? <Minus className="size-3" /> : <Check className="size-3" strokeWidth={3} />}
      </CheckboxPrimitive.Indicator>
    </CheckboxPrimitive.Root>
  )
}
export { Checkbox }
