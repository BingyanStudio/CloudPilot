import assert from 'node:assert/strict';
import { spawnSync } from 'node:child_process';
import { mkdir, mkdtemp, readFile, rm, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import path from 'node:path';
import test from 'node:test';

test('template builder handles help, provisioning, build failures and credential redaction', async t => {
  const directory = await mkdtemp(path.join(tmpdir(), 'cloudpilot-template-test-'));
  t.after(() => rm(directory, { recursive: true, force: true }));
  const builder = path.join(directory, 'build.mjs');
  await writeFile(builder, await readFile(new URL('build.mjs', import.meta.url)));
  await writeFile(path.join(directory, 'install-worker.sh'),
    (await readFile(new URL('install-worker.sh', import.meta.url), 'utf8')).replace(/\r?\n/g, '\r\n'));
  const run = (args, env = {}) => spawnSync(process.execPath, [builder, ...args], {
    encoding: 'utf8', env: { ...process.env, NOVITA_API_KEY: '', NOVITA_API_KEY_FILE: '', NODE_PATH: '', ...env },
  });
  const help = run(['--help']);
  assert.equal(help.status, 0, help.stderr);
  assert.match(help.stdout, /--name NAME/);
  assert.match(run([]).stderr, /Set NOVITA_API_KEY or pass --key-file/);

  const sdkDirectory = path.join(directory, 'node_modules', 'novita-sandbox');
  await mkdir(sdkDirectory, { recursive: true });
  await writeFile(path.join(sdkDirectory, 'index.js'), `
    const assert = require('node:assert/strict');
    const { writeFileSync } = require('node:fs');
    exports.Novita = class {
      constructor(config) {
        assert.equal(config.apiKey, 'test-secret');
        const definition = [];
        const builder = Object.fromEntries(['fromImage', 'setUser', 'runCmd', 'setWorkdir', 'setStartCmd'].map(name =>
          [name, (...args) => { definition.push([name, ...args]); return builder; }]));
        this.template = {
          new: () => builder,
          buildInBackground: async (_, name, resources) => {
            writeFileSync(process.env.CAPTURE, JSON.stringify({ config, definition, name, resources }));
            return { templateId: 'template-test', buildId: 'build-test' };
          },
          getBuildStatus: async () => ({
            status: process.env.BUILD_STATUS || 'ready', logEntries: [{ message: config.apiKey }],
            reason: { message: 'Failed: ' + config.apiKey },
          }),
        };
      }
    };
  `);
  const output = path.join(directory, 'result.json');
  const keyFile = path.join(directory, 'key');
  await writeFile(keyFile, 'test-secret\r\n');
  const env = { NODE_PATH: path.join(directory, 'node_modules'), CAPTURE: path.join(directory, 'capture.json') };
  const args = ['--key-file', keyFile, '--output', output, '--name', 'cloudpilot-test',
    '--image', 'node:24-bookworm-slim', '--domain', 'example.test', '--api-url', 'https://api.example.test'];
  const ready = run(args, env);
  assert.equal(ready.status, 0, ready.stderr);
  const result = JSON.parse(ready.stdout);
  assert.equal(result.templateId, 'template-test');
  assert.equal(result.status, 'ready');
  assert.deepEqual(JSON.parse(await readFile(output, 'utf8')), result);
  assert.ok(!ready.stderr.includes('test-secret'));
  assert.match(ready.stderr, /\[REDACTED\]/);
  const capture = JSON.parse(await readFile(env.CAPTURE, 'utf8'));
  assert.ok(!JSON.stringify(capture.definition).includes('test-secret'));
  assert.deepEqual(capture.config, { apiKey: 'test-secret', domain: 'example.test', apiUrl: 'https://api.example.test' });
  assert.equal(capture.name, 'cloudpilot-test');
  assert.deepEqual(capture.resources, { cpuCount: 8, memoryMB: 16384 });
  assert.deepEqual(capture.definition.filter(([step]) => step === 'setUser'), [['setUser', 'root'], ['setUser', 'user']]);
  assert.deepEqual(capture.definition.find(([step]) => step === 'setWorkdir'), ['setWorkdir', '/workspace']);
  const install = capture.definition.find(([step]) => step === 'runCmd')[1];
  assert.ok(!install.includes('\r'));
  assert.match(install, /bash coreutils findutils tar git/);
  const [, start, check] = capture.definition.find(([step]) => step === 'setStartCmd');
  assert.equal(start, 'sleep infinity');
  for (const requirement of ['id -un', '/workspace', '/output', '/tmp', '$HOME', '/proc/self/stat',
    '/proc/self/environ', 'codex app-server --help', 'git --version', 'gh --version']) assert.ok(check.includes(requirement), requirement);

  const failed = run(args, { ...env, BUILD_STATUS: 'error' });
  assert.equal(failed.status, 1);
  assert.equal(failed.stdout, '');
  const failure = await readFile(output, 'utf8');
  assert.equal(JSON.parse(failure).status, 'error');
  assert.ok(!failure.includes('test-secret'));
  assert.ok(!failed.stderr.includes('test-secret'));
  assert.match(JSON.parse(failure).error, /\[REDACTED\]/);
});
