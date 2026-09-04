import { Suspense, useEffect, useMemo, useState } from 'react';
import { Navigate, Route, Routes, useLocation, useNavigate } from 'react-router-dom';
import { useQuery, useQueryClient } from '@tanstack/react-query';
import {
  Banner as KumoBanner,
  Breadcrumbs,
  Button as KumoButton,
  CommandPalette,
  DropdownMenu,
  Sidebar,
  Text,
} from '@cloudflare/kumo';
import {
  Activity,
  AlertTriangle,
  Bell,
  Boxes,
  Cable,
  CaretRight,
  Cloud,
  Gauge,
  Gear,
  GitBranch,
  House,
  ListChecks,
  LogOut,
  MagnifyingGlass,
  Network,
  RadioTower,
  Scroll,
  ShieldCheck,
} from './components/icons';
import { ThemeSwitcher } from './components/ThemeSwitcher';
import { api } from './lib/api';
import {
  AlertsPage,
  AuditPage,
  ConnectionsPage,
  JobsPage,
  NodesPage,
  OptimizerPage,
  ProjectsPage,
  ReleasesPage,
  RoutesPage,
  SaaSPage,
  SettingsPage,
} from './pages/ConsolePages';
import { OverviewPage } from './pages/OverviewPage';
import { LoginPage } from './pages/LoginPage';

interface AuthStatus {
  setupRequired: boolean;
  authenticated: boolean;
  username?: string;
  totpEnabled?: boolean;
  devMode?: boolean;
  securityWarning?: string;
}

interface ProjectSummary {
  id: string;
  name: string;
  avatar?: string;
  color?: string;
  status?: string;
}

interface SummaryData {
  counts?: Record<string, number>;
  database?: string;
  version?: string;
}

type NavItem = {
  label: string;
  path: string;
  Icon: typeof House;
};

type NavGroup = {
  label: string;
  items: NavItem[];
};

const groups: NavGroup[] = [
  {
    label: '工作台',
    items: [
      { label: '总览', path: '/overview', Icon: House },
      { label: '项目与站点', path: '/projects', Icon: Boxes },
    ],
  },
  {
    label: '流量调度',
    items: [
      { label: 'Cloudflare SaaS', path: '/saas', Icon: Cloud },
      { label: 'CF 优选', path: '/optimizer', Icon: Gauge },
      { label: 'CNAME 线路', path: '/routes', Icon: GitBranch },
    ],
  },
  {
    label: '运维记录',
    items: [
      { label: '探测节点', path: '/nodes', Icon: RadioTower },
      { label: '发布记录', path: '/releases', Icon: Scroll },
      { label: '作业中心', path: '/jobs', Icon: ListChecks },
      { label: '告警', path: '/alerts', Icon: Bell },
      { label: '审计日志', path: '/audit', Icon: ShieldCheck },
    ],
  },
  {
    label: '管理设置',
    items: [
      { label: '厂商连接', path: '/connections', Icon: Cable },
      { label: '系统设置', path: '/settings', Icon: Gear },
    ],
  },
];

const allNavItems = groups.flatMap((group) => group.items.map((item) => ({ ...item, group: group.label })));

export default function App() {
  const auth = useQuery({
    queryKey: ['auth-status'],
    queryFn: () => api<AuthStatus>('/auth/status'),
    retry: false,
  });

  if (auth.isLoading) {
    return <div className="boot"><span className="brand-mark"><Network size={20} /></span><Text>正在连接控制面</Text></div>;
  }
  if (!auth.data?.authenticated) {
    return <LoginPage status={auth.data} onAuthenticated={() => void auth.refetch()} />;
  }
  return <AppShell auth={auth.data} />;
}

function AppShell({ auth }: { auth: AuthStatus }) {
  const location = useLocation();
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const [open, setOpen] = useState(() => {
    try {
      const saved = localStorage.getItem('newnexus.sidebar.open');
      if (saved === 'false') return false;
      if (saved === 'true') return true;
    } catch {
      // Storage may be unavailable; use the desktop default.
    }
    return typeof window === 'undefined' || window.innerWidth >= 820;
  });
  const [searchOpen, setSearchOpen] = useState(false);
  const [search, setSearch] = useState('');
  const projects = useQuery({
    queryKey: ['projects'],
    queryFn: () => api<ProjectSummary[]>('/projects'),
  });
  const summary = useQuery({
    queryKey: ['summary'],
    queryFn: () => api<SummaryData>('/summary'),
    refetchInterval: 30_000,
  });
  const project = projects.data?.[0];
  const current = allNavItems.find((item) => location.pathname === item.path || location.pathname.startsWith(`${item.path}/`));
  const alertCount = summary.data?.counts?.alerts ?? 0;
  const items = useMemo(() => allNavItems.map((item) => ({ ...item, id: item.path, title: item.label })), []);
  const filteredItems = useMemo(() => {
    const query = search.trim().toLocaleLowerCase();
    return query ? items.filter((item) => `${item.title} ${item.group}`.toLocaleLowerCase().includes(query)) : items;
  }, [items, search]);

  useEffect(() => {
    try {
      localStorage.setItem('newnexus.sidebar.open', String(open));
    } catch {
      // Storage may be unavailable.
    }
  }, [open]);

  useEffect(() => {
    const onKeyDown = (event: KeyboardEvent) => {
      if ((event.ctrlKey || event.metaKey) && event.key.toLowerCase() === 'k') {
        event.preventDefault();
        setSearchOpen(true);
      }
    };
    window.addEventListener('keydown', onKeyDown);
    return () => window.removeEventListener('keydown', onKeyDown);
  }, []);

  useEffect(() => {
    const source = new EventSource('/api/v1/events/stream');
    const update = () => void queryClient.invalidateQueries({ queryKey: ['summary'] });
    source.addEventListener('update', update);
    return () => {
      source.removeEventListener('update', update);
      source.close();
    };
  }, [queryClient]);

  useEffect(() => {
    setSearch('');
  }, [location.pathname]);

  function selectSearchItem(item: (typeof items)[number]) {
    setSearchOpen(false);
    setSearch('');
    navigate(item.path);
  }

  async function logout() {
    try {
      await api('/auth/logout', { method: 'POST' });
    } finally {
      await queryClient.invalidateQueries({ queryKey: ['auth-status'] });
    }
  }

  return (
    <Sidebar.Provider
      open={open}
      onOpenChange={setOpen}
      collapsible="icon"
      resizable={false}
      defaultWidth={240}
      minWidth={240}
      maxWidth={240}
      mobileBreakpoint={820}
      className="app-shell"
      style={{ '--sidebar-width': '240px', '--sidebar-width-icon': '57px' } as React.CSSProperties}
    >
      <Sidebar className="app-sidebar" aria-label="主导航">
        <Sidebar.Header className="brand">
          <div className="brand-line">
            <span className="cloud-mark" aria-hidden="true"><Cloud size={23} weight="fill" /></span>
            <div className="brand-copy">
              <strong>{project?.name || '选择项目'}</strong>
              <small>NewNexus</small>
            </div>
            <DropdownMenu>
              <DropdownMenu.Trigger render={<button className="workspace-chevron" type="button" aria-label="切换项目" />}>
                <CaretRight size={14} />
              </DropdownMenu.Trigger>
              <DropdownMenu.Content align="start" side="right" className="workspace-menu">
                {projects.data?.map((item) => (
                  <DropdownMenu.Item key={item.id} onClick={() => navigate(`/projects?project=${item.id}`)}>
                    <span className="workspace-option-mark" style={{ background: item.color || 'var(--brand-green)' }}>{item.avatar || item.name.slice(0, 1)}</span>
                    <span>{item.name}</span>
                  </DropdownMenu.Item>
                ))}
                <DropdownMenu.Separator />
                <DropdownMenu.Item onClick={() => navigate('/projects')} icon={<Boxes size={16} />}>管理项目</DropdownMenu.Item>
              </DropdownMenu.Content>
            </DropdownMenu>
          </div>
        </Sidebar.Header>

        <Sidebar.Content>
          <button className="sidebar-search" type="button" onClick={() => setSearchOpen(true)}>
            <MagnifyingGlass size={16} aria-hidden="true" />
            <span>快速搜索...</span>
            <kbd>Ctrl K</kbd>
          </button>

          {groups.map((group) => (
            <Sidebar.Group key={group.label}>
              <Sidebar.GroupLabel>{group.label}</Sidebar.GroupLabel>
              <Sidebar.Menu>
                {group.items.map(({ label, path, Icon }) => {
                  const active = location.pathname === path || location.pathname.startsWith(`${path}/`);
                  return (
                    <Sidebar.MenuButton key={path} href={path} active={active} icon={<Icon size={16} />} tooltip={label}>
                      <span className="menu-label">{label}</span>
                      <CaretRight className="menu-tail" size={13} aria-hidden="true" />
                      {label === '告警' && alertCount > 0 && <Sidebar.MenuBadge>{alertCount > 99 ? '99+' : alertCount}</Sidebar.MenuBadge>}
                    </Sidebar.MenuButton>
                  );
                })}
              </Sidebar.Menu>
            </Sidebar.Group>
          ))}
        </Sidebar.Content>

        <Sidebar.Footer className="sidebar-footer">
          <Sidebar.Trigger className="sidebar-collapse" aria-label={open ? '折叠导航' : '展开导航'} />
        </Sidebar.Footer>
      </Sidebar>

      <main className="main">
        <header className="topbar">
          <div className="topbar-left">
            <Sidebar.Trigger aria-label="切换导航" />
            <Breadcrumbs className="breadcrumb">
              <Breadcrumbs.Link href="/overview" icon={<House size={14} />}>NewNexus</Breadcrumbs.Link>
              {current && <><Breadcrumbs.Separator /><Breadcrumbs.Current>{current.label}</Breadcrumbs.Current></>}
            </Breadcrumbs>
          </div>
          <div className="topbar-right">
            <span className="live"><Activity size={15} />实时同步</span>
            <ThemeSwitcher />
            <button className="topbar-icon" type="button" aria-label="告警" onClick={() => navigate('/alerts')}>
              <AlertTriangle size={18} />
              {alertCount > 0 && <em>{alertCount > 99 ? '99+' : alertCount}</em>}
            </button>
            <DropdownMenu>
              <DropdownMenu.Trigger render={<button className="account" type="button" aria-label="账户菜单" />}>
                <span className="account-mark">{(auth.username || 'A').slice(0, 1).toUpperCase()}</span>
                <span className="account-copy"><strong>{auth.username || '管理员'}</strong><small><ShieldCheck size={12} /> {auth.totpEnabled ? 'TOTP 已保护' : 'TOTP 未启用'}</small></span>
              </DropdownMenu.Trigger>
              <DropdownMenu.Content align="end">
                <DropdownMenu.Item icon={<Gear size={16} />} onClick={() => navigate('/settings?tab=security')}>账户安全</DropdownMenu.Item>
                <DropdownMenu.Separator />
                <DropdownMenu.Item icon={<LogOut size={16} />} onClick={() => void logout()}>退出登录</DropdownMenu.Item>
              </DropdownMenu.Content>
            </DropdownMenu>
          </div>
        </header>

        <div className="content">
          {!auth.totpEnabled && <KumoBanner size="sm" variant="alert" icon={<ShieldCheck />} title="TOTP 尚未启用" description="生产环境请在系统设置中完成二次验证。" action={<KumoButton size="sm" variant="ghost" onClick={() => navigate('/settings?tab=security')}>立即配置</KumoButton>} />}
          <Suspense fallback={<div className="loading-state">正在载入视图</div>}>
            <Routes>
              <Route path="/overview" element={<OverviewPage />} />
              <Route path="/projects" element={<ProjectsPage />} />
              <Route path="/saas" element={<SaaSPage />} />
              <Route path="/optimizer" element={<OptimizerPage />} />
              <Route path="/routes" element={<RoutesPage />} />
              <Route path="/nodes" element={<NodesPage />} />
              <Route path="/releases" element={<ReleasesPage />} />
              <Route path="/jobs" element={<JobsPage />} />
              <Route path="/alerts" element={<AlertsPage />} />
              <Route path="/audit" element={<AuditPage />} />
              <Route path="/connections" element={<ConnectionsPage />} />
              <Route path="/settings" element={<SettingsPage auth={auth} />} />
              <Route path="*" element={<Navigate to="/overview" replace />} />
            </Routes>
          </Suspense>
        </div>
      </main>

      <CommandPalette.Root
        open={searchOpen}
        onOpenChange={setSearchOpen}
        items={filteredItems}
        value={search}
        onValueChange={setSearch}
        itemToStringValue={(item) => item.title}
        onSelect={(item) => selectSearchItem(item)}
      >
        <CommandPalette.Input placeholder="搜索页面或操作..." />
        <CommandPalette.List>
          <CommandPalette.Results>
            {(item: (typeof items)[number]) => (
              <CommandPalette.Item key={item.id} value={item} onClick={() => selectSearchItem(item)}>
                <span className="command-item"><item.Icon size={16} /><span>{item.title}</span><small>{item.group}</small></span>
              </CommandPalette.Item>
            )}
          </CommandPalette.Results>
          <CommandPalette.Empty>没有匹配结果</CommandPalette.Empty>
        </CommandPalette.List>
        <CommandPalette.Footer><span>↑↓ 浏览</span><span>Enter 打开</span><span>Esc 关闭</span></CommandPalette.Footer>
      </CommandPalette.Root>
    </Sidebar.Provider>
  );
}
