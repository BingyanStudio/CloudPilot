export interface OutputFile {
  id: number;
  kind: string;
  outputPath?: string;
}

const outputLink = /^(?:sandbox:)?(?:file:\/\/)?\/output\/(.+)$/;

// Resolves agent links such as /output/app.apk to the collected artifact download URL.
// Returns '' for /output links without a collected file and undefined for every other link.
export function artifactHref(href: string, artifacts: OutputFile[]): string | undefined {
  const match = outputLink.exec(href.trim());
  if (!match) return undefined;
  let path: string;
  try {
    path = decodeURIComponent(match[1].split(/[?#]/)[0]).replace(/^\.\//, '');
  } catch {
    return '';
  }
  const files = artifacts.filter((item) => item.kind === 'file' && item.outputPath);
  const found =
    files.find((item) => item.outputPath === path) ||
    files.find((item) => item.outputPath!.split('/').pop() === path.split('/').pop());
  return found ? `/api/artifacts/${found.id}/download` : '';
}
