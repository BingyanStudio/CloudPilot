import assert from 'node:assert/strict';
import test from 'node:test';
import { emptyTimeline, mergeTimeline, settledItem } from '../src/sessionTimeline.ts';

function event(id, fields = {}) {
  return {
    id,
    runId: 1,
    turnId: 10,
    type: 'message',
    itemId: 'assistant-1',
    role: 'assistant',
    status: 'streaming',
    text: '',
    tool: '',
    command: '',
    output: '',
    delta: true,
    controlId: 0,
    createdAt: '2026-10-31T12:00:00Z',
    ...fields,
  };
}

test('streamed text is deduplicated, completed snapshots replace it, and turns stay separate', () => {
  let state = mergeTimeline(emptyTimeline(), [
    event(2, { text: ' world' }),
    event(1, { text: 'Hello' }),
  ]);
  state = mergeTimeline(state, [
    event(2, { text: ' world' }),
    event(3, { text: 'Hello world!', delta: false, status: 'completed' }),
  ]);
  state = mergeTimeline(state, [event(4, { turnId: 11, text: 'Next turn' })]);
  assert.equal(state.cursor, 4);
  assert.equal(state.items.length, 2);
  assert.equal(state.items[0].text, 'Hello world!');
  assert.equal(state.items[0].status, 'completed');
  assert.equal(state.items[1].text, 'Next turn');
});

test('tool output merges into one card and a completed output does not duplicate streaming output', () => {
  const state = mergeTimeline(emptyTimeline(), [
    event(1, {
      type: 'tool',
      itemId: 'tool-1',
      tool: 'commandExecution',
      command: 'go test ./...',
      status: 'inProgress',
      delta: false,
    }),
    event(2, { type: 'tool', itemId: 'tool-1', output: 'ok\n' }),
    event(3, {
      type: 'tool',
      itemId: 'tool-1',
      output: 'ok\n',
      status: 'completed',
      delta: false,
    }),
  ]);
  assert.equal(state.items.length, 1);
  assert.equal(state.items[0].command, 'go test ./...');
  assert.equal(state.items[0].output, 'ok\n');
  assert.equal(state.items[0].status, 'completed');
});

test('a queued human message only becomes delivered after an acknowledgement', () => {
  let state = mergeTimeline(emptyTimeline(), [
    event(1, {
      itemId: 'human-1',
      role: 'user',
      text: 'Please check Android 14',
      delta: false,
      status: 'queued',
    }),
  ]);
  assert.equal(state.items[0].status, 'queued');
  state = mergeTimeline(state, [
    event(2, {
      type: 'control',
      itemId: '',
      controlId: 1,
      status: 'rejected',
      text: 'Turn already ended',
    }),
  ]);
  assert.equal(state.items.length, 1);
  assert.equal(state.items[0].status, 'rejected');
  assert.equal(state.items[0].deliveryError, 'Turn already ended');
  state = mergeTimeline(state, [
    event(3, { type: 'control', controlId: 1, status: 'accepted', text: '' }),
  ]);
  assert.equal(state.items[0].status, 'accepted');
  assert.equal(state.items[0].deliveryError, '');
});

test('interrupted/recovered turns settle incomplete cards without changing completed tools', () => {
  const state = mergeTimeline(emptyTimeline(), [
    event(1, {
      type: 'tool',
      itemId: 'active-tool',
      status: 'inProgress',
      output: 'running...',
    }),
    event(2, {
      itemId: 'active-message',
      status: 'streaming',
      text: 'Checking ',
    }),
    event(3, {
      type: 'tool',
      itemId: 'done-tool',
      status: 'completed',
      output: 'ok',
    }),
    event(4, {
      role: 'user',
      itemId: 'pending-input',
      status: 'queued',
      text: 'extra context',
    }),
  ]);
  assert.equal(settledItem(state.items[0], 'interrupted').status, 'interrupted');
  assert.equal(settledItem(state.items[1], 'failed').status, 'failed');
  assert.equal(settledItem(state.items[2], 'interrupted').status, 'completed');
  assert.equal(settledItem(state.items[3], 'interrupted').status, 'unconfirmed');
  assert.equal(settledItem(state.items[0], 'running').status, 'inProgress');
});
