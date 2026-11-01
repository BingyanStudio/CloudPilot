export const kinds = ['Input', 'Pipeline', 'Worker', 'Workspace', 'Runner'] as const;
export type Kind = (typeof kinds)[number];
export type Spec = Record<string, unknown>;
export interface Resource {
  apiVersion: string;
  kind: Kind;
  metadata: { name: string; generation?: number };
  spec: Spec;
}
export interface Event {
  id: number;
  inputName: string;
  pipelineName: string;
  status: string;
  payload: unknown;
  attempts: number;
  availableAt: string;
  createdAt: string;
  updatedAt: string;
}
export interface Run {
  id: number;
  eventId: number;
  pipelineName: string;
  status: string;
  attempt: number;
  workerName: string;
  workspaceName: string;
  runnerName: string;
  error: string;
  startedAt: string;
  finishedAt: string | null;
  createdAt: string;
}
export interface RunLog {
  id: number;
  runId: number;
  stream: string;
  message: string;
  createdAt: string;
}
export interface Artifact {
  id: number;
  runId: number;
  kind: string;
  name: string;
  url: string;
  content: string;
  contentType: string;
  size: number;
  createdAt: string;
}
export interface RunDetail {
  run: Run;
  event: Event;
  artifacts: Artifact[];
  session: AgentSession | null;
  capabilities: SessionCapabilities;
  turns: AgentTurn[];
}
export interface AgentSession {
  id: number;
  runId: number;
  threadId: string;
  state: string;
  currentTurnId: number;
  workspaceLost: boolean;
  lastError: string;
  createdAt: string;
  updatedAt: string;
  closedAt: string | null;
}
export interface SessionCapabilities {
  interactive: boolean;
  canSend: boolean;
  canInterrupt: boolean;
  canResume: boolean;
  canClose: boolean;
}
export interface AgentTurn {
  id: number;
  runId: number;
  sessionId: number;
  number: number;
  providerTurnId: string;
  input: string;
  initial: boolean;
  userId: number;
  status: string;
  createdAt: string;
  startedAt: string | null;
  finishedAt: string | null;
}
export interface SessionEvent {
  id: number;
  runId: number;
  turnId: number;
  type: string;
  itemId: string;
  role: string;
  status: string;
  text: string;
  tool: string;
  command: string;
  output: string;
  delta: boolean;
  controlId: number;
  createdAt: string;
}
export interface Overview {
  resources: Record<string, number>;
  events: Record<string, number>;
  runs: Record<string, number>;
}
export const kindLabels: Record<Kind, string> = {
  Input: '接入点',
  Pipeline: '流水线',
  Worker: '智能体',
  Workspace: '工作空间',
  Runner: '运行设施',
};
export const kindDescriptions: Record<Kind, string> = {
  Input: '将外部反馈转为持久化事件，交给流水线处理。',
  Pipeline: '为事件配置执行时间、并发限制和运行资源。',
  Worker: '配置 Codex 使用的模型、API 端点和密钥引用。',
  Workspace: '为智能体准备代码仓库、提示词、工具和 MCP 服务。',
  Runner: '使用 Docker 或 E2B 兼容沙箱提供独立的计算环境。',
};

export function runnerSpec(type: string): Spec {
  if (type === 'e2b')
    return {
      type,
      e2b: {
        provider: 'e2b',
        domain: 'e2b.app',
        apiKeySecretRef: 'E2B_API_KEY',
        template: '',
        leaseSeconds: 900,
        idleStrategy: 'pause',
      },
    };
  return {
    type: 'docker',
    image: 'cloudpilot-codex:latest',
    network: 'bridge',
    memory: '2g',
    cpus: '2',
  };
}

export function template(kind: Kind): Resource {
  const specs: Record<Kind, Spec> = {
    Input: {
      type: 'webhook',
      pipelineRef: '',
      secretRef: 'CLOUDPILOT_WEBHOOK_SECRET',
      paused: false,
    },
    Pipeline: {
      workerRef: '',
      workspaceRef: '',
      runnerRef: '',
      schedule: '0 20 * * *',
      timezone: 'Asia/Shanghai',
      concurrency: 1,
      maxRetries: 0,
      timeoutSeconds: 3600,
      paused: false,
    },
    Worker: {
      type: 'codex',
      model: 'gpt-5-codex',
      endpoint: 'https://api.openai.com/v1',
      apiKeySecretRef: 'OPENAI_API_KEY',
      instructions: '',
    },
    Workspace: {
      repositories: [],
      prompt: '',
      tools: [],
      mcpServers: {},
      env: {},
      secretEnv: {},
    },
    Runner: runnerSpec('docker'),
  };
  return {
    apiVersion: 'cloudpilot.dev/v1alpha1',
    kind,
    metadata: { name: '' },
    spec: specs[kind],
  };
}
