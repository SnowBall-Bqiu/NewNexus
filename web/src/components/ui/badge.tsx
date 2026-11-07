import type * as React from "react"
import { cva, type VariantProps } from "class-variance-authority"
import { cn } from "@/lib/utils"

const badgeVariants = cva("inline-flex w-fit shrink-0 items-center gap-1.5 whitespace-nowrap rounded-md border px-2 py-0.5 text-[11px] font-medium leading-4 [&_svg]:size-3", {
  variants: {
    variant: {
      default: "border-primary/15 bg-primary/10 text-primary",
      secondary: "border-border bg-secondary text-secondary-foreground",
      outline: "border-border text-muted-foreground",
      destructive: "border-red-500/20 bg-red-500/10 text-red-700 dark:text-red-400",
      success: "border-emerald-500/20 bg-emerald-500/10 text-emerald-700 dark:text-emerald-400",
      warning: "border-amber-500/20 bg-amber-500/10 text-amber-700 dark:text-amber-400",
    },
  },
  defaultVariants: { variant: "default" },
})

function Badge({ className, variant, ...props }: React.ComponentProps<"span"> & VariantProps<typeof badgeVariants>) {
  return <span data-slot="badge" className={cn(badgeVariants({ variant }), className)} {...props} />
}

export { Badge, badgeVariants }
