import { readFile, writeFile } from 'node:fs/promises';
import { createRequire } from 'node:module';
import { parseArgs } from 'node:util';
import { setTimeout as delay } from 'node:timers/promises';

const { values } = parseArgs({
  options: {
    name: { type: 'string', default: 'cloudpilot-codex' },
    image: { type: 'string', default: 'node:24-bookworm-slim' },
    domain: { type: 'string', default: process.env.NOVITA_DOMAIN || 'us-phx-1.sandbox.novita.ai' },
    'api-url': { type: 'string' },
    'key-file': { type: 'string', default: process.env.NOVITA_API_KEY_FILE },
    output: { type: 'string' },
    help: { type: 'boolean' },
  },
});

if (values.help) {
  console.log('Build a Novita worker template. Set NOVITA_API_KEY or pass --key-file PATH.\nOptions: --name NAME --image IMAGE --domain DOMAIN --api-url URL --output PATH');
  process.exit(0);
}

let apiKey = process.env.NOVITA_API_KEY?.trim();
if (values['key-file']) apiKey = (await readFile(values['key-file'], 'utf8')).trim();
if (!apiKey) throw new Error('Set NOVITA_API_KEY or pass --key-file PATH');
const redact = value => String(value).replaceAll(apiKey, '[REDACTED]');
let result;

try {
  const { Novita } = createRequire(import.meta.url)('novita-sandbox');
  const novita = new Novita({ apiKey, domain: values.domain, ...(values['api-url'] ? { apiUrl: values['api-url'] } : {}) });
  const template = novita.template.new()
    .fromImage(values.image)
    .setUser('root')
    .runCmd((await readFile(new URL('install-worker.sh', import.meta.url), 'utf8')).replaceAll('\r\n', '\n'))
    .setUser('user')
    .setWorkdir('/workspace')
    .setStartCmd('sleep infinity', [
      'test "$(id -un)" = user',
      'test -w /workspace && test -w /output && test -w /tmp && test -w "$HOME"',
      'test -r /proc/self/stat && test -r /proc/self/environ',
      'command -v sh bash tar find rm mkdir',
      'node --version && codex --version && codex app-server --help >/dev/null && git --version && gh --version',
    ].join(' && '));
  const build = await novita.template.buildInBackground(template, values.name, { cpuCount: 2, memoryMB: 2048 });
  result = { ...build, domain: values.domain, apiUrl: values['api-url'] || `https://api.${values.domain}`, image: values.image, status: 'building' };
  if (values.output) await writeFile(values.output, JSON.stringify({ ...result, status: 'building' }, null, 2) + '\n', { mode: 0o600 });
  console.error(JSON.stringify({ templateId: build.templateId, buildId: build.buildId, status: 'building' }));
  const deadline = Date.now() + 20 * 60_000;
  let logsOffset = 0;
  while (Date.now() < deadline) {
    const status = await novita.template.getBuildStatus(build, { logsOffset });
    result.status = status.status;
    for (const entry of status.logEntries ?? []) console.error(redact(entry.message ?? JSON.stringify(entry)));
    logsOffset += (status.logEntries ?? []).length;
    if (status.status === 'error') throw new Error(status.reason?.message || 'Template build failed');
    if (status.status === 'ready') {
      const output = JSON.stringify({ ...result, status: 'ready' }, null, 2) + '\n';
      if (values.output) await writeFile(values.output, output, { mode: 0o600 });
      process.stdout.write(output);
      process.exit(0);
    }
    await delay(5000);
  }
  throw new Error(`Build is still pending after 20 minutes; inspect template ${build.templateId}, build ${build.buildId} in Novita before retrying`);
} catch (error) {
  if (result && values.output) await writeFile(values.output, JSON.stringify({ ...result, error: redact(error.message) }, null, 2) + '\n', { mode: 0o600 });
  console.error(redact(error.message));
  process.exitCode = 1;
}
