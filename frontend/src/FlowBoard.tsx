import { useState } from 'react';
import { kindLabels, type Kind, type Resource } from './types';

const nodeWidth = 210;
const nodeHeight = 46;
const gap = 12;
const column = 310;
const top = 40;
const colors: Record<Kind, string> = {
  Input: '#0f62fe',
  Pipeline: '#8a3ffc',
  Worker: '#198038',
  Workspace: '#007d79',
  Runner: '#b28600',
};

interface Node {
  key: string;
  item: Resource;
  x: number;
  y: number;
}

function subtitle(item: Resource): string {
  const spec = item.spec;
  switch (item.kind) {
    case 'Input':
      return String(spec.type);
    case 'Pipeline':
      return String(spec.schedule || '即时调度');
    case 'Worker':
      return String(spec.model || spec.type);
    case 'Workspace':
      return `${((spec.repositories || []) as unknown[]).length} 个仓库`;
    case 'Runner':
      return String(spec.type);
  }
}

const clip = (value: string, length: number) =>
  value.length > length ? `${value.slice(0, length - 1)}…` : value;

// Lays resources out by dependency: Input → Pipeline → Worker / Workspace / Runner.
export default function FlowBoard({ resources }: { resources: Resource[] }) {
  const [active, setActive] = useState('');
  const nodes = new Map<string, Node>();
  const labels: { text: string; x: number; y: number }[] = [];
  const place = (kind: Kind, x: number, y: number) => {
    for (const item of resources.filter((resource) => resource.kind === kind)) {
      nodes.set(`${kind}/${item.metadata.name}`, { key: `${kind}/${item.metadata.name}`, item, x, y });
      y += nodeHeight + gap;
    }
    return y;
  };
  labels.push({ text: kindLabels.Input, x: 0, y: 16 }, { text: kindLabels.Pipeline, x: column, y: 16 });
  let height = Math.max(place('Input', 0, top), place('Pipeline', column, top));
  let y = top - 24;
  for (const kind of ['Worker', 'Workspace', 'Runner'] as const) {
    labels.push({ text: kindLabels[kind], x: column * 2, y: y + 16 });
    y = place(kind, column * 2, y + 24) + gap;
  }
  height = Math.max(height, y) + gap;
  const edges: { from: Node; to: Node }[] = [];
  const link = (from: Node, kind: Kind, ref: unknown) => {
    const to = nodes.get(`${kind}/${String(ref)}`);
    if (to) edges.push({ from, to });
  };
  for (const node of nodes.values()) {
    const spec = node.item.spec;
    if (node.item.kind === 'Input') link(node, 'Pipeline', spec.pipelineRef);
    if (node.item.kind === 'Pipeline') {
      link(node, 'Worker', spec.workerRef);
      link(node, 'Workspace', spec.workspaceRef);
      link(node, 'Runner', spec.runnerRef);
    }
  }
  const width = column * 2 + nodeWidth;
  return (
    <div className="panel flow-board">
      <svg
        style={{ minWidth: width, maxWidth: width * 1.4 }}
        viewBox={`0 0 ${width} ${height}`}
        role="group"
        aria-label="资源关系图"
      >
        {labels.map((label) => (
          <text key={label.text} className="flow-column" x={label.x} y={label.y}>
            {label.text}
          </text>
        ))}
        {edges.map(({ from, to }) => {
          const x1 = from.x + nodeWidth;
          const y1 = from.y + nodeHeight / 2;
          const x2 = to.x;
          const y2 = to.y + nodeHeight / 2;
          const state = !active
            ? ''
            : active === from.key || active === to.key
              ? 'is-active'
              : 'is-dim';
          return (
            <path
              key={`${from.key}>${to.key}`}
              className={`flow-edge ${state}`}
              d={`M ${x1} ${y1} C ${x1 + 50} ${y1}, ${x2 - 50} ${y2}, ${x2} ${y2}`}
            />
          );
        })}
        {[...nodes.values()].map(({ key, item, x, y }) => (
          <a
            key={key}
            href={`#/resources/${item.kind}/${encodeURIComponent(item.metadata.name)}`}
            aria-label={`${kindLabels[item.kind]} ${item.metadata.name}`}
            className={`flow-node ${item.spec.paused ? 'is-paused' : ''}`}
            onMouseEnter={() => setActive(key)}
            onMouseLeave={() => setActive('')}
            onFocus={() => setActive(key)}
            onBlur={() => setActive('')}
          >
            <title>{`${kindLabels[item.kind]} · ${item.metadata.name}${item.spec.paused ? '（已暂停）' : ''}`}</title>
            <rect x={x} y={y} width={nodeWidth} height={nodeHeight} />
            <rect x={x} y={y} width={3} height={nodeHeight} fill={colors[item.kind]} />
            <text className="flow-name" x={x + 14} y={y + 19}>
              {clip(item.metadata.name, 26)}
            </text>
            <text className="flow-subtitle" x={x + 14} y={y + 35}>
              {clip(subtitle(item), 30)}
            </text>
          </a>
        ))}
      </svg>
    </div>
  );
}
