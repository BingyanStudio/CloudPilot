import { spawn } from 'node:child_process';
import { readFile, rm } from 'node:fs/promises';
import { prepareWorkspace } from './cloudpilot-workspace.mjs';

try {
  const task = JSON.parse(await readFile('/tmp/cloudpilot-task.json', 'utf8'));
  await prepareWorkspace(task, true);
  // Each turn publishes a fresh manifest; previous downloaded artifacts remain in control-plane storage.
  await rm('/output/artifacts.json', { force: true });
  await rm('/output/summary.md', { force: true });
  console.log(JSON.stringify({ cloudpilotReady: true }));
  const child = spawn('codex', ['app-server', '--listen', 'stdio://'], { stdio: 'inherit', env: process.env });
  child.on('error', error => { console.error(error.message); process.exitCode = 1; });
  child.on('exit', (code, signal) => { process.exitCode = code ?? (signal ? 1 : 0); });
} catch (error) {
  console.error(error.message);
  process.exitCode = 1;
}
