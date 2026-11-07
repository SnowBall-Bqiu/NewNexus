import type { ReactNode } from 'react';
import { AlertTriangle, Check, Circle, Copy, Inbox } from 'lucide-react';
import { toast } from 'sonner';
import { Button } from '@/components/ui/button';
import { t } from '@/lib/i18n';
import { statusInfo } from '@/lib/display-labels';

export function PageHeading({ title, description, actions }: { title: string; description?: string; actions?: ReactNode }) { return <header className="page-heading"><div><h1>{title}</h1>{description && <p>{description}</p>}</div>{actions && <div className="page-actions">{actions}</div>}</header>; }
export function EmptyState({ title, description, action }: { title: string; description?: string; action?: ReactNode }) { return <div className="empty-state"><span className="empty-icon"><Inbox size={20} /></span><h2>{title}</h2>{description && <p>{description}</p>}{action}</div>; }
export function ErrorState({ error, retry }: { error: Error; retry?: () => void }) { return <div className="notice error" role="alert"><AlertTriangle size={16} /><div><strong>{t('common.loadFailed', '载入失败')}</strong><p>{error.message}</p>{retry && <Button size="sm" variant="outline" className="mt-3" onClick={retry}>{t('common.retry', '重试')}</Button>}</div></div>; }
export function StatusBadge({ value }: { value: unknown }) { const { label, tone } = statusInfo(value); const good = tone === 'good'; const bad = tone === 'bad'; const warn = tone === 'warn'; return <span className={`status-badge ${good ? 'good' : bad ? 'bad' : warn ? 'warn' : ''}`}>{good ? <Check size={10} /> : bad || warn ? <AlertTriangle size={10} /> : <Circle size={7} />}{label}</span>; }
export function CopyButton({ value, label = t("common.copy") }: { value: string; label?: string }) { return <Button type="button" size="sm" variant="outline" onClick={() => { void navigator.clipboard.writeText(value).then(() => toast.success(t('common.copied', '已复制'))).catch(() => toast.error(t('common.copyFailed', '复制失败，请手动选择文字'))); }}><Copy size={13} />{label}</Button>; }
