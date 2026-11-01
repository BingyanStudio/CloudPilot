import { spawn } from 'node:child_process';
import { randomUUID } from 'node:crypto';
import { closeSync, openSync, readFileSync, readdirSync, unlinkSync, writeFileSync } from 'node:fs';
import { constants } from 'node:os';
import { setTimeout as delay } from 'node:timers/promises';
import { pathToFileURL } from 'node:url';

const defaultRecord = '/tmp/cloudpilot-process-group';
const tokenVariable = 'CLOUDPILOT_PROCESS_TOKEN';

function processInfo(pid) {
  try {
    const stat = readFileSync(`/proc/${pid}/stat`, 'utf8');
    const fields = stat.slice(stat.lastIndexOf(')') + 2).split(' ');
    return { pid: Number(pid), state: fields[0], group: Number(fields[2]), startTime: fields[19] };
  } catch (error) {
    if (error.code === 'ENOENT' || error.code === 'ESRCH') return null;
    throw error;
  }
}

function runningProcesses() {
  return readdirSync('/proc').filter(name => /^\d+$/.test(name))
    .map(processInfo).filter(info => info && info.state !== 'Z' && info.state !== 'X');
}

function hasToken(pid, token) {
  try {
    return readFileSync(`/proc/${pid}/environ`, 'utf8').split('\0').includes(`${tokenVariable}=${token}`);
  } catch (error) {
    if (['ENOENT', 'ESRCH', 'EACCES'].includes(error.code)) return false;
    throw error;
  }
}

async function stopGroup(record) {
  if (!Number.isSafeInteger(record.pid) || record.pid <= 1 || typeof record.token !== 'string' || !record.token) {
    throw new Error('Invalid worker process identity');
  }
  const known = new Map();
  let emptyScans = 0;
  const kill = pid => {
    try { process.kill(pid, 'SIGKILL'); }
    catch (error) { if (error.code !== 'ESRCH') throw error; }
  };
  for (let attempt = 0; attempt < 60; attempt++) {
    const processes = runningProcesses();
    const members = processes.filter(info => info.group === record.pid);
    // Detached descendants keep the inherited token even after creating a new session.
    const tagged = processes.filter(info => hasToken(info.pid, record.token));
    const leader = processes.find(info => info.pid === record.pid);
    if (leader && record.startTime && leader.startTime !== record.startTime) {
      throw new Error('Worker process identity changed; refusing to stop another process group');
    }
    if (!members.length && !tagged.length) {
      if (++emptyScans === 2) return;
    } else {
      emptyScans = 0;
      if (members.length) {
        const verified = leader && record.startTime || members.some(info => known.get(info.pid) === info.startTime || tagged.some(item => item.pid === info.pid));
        if (!verified) throw new Error('Worker process identity changed; refusing to stop another process group');
        for (const info of members) known.set(info.pid, info.startTime);
        kill(-record.pid);
      }
      for (const info of tagged) {
        const current = processInfo(info.pid);
        if (current?.startTime === info.startTime && hasToken(info.pid, record.token)) kill(info.pid);
      }
    }
    await delay(50);
  }
  throw new Error('Worker processes did not stop');
}

function readRecord(recordPath) {
  try {
    return JSON.parse(readFileSync(recordPath, 'utf8'));
  } catch (error) {
    if (error.code === 'ENOENT') return null;
    throw error;
  }
}

function removeRecord(recordPath, record) {
  const current = readRecord(recordPath);
  if (current?.pid === record.pid && current?.token === record.token) unlinkSync(recordPath);
}

// Stop the recorded worker group and tagged descendants, retaining unverifiable records.
export async function stopWorker(recordPath = defaultRecord) {
  const record = readRecord(recordPath);
  if (!record) return;
  await stopGroup(record);
  removeRecord(recordPath, record);
}

// Each turn gets its own process group; stdout remains the worker's protocol stream.
export async function startWorker(config, recordPath = defaultRecord) {
  if (typeof config.entrypoint !== 'string' || !config.entrypoint || !Array.isArray(config.args) || config.args.some(arg => typeof arg !== 'string')) {
    throw new Error('Invalid worker process configuration');
  }
  const descriptor = openSync(recordPath, 'wx', 0o600);
  let record;
  let child;
  let cleanup;
  const stop = () => cleanup ??= stopGroup(record);
  const onSignal = () => { void stop().catch(error => { console.error(error.message); }); };
  try {
    const token = randomUUID();
    child = spawn(config.entrypoint, config.args, {
      detached: true, stdio: ['pipe', 'inherit', 'inherit'],
      env: { ...process.env, [tokenVariable]: token },
    });
    const exited = new Promise((resolve, reject) => {
      child.once('error', reject);
      child.once('exit', (code, signal) => resolve(code ?? 128 + (constants.signals[signal] || 0)));
    });
    if (!child.pid) return await exited;
    record = { pid: child.pid, startTime: processInfo(child.pid)?.startTime, token };
    writeFileSync(descriptor, JSON.stringify(record));
    child.stdin.on('error', error => { if (error.code !== 'EPIPE') console.error(error.message); });
    process.on('SIGTERM', onSignal);
    process.on('SIGINT', onSignal);
    process.stdin.pipe(child.stdin);
    return await exited;
  } finally {
    closeSync(descriptor);
    process.removeListener('SIGTERM', onSignal);
    process.removeListener('SIGINT', onSignal);
    if (child) process.stdin.unpipe(child.stdin);
    process.stdin.pause();
    if (record) {
      await stop();
      removeRecord(recordPath, record);
    } else {
      unlinkSync(recordPath);
    }
  }
}

if (process.argv[1] && pathToFileURL(process.argv[1]).href === import.meta.url) {
  try {
    if (process.argv[2] === 'stop') await stopWorker();
    else if (process.argv[2] === 'start') process.exitCode = await startWorker(JSON.parse(readFileSync('/tmp/cloudpilot-process.json', 'utf8')));
    else throw new Error('Expected start or stop');
  } catch (error) {
    console.error(error.message);
    process.exitCode = 1;
  }
}
