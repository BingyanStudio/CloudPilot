#!/usr/bin/env node
import fs from 'node:fs';
import { spawnSync } from 'node:child_process';
if (process.env.FIXTURE_WAIT) {
  await new Promise(resolve => setTimeout(resolve, 60000));
}
let prompt = '';
for await (const chunk of process.stdin) prompt += chunk;
if (!prompt.includes('fixture')) throw new Error('No feedback received');
const config = fs.readFileSync('/workspace/.codex/config.toml', 'utf8');
if (!config.includes('model_provider = "cloudpilot"')) throw new Error('No Codex configuration');
if (process.env.GH_TOKEN) {
  const credential = spawnSync('git', ['credential', 'fill'], {input:'protocol=https\nhost=github.com\n\n', encoding:'utf8'});
  if (credential.status !== 0 || !credential.stdout.includes('password=' + process.env.GH_TOKEN)) throw new Error('GitHub credential helper failed');
  const otherHost = spawnSync('git', ['credential', 'fill'], {input:'protocol=https\nhost=example.com\n\n', encoding:'utf8'});
  if (otherHost.stdout.includes(process.env.GH_TOKEN)) throw new Error('GitHub credential was supplied to another host');
}
console.log(JSON.stringify({type: 'fixture', secret: process.env.OPENAI_API_KEY}));
fs.writeFileSync('/output/app.apk', 'fixture APK');
fs.writeFileSync('/output/artifacts.json', JSON.stringify({artifacts:[
  {kind:'file', name:'APK', path:'app.apk'},
  {kind:'pr', name:'Fix', url:'https://github.com/example/app/pull/1'},
]}));
fs.writeFileSync('/output/summary.md', 'Fixture completed');
