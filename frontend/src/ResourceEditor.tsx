import { useState } from 'react';
import {
  Button,
  TextInput,
  TextArea,
  Select,
  SelectItem,
  NumberInput,
  Toggle,
  Tabs,
  TabList,
  Tab,
  TabPanels,
  TabPanel,
  InlineNotification,
} from '@carbon/react';
import { ArrowLeft, Save, Add, TrashCan } from '@carbon/icons-react';
import { parseDocument, stringify } from 'yaml';
import { message, request, resourcePath } from './api';
import { ErrorNotice, PageHeading } from './components';
import { navigate } from './hooks';
import { kindLabels, runnerSpec, type GitHubInput, type Kind, type Resource, type Spec } from './types';

function assertFormShape(value: Resource) {
  const spec = value.spec;
  const object = (candidate: unknown): candidate is Record<string, unknown> =>
    Boolean(candidate) && typeof candidate === 'object' && !Array.isArray(candidate);
  if (!object(spec) || typeof value.metadata.name !== 'string')
    throw new Error('metadata.name 必须是字符串，spec 必须是对象。');
  const stringKeys = [
    'type',
    'pipelineRef',
    'secretRef',
    'workerRef',
    'workspaceRef',
    'runnerRef',
    'schedule',
    'timezone',
    'model',
    'endpoint',
    'apiKeySecretRef',
    'instructions',
    'prompt',
    'image',
    'network',
    'memory',
    'cpus',
  ];
  for (const key of stringKeys)
    if (spec[key] !== undefined && typeof spec[key] !== 'string')
      throw new Error(`spec.${key} 必须是字符串。`);
  for (const key of ['concurrency', 'maxRetries', 'timeoutSeconds'])
    if (spec[key] !== undefined && typeof spec[key] !== 'number')
      throw new Error(`spec.${key} 必须是数字。`);
  if (spec.e2b !== undefined) {
    if (!object(spec.e2b)) throw new Error('spec.e2b 必须是对象。');
    for (const key of ['provider', 'domain', 'apiUrl', 'apiKeySecretRef', 'template', 'idleStrategy'])
      if (spec.e2b[key] !== undefined && typeof spec.e2b[key] !== 'string')
        throw new Error(`spec.e2b.${key} 必须是字符串。`);
    if (spec.e2b.leaseSeconds !== undefined && typeof spec.e2b.leaseSeconds !== 'number')
      throw new Error('spec.e2b.leaseSeconds 必须是数字。');
  }
  if (spec.paused !== undefined && typeof spec.paused !== 'boolean')
    throw new Error('spec.paused 必须是布尔值。');
  if (spec.github !== undefined) {
    if (!object(spec.github)) throw new Error('spec.github 必须是 GitHub 配置对象。');
    for (const key of ['repository', 'tokenSecretRef'])
      if (spec.github[key] !== undefined && typeof spec.github[key] !== 'string')
        throw new Error(`spec.github.${key} 必须是字符串。`);
    for (const key of ['assignees', 'labels']) {
      const items = spec.github[key];
      if (
        items !== undefined &&
        (!Array.isArray(items) || items.some((item) => typeof item !== 'string'))
      )
        throw new Error(`spec.github.${key} 必须是字符串数组。`);
    }
  }
  if (
    spec.tools !== undefined &&
    (!Array.isArray(spec.tools) || spec.tools.some((item) => typeof item !== 'string'))
  )
    throw new Error('spec.tools 必须是字符串数组。');
  if (
    spec.repositories !== undefined &&
    (!Array.isArray(spec.repositories) ||
      spec.repositories.some(
        (item) =>
          !object(item) ||
          ['url', 'ref', 'path'].some(
            (key) => item[key] !== undefined && typeof item[key] !== 'string',
          ),
      ))
  )
    throw new Error('spec.repositories 必须是包含 url、ref、path 的对象数组。');
  for (const key of ['env', 'secretEnv'])
    if (
      spec[key] !== undefined &&
      (!object(spec[key]) || Object.values(spec[key]).some((item) => typeof item !== 'string'))
    )
      throw new Error(`spec.${key} 必须是字符串键值对象。`);
  if (
    spec.mcpServers !== undefined &&
    (!object(spec.mcpServers) || Object.values(spec.mcpServers).some((item) => !object(item)))
  )
    throw new Error('spec.mcpServers 必须是 MCP 配置对象。');
}

function jsonObject(raw: string): Record<string, unknown> {
  const value: unknown = JSON.parse(raw);
  if (!value || typeof value !== 'object' || Array.isArray(value))
    throw new Error('请输入 JSON 对象。');
  return value as Record<string, unknown>;
}

export default function ResourceEditor({
  initial,
  existing,
  resources,
  onSaved,
}: {
  initial: Resource;
  existing: boolean;
  resources: Resource[];
  onSaved: () => void;
}) {
  const [doc, setDoc] = useState<Resource>(structuredClone(initial));
  const [yaml, setYAML] = useState(stringify(initial));
  const [tab, setTab] = useState(0);
  const [error, setError] = useState('');
  const [busy, setBusy] = useState(false);
  const [jsonFields, setJSONFields] = useState<Record<string, string>>(() =>
    Object.fromEntries(
      ['mcpServers', 'env', 'secretEnv'].map((key) => [
        key,
        JSON.stringify(initial.spec[key] || {}, null, 2),
      ]),
    ),
  );
  const spec = doc.spec;
  const e2b = (spec.e2b ?? {}) as Spec;
  const github = (spec.github || {}) as Partial<GitHubInput>;
  const setGitHub = (key: keyof GitHubInput, value: string | string[]) =>
    setDoc((current) => ({
      ...current,
      spec: {
        ...current.spec,
        github: { ...(current.spec.github as Partial<GitHubInput>), [key]: value },
      },
    }));
  const setInputType = (type: string) =>
    setDoc((current) => {
      const next: Spec = { ...current.spec, type };
      if (type === 'github_issue')
        next.github = { repository: '', tokenSecretRef: 'GITHUB_TOKEN', assignees: [], labels: [] };
      else delete next.github;
      return { ...current, spec: next };
    });
  const set = (key: string, value: unknown) =>
    setDoc((current) => ({
      ...current,
      spec: { ...current.spec, [key]: value },
    }));
  const text = (key: string) => String(spec[key] ?? '');
  const field = (key: string, label: string, helper?: string, placeholder?: string) => (
    <TextInput
      key={key}
      id={`resource-${key}`}
      labelText={label}
      value={text(key)}
      onChange={(event) => set(key, event.target.value)}
      helperText={helper}
      placeholder={placeholder}
    />
  );
  const setE2B = (key: string, value: unknown) =>
    setDoc((current) => ({
      ...current,
      spec: { ...current.spec, e2b: { ...((current.spec.e2b ?? {}) as Spec), [key]: value } },
    }));
  const e2bField = (key: string, label: string, helper?: string) => (
    <TextInput
      key={key}
      id={`resource-e2b-${key}`}
      labelText={label}
      value={String(e2b[key] ?? '')}
      onChange={(event) => setE2B(key, event.target.value)}
      helperText={helper}
    />
  );
  const textarea = (key: string, label: string, helper?: string) => (
    <TextArea
      key={key}
      id={`resource-${key}`}
      labelText={label}
      value={text(key)}
      rows={5}
      onChange={(event) => set(key, event.target.value)}
      helperText={helper}
    />
  );
  const reference = (key: string, kind: Kind) => (
    <Select
      key={key}
      id={`resource-${key}`}
      labelText={`${kindLabels[kind]} · ${kind}`}
      value={text(key)}
      onChange={(event) => set(key, event.target.value)}
    >
      <SelectItem value="" text={`选择${kindLabels[kind]}`} />
      {resources
        .filter((item) => item.kind === kind)
        .map((item) => (
          <SelectItem
            key={item.metadata.name}
            value={item.metadata.name}
            text={item.metadata.name}
          />
        ))}
    </Select>
  );
  const number = (key: string, label: string, min: number, max: number, helper?: string) => (
    <NumberInput
      key={key}
      id={`resource-${key}`}
      label={label}
      value={Number(spec[key] ?? min)}
      min={min}
      max={max}
      helperText={helper}
      onChange={(_, data) => set(key, Number(data.value))}
    />
  );
  const paused = (
    <Toggle
      id="resource-paused"
      labelText="接收与调度"
      labelA="已启用"
      labelB="已暂停"
      toggled={Boolean(spec.paused)}
      onToggle={(value) => set('paused', value)}
    />
  );
  const objectField = (key: string, label: string, helper: string) => (
    <TextArea
      key={key}
      className="code-input"
      id={`resource-${key}`}
      labelText={label}
      value={jsonFields[key]}
      rows={5}
      onChange={(event) => setJSONFields((current) => ({ ...current, [key]: event.target.value }))}
      helperText={helper}
    />
  );
  const fromForm = (): Resource => {
    if (doc.kind === 'Input' && spec.type === 'github_issue') {
      const normalized = {
        ...github,
        assignees: (github.assignees || []).map((value) => value.trim()).filter(Boolean),
        labels: (github.labels || []).map((value) => value.trim()).filter(Boolean),
      };
      return { ...doc, spec: { ...spec, github: normalized } };
    }
    if (doc.kind !== 'Workspace') return doc;
    const next: Spec = { ...doc.spec };
    for (const key of ['mcpServers', 'env', 'secretEnv']) next[key] = jsonObject(jsonFields[key]);
    return { ...doc, spec: next };
  };
  const parseYAML = (): Resource => {
    const parsed = parseDocument(yaml, { uniqueKeys: true });
    if (parsed.errors.length) throw new Error(parsed.errors[0].message);
    const value = parsed.toJS() as Resource;
    if (
      !value ||
      typeof value !== 'object' ||
      !value.metadata ||
      !value.spec ||
      Array.isArray(value.spec)
    )
      throw new Error('YAML 需要包含 apiVersion、kind、metadata 和 spec。');
    if (value.kind !== initial.kind) throw new Error('资源类型不可在编辑器中修改。');
    if (existing && value.metadata.name !== initial.metadata.name)
      throw new Error('资源名称不可修改；请创建新资源。');
    assertFormShape(value);
    return value;
  };
  const switchTab = (selected: number) => {
    if (selected === tab) return;
    try {
      if (selected === 1) setYAML(stringify(fromForm()));
      else {
        const next = parseYAML();
        setDoc(next);
        setJSONFields(
          Object.fromEntries(
            ['mcpServers', 'env', 'secretEnv'].map((key) => [
              key,
              JSON.stringify(next.spec[key] || {}, null, 2),
            ]),
          ),
        );
      }
      setError('');
      setTab(selected);
    } catch (cause) {
      setError(message(cause));
    }
  };
  const save = async () => {
    setBusy(true);
    setError('');
    try {
      const value = tab === 1 ? parseYAML() : fromForm();
      await request<Resource>(
        existing ? resourcePath(initial.kind, initial.metadata.name) : '/api/resources',
        { method: existing ? 'PUT' : 'POST', body: JSON.stringify(value) },
      );
      onSaved();
      navigate(`/resources/${initial.kind}`);
    } catch (cause) {
      setError(message(cause));
    } finally {
      setBusy(false);
    }
  };
  const repositories = (spec.repositories || []) as {
    url: string;
    ref: string;
    path: string;
  }[];
  const repoField = (
    index: number,
    key: 'url' | 'ref' | 'path',
    label: string,
    placeholder: string,
  ) => (
    <TextInput
      id={`repo-${index}-${key}`}
      labelText={label}
      value={repositories[index][key] || ''}
      placeholder={placeholder}
      onChange={(event) =>
        set(
          'repositories',
          repositories.map((repo, i) =>
            i === index ? { ...repo, [key]: event.target.value } : repo,
          ),
        )
      }
    />
  );
  return (
    <>
      <Button
        kind="ghost"
        size="sm"
        renderIcon={ArrowLeft}
        onClick={() => navigate(`/resources/${initial.kind}`)}
      >
        返回{kindLabels[initial.kind]}
      </Button>
      <PageHeading
        eyebrow={`CONFIGURATION / ${initial.kind.toUpperCase()}`}
        title={existing ? `编辑 ${initial.metadata.name}` : `创建${kindLabels[initial.kind]}`}
        description="表单与 YAML 使用同一份声明式资源配置。保存前将校验字段和资源引用。"
      />
      <ErrorNotice error={error} />
      <div className="editor-panel">
        <Tabs selectedIndex={tab} onChange={({ selectedIndex }) => switchTab(selectedIndex)}>
          <TabList aria-label="资源编辑方式">
            <Tab>表单配置</Tab>
            <Tab>YAML</Tab>
          </TabList>
          <TabPanels>
            <TabPanel>
              <div className="resource-form">
                <div className="form-section">
                  <h3>资源标识</h3>
                  <TextInput
                    id="resource-name"
                    labelText="名称"
                    helperText="使用小写字母、数字和连字符，例如 feedback-nightly。"
                    value={doc.metadata.name}
                    disabled={existing}
                    onChange={(event) =>
                      setDoc((current) => ({
                        ...current,
                        metadata: {
                          ...current.metadata,
                          name: event.target.value,
                        },
                      }))
                    }
                  />
                  <div className="form-context">
                    <span>API 版本</span>
                    <code>{doc.apiVersion}</code>
                    <span>资源类型</span>
                    <code>{doc.kind}</code>
                  </div>
                </div>
                <div className="form-section">
                  <h3>{kindLabels[doc.kind]}配置</h3>
                  <div className="form-grid">
                    {doc.kind === 'Input' && (
                      <>
                        <Select
                          id="resource-type"
                          labelText="接入类型"
                          value={text('type') || 'webhook'}
                          onChange={(event) => setInputType(event.target.value)}
                        >
                          <SelectItem value="webhook" text="通用 Webhook" />
                          <SelectItem value="github_issue" text="GitHub issue" />
                        </Select>
                        {reference('pipelineRef', 'Pipeline')}
                        {field(
                          'secretRef',
                          'Webhook 密钥环境变量',
                          spec.type === 'github_issue'
                            ? '填写保存 GitHub Webhook Secret 的服务器环境变量名称，用于校验签名。'
                            : '填写服务器环境变量名称，不要填写密钥原文。',
                        )}
                        {spec.type === 'github_issue' && (
                          <>
                            <TextInput
                              id="github-repository"
                              labelText="GitHub 仓库"
                              placeholder="owner/repo"
                              value={github.repository || ''}
                              onChange={(event) => setGitHub('repository', event.target.value)}
                              helperText="只接收此仓库的 issue。"
                            />
                            <TextInput
                              id="github-token-secret"
                              labelText="GitHub Token 环境变量"
                              value={github.tokenSecretRef || ''}
                              onChange={(event) => setGitHub('tokenSecretRef', event.target.value)}
                              helperText="填写服务器环境变量名称；Token 需要该仓库 Issues 读写权限，以回复任务链接。"
                            />
                            <TextArea
                              id="github-assignees"
                              labelText="触发条件 · Assignee"
                              rows={3}
                              value={(github.assignees || []).join('\n')}
                              onChange={(event) => setGitHub('assignees', event.target.value.split('\n'))}
                              helperText="每行一个 GitHub 用户名；填写 * 表示任意已指派的 issue。"
                            />
                            <TextArea
                              id="github-labels"
                              labelText="触发条件 · Label"
                              rows={3}
                              value={(github.labels || []).join('\n')}
                              onChange={(event) => setGitHub('labels', event.target.value.split('\n'))}
                              helperText="每行一个标签。Assignee 或 Label 至少填写一项，满足任意一个条件即可触发。"
                            />
                            <div className="full-width help-note">
                              <strong>连接 GitHub Webhook</strong>
                              <p>
                                在仓库 Webhook 中填写下方地址，选择 application/json 和 Issues 事件，
                                并设置与服务器环境变量一致的 Secret。服务器需配置公开访问地址 server.public_url。
                              </p>
                              <code>
                                {`${window.location.origin}/hooks/${doc.metadata.name || '<接入点名称>'}`}
                              </code>
                            </div>
                          </>
                        )}
                        {paused}
                      </>
                    )}
                    {doc.kind === 'Pipeline' && (
                      <>
                        {reference('workerRef', 'Worker')}
                        {reference('workspaceRef', 'Workspace')}
                        {reference('runnerRef', 'Runner')}
                        {field(
                          'schedule',
                          'Cron 调度表达式',
                          '留空表示立即处理；0 20 * * * 表示每天 20:00。',
                        )}
                        {field('timezone', '时区', 'IANA 时区，例如 Asia/Shanghai。')}
                        {number('concurrency', '最大并发', 1, 32)}
                        {number(
                          'maxRetries',
                          '自动重试次数',
                          0,
                          10,
                          '适用于批处理及交互会话建立前的启动失败。会话建立后的失败需要人工继续。',
                        )}
                        {number(
                          'timeoutSeconds',
                          '每轮执行超时（秒）',
                          10,
                          86400,
                          '每轮继续执行独立计时；等待人工输入不占用运行并发。',
                        )}
                        {paused}
                      </>
                    )}
                    {doc.kind === 'Worker' && (
                      <>
                        {field('type', '智能体类型', '首版支持 codex。')}
                        {field('model', '模型', '使用你的 API 服务支持的模型 ID。')}
                        {field('endpoint', 'API 端点', '兼容 OpenAI Responses API 的服务地址。')}
                        {field(
                          'apiKeySecretRef',
                          'API 密钥环境变量',
                          '密钥保留在服务器环境变量中，配置仅存储引用。',
                        )}
                        <div className="full-width">{textarea('instructions', '智能体指令')}</div>
                      </>
                    )}
                    {doc.kind === 'Runner' && (
                      <>
                        <Select
                          id="resource-runner-type"
                          labelText="设施类型"
                          value={text('type') || 'docker'}
                          onChange={(event) =>
                            setDoc((current) => ({ ...current, spec: runnerSpec(event.target.value) }))
                          }
                        >
                          <SelectItem value="docker" text="Docker" />
                          <SelectItem value="e2b" text="E2B 兼容沙箱" />
                        </Select>
                        {spec.type === 'e2b' ? (
                          <>
                            <Select
                              id="resource-e2b-provider"
                              labelText="沙箱服务商"
                              value={String(e2b.provider || 'e2b')}
                              onChange={(event) => {
                                const provider = event.target.value;
                                const domain =
                                  provider === 'novita' ? 'us-phx-1.sandbox.novita.ai' : 'e2b.app';
                                const apiKeySecretRef =
                                  provider === 'novita' ? 'NOVITA_API_KEY' : 'E2B_API_KEY';
                                const defaultDomain =
                                  !e2b.domain ||
                                  ['e2b.app', 'us-phx-1.sandbox.novita.ai'].includes(String(e2b.domain));
                                const defaultKey =
                                  !e2b.apiKeySecretRef ||
                                  ['E2B_API_KEY', 'NOVITA_API_KEY'].includes(String(e2b.apiKeySecretRef));
                                set('e2b', {
                                  ...e2b,
                                  provider,
                                  domain: defaultDomain ? domain : e2b.domain,
                                  apiKeySecretRef: defaultKey ? apiKeySecretRef : e2b.apiKeySecretRef,
                                });
                              }}
                            >
                              <SelectItem value="e2b" text="E2B / 兼容服务" />
                              <SelectItem value="novita" text="Novita" />
                            </Select>
                            {e2bField('domain', '沙箱域名', '仅填写域名，不含协议、端口或路径。')}
                            {e2bField('apiUrl', '管理 API 地址', '可选；留空使用服务商默认地址。')}
                            {e2bField('apiKeySecretRef', 'API 密钥环境变量', '密钥保留在服务器环境变量中，配置仅存储引用。')}
                            {e2bField('template', '沙箱模板', '必填。模板需预装 Node.js 和 Codex；CPU、内存由模板配置。')}
                            <NumberInput
                              id="resource-e2b-lease"
                              label="沙箱租期（秒）"
                              value={Number(e2b.leaseSeconds ?? 900)}
                              min={60}
                              max={86400}
                              onChange={(_, data) => setE2B('leaseSeconds', Number(data.value))}
                              helperText="运行期间续期；上限取决于服务商账户，独立于流水线执行超时。"
                            />
                            <Select
                              id="resource-e2b-idle"
                              labelText="空闲策略"
                              value={String(e2b.idleStrategy || 'pause')}
                              onChange={(event) => setE2B('idleStrategy', event.target.value)}
                              helperText="轮次结束后暂停沙箱，继续时恢复工作区。"
                            >
                              <SelectItem value="pause" text="暂停并保留工作区" />
                            </Select>
                          </>
                        ) : (
                          <>
                            {field('image', 'Docker 镜像')}
                            {field('network', 'Docker 网络')}
                            {field('memory', '内存限制', '例如 2g、4096m。')}
                            {field('cpus', 'CPU 限制', '例如 2、0.5。')}
                          </>
                        )}
                      </>
                    )}
                    {doc.kind === 'Workspace' && (
                      <>
                        <div className="full-width">
                          {textarea(
                            'prompt',
                            '工作空间提示词',
                            '为所有任务提供项目背景、工作规范和产物要求。',
                          )}
                        </div>
                        <div className="full-width repository-list">
                          <div className="section-line">
                            <h4>代码仓库</h4>
                            <Button
                              kind="ghost"
                              size="sm"
                              renderIcon={Add}
                              onClick={() =>
                                set('repositories', [
                                  ...repositories,
                                  { url: '', ref: '', path: '' },
                                ])
                              }
                            >
                              添加仓库
                            </Button>
                          </div>
                          {repositories.length === 0 && (
                            <p className="muted">尚未添加仓库。也可以使用空工作空间。</p>
                          )}
                          {repositories.map((_, index) => (
                            <div className="repository-row" key={index}>
                              {repoField(
                                index,
                                'url',
                                '仓库 URL',
                                'https://github.com/org/repo.git',
                              )}
                              {repoField(index, 'ref', '分支 / 标签', 'main')}
                              {repoField(index, 'path', '检出路径', 'app')}
                              <Button
                                kind="danger--ghost"
                                size="sm"
                                hasIconOnly
                                iconDescription={`移除仓库 ${index + 1}`}
                                renderIcon={TrashCan}
                                onClick={() =>
                                  set(
                                    'repositories',
                                    repositories.filter((_, i) => i !== index),
                                  )
                                }
                              />
                            </div>
                          ))}
                        </div>
                        <div className="full-width">
                          <TextArea
                            id="resource-tools"
                            labelText="环境准备命令"
                            rows={4}
                            helperText="每行一条 shell 命令，在 Codex 启动前执行。"
                            value={((spec.tools || []) as string[]).join('\n')}
                            onChange={(event) => set('tools', event.target.value.split('\n'))}
                          />
                        </div>
                        <div className="full-width">
                          {objectField(
                            'mcpServers',
                            'MCP 服务 · JSON',
                            '按名称配置 command / args / env 或 url。',
                          )}
                        </div>
                        {objectField(
                          'env',
                          '环境变量 · JSON',
                          '普通环境变量，例如 {"CI":"true"}。',
                        )}
                        {objectField(
                          'secretEnv',
                          '密钥环境变量引用 · JSON',
                          '容器变量名到服务器密钥变量名，例如 {"GH_TOKEN":"GITHUB_TOKEN"}。',
                        )}
                      </>
                    )}
                  </div>
                </div>
              </div>
            </TabPanel>
            <TabPanel>
              <div className="yaml-editor">
                <InlineNotification
                  kind="info"
                  title="声明式配置"
                  subtitle="支持编辑当前资源的全部字段。多个资源请使用配置中心的批量导入。"
                  hideCloseButton
                  lowContrast
                />
                <TextArea
                  id="resource-yaml"
                  className="code-input"
                  labelText="资源 YAML"
                  rows={25}
                  value={yaml}
                  onChange={(event) => setYAML(event.target.value)}
                  spellCheck={false}
                />
              </div>
            </TabPanel>
          </TabPanels>
        </Tabs>
        <div className="editor-footer">
          <span className="muted">
            {existing ? `当前版本 ${initial.metadata.generation || 1}` : '新资源将在保存后生效'}
          </span>
          <div>
            <Button
              kind="secondary"
              disabled={busy}
              onClick={() => navigate(`/resources/${initial.kind}`)}
            >
              取消
            </Button>
            <Button renderIcon={Save} disabled={busy} onClick={() => void save()}>
              {busy ? '正在保存…' : '保存配置'}
            </Button>
          </div>
        </div>
      </div>
    </>
  );
}
