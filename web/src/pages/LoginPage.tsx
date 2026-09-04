import { useState, type FormEvent } from 'react';
import { Banner as KumoBanner, Button, Field, Input, LayerCard, SensitiveInput, Text } from '@cloudflare/kumo';
import { Copy, Network, ShieldCheck } from '../components/icons';
import { api, jsonBody } from '../lib/api';
import { toast } from '../lib/toast';

interface AuthStatus { setupRequired: boolean; username?: string; devMode?: boolean; securityWarning?: string }
interface SetupResponse { username: string; recoveryCodes?: string[]; totpEnabled?: boolean }

export function LoginPage({ status, onAuthenticated }: { status?: AuthStatus; onAuthenticated: () => void }) {
  const [user, setUser] = useState('admin');
  const [password, setPassword] = useState('');
  const [confirmPassword, setConfirmPassword] = useState('');
  const [busy, setBusy] = useState(false);
  const [recoveryCodes, setRecoveryCodes] = useState<string[] | null>(null);
  const setup = Boolean(status?.setupRequired);

  async function submit(event: FormEvent) {
    event.preventDefault();
    if (setup && password !== confirmPassword) {
      toast.error('两次输入的密码不一致');
      return;
    }
    setBusy(true);
    try {
      if (setup) {
        const result = await api<SetupResponse>('/auth/setup', jsonBody({ username: user, password }));
        setRecoveryCodes(result.recoveryCodes ?? []);
        toast.success('控制面初始化完成');
        if (!result.recoveryCodes?.length) onAuthenticated();
      } else {
        await api('/auth/login', jsonBody({ username: user, password }));
        onAuthenticated();
      }
    } catch (error) {
      toast.error(error instanceof Error ? error.message : setup ? '初始化失败' : '登录失败');
    } finally {
      setBusy(false);
    }
  }

  function copyRecoveryCodes() {
    if (!recoveryCodes?.length) return;
    void navigator.clipboard.writeText(recoveryCodes.join('\n'));
    toast.info('恢复码已复制');
  }

  if (recoveryCodes) {
    return <main className="login-page"><LayerCard className="login-card setup-card"><header className="login-brand"><span className="brand-mark"><ShieldCheck size={19} /></span><div><Text variant="heading2" as="h1">保存恢复码</Text><Text variant="secondary" as="p">这些恢复码只会显示一次，用于无法使用 TOTP 时登录。</Text></div></header><div className="setup-content"><KumoBanner size="sm" variant="alert" title="请立即保存" description="建议下载或复制到离线密码管理器。不要把恢复码提交到代码仓库。" /><div className="recovery-grid">{recoveryCodes.map((code) => <code key={code}>{code}</code>)}</div><div className="setup-actions"><Button variant="secondary" onClick={copyRecoveryCodes}><Copy size={16} /> 复制全部恢复码</Button><Button variant="primary" onClick={onAuthenticated}>进入控制面</Button></div></div></LayerCard></main>;
  }

  return <main className="login-page"><LayerCard className="login-card"><header className="login-brand"><span className="brand-mark"><Network size={19} /></span><div><Text variant="heading2" as="h1">NewNexus</Text><Text variant="secondary" as="p">网络调度控制面</Text></div></header>{status?.devMode && <KumoBanner size="sm" variant="alert" icon={<ShieldCheck />} title="开发模式已启用" description={status.securityWarning || '生产环境请配置主密钥和 TOTP。'} />}{setup && <KumoBanner size="sm" variant="secondary" title="首次初始化" description="创建唯一管理员并生成一次性恢复码，完成后会自动建立会话。" />}<div className="setup-steps" aria-label="初始化步骤"><span className="is-current"><b>1</b><em>管理员</em></span><i /><span className={password.length >= 12 ? 'is-current' : ''}><b>2</b><em>安全凭据</em></span><i /><span className={password.length >= 12 && confirmPassword === password ? 'is-current' : ''}><b>3</b><em>恢复码</em></span></div><form className="login-form" onSubmit={submit}><Field label="管理员账号" required><Input value={user} onChange={(event) => setUser(event.target.value)} autoComplete="username" /></Field><Field label="密码" description={setup ? '至少 12 个字符' : undefined} required><SensitiveInput value={password} onValueChange={setPassword} autoComplete={setup ? 'new-password' : 'current-password'} /></Field>{setup && <Field label="确认密码" required><SensitiveInput value={confirmPassword} onValueChange={setConfirmPassword} autoComplete="new-password" /></Field>}<Button variant="primary" type="submit" loading={busy} disabled={!user || password.length < (setup ? 12 : 1) || (setup && confirmPassword !== password)}>{setup ? '创建管理员并初始化' : '登录控制面'}</Button></form><footer className="login-foot"><Text variant="secondary" as="p" size="sm">所有外部写操作都会记录作业和审计事件。</Text></footer></LayerCard></main>;
}
