import { useContext, useEffect, useRef, useState } from 'react';
import {
  Button,
  InlineNotification,
  Modal,
  PasswordInput,
  Select,
  SelectItem,
  Tab,
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
  TabList,
  TabPanel,
  TabPanels,
  Tabs,
  Tag,
  TextInput,
  Toggle,
} from '@carbon/react';
import { Add, Download, Password, Save, TrashCan, Upload } from '@carbon/icons-react';
import { message, post, request } from './api';
import { Empty, ErrorNotice, LoadingState, PageHeading, formatTime } from './components';
import { useData, usePermissions, UserContext } from './hooks';
import { roleLabels, type Resource, type Role, type User } from './types';

const roles = Object.keys(roleLabels) as Role[];

function RoleSelect({
  id,
  value,
  onChange,
  disabled,
  labelText = '权限',
}: {
  id: string;
  value: string;
  onChange: (role: Role) => void;
  disabled?: boolean;
  labelText?: string;
}) {
  return (
    <Select
      id={id}
      labelText={labelText}
      size="sm"
      value={value}
      disabled={disabled}
      onChange={(event) => onChange(event.target.value as Role)}
    >
      {roles.map((role) => (
        <SelectItem key={role} value={role} text={roleLabels[role]} />
      ))}
    </Select>
  );
}

export default function ConfigCenter() {
  const { isAdmin } = usePermissions();
  return (
    <>
      <PageHeading eyebrow="CONFIGURATION CENTER" title="配置中心" />
      <div className="editor-panel">
        <Tabs>
          <TabList aria-label="配置中心">
            <Tab>资源导入导出</Tab>
            {isAdmin && <Tab>用户管理</Tab>}
            {isAdmin && <Tab>单点登录</Tab>}
          </TabList>
          <TabPanels>
            <TabPanel>
              <ResourceTransfer />
            </TabPanel>
            {isAdmin && (
              <TabPanel>
                <Users />
              </TabPanel>
            )}
            {isAdmin && (
              <TabPanel>
                <SSOSettings />
              </TabPanel>
            )}
          </TabPanels>
        </Tabs>
      </div>
    </>
  );
}

function ResourceTransfer() {
  const { canEdit } = usePermissions();
  const [error, setError] = useState('');
  const [result, setResult] = useState<{ file: string; docs: Resource[] } | null>(null);
  const [busy, setBusy] = useState(false);
  const file = useRef<HTMLInputElement>(null);
  const importFile = async (selected: File) => {
    setError('');
    setResult(null);
    if (selected.size > 1024 * 1024) {
      setError('YAML 文件不能超过 1 MiB。');
      return;
    }
    setBusy(true);
    try {
      const docs = await post<Resource[]>('/api/config/import', { yaml: await selected.text() });
      setResult({ file: selected.name, docs });
    } catch (cause) {
      setError(message(cause));
    } finally {
      setBusy(false);
    }
  };
  const exportYAML = async () => {
    setError('');
    try {
      const response = await fetch('/api/config/export', { credentials: 'same-origin' });
      if (!response.ok) throw new Error('导出失败，请检查登录状态并重试。');
      const url = URL.createObjectURL(await response.blob());
      const anchor = document.createElement('a');
      anchor.href = url;
      anchor.download = 'cloudpilot-resources.yaml';
      anchor.click();
      URL.revokeObjectURL(url);
    } catch (cause) {
      setError(message(cause));
    }
  };
  return (
    <div className="config-layout config-tab">
      <div className="config-panel">
        <div className="transfer-actions">
          <div>
            <h3>导出</h3>
            <p className="muted">下载全部资源的 YAML 清单，不包含任何密钥。</p>
            <Button kind="tertiary" renderIcon={Download} onClick={() => void exportYAML()}>
              导出所有资源
            </Button>
          </div>
          {canEdit && (
            <div>
              <h3>导入</h3>
              <p className="muted">上传 .yaml 文件，校验通过后整批应用；用 --- 分隔多个资源。</p>
              <Button renderIcon={Upload} disabled={busy} onClick={() => file.current?.click()}>
                {busy ? '正在应用…' : '上传 YAML 文件'}
              </Button>
              <input
                ref={file}
                type="file"
                className="visually-hidden"
                aria-label="选择 YAML 文件"
                accept=".yaml,.yml,text/yaml,application/yaml"
                onChange={(event) => {
                  const selected = event.target.files?.[0];
                  if (selected) void importFile(selected);
                  event.target.value = '';
                }}
              />
            </div>
          )}
        </div>
        <ErrorNotice error={error} />
        {result && (
          <InlineNotification
            kind="success"
            title={`${result.file}：已应用 ${result.docs.length} 个资源`}
            subtitle={result.docs.map((item) => `${item.kind}/${item.metadata.name}`).join(' · ')}
            hideCloseButton
            lowContrast
          />
        )}
      </div>
      <aside className="config-aside">
        <h3>配置提示</h3>
        <ol>
          <li>
            <strong>声明资源</strong>
            <p>使用 kind 指定类型，metadata.name 标识资源。</p>
          </li>
          <li>
            <strong>通过引用连接</strong>
            <p>流水线引用 Worker、Workspace 和 Runner。Input 指向 Pipeline。</p>
          </li>
          <li>
            <strong>原子应用</strong>
            <p>只要有一个资源校验失败，整批配置都不会写入。</p>
          </li>
        </ol>
        <div className="security-note">
          <strong>接入点凭据与 API 密钥加密保存</strong>
          <p>导出不包含密钥，向新环境导入后需在资源编辑器中重新填写。</p>
        </div>
      </aside>
    </div>
  );
}

function Users() {
  const me = useContext(UserContext);
  const { data, error, loading, refresh } = useData<User[]>('/api/users');
  const [actionError, setActionError] = useState('');
  const [creating, setCreating] = useState(false);
  const [draft, setDraft] = useState({ username: '', password: '', role: 'viewer' as Role });
  const [resetting, setResetting] = useState<User | null>(null);
  const [password, setPassword] = useState('');
  const [deleting, setDeleting] = useState<User | null>(null);
  const [busy, setBusy] = useState(false);
  const act = async (callback: () => Promise<unknown>) => {
    setBusy(true);
    setActionError('');
    try {
      await callback();
      refresh();
      return true;
    } catch (cause) {
      setActionError(message(cause));
      return false;
    } finally {
      setBusy(false);
    }
  };
  const patch = (user: User, body: unknown) =>
    request(`/api/users/${user.id}`, { method: 'PATCH', body: JSON.stringify(body) });
  return (
    <div className="config-tab">
      <div className="table-toolbar">
        <span className="table-count">{data?.length ?? 0} 个用户</span>
        <Button size="sm" renderIcon={Add} onClick={() => setCreating(true)}>
          添加用户
        </Button>
      </div>
      <ErrorNotice error={error || (!creating && !resetting && !deleting ? actionError : '')} />
      {loading && !data ? (
        <LoadingState />
      ) : !data?.length ? (
        <Empty title="暂无用户" />
      ) : (
        <div className="table-scroll">
          <Table size="lg" aria-label="用户列表">
            <TableHead>
              <TableRow>
                {['用户名', '来源', '权限', '创建时间 · 上海', '操作'].map((label) => (
                  <TableHeader key={label}>{label}</TableHeader>
                ))}
              </TableRow>
            </TableHead>
            <TableBody>
              {data.map((user) => (
                <TableRow key={user.id}>
                  <TableCell>
                    <span className="mono">{user.username}</span>
                    {user.id === me?.id && <span className="cell-subtitle">当前账户</span>}
                  </TableCell>
                  <TableCell>
                    <Tag size="sm" type={user.sso ? 'teal' : 'gray'}>
                      {user.sso ? 'SSO' : '本地账户'}
                    </Tag>
                  </TableCell>
                  <TableCell>
                    <RoleSelect
                      id={`user-role-${user.id}`}
                      labelText={`${user.username} 的权限`}
                      value={user.role}
                      disabled={busy || user.id === me?.id}
                      onChange={(role) => void act(() => patch(user, { role }))}
                    />
                  </TableCell>
                  <TableCell>{formatTime(user.createdAt)}</TableCell>
                  <TableCell>
                    <div className="row-actions">
                      {!user.sso && (
                        <Button
                          kind="ghost"
                          size="sm"
                          hasIconOnly
                          iconDescription="重置密码"
                          renderIcon={Password}
                          disabled={busy}
                          onClick={() => {
                            setPassword('');
                            setActionError('');
                            setResetting(user);
                          }}
                        />
                      )}
                      <Button
                        kind="danger--ghost"
                        size="sm"
                        hasIconOnly
                        iconDescription="删除用户"
                        renderIcon={TrashCan}
                        disabled={busy || user.id === me?.id}
                        onClick={() => {
                          setActionError('');
                          setDeleting(user);
                        }}
                      />
                    </div>
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        </div>
      )}
      <ul className="role-help">
        <li>管理员：管理用户和单点登录，编辑所有资源并触发运行。</li>
        <li>编辑者：编辑资源、触发运行、向 Agent 发送消息。</li>
        <li>查看者：只能查看资源和运行进度。</li>
      </ul>
      <Modal
        open={creating}
        modalHeading="添加用户"
        primaryButtonText="创建"
        secondaryButtonText="取消"
        primaryButtonDisabled={busy || !draft.username.trim() || draft.password.length < 12}
        onRequestClose={() => setCreating(false)}
        onRequestSubmit={() =>
          void act(() => post('/api/users', draft)).then((ok) => {
            if (ok) {
              setCreating(false);
              setDraft({ username: '', password: '', role: 'viewer' });
            }
          })
        }
      >
        <div className="modal-form">
          <TextInput
            id="new-user-name"
            labelText="用户名"
            autoComplete="off"
            value={draft.username}
            onChange={(event) => setDraft({ ...draft, username: event.target.value })}
          />
          <PasswordInput
            id="new-user-password"
            labelText="初始密码"
            helperText="12–72 字节。"
            autoComplete="new-password"
            value={draft.password}
            onChange={(event) => setDraft({ ...draft, password: event.target.value })}
          />
          <RoleSelect
            id="new-user-role"
            value={draft.role}
            onChange={(role) => setDraft({ ...draft, role })}
          />
          <ErrorNotice error={actionError} />
        </div>
      </Modal>
      <Modal
        open={Boolean(resetting)}
        modalHeading={`重置 ${resetting?.username} 的密码`}
        primaryButtonText="重置"
        secondaryButtonText="取消"
        primaryButtonDisabled={busy || password.length < 12}
        onRequestClose={() => setResetting(null)}
        onRequestSubmit={() =>
          resetting &&
          void act(() => patch(resetting, { password })).then((ok) => ok && setResetting(null))
        }
      >
        <div className="modal-form">
          <PasswordInput
            id="reset-password"
            labelText="新密码"
            helperText="12–72 字节。重置后该用户的所有登录会话失效。"
            autoComplete="new-password"
            value={password}
            onChange={(event) => setPassword(event.target.value)}
          />
          <ErrorNotice error={actionError} />
        </div>
      </Modal>
      <Modal
        open={Boolean(deleting)}
        danger
        modalHeading="删除用户"
        primaryButtonText="删除"
        secondaryButtonText="取消"
        primaryButtonDisabled={busy}
        onRequestClose={() => setDeleting(null)}
        onRequestSubmit={() =>
          deleting &&
          void act(() => request(`/api/users/${deleting.id}`, { method: 'DELETE' })).then(
            (ok) => ok && setDeleting(null),
          )
        }
      >
        <p>
          确定删除 <strong>{deleting?.username}</strong>？该用户会立即退出登录。
          {deleting?.sso && ' SSO 用户再次登录时会以默认权限重新创建。'}
        </p>
        <ErrorNotice error={actionError} />
      </Modal>
    </div>
  );
}

interface OIDCSettings {
  enabled: boolean;
  discoveryUrl: string;
  clientId: string;
  clientSecret?: string;
  hasClientSecret: boolean;
  scopes: string;
  defaultRole: Role;
  redirectUrl: string;
}

function SSOSettings() {
  const { data, error, loading } = useData<OIDCSettings>('/api/settings/oidc');
  const [form, setForm] = useState<OIDCSettings | null>(null);
  const [saveError, setSaveError] = useState('');
  const [saved, setSaved] = useState(false);
  const [busy, setBusy] = useState(false);
  useEffect(() => {
    if (data) setForm({ ...data, clientSecret: '' });
  }, [data]);
  if (loading && !form) return <LoadingState />;
  if (!form) return <ErrorNotice error={error} />;
  const set = (patch: Partial<OIDCSettings>) => {
    setSaved(false);
    setForm({ ...form, ...patch });
  };
  const save = async () => {
    setBusy(true);
    setSaveError('');
    try {
      const next = await request<OIDCSettings>('/api/settings/oidc', {
        method: 'PUT',
        body: JSON.stringify(form),
      });
      setForm({ ...next, clientSecret: '' });
      setSaved(true);
    } catch (cause) {
      setSaveError(message(cause));
    } finally {
      setBusy(false);
    }
  };
  return (
    <div className="config-tab sso-form">
      <Toggle
        id="oidc-enabled"
        labelText="启用 OIDC 单点登录"
        labelA="关"
        labelB="开"
        toggled={form.enabled}
        onToggle={(enabled) => set({ enabled })}
      />
      <TextInput
        id="oidc-discovery"
        labelText="Discovery URL"
        helperText="Issuer 地址或完整的 /.well-known/openid-configuration 地址。"
        placeholder="https://idp.example.com/.well-known/openid-configuration"
        value={form.discoveryUrl}
        onChange={(event) => set({ discoveryUrl: event.target.value })}
      />
      <div className="form-grid">
        <TextInput
          id="oidc-client-id"
          labelText="Client ID"
          value={form.clientId}
          onChange={(event) => set({ clientId: event.target.value })}
        />
        <PasswordInput
          id="oidc-client-secret"
          labelText="Client secret"
          autoComplete="new-password"
          helperText={form.hasClientSecret ? '留空保留已保存的密钥。' : '加密保存。'}
          value={form.clientSecret || ''}
          onChange={(event) => set({ clientSecret: event.target.value })}
        />
        <TextInput
          id="oidc-scopes"
          labelText="Scope"
          helperText="空格分隔；openid 会自动加入。"
          value={form.scopes}
          onChange={(event) => set({ scopes: event.target.value })}
        />
        <RoleSelect
          id="oidc-default-role"
          labelText="SSO 用户默认权限"
          value={form.defaultRole}
          onChange={(defaultRole) => set({ defaultRole })}
        />
      </div>
      <div className="form-context">
        <span>回调地址</span>
        <code>{form.redirectUrl}</code>
      </div>
      <p className="muted">
        在身份提供方注册上面的回调地址。默认权限只在 SSO 用户首次登录时生效，之后可在用户管理中调整。
      </p>
      <ErrorNotice error={saveError} />
      {saved && (
        <InlineNotification
          kind="success"
          title="单点登录配置已保存"
          hideCloseButton
          lowContrast
        />
      )}
      <div className="config-footer">
        <Button renderIcon={Save} disabled={busy} onClick={() => void save()}>
          {busy ? '正在校验…' : '校验并保存'}
        </Button>
      </div>
    </div>
  );
}
