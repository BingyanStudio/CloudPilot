import type { SessionEvent } from './types';

export interface TimelineItem extends SessionEvent {
  key: string;
  firstEventId: number;
  lastEventId: number;
  deliveryError: string;
  truncated: boolean;
}

export interface TimelineState {
  cursor: number;
  items: TimelineItem[];
  controls: Record<number, { status: string; error: string }>;
}

const maxItemText = 256 * 1024;
const maxItems = 2000;

export function emptyTimeline(): TimelineState {
  return { cursor: 0, items: [], controls: {} };
}

// A terminal turn cannot leave an unfinished card appearing live after reconnect/recovery.
export function settledItem(item: TimelineItem, turnStatus?: string): TimelineItem {
  if (!turnStatus || !['completed', 'interrupted', 'failed'].includes(turnStatus)) return item;
  if (item.role === 'user' && item.status === 'queued') return { ...item, status: 'unconfirmed' };
  if (!['inProgress', 'streaming', 'running'].includes(item.status)) return item;
  return {
    ...item,
    status: turnStatus === 'completed' ? 'unconfirmed' : turnStatus,
  };
}

// Fold append-only provider events into stable message/tool cards; snapshots replace deltas.
export function mergeTimeline(state: TimelineState, events: SessionEvent[]): TimelineState {
  if (!events.length) return state;
  const items = [...state.items];
  const positions = new Map(items.map((item, index) => [item.key, index]));
  const controls = { ...state.controls };
  let cursor = state.cursor;
  for (const event of [...events].sort((a, b) => a.id - b.id)) {
    if (event.id <= cursor) continue;
    cursor = event.id;
    if (event.type === 'control' && event.controlId) {
      controls[event.controlId] = {
        status: event.status,
        error: event.status === 'rejected' ? event.text : '',
      };
      const index = items.findIndex((item) => item.firstEventId === event.controlId);
      if (index >= 0)
        items[index] = {
          ...items[index],
          status: event.status,
          deliveryError: controls[event.controlId].error,
          lastEventId: event.id,
        };
      continue;
    }
    const key = `${event.type}:${event.turnId}:${event.itemId || event.id}`;
    const index = positions.get(key);
    const previous = index === undefined ? undefined : items[index];
    const text = event.delta
      ? (previous?.text || '') + event.text
      : event.text || previous?.text || '';
    const output = event.delta
      ? (previous?.output || '') + event.output
      : event.output || previous?.output || '';
    const firstEventId = previous?.firstEventId ?? event.id;
    const delivery = controls[firstEventId];
    const item: TimelineItem = {
      ...event,
      key,
      firstEventId,
      lastEventId: event.id,
      role: event.role || previous?.role || '',
      status: delivery?.status || event.status || previous?.status || '',
      text: text.slice(-maxItemText),
      output: output.slice(-maxItemText),
      command: event.command || previous?.command || '',
      tool: event.tool || previous?.tool || '',
      createdAt: previous?.createdAt || event.createdAt,
      deliveryError: delivery?.error || previous?.deliveryError || '',
      truncated: Boolean(
        previous?.truncated || text.length > maxItemText || output.length > maxItemText,
      ),
    };
    if (index === undefined) {
      positions.set(key, items.length);
      items.push(item);
    } else items[index] = item;
  }
  const retained = items.slice(-maxItems);
  const retainedIDs = new Set(retained.map((item) => item.firstEventId));
  for (const key of Object.keys(controls)) {
    if (!retainedIDs.has(Number(key))) delete controls[Number(key)];
  }
  return { cursor, items: retained, controls };
}
