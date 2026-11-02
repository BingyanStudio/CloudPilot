import assert from 'node:assert/strict';
import test from 'node:test';
import { artifactHref } from '../src/artifactLinks.ts';

test('agent /output links resolve to collected artifact downloads', () => {
  const artifacts = [
    { id: 7, kind: 'file', outputPath: 'Hustoday-1.7.0-arm64-release.apk' },
    { id: 8, kind: 'file', outputPath: 'logs/build.log' },
    { id: 9, kind: 'text' },
  ];
  assert.equal(artifactHref('/output/Hustoday-1.7.0-arm64-release.apk', artifacts), '/api/artifacts/7/download');
  assert.equal(artifactHref('sandbox:/output/logs/build.log', artifacts), '/api/artifacts/8/download');
  assert.equal(artifactHref('/output/build.log', artifacts), '/api/artifacts/8/download');
  assert.equal(artifactHref('/output/missing.zip', artifacts), '');
  assert.equal(artifactHref('https://example.com/output/a', artifacts), undefined);
});
