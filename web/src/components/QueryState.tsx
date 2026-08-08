import type { PropsWithChildren } from 'react';
import { AlertCircle } from 'lucide-react';
import { EmptyState, Skeleton } from './ui';

export function QueryState({ loading, error, children }: PropsWithChildren<{ loading: boolean; error?: Error | null }>) {
  if (loading) return <div className="query-loading"><Skeleton width="40%" height={24} /><Skeleton /><Skeleton width="84%" /><Skeleton width="92%" /></div>;
  if (error) return <EmptyState icon={<AlertCircle />} title="数据加载失败" description={error.message} />;
  return children;
}
