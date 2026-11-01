import { useEffect, useState } from 'react';
import type { ComponentType } from 'react';
import { Button, InlineLoading, PasswordInput, TextInput, Theme } from '@carbon/react';
import {
  Cloud,
  Dashboard,
  Flow,
  Connect,
  Bot,
  Code,
  ContainerSoftware,
  Terminal,
  Document,
  Settings,
  Logout,
  Menu,
  Close,
  ArrowRight,
} from '@carbon/icons-react';
import { APIError, message, post, request } from './api';
import { ErrorNotice } from './components';
import { useRoute } from './hooks';
import {
  Artifacts,
  Configuration,
  Dashboard as DashboardPage,
  Events,
  EventPage,
  isKind,
  NotFound,
  Resources,
  RunPage,
  Runs,
} from './pages';

interface User {
  username: string;
}
interface Navigation {
  path: string;
  label: string;
  icon: ComponentType<{ size?: number }>;
  group: string;
}
const navigation: Navigation[] = [
  { path: '/', label: '运行概览', icon: Dashboard, group: '总览' },
  { path: '/resources/Pipeline', label: '流水线', icon: Flow, group: '资源配置' },
  { path: '/resources/Input', label: '接入点', icon: Connect, group: '资源配置' },
  { path: '/resources/Worker', label: '智能体', icon: Bot, group: '资源配置' },
  { path: '/resources/Workspace', label: '工作空间', icon: Code, group: '资源配置' },
  { path: '/resources/Runner', label: '运行设施', icon: ContainerSoftware, group: '资源配置' },
  { path: '/events', label: '事件队列', icon: Flow, group: '观测与交付' },
  { path: '/runs', label: '运行记录', icon: Terminal, group: '观测与交付' },
  { path: '/artifacts', label: '产物中心', icon: Document, group: '观测与交付' },
  { path: '/config', label: '配置中心', icon: Settings, group: '管理' },
];

export default function App() {
  const [user, setUser] = useState<User | null>(null);
  const [checking, setChecking] = useState(true);
  const [bootError, setBootError] = useState('');
  const [sidebarOpen, setSidebarOpen] = useState(false);
  const [logoutError, setLogoutError] = useState('');
  const route = useRoute();
  useEffect(() => {
    let active = true;
    const expired = () => {
      setUser(null);
      setBootError('登录已过期，请重新登录。');
    };
    window.addEventListener('session-expired', expired);
    void request<User>('/api/auth/me')
      .then((value) => {
        if (active) setUser(value);
      })
      .catch((cause) => {
        if (active && !(cause instanceof APIError && cause.status === 401))
          setBootError(message(cause));
      })
      .finally(() => {
        if (active) setChecking(false);
      });
    return () => {
      active = false;
      window.removeEventListener('session-expired', expired);
    };
  }, []);
  useEffect(() => {
    setSidebarOpen(false);
    window.scrollTo(0, 0);
  }, [route]);
  const logout = async () => {
    try {
      await post('/api/auth/logout');
      setUser(null);
      setBootError('');
    } catch (cause) {
      setLogoutError(message(cause));
    }
  };
  if (checking)
    return (
      <Theme theme="white">
        <div className="boot-screen">
          <Brand />
          <InlineLoading description="正在连接控制平面…" />
        </div>
      </Theme>
    );
  if (!user)
    return (
      <Theme theme="white">
        <Login initialError={bootError} onLogin={setUser} />
      </Theme>
    );
  const segments = route.split('/').filter(Boolean);
  let content;
  if (!segments.length) content = <DashboardPage />;
  else if (segments[0] === 'resources' && isKind(segments[1]))
    content = (
      <Resources
        key={segments[1]}
        kind={segments[1]}
        name={segments[2] ? decodeURIComponent(segments[2]) : undefined}
      />
    );
  else if (route === '/events') content = <Events />;
  else if (segments.length === 2 && segments[0] === 'events' && /^[1-9]\d*$/.test(segments[1]))
    content = <EventPage key={segments[1]} id={segments[1]} />;
  else if (route === '/runs') content = <Runs />;
  else if (segments[0] === 'runs' && /^\d+$/.test(segments[1]))
    content = <RunPage key={segments[1]} id={segments[1]} />;
  else if (route === '/artifacts') content = <Artifacts />;
  else if (route === '/config') content = <Configuration />;
  else content = <NotFound />;
  const current = navigation.find((item) =>
    item.path === '/' ? route === '/' : route.startsWith(item.path),
  );
  return (
    <Theme theme="g10">
      <div className="app-shell">
        <a
          className="skip-link"
          href="#main-content"
          onClick={(event) => {
            event.preventDefault();
            document.getElementById('main-content')?.focus();
          }}
        >
          跳转到主要内容
        </a>
        <header className="app-header">
          <button
            className="menu-toggle"
            aria-label={sidebarOpen ? '关闭导航' : '打开导航'}
            aria-expanded={sidebarOpen}
            onClick={() => setSidebarOpen((value) => !value)}
          >
            {sidebarOpen ? <Close size={20} /> : <Menu size={20} />}
          </button>
          <a href="#/" className="brand-link">
            <Brand />
          </a>
          <span className="header-divider" />
          <span className="header-subtitle">Agent control plane</span>
          <div className="header-right">
            <span className="version-badge">v0.1</span>
            <span className="user-avatar" aria-hidden="true">
              {user.username.slice(0, 2).toUpperCase()}
            </span>
            <span className="username">{user.username}</span>
            <Button
              kind="ghost"
              size="sm"
              hasIconOnly
              iconDescription="退出登录"
              renderIcon={Logout}
              onClick={() => void logout()}
            />
          </div>
        </header>
        {sidebarOpen && (
          <button
            className="sidebar-scrim"
            aria-label="关闭导航"
            onClick={() => setSidebarOpen(false)}
          />
        )}
        <aside className={`app-sidebar ${sidebarOpen ? 'is-open' : ''}`}>
          <div className="workspace-selector">
            <span className="workspace-mark">CP</span>
            <div>
              <strong>CloudPilot</strong>
              <span>管理工作空间</span>
            </div>
            <span className="workspace-dot" />
          </div>
          <nav aria-label="主导航">
            {navigation.map((item, index) => (
              <div key={item.path}>
                {navigation[index - 1]?.group !== item.group && (
                  <div className="nav-group">{item.group}</div>
                )}
                <a
                  className={`nav-item ${(item.path === '/' ? route === '/' : route.startsWith(item.path)) ? 'active' : ''}`}
                  href={`#${item.path}`}
                  aria-current={
                    (item.path === '/' ? route === '/' : route.startsWith(item.path))
                      ? 'page'
                      : undefined
                  }
                >
                  <item.icon size={20} />
                  <span>{item.label}</span>
                </a>
              </div>
            ))}
          </nav>
          <div className="sidebar-footer">
            <span className="tiny-dot" />
            <div>
              <strong>声明式控制平面</strong>
              <span>cloudpilot.dev/v1alpha1</span>
            </div>
          </div>
        </aside>
        <main className="main-content" id="main-content" tabIndex={-1}>
          <div className="breadcrumb">
            <span>CloudPilot</span>
            <span>/</span>
            <strong>{current?.label || '详情'}</strong>
          </div>
          <ErrorNotice error={logoutError} />
          {content}
          <footer className="page-footer">
            <span>CLOUDPILOT</span>
            <span>将意图交给 Agent，让过程保持可见。</span>
          </footer>
        </main>
      </div>
    </Theme>
  );
}

function Brand() {
  return (
    <span className="brand">
      <Cloud size={25} />
      <span>
        cloud<span className="brand-weight">pilot</span>
        <i />
      </span>
    </span>
  );
}

function Login({ initialError, onLogin }: { initialError: string; onLogin: (user: User) => void }) {
  const [username, setUsername] = useState('');
  const [password, setPassword] = useState('');
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState(initialError);
  const login = async () => {
    setBusy(true);
    setError('');
    try {
      onLogin(await post<User>('/api/auth/login', { username, password }));
    } catch (cause) {
      setError(message(cause));
    } finally {
      setBusy(false);
    }
  };
  return (
    <div className="login-page">
      <section className="login-story">
        <Brand />
        <div className="login-story-body">
          <span className="eyebrow">THE AGENT CONTROL PLANE</span>
          <h1>
            从反馈，
            <br />
            到下一次交付<span>。</span>
          </h1>
          <p>
            连接你的工具、代码和智能体。
            <br />
            让每个任务有序开始，让每次运行清晰可见。
          </p>
          <div className="login-flow">
            <span>INPUT</span>
            <ArrowRight size={16} />
            <span>AGENT</span>
            <ArrowRight size={16} />
            <span>ARTIFACT</span>
          </div>
        </div>
        <span className="login-footnote">Built for work that moves forward.</span>
        <div className="login-orbit orbit-one" />
        <div className="login-orbit orbit-two" />
      </section>
      <section className="login-form-wrap">
        <div className="login-form">
          <span className="eyebrow">WELCOME TO CLOUDPILOT</span>
          <h2>登录控制台</h2>
          <p>使用初始配置中的管理员账户继续。</p>
          <ErrorNotice error={error} />
          <form
            onSubmit={(event) => {
              event.preventDefault();
              void login();
            }}
          >
            <TextInput
              id="login-username"
              labelText="用户名"
              placeholder="管理员用户名"
              autoComplete="username"
              value={username}
              required
              onChange={(event) => setUsername(event.target.value)}
            />
            <PasswordInput
              id="login-password"
              labelText="密码"
              autoComplete="current-password"
              value={password}
              required
              onChange={(event) => setPassword(event.target.value)}
            />
            <Button type="submit" renderIcon={ArrowRight} disabled={busy || !username || !password}>
              {busy ? '正在登录…' : '登录'}
            </Button>
          </form>
          <span className="login-note">账户由服务器初始 YAML 配置创建。</span>
        </div>
        <span className="login-version">CloudPilot · v0.1</span>
      </section>
    </div>
  );
}
