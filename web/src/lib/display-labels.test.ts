import { describe, expect, it } from 'vitest';
import { auditFieldValue, jobLabel, resourceLabel, statusInfo } from './display-labels';

describe('dynamic API labels', () => {
  it('names the action in the context of the resource it operates on', () => {
    expect(jobLabel('sync', 'sources')).toBe('同步候选源');
    expect(jobLabel('sync', 'connections')).toBe('同步连接');
    expect(jobLabel('sync', 'saas/hostnames')).toBe('同步 SaaS 主机名');
    expect(jobLabel('notify', 'alerts')).toBe('发送告警通知');
    expect(resourceLabel('saas/hostnames')).toBe('自定义主机名');
  });
  it('localizes retry, completion and alert states and preserves unknown enums', () => {
    expect(statusInfo('retrying')).toEqual({ label: '重试中', tone: 'warn' });
    expect(statusInfo('completed')).toEqual({ label: '已完成', tone: 'good' });
    expect(statusInfo('critical')).toEqual({ label: '严重', tone: 'bad' });
    expect(statusInfo('warning')).toEqual({ label: '警告', tone: 'warn' });
    expect(statusInfo('error')).toEqual({ label: '错误', tone: 'bad' });
    expect(statusInfo('future_status')).toEqual({ label: 'future_status', tone: 'neutral' });
    expect(jobLabel('future_action', 'sources')).toBe('future_action');
    expect(resourceLabel('future_resource')).toBe('future_resource');
    expect(jobLabel('toString')).toBe('toString');
    expect(statusInfo('__proto__')).toEqual({ label: '__proto__', tone: 'neutral' });
  });
  it('localizes node redeployment actions without changing stored audit evidence', () => {
    const event = { resourceType: 'nodes', action: 'redeploy', summary: 'node redeployment token issued and previous credentials revoked' };
    expect(auditFieldValue('action', event)).toBe('重新部署此节点');
    expect(auditFieldValue('summary', event)).toBe('已撤销节点旧凭据并生成重新部署注册码');
    expect(event.action).toBe('redeploy');
  });
});
