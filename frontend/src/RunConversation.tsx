import { useEffect, useRef, useState, type ReactNode } from 'react';
import {
  Button,
  InlineNotification,
  Modal,
  Tab,
  TabList,
  TabPanel,
  TabPanels,
  Tabs,
  TextArea,
  Toggle,
} from '@carbon/react';
import { Bot, UserAvatar, Terminal, SendAlt, Play, Pause, Stop } from '@carbon/icons-react';
import { message, request } from './api';
import { Empty, ErrorNotice, Status, formatTime } from './components';
import type { AgentSession, AgentTurn, SessionCapabilities, SessionEvent } from './types';
import { emptyTimeline, mergeTimeline, settledItem, type TimelineItem } from './sessionTimeline';

type Action = (path: string, body?: unknown) => Promise<boolean>;

const sessionDescriptions: Record<string, string> = {
  starting: '正在准备工作目录与 Agent 会话。',
  queued: '下一轮已排队，等待流水线调度。工作目录和对话会继续保留。',
  running: 'Agent 正在执行。你可以补充信息，或中断当前轮次后调整方向。',
  interrupting: '正在中断当前轮次。工作目录和对话会保留，完成后可继续。',
  idle: '本轮已完成。可以追加需求并继续，或结束任务释放工作目录。',
  interrupted: '当前轮次已中断。已有对话和工作目录保留，可补充信息后继续。',
  failed: '本轮执行失败。可以查看错误并尝试继续，或结束任务。',
  closing: '正在停止执行并清理会话工作目录。',
  closed: '任务已结束。对话、日志和已收集的产物仍可查看，工作目录已释放。',
};

export default function RunConversation({
  id,
  session,
  turns,
  capabilities,
  busy,
  actionError,
  perform,
  rawLogs,
}: {
  id: string;
  session: AgentSession | null;
  turns: AgentTurn[];
  capabilities: SessionCapabilities;
  busy: boolean;
  actionError: string;
  perform: Action;
  rawLogs: ReactNode;
}) {
  const [timeline, setTimeline] = useState(emptyTimeline);
  const [timelineError, setTimelineError] = useState('');
  const [draft, setDraft] = useState('');
  const [autoScroll, setAutoScroll] = useState(true);
  const [closing, setClosing] = useState(false);
  const scrollArea = useRef<HTMLDivElement>(null);
  const state = session?.state || 'starting';
  const inputBytes = new TextEncoder().encode(draft).length;
  const tooLong = inputBytes > 32 * 1024;
  const hasMessage = Boolean(draft.trim());
  const editable = capabilities.canSend || capabilities.canResume;
  const canSubmit = hasMessage ? editable && !tooLong : capabilities.canResume;

  useEffect(() => {
    let active = true;
    let cursor = 0;
    let timer: ReturnType<typeof setTimeout> | undefined;
    const controller = new AbortController();
    setTimeline(emptyTimeline());
    setTimelineError('');
    setDraft('');
    setClosing(false);
    const poll = async () => {
      let batchSize = 0;
      try {
        const events = await request<SessionEvent[]>(`/api/runs/${id}/timeline?after=${cursor}`, {
          signal: controller.signal,
        });
        if (active) {
          batchSize = events.length;
          if (events.length) {
            cursor = Math.max(cursor, ...events.map((event) => event.id));
            setTimeline((current) => mergeTimeline(current, events));
          }
          setTimelineError('');
        }
      } catch (cause) {
        if (active) setTimelineError(message(cause));
      } finally {
        if (active) timer = setTimeout(poll, batchSize >= 1000 ? 50 : 1000);
      }
    };
    void poll();
    return () => {
      active = false;
      controller.abort();
      clearTimeout(timer);
    };
  }, [id]);

  useEffect(() => {
    if (autoScroll && scrollArea.current)
      scrollArea.current.scrollTop = scrollArea.current.scrollHeight;
  }, [timeline, autoScroll]);

  const submit = async () => {
    if (!canSubmit || busy) return;
    const path = hasMessage && capabilities.canSend ? 'messages' : 'resume';
    const body = hasMessage ? { message: draft.trim() } : {};
    const sentDraft = draft;
    if (await perform(`/api/runs/${id}/${path}`, body))
      setDraft((current) => (current === sentDraft ? '' : current));
  };

  const knownIDs = new Set(turns.map((turn) => turn.id));
  const unknownIDs = [
    ...new Set(
      timeline.items
        .filter((item) => item.turnId && !knownIDs.has(item.turnId))
        .map((item) => item.turnId),
    ),
  ];
  const groups = [
    ...turns.map((turn) => ({ id: turn.id, number: turn.number, turn })),
    ...unknownIDs.map((turnId) => ({ id: turnId, number: 0, turn: undefined })),
  ];
  const ungrouped = timeline.items.filter((item) => !item.turnId);
  const toolCount = timeline.items.filter((item) => item.type === 'tool').length;

  return (
    <section className="run-session" aria-label="Agent 会话">
      <div className={`session-banner session-${state}`}>
        <div className="session-emblem">
          <Bot size={24} />
        </div>
        <div className="session-summary">
          <div className="session-title">
            <strong>Agent 会话</strong>
            <Status value={state} />
          </div>
          <p>{sessionDescriptions[state] || '会话状态正在更新。'}</p>
        </div>
        <div className="session-controls">
          {capabilities.canInterrupt && (
            <Button
              kind="tertiary"
              size="sm"
              renderIcon={Pause}
              disabled={busy}
              onClick={() => void perform(`/api/runs/${id}/interrupt`)}
            >
              中断当前轮次
            </Button>
          )}
          {capabilities.canClose && (
            <Button
              kind="danger--ghost"
              size="sm"
              renderIcon={Stop}
              disabled={busy}
              onClick={() => setClosing(true)}
            >
              结束任务
            </Button>
          )}
        </div>
      </div>
      {session?.lastError && <ErrorNotice error={session.lastError} />}
      <div className="session-tabs">
        <Tabs>
          <TabList aria-label="运行详情视图">
            <Tab>对话与工具</Tab>
            <Tab>原始日志</Tab>
          </TabList>
          <TabPanels>
            <TabPanel>
              <div className="conversation-toolbar">
                <div>
                  <strong>执行时间线</strong>
                  <span>
                    {turns.length} 轮对话 · {toolCount} 次工具调用
                  </span>
                </div>
                <Toggle
                  id={`timeline-follow-${id}`}
                  labelText="自动跟随"
                  labelA="关"
                  labelB="开"
                  size="sm"
                  toggled={autoScroll}
                  onToggle={setAutoScroll}
                />
              </div>
              <ErrorNotice error={timelineError} />
              <div
                className="conversation-scroll"
                ref={scrollArea}
                tabIndex={0}
                aria-label="对话和工具调用时间线"
              >
                {!timeline.items.length && !turns.length ? (
                  <Empty
                    title="等待会话开始"
                    description="Agent 的回复、工具调用和你的补充信息会显示在这里。"
                  />
                ) : (
                  <>
                    {ungrouped.map((item) => (
                      <TimelineCard item={item} key={item.key} />
                    ))}
                    {groups.map(({ id: turnId, number, turn }) => {
                      const items = timeline.items.filter((item) => item.turnId === turnId);
                      return (
                        <section
                          className="conversation-turn"
                          key={turnId}
                          aria-label={number ? `第 ${number} 轮` : '当前轮次'}
                        >
                          <div className="turn-divider">
                            <span className="turn-number">
                              {number ? `第 ${number} 轮` : '当前轮次'}
                            </span>
                            {turn && (
                              <>
                                <span>{turn.initial ? '反馈任务' : '人工继续'}</span>
                                <time>{formatTime(turn.startedAt || turn.createdAt)}</time>
                                <Status value={turn.status} />
                              </>
                            )}
                          </div>
                          {items.map((item) => (
                            <TimelineCard
                              item={settledItem(item, turn?.status)}
                              key={item.key}
                              feedback={Boolean(
                                turn?.initial &&
                                item.firstEventId ===
                                  items.find((candidate) => candidate.role === 'user')
                                    ?.firstEventId,
                              )}
                            />
                          ))}
                          {!items.length && (
                            <p className="turn-empty">
                              {turn?.status === 'queued'
                                ? '已接收本轮输入，等待调度。'
                                : '等待本轮消息…'}
                            </p>
                          )}
                        </section>
                      );
                    })}
                  </>
                )}
              </div>
              {state !== 'closed' && state !== 'closing' && (
                <div className="conversation-composer">
                  <TextArea
                    id={`session-message-${id}`}
                    labelText={state === 'running' ? '补充信息' : '继续处理'}
                    placeholder={
                      state === 'running'
                        ? '补充复现步骤、约束或新的线索…'
                        : '说明下一步要做什么，或直接继续当前任务…'
                    }
                    value={draft}
                    rows={3}
                    disabled={!editable || busy}
                    invalid={tooLong}
                    invalidText="信息不能超过 32 KiB，请缩短内容后重试。"
                    onChange={(event) => setDraft(event.target.value)}
                    onKeyDown={(event) => {
                      if (event.key === 'Enter' && (event.ctrlKey || event.metaKey)) {
                        event.preventDefault();
                        void submit();
                      }
                    }}
                  />
                  <div className="composer-footer">
                    <p>
                      {state === 'running'
                        ? '补充信息会发送给当前轮次，请留意消息的接收状态。'
                        : '继续会保留已有对话和代码修改，并按流水线的并发设置调度。'}
                      <span>Ctrl / ⌘ + Enter 发送 · {Math.ceil(inputBytes / 1024)} / 32 KiB</span>
                    </p>
                    <Button
                      size="md"
                      renderIcon={hasMessage ? SendAlt : Play}
                      disabled={!canSubmit || busy}
                      onClick={() => void submit()}
                    >
                      {busy
                        ? '正在提交…'
                        : state === 'running'
                          ? '发送补充信息'
                          : hasMessage
                            ? '发送并继续'
                            : '继续执行'}
                    </Button>
                  </div>
                  {actionError && <ErrorNotice error={actionError} />}
                </div>
              )}
              {state === 'closed' && (
                <div className="session-closed-note">
                  <Stop size={16} />
                  会话已结束，无法再追加信息；历史记录和产物会继续保留。
                </div>
              )}
            </TabPanel>
            <TabPanel>
              <div className="session-raw-logs">{rawLogs}</div>
            </TabPanel>
          </TabPanels>
        </Tabs>
      </div>
      <Modal
        open={closing}
        danger
        modalHeading="结束任务并释放工作目录？"
        primaryButtonText="结束任务"
        secondaryButtonText="返回"
        primaryButtonDisabled={busy}
        onRequestClose={() => setClosing(false)}
        onRequestSubmit={() =>
          void (async () => {
            if (await perform(`/api/runs/${id}/close`)) setClosing(false);
          })()
        }
      >
        <p>
          这会停止当前执行，并清理会话和仓库工作目录。对话记录、日志和已收集的产物会保留，之后无法继续这个会话。
        </p>
        <p className="close-session-note">
          如果还需要补充信息，请使用“中断当前轮次”，它会保留工作现场。
        </p>
        <ErrorNotice error={actionError} />
      </Modal>
    </section>
  );
}

function TimelineCard({ item, feedback = false }: { item: TimelineItem; feedback?: boolean }) {
  if (item.type === 'tool') {
    return (
      <details className={`tool-card tool-${item.status}`}>
        <summary>
          <Terminal size={18} />
          <span className="tool-card-title">
            <strong>{toolLabel(item.tool)}</strong>
            <code>{item.command || item.text || '工具调用'}</code>
          </span>
          <Status value={item.status || 'inProgress'} />
          <span className="tool-expand-label">展开</span>
        </summary>
        <div className="tool-card-body">
          {item.command && (
            <>
              <span className="eyebrow">COMMAND</span>
              <pre>{item.command}</pre>
            </>
          )}
          {item.output ? (
            <>
              <span className="eyebrow">OUTPUT</span>
              <pre>{item.output}</pre>
            </>
          ) : (
            <p className="muted">
              {item.status === 'inProgress' ? '等待工具输出…' : '此工具没有文本输出。'}
            </p>
          )}
          {item.text && item.text !== item.command && (
            <p className="tool-description">{item.text}</p>
          )}
          {item.truncated && <p className="timeline-truncated">内容较长，此处显示最近 256 KiB。</p>}
          <time>{formatTime(item.createdAt)}</time>
        </div>
      </details>
    );
  }
  if (item.type === 'message') {
    const human = item.role === 'user';
    return (
      <article
        className={`conversation-message message-${human ? 'user' : item.role || 'assistant'}`}
      >
        <div className="message-avatar">{human ? <UserAvatar size={19} /> : <Bot size={19} />}</div>
        <div className="message-main">
          <div className="message-meta">
            <strong>
              {feedback
                ? '反馈输入'
                : human
                  ? '人工输入'
                  : item.role === 'system'
                    ? '系统'
                    : 'Agent'}
            </strong>
            <time>{formatTime(item.createdAt)}</time>
            {item.status && (
              <Status
                value={item.status}
                label={human && item.status === 'queued' ? '等待接收' : undefined}
              />
            )}
          </div>
          <div className="message-content">
            {item.text || (item.status === 'streaming' ? '正在输出…' : '（空消息）')}
          </div>
          {item.deliveryError && (
            <p className="message-delivery-error">
              信息未送达：{item.deliveryError}。可以在当前轮次结束后重新发送。
            </p>
          )}
          {item.truncated && <p className="timeline-truncated">内容较长，此处显示最近 256 KiB。</p>}
        </div>
      </article>
    );
  }
  if (item.type === 'error')
    return (
      <InlineNotification
        className="timeline-error"
        kind="error"
        title="执行提示"
        subtitle={item.text || item.output || '发生执行错误，请查看原始日志。'}
        hideCloseButton
        lowContrast
      />
    );
  return item.text ? (
    <div className="timeline-status">
      <span />
      <p>{item.text}</p>
      <time>{formatTime(item.createdAt)}</time>
    </div>
  ) : null;
}

function toolLabel(name: string) {
  return (
    (
      {
        commandExecution: '执行命令',
        mcpToolCall: 'MCP 工具',
        fileChange: '文件修改',
        webSearch: '网页搜索',
      } as Record<string, string>
    )[name] ||
    name ||
    '工具调用'
  );
}
