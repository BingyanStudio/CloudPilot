import { readFile } from 'node:fs/promises';
import { execute, instructions, prepareWorkspace } from './cloudpilot-workspace.mjs';

const task = JSON.parse(await readFile('/tmp/cloudpilot-task.json', 'utf8'));
try {
  await prepareWorkspace(task);
  const prompt = instructions(task) + '\n\nFeedback event JSON follows:\n' + JSON.stringify(task.event, null, 2);
  const args = ['exec', '--json', '--skip-git-repo-check', '--dangerously-bypass-approvals-and-sandbox', '--output-last-message', '/output/summary.md'];
  if (task.worker.model) args.push('--model', task.worker.model);
  args.push('-');
  await execute('codex', args, { input: prompt, env: { ...process.env, CODEX_HOME: '/workspace/.codex' } });
} catch (error) {
  console.error(error.message);
  process.exitCode = 1;
}
