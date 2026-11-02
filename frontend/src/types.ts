// Ordered by dependency: each kind only references kinds listed before it.
export const kinds = ['Runner', 'Workspace', 'Worker', 'Pipeline', 'Input'] as const;
export type Kind = (typeof kinds)[number];
export type Spec = Record<string, unknown>;
export type Role = 'admin' | 'editor' | 'viewer';
export const roleLabels: Record<Role, string> = {
  admin: '管理员',
  editor: '编辑者',
  viewer: '查看者',
};
export interface User {
  id: number;
  username: string;
  role: Role;
  sso: boolean;
  createdAt: string;
}
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
export interface EventDetail {
  event: Event;
  runs: Run[];
}
export interface GitHubInput {
  repository: string;
  token?: string;
  tokenSecretRef?: string;
  assignees: string[];
  labels: string[];
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
  outputPath?: string;
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
  Runner: '运行设施',
  Workspace: '工作空间',
  Worker: '智能体',
  Pipeline: '流水线',
  Input: '接入点',
};

export function runnerSpec(type: string): Spec {
  if (type === 'e2b')
    return {
      type,
      e2b: {
        provider: 'e2b',
        domain: 'e2b.app',
        apiKey: '',
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
      secret: '',
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
      apiKey: '',
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
