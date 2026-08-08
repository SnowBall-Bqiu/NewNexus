import { lazy, Suspense, useEffect, useState } from 'react';
import { NavLink, Navigate, Route, Routes, useLocation } from 'react-router-dom';
import { useQuery, useQueryClient } from '@tanstack/react-query';
import {
  Activity, AlertTriangle, Bell, Boxes, Cable, ChevronDown, Cloud, Gauge, GitBranch,
  LayoutDashboard, Menu, Network, RadioTower, ScrollText, Settings, ShieldCheck, X,
} from 'lucide-react';
import { api } from './lib/api';
import { t, type MessageKey } from './lib/i18n';
import { IconButton } from './components/ui';
import { LoginPage } from './pages/LoginPage';

const OverviewPage = lazy(() => import('./pages/OverviewPage').then((module) => ({ default: module.OverviewPage })));
const ProjectsPage = lazy(() => import('./pages/ProjectsPage').then((module) => ({ default: module.ProjectsPage })));
const SaaSPage = lazy(() => import('./pages/SaaSPage').then((module) => ({ default: module.SaaSPage })));
const OptimizerPage = lazy(() => import('./pages/OptimizerPage').then((module) => ({ default: module.OptimizerPage })));
const RoutesPage = lazy(() => import('./pages/RoutesPage').then((module) => ({ default: module.RoutesPage })));
const NodesPage = lazy(() => import('./pages/NodesPage').then((module) => ({ default: module.NodesPage })));
const ReleasesPage = lazy(() => import('./pages/ReleasesPage').then((module) => ({ default: module.ReleasesPage })));
const AlertsPage = lazy(() => import('./pages/AlertsPage').then((module) => ({ default: module.AlertsPage })));
const ConnectionsPage = lazy(() => import('./pages/ConnectionsPage').then((module) => ({ default: module.ConnectionsPage })));
const SettingsPage = lazy(() => import('./pages/SettingsPage').then((module) => ({ default: module.SettingsPage })));

interface AuthStatus { setupRequired: boolean; authenticated: boolean; username?: string; totpEnabled?: boolean; devMode?: boolean }

const navigation: { key: MessageKey; path: string; icon: typeof LayoutDashboard }[] = [
  { key: 'overview', path: '/overview', icon: LayoutDashboard },
  { key: 'projects', path: '/projects', icon: Boxes },
  { key: 'saas', path: '/saas', icon: Cloud },
  { key: 'optimizer', path: '/optimizer', icon: Gauge },
  { key: 'routes', path: '/routes', icon: GitBranch },
  { key: 'nodes', path: '/nodes', icon: RadioTower },
  { key: 'releases', path: '/releases', icon: ScrollText },
  { key: 'alerts', path: '/alerts', icon: Bell },
  { key: 'connections', path: '/connections', icon: Cable },
  { key: 'settings', path: '/settings', icon: Settings },
];

export default function App() {
  const auth = useQuery({ queryKey: ['auth-status'], queryFn: () => api<AuthStatus>('/auth/status'), retry: false });
  if (auth.isLoading) return <div className="boot-screen"><div className="brand-mark brand-mark--large"><Network /></div><span>正在连接 NewNexus</span></div>;
  if (!auth.data?.authenticated) return <LoginPage status={auth.data} onAuthenticated={() => auth.refetch()} />;
  return <AppShell username={auth.data.username ?? '管理员'} />;
}

function AppShell({ username }: { username: string }) {
  const [mobileOpen, setMobileOpen] = useState(false);
  const location = useLocation();
  const queryClient = useQueryClient();
  useEffect(() => setMobileOpen(false), [location.pathname]);
  useEffect(() => {
    const source = new EventSource('/api/v1/events/stream');
    source.addEventListener('update', () => queryClient.invalidateQueries());
    return () => source.close();
  }, [queryClient]);
  const current = navigation.find((item) => location.pathname.startsWith(item.path));
  return <div className="app-shell">
    <div className={mobileOpen ? 'mobile-scrim is-open' : 'mobile-scrim'} onClick={() => setMobileOpen(false)} />
    <aside className={mobileOpen ? 'sidebar is-open' : 'sidebar'}>
      <div className="sidebar__brand"><div className="brand-mark"><Network size={19} /></div><div><strong>NewNexus</strong><span>网络调度控制面</span></div><IconButton className="sidebar__close" label="关闭导航" onClick={() => setMobileOpen(false)}><X size={18} /></IconButton></div>
      <div className="workspace-switch"><span className="workspace-switch__avatar">星</span><div><strong>星桥网络</strong><span>生产工作区</span></div><ChevronDown size={15} /></div>
      <nav aria-label="主导航">{navigation.map(({ key, path, icon: Icon }) => <NavLink key={path} to={path} className={({ isActive }) => isActive ? 'nav-item is-active' : 'nav-item'}><Icon size={17} /><span>{t(key)}</span>{key === 'alerts' && <span className="nav-item__count">2</span>}</NavLink>)}</nav>
      <div className="sidebar__footer"><div className="system-health"><span className="system-health__pulse" /><div><strong>控制面运行正常</strong><span>SQLite · 本地模式</span></div></div></div>
    </aside>
    <main className="main-area">
      <header className="topbar"><div className="topbar__left"><IconButton className="mobile-menu" label="打开导航" onClick={() => setMobileOpen(true)}><Menu size={19} /></IconButton><div><span className="topbar__eyebrow">星桥网络</span><strong>{current ? t(current.key) : 'NewNexus'}</strong></div></div><div className="topbar__right"><div className="live-indicator"><Activity size={15} /><span>实时同步</span></div><IconButton label="告警"><AlertTriangle size={18} /></IconButton><div className="user-menu"><span>{username.slice(0, 1).toUpperCase()}</span><div><strong>{username}</strong><small><ShieldCheck size={12} /> TOTP 已保护</small></div></div></div></header>
      <div className="page-container"><Suspense fallback={<div className="route-loading">正在载入视图</div>}><Routes><Route path="/overview" element={<OverviewPage />} /><Route path="/projects" element={<ProjectsPage />} /><Route path="/saas" element={<SaaSPage />} /><Route path="/optimizer" element={<OptimizerPage />} /><Route path="/routes" element={<RoutesPage />} /><Route path="/nodes" element={<NodesPage />} /><Route path="/releases" element={<ReleasesPage />} /><Route path="/alerts" element={<AlertsPage />} /><Route path="/connections" element={<ConnectionsPage />} /><Route path="/settings" element={<SettingsPage />} /><Route path="*" element={<Navigate to="/overview" replace />} /></Routes></Suspense></div>
    </main>
  </div>;
}
