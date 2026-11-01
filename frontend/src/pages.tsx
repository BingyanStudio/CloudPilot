import { useEffect, useRef, useState } from 'react';
import {
  Button,
  Search,
  Select,
  SelectItem,
  Table,
  TableHead,
  TableHeader,
  TableRow,
  TableBody,
  TableCell,
  Modal,
  TextArea,
  InlineNotification,
  Tag,
  Toggle,
} from '@carbon/react';
import {
  Add,
  ArrowRight,
  Play,
  Pause,
  Edit,
  TrashCan,
  Download,
  Upload,
  Launch,
  Copy,
  Stop,
  Renew,
  Checkmark,
} from '@carbon/icons-react';
import { message, post, request, resourcePath } from './api';
import {
  Empty,
  ErrorNotice,
  LoadingState,
  MetadataItem,
  PageHeading,
  RefreshButton,
  RunTable,
  Status,
  duration,
  formatTime,
  safeURL,
} from './components';
import { navigate, useData } from './hooks';
import {
  kinds,
  kindLabels,
  template,
  type Artifact,
  type Event,
  type EventDetail,
  type GitHubInput,
  type Kind,
  type Overview,
  type Resource,
  type Run,
  type RunDetail,
  type RunLog,
} from './types';
import ResourceEditor from './ResourceEditor';
import RunConversation from './RunConversation';

export function Dashboard() {
  const overview = useData<Overview>('/api/overview', 10000);
  const runs = useData<Run[]>('/api/runs', 10000);
  const resources = useData<Resource[]>('/api/resources');
  const stats = [
    {
      label: '等待处理',
      value: (overview.data?.events.queued ?? 0) + (overview.data?.events.waiting ?? 0),
      color: 'blue',
    },
    {
      label: '正在运行',
      value: overview.data?.runs.running ?? 0,
      color: 'purple',
    },
    {
      label: '运行完成',
      value: overview.data?.runs.succeeded ?? 0,
      color: 'green',
    },
    {
      label: '失败或中断',
      value: (overview.data?.runs.failed ?? 0) + (overview.data?.runs.interrupted ?? 0),
      color: 'red',
    },
  ];
  const activePipelines = resources.data?.filter((item) => item.kind === 'Pipeline') ?? [];
  return (
    <>
      <PageHeading
        eyebrow="CONTROL PLANE"
        title="运行概览"
        actions={
          <RefreshButton
            onClick={() => {
              overview.refresh();
              runs.refresh();
              resources.refresh();
            }}
          />
        }
      />
      <ErrorNotice error={overview.error || runs.error || resources.error} />
      <div className="stat-grid">
        {stats.map((stat) => (
          <div className={`stat-card ${stat.color}`} key={stat.label}>
            <span className="stat-label">
              <span className="tiny-dot" />
              {stat.label}
            </span>
            <strong>{overview.loading && !overview.data ? '—' : stat.value}</strong>
          </div>
        ))}
      </div>
      <div className="section-line">
        <h2>最近运行</h2>
        <Button kind="ghost" size="sm" renderIcon={ArrowRight} onClick={() => navigate('/runs')}>
          全部运行
        </Button>
      </div>
      <div className="panel">
        {runs.loading && !runs.data ? (
          <LoadingState />
        ) : runs.data?.length ? (
          <RunTable runs={runs.data.slice(0, 6)} />
        ) : (
          <Empty
            title="暂无运行记录"
            description="导入资源配置后，通过接入点提交任务。"
            action={
              <Button
                kind="tertiary"
                size="sm"
                renderIcon={Add}
                onClick={() => navigate('/config')}
              >
                导入资源配置
              </Button>
            }
          />
        )}
      </div>
      <div className="section-line">
        <h2>流水线</h2>
        <span className="mono muted">{activePipelines.length} CONFIGURED</span>
      </div>
      <div className="pipeline-grid">
        {activePipelines.length ? (
          activePipelines.slice(0, 6).map((item) => (
            <a
              key={item.metadata.name}
              href={`#/resources/Pipeline/${encodeURIComponent(item.metadata.name)}`}
              className="pipeline-card"
            >
              <div>
                <span className="mono">{item.metadata.name}</span>
                <Status value={item.spec.paused ? 'paused' : 'active'} />
              </div>
              <h3>{String(item.spec.schedule || '即时调度')}</h3>
              <p>
                {String(item.spec.timezone || 'Asia/Shanghai')}
                <span>并发 {String(item.spec.concurrency || 1)}</span>
              </p>
              <footer>
                {String(item.spec.workerRef)}
                <ArrowRight size={16} />
              </footer>
            </a>
          ))
        ) : (
          <a className="pipeline-card add-pipeline" href="#/resources/Pipeline/_new">
            <Add size={24} />
            <span>创建第一条流水线</span>
          </a>
        )}
      </div>
    </>
  );
}

function resourceSummary(item: Resource): string {
  const spec = item.spec;
  switch (item.kind) {
    case 'Input':
      return spec.type === 'github_issue'
        ? `GitHub issue · ${(spec.github as GitHubInput)?.repository || ''} → ${spec.pipelineRef}`
        : `Webhook → ${spec.pipelineRef}`;
    case 'Pipeline':
      return `${spec.schedule || '即时调度'} · ${spec.timezone || 'Asia/Shanghai'} · 并发 ${spec.concurrency}`;
    case 'Worker':
      return `${spec.type} · ${spec.model}`;
    case 'Workspace':
      return `${((spec.repositories || []) as unknown[]).length} 个仓库 · ${Object.keys(spec.mcpServers || {}).length} 个 MCP 服务`;
    case 'Runner': {
      const sandbox = spec.e2b as { provider?: string; template?: string } | undefined;
      return spec.type === 'e2b'
        ? `${sandbox?.provider || 'e2b'} · ${sandbox?.template || ''}`
        : `${spec.image}`;
    }
  }
}

export function Resources({ kind, name }: { kind: Kind; name?: string }) {
  const { data, error, loading, refresh } = useData<Resource[]>('/api/resources');
  const [search, setSearch] = useState('');
  const [actionError, setActionError] = useState('');
  const [notice, setNotice] = useState('');
  const [deleting, setDeleting] = useState<Resource | null>(null);
  const [busy, setBusy] = useState(false);
  const selected = data?.find((item) => item.kind === kind && item.metadata.name === name);
  if (name) {
    if (loading && !data) return <LoadingState />;
    if (error) return <ErrorNotice error={error} />;
    if (name !== '_new' && !selected)
      return (
        <Empty
          title="资源不存在"
          action={<Button onClick={() => navigate(`/resources/${kind}`)}>返回列表</Button>}
        />
      );
    return (
      <ResourceEditor
        key={`${kind}/${name}`}
        initial={selected || template(kind)}
        existing={name !== '_new'}
        resources={data ?? []}
        onSaved={refresh}
      />
    );
  }
  const items = (data ?? []).filter(
    (item) => item.kind === kind && item.metadata.name.toLowerCase().includes(search.toLowerCase()),
  );
  const action = async (callback: () => Promise<unknown>, success: string) => {
    setBusy(true);
    setActionError('');
    setNotice('');
    try {
      await callback();
      setNotice(success);
      refresh();
    } catch (cause) {
      setActionError(message(cause));
    } finally {
      setBusy(false);
    }
  };
  return (
    <>
      <PageHeading
        eyebrow={`RESOURCES / ${kind.toUpperCase()}`}
        title={kindLabels[kind]}
        actions={
          <>
            <Button
              kind="tertiary"
              size="md"
              renderIcon={Upload}
              onClick={() => navigate('/config')}
            >
              导入 YAML
            </Button>
            <Button size="md" renderIcon={Add} onClick={() => navigate(`/resources/${kind}/_new`)}>
              创建{kindLabels[kind]}
            </Button>
          </>
        }
      />
      <ErrorNotice error={error || actionError} />
      {notice && (
        <InlineNotification
          kind="success"
          title={notice}
          onClose={() => setNotice('')}
          lowContrast
        />
      )}
      <div className="panel">
        <div className="table-toolbar">
          <Search
            id="resource-search"
            labelText="搜索资源"
            placeholder={`搜索${kindLabels[kind]}名称`}
            size="lg"
            value={search}
            onChange={(event) => setSearch(event.target.value)}
          />
          <span className="table-count">{items.length} 个资源</span>
          <RefreshButton onClick={refresh} />
        </div>
        {loading && !data ? (
          <LoadingState />
        ) : !items.length ? (
          <Empty
            title={search ? '没有匹配的资源' : `尚未配置${kindLabels[kind]}`}
            action={
              !search && (
                <Button
                  kind="tertiary"
                  size="sm"
                  renderIcon={Add}
                  onClick={() => navigate(`/resources/${kind}/_new`)}
                >
                  创建{kindLabels[kind]}
                </Button>
              )
            }
          />
        ) : (
          <div className="table-scroll">
            <Table size="xl" aria-label={`${kindLabels[kind]}列表`}>
              <TableHead>
                <TableRow>
                  {['资源名称', '配置', '状态', '版本', '操作'].map((label) => (
                    <TableHeader key={label}>{label}</TableHeader>
                  ))}
                </TableRow>
              </TableHead>
              <TableBody>
                {items.map((item) => (
                  <TableRow key={item.metadata.name}>
                    <TableCell>
                      <a
                        className="table-link mono"
                        href={`#/resources/${kind}/${encodeURIComponent(item.metadata.name)}`}
                      >
                        {item.metadata.name}
                      </a>
                      <span className="cell-subtitle">{kind}</span>
                    </TableCell>
                    <TableCell>
                      <span className="resource-description">{resourceSummary(item)}</span>
                      {kind === 'Input' && (
                        <code className="cell-subtitle">
                          {window.location.origin}/hooks/{item.metadata.name}
                        </code>
                      )}
                    </TableCell>
                    <TableCell>
                      <Status value={item.spec.paused ? 'paused' : 'active'} />
                    </TableCell>
                    <TableCell className="mono">v{item.metadata.generation || 1}</TableCell>
                    <TableCell>
                      <div className="row-actions">
                        {kind === 'Pipeline' && (
                          <Button
                            kind="ghost"
                            size="sm"
                            hasIconOnly
                            iconDescription="立即调度队列"
                            renderIcon={Play}
                            disabled={busy || Boolean(item.spec.paused)}
                            onClick={() =>
                              void action(
                                () =>
                                  post(
                                    `/api/pipelines/${encodeURIComponent(item.metadata.name)}/trigger`,
                                  ),
                                '已触发队列调度',
                              )
                            }
                          />
                        )}
                        {(kind === 'Pipeline' || kind === 'Input') && (
                          <Button
                            kind="ghost"
                            size="sm"
                            hasIconOnly
                            iconDescription={item.spec.paused ? '启用' : '暂停'}
                            renderIcon={item.spec.paused ? Play : Pause}
                            disabled={busy}
                            onClick={() =>
                              void action(
                                () =>
                                  request(resourcePath(kind, item.metadata.name), {
                                    method: 'PUT',
                                    body: JSON.stringify({
                                      ...item,
                                      spec: {
                                        ...item.spec,
                                        paused: !item.spec.paused,
                                      },
                                    }),
                                  }),
                                item.spec.paused ? '已启用' : '已暂停',
                              )
                            }
                          />
                        )}
                        <Button
                          kind="ghost"
                          size="sm"
                          hasIconOnly
                          iconDescription="编辑资源"
                          renderIcon={Edit}
                          onClick={() =>
                            navigate(`/resources/${kind}/${encodeURIComponent(item.metadata.name)}`)
                          }
                        />
                        <Button
                          kind="danger--ghost"
                          size="sm"
                          hasIconOnly
                          iconDescription="删除资源"
                          renderIcon={TrashCan}
                          disabled={busy}
                          onClick={() => setDeleting(item)}
                        />
                      </div>
                    </TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          </div>
        )}
      </div>
      {kind === 'Input' && (
        <div className="help-note">
          <strong>连接接入点</strong>
          <p>
            通用 Webhook 接收 JSON，通过 Authorization: Bearer 携带密钥。使用 Idempotency-Key
            防止反馈被重复入队。
          </p>
          <p>
            GitHub issue 接入点使用 GitHub Webhook 签名认证。在仓库中订阅 Issues 事件，
            符合 Assignee 或 Label 条件的 issue 会创建任务，并收到包含任务查看链接的回复。
          </p>
        </div>
      )}
      <Modal
        open={Boolean(deleting)}
        danger
        modalHeading="删除资源"
        primaryButtonText="删除"
        secondaryButtonText="取消"
        primaryButtonDisabled={busy}
        onRequestClose={() => setDeleting(null)}
        onRequestSubmit={() => {
          if (!deleting) return;
          void action(async () => {
            await request(resourcePath(deleting.kind, deleting.metadata.name), {
              method: 'DELETE',
            });
            setDeleting(null);
          }, '资源已删除');
        }}
      >
        <p>
          确定删除 <strong>{deleting?.metadata.name}</strong>
          ？正在被其他资源引用的配置无法删除。
        </p>
        {actionError && <ErrorNotice error={actionError} />}
      </Modal>
    </>
  );
}

export function Configuration() {
  const [yaml, setYAML] = useState('');
  const [error, setError] = useState('');
  const [result, setResult] = useState<Resource[] | null>(null);
  const [busy, setBusy] = useState(false);
  const file = useRef<HTMLInputElement>(null);
  const importYAML = async () => {
    setBusy(true);
    setError('');
    setResult(null);
    try {
      const docs = await post<Resource[]>('/api/config/import', { yaml });
      setResult(docs);
    } catch (cause) {
      setError(message(cause));
    } finally {
      setBusy(false);
    }
  };
  const exportYAML = async () => {
    setError('');
    try {
      const response = await fetch('/api/config/export', {
        credentials: 'same-origin',
      });
      if (!response.ok) throw new Error('导出失败，请检查登录状态并重试。');
      const blob = await response.blob();
      const url = URL.createObjectURL(blob);
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
    <>
      <PageHeading
        eyebrow="DECLARATIVE CONFIGURATION"
        title="配置中心"
        actions={
          <Button kind="tertiary" renderIcon={Download} onClick={() => void exportYAML()}>
            导出所有资源
          </Button>
        }
      />
      <div className="config-layout">
        <div className="panel config-panel">
          <div className="section-line">
            <h2>导入 YAML</h2>
            <Button
              kind="ghost"
              size="sm"
              renderIcon={Upload}
              onClick={() => file.current?.click()}
            >
              选择文件
            </Button>
          </div>
          <input
            ref={file}
            type="file"
            className="visually-hidden"
            accept=".yaml,.yml,text/yaml,application/yaml"
            onChange={(event) => {
              const selected = event.target.files?.[0];
              if (selected) {
                if (selected.size > 1024 * 1024) setError('YAML 文件不能超过 1 MiB。');
                else
                  void selected.text().then((value) => {
                    setYAML(value);
                    setError('');
                  });
              }
              event.target.value = '';
            }}
          />
          <TextArea
            className="code-input"
            id="config-yaml"
            labelText="资源清单"
            helperText="使用 --- 分隔多个资源。"
            rows={22}
            spellCheck={false}
            value={yaml}
            onChange={(event) => setYAML(event.target.value)}
            placeholder={
              'apiVersion: cloudpilot.dev/v1alpha1\nkind: Runner\nmetadata:\n  name: docker\nspec:\n  type: docker\n  image: cloudpilot-codex:latest'
            }
          />
          <ErrorNotice error={error} />
          {result && (
            <InlineNotification
              kind="success"
              title={`已应用 ${result.length} 个资源`}
              subtitle={result.map((item) => `${item.kind}/${item.metadata.name}`).join(' · ')}
              hideCloseButton
              lowContrast
            />
          )}
          <div className="config-footer">
            <Button
              renderIcon={Checkmark}
              disabled={busy || !yaml.trim()}
              onClick={() => void importYAML()}
            >
              {busy ? '正在应用…' : '校验并应用'}
            </Button>
          </div>
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
            <p>
              可在资源编辑器中直接填写；导出不包含密钥，向新环境导入时需重新填写。
            </p>
          </div>
        </aside>
      </div>
    </>
  );
}

export function Events() {
  const [status, setStatus] = useState('');
  const [pipeline, setPipeline] = useState('');
  const query = new URLSearchParams();
  if (status) query.set('status', status);
  if (pipeline) query.set('pipeline', pipeline);
  const { data, error, loading, refresh } = useData<Event[]>(`/api/events?${query}`, 5000);
  const resources = useData<Resource[]>('/api/resources?kind=Pipeline');
  const [actionError, setActionError] = useState('');
  const [busy, setBusy] = useState(false);
  const retry = async (event: Event) => {
    setBusy(true);
    setActionError('');
    try {
      await post(`/api/events/${event.id}/retry`);
      refresh();
    } catch (cause) {
      const detail = message(cause);
      setActionError(
        detail.includes('close the existing interactive session')
          ? '此事件仍有可继续的 Agent 会话。请到运行详情继续处理，或结束会话后重新入队。'
          : detail,
      );
    } finally {
      setBusy(false);
    }
  };
  return (
    <>
      <PageHeading
        eyebrow="OBSERVABILITY / EVENTS"
        title="事件队列"
        description="每 5 秒更新 · 最近 200 条"
        actions={<RefreshButton onClick={refresh} />}
      />
      <ErrorNotice error={error || actionError || resources.error} />
      <div className="panel">
        <Filters
          status={status}
          setStatus={setStatus}
          pipeline={pipeline}
          setPipeline={setPipeline}
          pipelines={resources.data ?? []}
          events
        />
        {loading && !data ? (
          <LoadingState />
        ) : data?.length ? (
          <div className="table-scroll">
            <Table aria-label="事件队列" size="lg">
              <TableHead>
                <TableRow>
                  {['事件', '接入点', '流水线', '状态', '尝试次数', '接收时间 · 上海', ''].map(
                    (label, i) => (
                      <TableHeader key={i}>{label}</TableHeader>
                    ),
                  )}
                </TableRow>
              </TableHead>
              <TableBody>
                {data.map((event) => (
                  <TableRow key={event.id}>
                    <TableCell>
                      <a className="table-link mono" href={`#/events/${event.id}`}>
                        EVT-{String(event.id).padStart(4, '0')}
                      </a>
                    </TableCell>
                    <TableCell>{event.inputName}</TableCell>
                    <TableCell>{event.pipelineName}</TableCell>
                    <TableCell>
                      <Status value={event.status} />
                    </TableCell>
                    <TableCell className="mono">{event.attempts}</TableCell>
                    <TableCell>{formatTime(event.createdAt)}</TableCell>
                    <TableCell>
                      {['failed', 'cancelled'].includes(event.status) ? (
                        <Button
                          kind="ghost"
                          size="sm"
                          renderIcon={Renew}
                          disabled={busy}
                          onClick={() => void retry(event)}
                        >
                          重试
                        </Button>
                      ) : (
                        <Button
                          kind="ghost"
                          size="sm"
                          renderIcon={ArrowRight}
                          onClick={() => navigate(`/events/${event.id}`)}
                        >
                          详情
                        </Button>
                      )}
                    </TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          </div>
        ) : (
          <Empty title={status || pipeline ? '没有匹配的事件' : '暂无事件'} />
        )}
      </div>
    </>
  );
}

export function EventPage({ id }: { id: string }) {
  const { data, error, loading, refresh } = useData<EventDetail>(`/api/events/${id}`, 5000);
  if (loading && !data) return <LoadingState />;
  if (!data) return <ErrorNotice error={error || '事件不存在。'} />;
  const { event, runs } = data;
  return (
    <>
      <a className="back-link" href="#/events">
        ← 返回事件队列
      </a>
      <PageHeading
        eyebrow={`EVENT / ${event.pipelineName}`}
        title={`EVT-${String(event.id).padStart(4, '0')}`}
        description="每 5 秒更新"
        actions={
          <>
            <Status value={event.status} />
            <RefreshButton onClick={refresh} />
          </>
        }
      />
      <ErrorNotice error={error} />
      <div className="metadata-grid">
        <MetadataItem label="接入点">{event.inputName}</MetadataItem>
        <MetadataItem label="流水线">{event.pipelineName}</MetadataItem>
        <MetadataItem label="尝试次数">{event.attempts}</MetadataItem>
        <MetadataItem label="接收时间 · 上海">{formatTime(event.createdAt)}</MetadataItem>
      </div>
      <div className="section-line">
        <h2>运行记录</h2>
        <span className="mono muted">{runs.length} RUNS</span>
      </div>
      <div className="panel">
        {runs.length ? (
          <RunTable runs={runs} />
        ) : (
          <Empty title="等待调度" />
        )}
      </div>
      <details className="event-payload" open>
        <summary>任务上下文 · 原始事件</summary>
        <pre className="payload-box">{JSON.stringify(event.payload, null, 2)}</pre>
      </details>
    </>
  );
}

function Filters({
  status,
  setStatus,
  pipeline,
  setPipeline,
  pipelines,
  events = false,
}: {
  status: string;
  setStatus: (value: string) => void;
  pipeline: string;
  setPipeline: (value: string) => void;
  pipelines: Resource[];
  events?: boolean;
}) {
  return (
    <div className="filters">
      <Select
        id="filter-status"
        labelText="状态"
        size="sm"
        value={status}
        onChange={(event) => setStatus(event.target.value)}
      >
        <SelectItem value="" text="全部状态" />
        {(events
          ? ['queued', 'waiting', 'running', 'interrupted', 'succeeded', 'failed', 'cancelled']
          : ['queued', 'running', 'interrupted', 'succeeded', 'failed', 'cancelled']
        ).map((value) => (
          <SelectItem
            key={value}
            value={value}
            text={
              {
                queued: '等待调度',
                waiting: '等待继续',
                interrupted: '已中断',
                running: '运行中',
                succeeded: '已完成',
                failed: '失败',
                cancelled: '已取消',
              }[value] || value
            }
          />
        ))}
      </Select>
      <Select
        id="filter-pipeline"
        labelText="流水线"
        size="sm"
        value={pipeline}
        onChange={(event) => setPipeline(event.target.value)}
      >
        <SelectItem value="" text="全部流水线" />
        {pipelines.map((item) => (
          <SelectItem
            key={item.metadata.name}
            value={item.metadata.name}
            text={item.metadata.name}
          />
        ))}
      </Select>
      {(status || pipeline) && (
        <Button
          kind="ghost"
          size="sm"
          onClick={() => {
            setStatus('');
            setPipeline('');
          }}
        >
          清除筛选
        </Button>
      )}
    </div>
  );
}

export function Runs() {
  const [status, setStatus] = useState('');
  const [pipeline, setPipeline] = useState('');
  const query = new URLSearchParams();
  if (status) query.set('status', status);
  if (pipeline) query.set('pipeline', pipeline);
  const { data, error, loading, refresh } = useData<Run[]>(`/api/runs?${query}`, 5000);
  const resources = useData<Resource[]>('/api/resources?kind=Pipeline');
  return (
    <>
      <PageHeading
        eyebrow="OBSERVABILITY / RUNS"
        title="运行记录"
        description="每 5 秒更新 · 最近 200 条"
        actions={<RefreshButton onClick={refresh} />}
      />
      <ErrorNotice error={error || resources.error} />
      <div className="panel">
        <Filters
          status={status}
          setStatus={setStatus}
          pipeline={pipeline}
          setPipeline={setPipeline}
          pipelines={resources.data ?? []}
        />
        {loading && !data ? (
          <LoadingState />
        ) : data?.length ? (
          <RunTable runs={data} />
        ) : (
          <Empty title={status || pipeline ? '没有匹配的运行记录' : '暂无运行记录'} />
        )}
      </div>
    </>
  );
}

export function RunPage({ id }: { id: string }) {
  const { data, error, loading, refresh } = useData<RunDetail>(`/api/runs/${id}`, 2000);
  const [logs, setLogs] = useState<RunLog[]>([]);
  const [logError, setLogError] = useState('');
  const [actionError, setActionError] = useState('');
  const [busy, setBusy] = useState(false);
  const [confirmCancel, setConfirmCancel] = useState(false);
  const [autoScroll, setAutoScroll] = useState(true);
  const logEnd = useRef<HTMLDivElement>(null);
  const status = data?.run.status;
  useEffect(() => {
    let active = true;
    let cursor = 0;
    let timer: ReturnType<typeof setTimeout> | undefined;
    const controller = new AbortController();
    setLogs([]);
    const load = async () => {
      try {
        const next = await request<RunLog[]>(`/api/runs/${id}/logs?after=${cursor}`, {
          signal: controller.signal,
        });
        if (active) {
          if (next.length) {
            cursor = next[next.length - 1].id;
            setLogs((current) => [...current, ...next].slice(-10000));
          }
          setLogError('');
        }
      } catch (cause) {
        if (active) setLogError(message(cause));
      } finally {
        if (active) timer = setTimeout(load, 2000);
      }
    };
    void load();
    return () => {
      active = false;
      controller.abort();
      clearTimeout(timer);
    };
  }, [id]);
  useEffect(() => {
    if (autoScroll && logEnd.current?.offsetParent)
      logEnd.current.scrollIntoView({ block: 'nearest' });
  }, [logs, autoScroll]);
  const act = async (path: string, body?: unknown): Promise<boolean> => {
    setBusy(true);
    setActionError('');
    try {
      await post(path, body);
      refresh();
      setConfirmCancel(false);
      return true;
    } catch (cause) {
      setActionError(message(cause));
      return false;
    } finally {
      setBusy(false);
    }
  };
  if (loading && !data) return <LoadingState />;
  if (!data) return <ErrorNotice error={error || '运行记录不存在。'} />;
  if (String(data.run.id) !== id) return <LoadingState />;
  const { run, event, artifacts, session, turns = [] } = data;
  const capabilities = data.capabilities || {
    interactive: false,
    canSend: false,
    canInterrupt: false,
    canResume: false,
    canClose: false,
  };
  const interactive = Boolean(session) || capabilities.interactive;
  const rawLogs = (
    <>
      <div className="section-line">
        <div>
          <h2>执行日志</h2>
          <p className="muted">每 2 秒增量更新 · 最多保留最近 10,000 行</p>
        </div>
        <Toggle
          id="logs-autoscroll"
          labelText="自动滚动"
          labelA="关"
          labelB="开"
          toggled={autoScroll}
          size="sm"
          onToggle={setAutoScroll}
        />
      </div>
      <ErrorNotice error={logError} />
      <div className="terminal">
        <div className="terminal-bar">
          <span>
            <i />
            <i />
            <i />
          </span>
          <code>cloudpilot / run-{id}</code>
          <span>{status === 'running' ? 'LIVE' : 'LOG'}</span>
        </div>
        <div className="terminal-content" tabIndex={0} aria-label="执行日志">
          {logs.length ? (
            logs.map((log) => (
              <div
                className={`log-line ${log.stream === 'stderr' ? 'log-error' : ''}`}
                key={log.id}
              >
                <time>{formatTime(log.createdAt)}</time>
                <span className="log-stream">{log.stream}</span>
                <pre>{log.message}</pre>
              </div>
            ))
          ) : (
            <p className="terminal-empty">
              {status === 'running' ? '等待运行日志…' : '此运行没有日志。'}
            </p>
          )}
          <div ref={logEnd} />
        </div>
      </div>
    </>
  );
  return (
    <>
      <a className="back-link" href="#/runs">
        ← 返回运行记录
      </a>
      <PageHeading
        eyebrow={`RUN / ${run.pipelineName}`}
        title={`RUN-${String(run.id).padStart(4, '0')}`}
        description={`事件 EVT-${event.id} · 第 ${run.attempt} 次尝试 · ${formatTime(run.startedAt)} 开始（Asia/Shanghai）`}
        actions={
          <>
            <Status value={run.status} />
            {status === 'running' && !interactive && (
              <Button
                kind="danger--tertiary"
                size="md"
                renderIcon={Stop}
                disabled={busy}
                onClick={() => setConfirmCancel(true)}
              >
                取消运行
              </Button>
            )}
            {(!interactive || session?.state === 'closed') &&
              ['failed', 'cancelled'].includes(event.status) && (
                <Button
                  kind="tertiary"
                  size="md"
                  renderIcon={Renew}
                  disabled={busy}
                  onClick={() => void act(`/api/events/${event.id}/retry`)}
                >
                  重新入队
                </Button>
              )}
          </>
        }
      />
      <ErrorNotice error={error || actionError} />
      {run.error && (
        <InlineNotification
          kind="error"
          title="运行错误"
          subtitle={run.error}
          hideCloseButton
          lowContrast
        />
      )}
      <div className="panel metadata-grid">
        <MetadataItem label="智能体">{run.workerName}</MetadataItem>
        <MetadataItem label="工作空间">{run.workspaceName}</MetadataItem>
        <MetadataItem label="运行设施">{run.runnerName}</MetadataItem>
        <MetadataItem label="执行耗时">{duration(run)}</MetadataItem>
      </div>
      {interactive ? (
        <RunConversation
          id={id}
          session={session}
          turns={turns}
          capabilities={capabilities}
          busy={busy}
          actionError={actionError}
          perform={act}
          rawLogs={rawLogs}
        />
      ) : (
        rawLogs
      )}
      <div className="section-line">
        <h2>运行产物</h2>
        <span className="mono muted">{artifacts.length} ARTIFACTS</span>
      </div>
      <ArtifactGrid artifacts={artifacts} />
      <details className="event-payload">
        <summary>查看原始事件 · EVT-{event.id}</summary>
        <pre className="payload-box">{JSON.stringify(event.payload, null, 2)}</pre>
      </details>
      <Modal
        open={confirmCancel}
        danger
        modalHeading="取消本次运行？"
        primaryButtonText="取消运行"
        secondaryButtonText="返回"
        primaryButtonDisabled={busy}
        onRequestClose={() => setConfirmCancel(false)}
        onRequestSubmit={() => void act(`/api/runs/${id}/cancel`)}
      >
        <p>将停止正在执行的容器。本次运行保留日志，事件随后可以手动重新入队。</p>
        <ErrorNotice error={actionError} />
      </Modal>
    </>
  );
}

export function Artifacts() {
  const { data, error, loading, refresh } = useData<Artifact[]>('/api/artifacts', 10000);
  const [filter, setFilter] = useState('');
  const artifacts = (data ?? []).filter((item) => !filter || item.kind === filter);
  return (
    <>
      <PageHeading
        eyebrow="DELIVERY / ARTIFACTS"
        title="产物中心"
        description="最近 200 条"
        actions={<RefreshButton onClick={refresh} />}
      />
      <ErrorNotice error={error} />
      <div className="artifact-filters">
        {[
          ['', '全部'],
          ['pr', 'Pull requests'],
          ['issue', 'Issues'],
          ['file', '文件'],
          ['text', '说明'],
        ].map(([value, label]) => (
          <Button
            key={value}
            kind={filter === value ? 'primary' : 'ghost'}
            size="sm"
            onClick={() => setFilter(value)}
          >
            {label}
          </Button>
        ))}
      </div>
      {loading && !data ? <LoadingState /> : <ArtifactGrid artifacts={artifacts} showRun />}
    </>
  );
}

function ArtifactGrid({
  artifacts,
  showRun = false,
}: {
  artifacts: Artifact[];
  showRun?: boolean;
}) {
  const [selected, setSelected] = useState<Artifact | null>(null);
  const [copyState, setCopyState] = useState('');
  if (!artifacts.length)
    return (
      <div className="panel">
        <Empty title="暂无产物" />
      </div>
    );
  return (
    <>
      <div className="artifact-grid">
        {artifacts.map((item) => (
          <article className="artifact-card" key={item.id}>
            <div className="artifact-topline">
              <Tag
                type={
                  ({ pr: 'purple', issue: 'blue', file: 'teal', text: 'gray' }[item.kind] as
                    'purple' | 'blue' | 'teal' | 'gray') || 'gray'
                }
                size="sm"
              >
                {item.kind.toUpperCase()}
              </Tag>
              <span>{formatTime(item.createdAt)}</span>
            </div>
            <h3>{item.name}</h3>
            {item.content ? (
              <p className="artifact-preview">{item.content}</p>
            ) : (
              <p className="muted">
                {item.kind === 'file'
                  ? `${item.contentType || '文件'} · ${formatSize(item.size)}`
                  : item.url || '运行产物'}
              </p>
            )}
            <footer>
              {showRun ? (
                <a href={`#/runs/${item.runId}`} className="mono">
                  RUN-{String(item.runId).padStart(4, '0')}
                </a>
              ) : (
                <span />
              )}
              {item.kind === 'file' ? (
                <a className="artifact-action" href={`/api/artifacts/${item.id}/download`} download>
                  下载文件
                  <Download size={16} />
                </a>
              ) : safeURL(item.url) ? (
                <a
                  className="artifact-action"
                  href={safeURL(item.url)}
                  target="_blank"
                  rel="noreferrer"
                >
                  打开链接
                  <Launch size={16} />
                </a>
              ) : (
                <Button
                  kind="ghost"
                  size="sm"
                  renderIcon={ArrowRight}
                  onClick={() => {
                    setSelected(item);
                    setCopyState('');
                  }}
                >
                  阅读说明
                </Button>
              )}
            </footer>
          </article>
        ))}
      </div>
      <Modal
        open={Boolean(selected)}
        passiveModal
        size="lg"
        modalHeading={selected?.name || '产物内容'}
        onRequestClose={() => setSelected(null)}
      >
        <pre className="text-artifact">{selected?.content || '此产物没有文本内容。'}</pre>
        {selected?.content && (
          <Button
            kind="ghost"
            size="sm"
            renderIcon={Copy}
            onClick={() =>
              void navigator.clipboard.writeText(selected.content).then(
                () => setCopyState('已复制'),
                () => setCopyState('复制失败，请手动选择文本。'),
              )
            }
          >
            {copyState || '复制文本'}
          </Button>
        )}
      </Modal>
    </>
  );
}

function formatSize(bytes: number) {
  return bytes >= 1024 * 1024
    ? `${(bytes / (1024 * 1024)).toFixed(1)} MB`
    : bytes >= 1024
      ? `${(bytes / 1024).toFixed(1)} KB`
      : `${bytes} B`;
}

export function NotFound() {
  return (
    <Empty
      title="页面不存在"
      action={<Button onClick={() => navigate('/')}>回到概览</Button>}
    />
  );
}
export const isKind = (value: string): value is Kind =>
  (kinds as readonly string[]).includes(value);
