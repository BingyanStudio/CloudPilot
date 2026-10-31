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
import { kindLabels, type Kind, type Resource, type Spec } from './types';

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
  if (spec.paused !== undefined && typeof spec.paused !== 'boolean')
    throw new Error('spec.paused 必须是布尔值。');
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
  const set = (key: string, value: unknown) =>
    setDoc((current) => ({ ...current, spec: { ...current.spec, [key]: value } }));
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
  const number = (key: string, label: string, min: number, max: number) => (
    <NumberInput
      key={key}
      id={`resource-${key}`}
      label={label}
      value={Number(spec[key] ?? min)}
      min={min}
      max={max}
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
  const repositories = (spec.repositories || []) as { url: string; ref: string; path: string }[];
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
                        metadata: { ...current.metadata, name: event.target.value },
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
                        {field('type', '接入类型', '首版支持 webhook。')}
                        {reference('pipelineRef', 'Pipeline')}
                        {field(
                          'secretRef',
                          'Webhook 密钥环境变量',
                          '填写服务器环境变量名称，不要填写密钥原文。',
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
                        {number('maxRetries', '失败重试次数', 0, 10)}
                        {number('timeoutSeconds', '单次运行超时（秒）', 10, 86400)}
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
                        {field('type', '设施类型', '首版支持 docker。')}
                        {field('image', 'Docker 镜像')}
                        {field('network', 'Docker 网络')}
                        {field('memory', '内存限制', '例如 2g、4096m。')}
                        {field('cpus', 'CPU 限制', '例如 2、0.5。')}
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
