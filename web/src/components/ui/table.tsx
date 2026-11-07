import type * as React from "react"
import { cn } from "@/lib/utils"

function TableRoot({ className, ...props }: React.ComponentProps<"table">) {
  return <table data-slot="table" className={cn("w-full caption-bottom border-collapse text-sm", className)} {...props} />
}
function Table(props: React.ComponentProps<"table">) {
  return <div data-slot="table-container" className="relative w-full overflow-x-auto"><TableRoot {...props} /></div>
}
function TableHeader({ className, ...props }: React.ComponentProps<"thead">) {
  return <thead data-slot="table-header" className={cn("bg-muted/45 [&_tr]:border-b", className)} {...props} />
}
function TableBody({ className, ...props }: React.ComponentProps<"tbody">) {
  return <tbody data-slot="table-body" className={cn("[&_tr:last-child]:border-0", className)} {...props} />
}
function TableFooter({ className, ...props }: React.ComponentProps<"tfoot">) {
  return <tfoot data-slot="table-footer" className={cn("border-t bg-muted/50 font-medium", className)} {...props} />
}
function TableRow({ className, ...props }: React.ComponentProps<"tr">) {
  return <tr data-slot="table-row" className={cn("border-b border-border hover:bg-muted/40 data-[state=selected]:bg-primary/5", className)} {...props} />
}
function TableHead({ className, ...props }: React.ComponentProps<"th">) {
  return <th data-slot="table-head" className={cn("h-10 whitespace-nowrap px-4 text-left align-middle text-xs font-medium text-muted-foreground [&:has([role=checkbox])]:w-10 [&:has([role=checkbox])]:pr-0", className)} {...props} />
}
function TableCell({ className, ...props }: React.ComponentProps<"td">) {
  return <td data-slot="table-cell" className={cn("h-13 whitespace-nowrap px-4 py-3 align-middle text-[13px] [&:has([role=checkbox])]:pr-0", className)} {...props} />
}
function TableCaption({ className, ...props }: React.ComponentProps<"caption">) {
  return <caption data-slot="table-caption" className={cn("mt-4 text-sm text-muted-foreground", className)} {...props} />
}
export { Table, TableRoot, TableHeader, TableBody, TableFooter, TableRow, TableHead, TableCell, TableCaption }
