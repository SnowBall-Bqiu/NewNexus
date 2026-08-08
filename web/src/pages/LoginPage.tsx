import { useState, type FormEvent } from 'react';
import { Network, ShieldCheck } from 'lucide-react';
import { api } from '../lib/api';
import { Button, Field } from '../components/ui';

interface Props { status?: { setupRequired: boolean; authenticated: boolean }; onAuthenticated: () => void }

export function LoginPage({ status, onAuthenticated }: Props) {
  const setup = status?.setupRequired ?? false;
  const [username, setUsername] = useState('admin');
  const [password, setPassword] = useState('');
  const [code, setCode] = useState('');
  const [error, setError] = useState('');
  const [loading, setLoading] = useState(false);
  async function submit(event: FormEvent) {
    event.preventDefault(); setLoading(true); setError('');
    try { await api(setup ? '/auth/setup' : '/auth/login', { method: 'POST', body: JSON.stringify({ username, password, code }) }); onAuthenticated(); }
    catch (caught) { setError(caught instanceof Error ? caught.message : '登录失败'); }
    finally { setLoading(false); }
  }
  return <main className="auth-layout"><section className="auth-panel"><div className="auth-brand"><div className="brand-mark brand-mark--large"><Network /></div><div><strong>NewNexus</strong><span>网络调度控制面</span></div></div><div className="auth-copy"><h1>{setup ? '创建本地管理员' : '登录控制台'}</h1><p>{setup ? '此账户拥有实例的全部控制权限，部署完成后请立即启用 TOTP。' : '使用本地管理员账户继续。'}</p></div><form onSubmit={submit} className="auth-form"><Field label="用户名" required><input value={username} onChange={(e) => setUsername(e.target.value)} autoComplete="username" /></Field><Field label="密码" required hint={setup ? '至少 12 个字符' : undefined}><input type="password" value={password} onChange={(e) => setPassword(e.target.value)} autoComplete={setup ? 'new-password' : 'current-password'} /></Field>{!setup && <Field label="TOTP 验证码" hint="未启用时可留空"><input inputMode="numeric" value={code} onChange={(e) => setCode(e.target.value)} autoComplete="one-time-code" /></Field>}{error && <p className="form-error" role="alert">{error}</p>}<Button variant="primary" type="submit" disabled={loading}>{loading ? '正在处理…' : setup ? '创建并进入控制台' : '登录'}</Button></form><div className="auth-footnote"><ShieldCheck size={15} /><span>凭据仅保存在你的 NewNexus 实例中</span></div></section><aside className="auth-context"><div className="auth-context__grid" /><div className="auth-context__content"><span>CONTROL PLANE</span><h2>把复杂线路<br />收束成可验证的状态。</h2><p>优选、SaaS、自定义线路、健康仲裁与发布回滚在同一控制面协同。</p></div><div className="auth-context__status"><span className="system-health__pulse" /> 本地优先 · 无遥测</div></aside></main>;
}
