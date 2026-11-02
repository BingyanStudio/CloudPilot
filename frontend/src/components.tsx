import type { ReactNode } from 'react';
import {
  Button,
  InlineLoading,
  InlineNotification,
  Tag,
  Table,
  TableHead,
  TableRow,
  TableHeader,
  TableBody,
  TableCell,
} from '@carbon/react';
import { ArrowRight, Renew, Cloud } from '@carbon/icons-react';
import ReactMarkdown, { defaultUrlTransform } from 'react-markdown';
import remarkGfm from 'remark-gfm';
import type { Artifact, Run } from './types';
import { navigate } from './hooks';
import { artifactHref } from './artifactLinks';

export function PageHeading({
  eyebrow,
  title,
  description,
  actions,
}: {
  eyebrow: string;
  title: string;
  description?: string;
  actions?: ReactNode;
}) {
  return (
    <div className="page-heading">
      <div>
        <span className="eyebrow">{eyebrow}</span>
        <h1>{title}</h1>
        {description && <p>{description}</p>}
      </div>
      <div className="heading-actions">{actions}</div>
    </div>
  );
}

export function ErrorNotice({ error }: { error: string }) {
  return error ? (
    <InlineNotification
      kind="error"
      title="操作未完成"
      subtitle={error}
      hideCloseButton
      lowContrast
    />
  ) : null;
}

export function Empty({
  title,
  description,
  action,
}: {
  title: string;
  description?: string;
  action?: ReactNode;
}) {
  return (
    <div className="empty-state">
      <div className="empty-icon">
        <Cloud size={32} />
      </div>
      <h3>{title}</h3>
      {description && <p>{description}</p>}
      {action}
    </div>
  );
}

export function LoadingState() {
  return (
    <div className="loading-state">
      <InlineLoading description="正在加载…" />
    </div>
  );
}

export function RefreshButton({ onClick }: { onClick: () => void }) {
  return (
    <Button kind="ghost" size="sm" renderIcon={Renew} onClick={onClick}>
      刷新
    </Button>
  );
}

const statuses: Record<
  string,
  { label: string; color: 'blue' | 'green' | 'red' | 'gray' | 'purple' }
> = {
  queued: { label: '等待调度', color: 'blue' },
  waiting: { label: '等待继续', color: 'blue' },
  starting: { label: '准备会话', color: 'blue' },
  running: { label: '运行中', color: 'purple' },
  succeeded: { label: '已完成', color: 'green' },
  completed: { label: '已完成', color: 'green' },
  idle: { label: '可继续', color: 'green' },
  interrupted: { label: '已中断', color: 'gray' },
  interrupting: { label: '正在中断', color: 'purple' },
  closing: { label: '正在结束', color: 'gray' },
  closed: { label: '已结束', color: 'gray' },
  inProgress: { label: '执行中', color: 'purple' },
  streaming: { label: '正在输出', color: 'purple' },
  accepted: { label: '已接收', color: 'green' },
  rejected: { label: '未送达', color: 'red' },
  unconfirmed: { label: '状态未确认', color: 'gray' },
  failed: { label: '失败', color: 'red' },
  cancelled: { label: '已取消', color: 'gray' },
  paused: { label: '已暂停', color: 'gray' },
  active: { label: '已启用', color: 'green' },
};
export function Status({ value, label }: { value: string; label?: string }) {
  const state = statuses[value] || { label: value, color: 'gray' as const };
  return (
    <Tag type={state.color} size="sm">
      <span className={`status-dot ${value}`} />
      {label || state.label}
    </Tag>
  );
}
export function formatTime(value?: string | null) {
  if (!value || value.startsWith('0001-')) return '—';
  return new Intl.DateTimeFormat('zh-CN', {
    month: '2-digit',
    day: '2-digit',
    hour: '2-digit',
    minute: '2-digit',
    second: '2-digit',
    hour12: false,
    timeZone: 'Asia/Shanghai',
  }).format(new Date(value));
}
export function duration(run: Run) {
  const seconds = Math.max(
    0,
    Math.floor(
      ((run.finishedAt ? new Date(run.finishedAt).getTime() : Date.now()) -
        new Date(run.startedAt).getTime()) /
        1000,
    ),
  );
  return seconds < 60 ? `${seconds}s` : `${Math.floor(seconds / 60)}m ${seconds % 60}s`;
}
export function RunTable({ runs }: { runs: Run[] }) {
  return (
    <div className="table-scroll">
      <Table size="lg" aria-label="运行记录">
        <TableHead>
          <TableRow>
            {['运行', '流水线', '状态', '智能体', '耗时', '开始时间 · 上海', ''].map(
              (label, index) => (
                <TableHeader key={index}>{label}</TableHeader>
              ),
            )}
          </TableRow>
        </TableHead>
        <TableBody>
          {runs.map((run) => (
            <TableRow key={run.id}>
              <TableCell>
                <a className="table-link mono" href={`#/runs/${run.id}`}>
                  RUN-{String(run.id).padStart(4, '0')}
                </a>
                <span className="cell-subtitle">第 {run.attempt} 次尝试</span>
              </TableCell>
              <TableCell>{run.pipelineName}</TableCell>
              <TableCell>
                <Status value={run.status} />
              </TableCell>
              <TableCell>
                <span className="mono">{run.workerName}</span>
              </TableCell>
              <TableCell className="mono">{duration(run)}</TableCell>
              <TableCell>{formatTime(run.startedAt)}</TableCell>
              <TableCell>
                <Button
                  kind="ghost"
                  size="sm"
                  hasIconOnly
                  iconDescription={`查看运行 ${run.id}`}
                  renderIcon={ArrowRight}
                  onClick={() => navigate(`/runs/${run.id}`)}
                />
              </TableCell>
            </TableRow>
          ))}
        </TableBody>
      </Table>
    </div>
  );
}
export function MetadataItem({ label, children }: { label: string; children: ReactNode }) {
  return (
    <div className="metadata-item">
      <span>{label}</span>
      <strong>{children}</strong>
    </div>
  );
}
export function safeURL(value: string): string | undefined {
  try {
    const url = new URL(value);
    return ['http:', 'https:'].includes(url.protocol) ? url.toString() : undefined;
  } catch {
    return undefined;
  }
}

export function Markdown({ text, artifacts = [] }: { text: string; artifacts?: Artifact[] }) {
  return (
    <div className="markdown">
      <ReactMarkdown
        remarkPlugins={[remarkGfm]}
        urlTransform={(url) => artifactHref(url, artifacts) ?? defaultUrlTransform(url)}
        components={{
          a: ({ href, children }) => {
            if (!href)
              return (
                <span className="markdown-missing" title="此运行没有收集到对应的产物文件">
                  {children}
                </span>
              );
            if (href.startsWith('/api/artifacts/'))
              return (
                <a href={href} download>
                  {children}
                </a>
              );
            return (
              <a href={href} target="_blank" rel="noreferrer">
                {children}
              </a>
            );
          },
        }}
      >
        {text}
      </ReactMarkdown>
    </div>
  );
}
