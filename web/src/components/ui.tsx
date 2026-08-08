import type { ButtonHTMLAttributes, HTMLAttributes, PropsWithChildren, ReactNode } from 'react';
import { Dialog } from '@base-ui-components/react/dialog';
import { Tooltip } from '@base-ui-components/react/tooltip';
import { X } from 'lucide-react';
import clsx from 'clsx';

export function Button({ className, variant = 'secondary', size = 'md', ...props }: ButtonHTMLAttributes<HTMLButtonElement> & { variant?: 'primary' | 'secondary' | 'ghost' | 'danger'; size?: 'sm' | 'md' | 'icon' }) {
  return <button className={clsx('button', `button--${variant}`, `button--${size}`, className)} {...props} />;
}

export function IconButton({ label, className, ...props }: ButtonHTMLAttributes<HTMLButtonElement> & { label: string }) {
  return <Tooltip.Root>
    <Tooltip.Trigger className={clsx('button', 'button--ghost', 'button--icon', className)} aria-label={label} {...props} />
    <Tooltip.Portal>
      <Tooltip.Positioner className="tooltip-positioner" sideOffset={7}>
        <Tooltip.Popup className="tooltip-popup">{label}</Tooltip.Popup>
      </Tooltip.Positioner>
    </Tooltip.Portal>
  </Tooltip.Root>;
}

export function Badge({ status, children }: PropsWithChildren<{ status: string }>) {
  const label = children ?? statusLabels[status] ?? status;
  return <span className={clsx('badge', `badge--${status}`)}><span className="badge__dot" />{label}</span>;
}

const statusLabels: Record<string, string> = { healthy: '健康', connected: '已连接', active: '已生效', online: '在线', applied: '已发布', synced: '已同步', warning: '注意', degraded: '退化', recovering: '恢复中', pending: '等待中', pending_validation: '待验证', offline: '离线', error: '异常', failed: '失败', unknown: '未知', preview: '仅预览', open: '处理中', acknowledged: '已确认' };

export function PageHeader({ title, description, actions }: { title: string; description: string; actions?: ReactNode }) {
  return <header className="page-header"><div><h1>{title}</h1><p>{description}</p></div>{actions && <div className="page-header__actions">{actions}</div>}</header>;
}

export function EmptyState({ icon, title, description, action }: { icon: ReactNode; title: string; description: string; action?: ReactNode }) {
  return <div className="empty-state"><div className="empty-state__icon">{icon}</div><h3>{title}</h3><p>{description}</p>{action}</div>;
}

export function DialogShell({ open, onOpenChange, title, description, children, footer, size = 'md' }: PropsWithChildren<{ open: boolean; onOpenChange: (open: boolean) => void; title: string; description?: string; footer?: ReactNode; size?: 'sm' | 'md' | 'lg' }>) {
  return <Dialog.Root open={open} onOpenChange={onOpenChange}>
    <Dialog.Portal>
      <Dialog.Backdrop className="dialog-backdrop" />
      <Dialog.Viewport className="dialog-viewport">
        <Dialog.Popup className={clsx('dialog-popup', `dialog-popup--${size}`)}>
          <div className="dialog-header"><div><Dialog.Title className="dialog-title">{title}</Dialog.Title>{description && <Dialog.Description className="dialog-description">{description}</Dialog.Description>}</div><Dialog.Close className="dialog-close" aria-label="关闭"><X size={17} /></Dialog.Close></div>
          <div className="dialog-body">{children}</div>
          {footer && <div className="dialog-footer">{footer}</div>}
        </Dialog.Popup>
      </Dialog.Viewport>
    </Dialog.Portal>
  </Dialog.Root>;
}

export function Field({ label, hint, required, children }: PropsWithChildren<{ label: string; hint?: string; required?: boolean }>) {
  return <label className="field"><span className="field__label">{label}{required && <span aria-hidden="true"> *</span>}</span>{children}{hint && <span className="field__hint">{hint}</span>}</label>;
}

export function Segmented({ value, options, onChange, label }: { value: string; options: { value: string; label: string }[]; onChange: (value: string) => void; label: string }) {
  return <div className="segmented" role="radiogroup" aria-label={label}>{options.map((option) => <button key={option.value} type="button" role="radio" aria-checked={value === option.value} className={value === option.value ? 'is-active' : ''} onClick={() => onChange(option.value)}>{option.label}</button>)}</div>;
}

export function Skeleton({ width = '100%', height = 16 }: { width?: string | number; height?: number }) { return <span className="skeleton" style={{ width, height }} />; }
export function Section({ title, description, actions, children, className }: PropsWithChildren<{ title?: string; description?: string; actions?: ReactNode; className?: string }>) { return <section className={clsx('section', className)}>{title && <div className="section__header"><div><h2>{title}</h2>{description && <p>{description}</p>}</div>{actions}</div>}{children}</section>; }
export function Stat({ label, value, detail, tone = 'default' }: { label: string; value: ReactNode; detail: ReactNode; tone?: string }) { return <div className={clsx('stat', `stat--${tone}`)}><span className="stat__label">{label}</span><strong>{value}</strong><span className="stat__detail">{detail}</span></div>; }
export function InlineCode({ children }: PropsWithChildren) { return <code className="inline-code">{children}</code>; }
export function Divider(props: HTMLAttributes<HTMLHRElement>) { return <hr className="divider" {...props} />; }
