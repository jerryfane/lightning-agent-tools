import {mkdir, readdir, readFile, rm, writeFile} from 'node:fs/promises';
import path from 'node:path';
import {fileURLToPath} from 'node:url';

const __dirname = path.dirname(fileURLToPath(import.meta.url));
const siteRoot = path.resolve(__dirname, '..');
const repoRoot = path.resolve(siteRoot, '..');
const sourceDocs = path.join(repoRoot, 'docs');
const generatedRoot = path.join(siteRoot, 'docs', 'generated');

function titleFromMarkdown(markdown, fallback) {
  const heading = markdown.match(/^#\s+(.+)$/m);
  if (heading) {
    return heading[1].trim().replace(/\s+#*$/, '');
  }

  return fallback
    .replace(/\.md$/i, '')
    .split(/[-_]/)
    .filter(Boolean)
    .map((part) => part.charAt(0).toUpperCase() + part.slice(1))
    .join(' ');
}

async function listMarkdownFiles(root, current = root) {
  const entries = await readdir(current, {withFileTypes: true});
  const files = [];

  for (const entry of entries) {
    const absolute = path.join(current, entry.name);
    if (entry.isDirectory()) {
      files.push(...(await listMarkdownFiles(root, absolute)));
      continue;
    }

    if (entry.isFile() && entry.name.endsWith('.md')) {
      files.push(path.relative(root, absolute));
    }
  }

  return files.sort((a, b) => a.localeCompare(b));
}

async function writeCategoryMetadata() {
  await writeFile(
    path.join(generatedRoot, '_category_.json'),
    `${JSON.stringify({label: 'Generated Docs', position: 10}, null, 2)}\n`,
  );
}

async function generate() {
  await rm(generatedRoot, {recursive: true, force: true});
  await mkdir(generatedRoot, {recursive: true});
  await writeCategoryMetadata();

  const markdownFiles = await listMarkdownFiles(sourceDocs);

  for (const relativePath of markdownFiles) {
    const sourcePath = path.join(sourceDocs, relativePath);
    const outputPath = path.join(generatedRoot, relativePath);
    const markdown = await readFile(sourcePath, 'utf8');
    const title = titleFromMarkdown(markdown, path.basename(relativePath));
    const body = [
      '---',
      `title: ${JSON.stringify(title)}`,
      `description: ${JSON.stringify(`Generated from docs/${relativePath}`)}`,
      '---',
      '',
      markdown.trimEnd(),
      '',
    ].join('\n');

    await mkdir(path.dirname(outputPath), {recursive: true});
    await writeFile(outputPath, body);
  }
}

generate().catch((error) => {
  console.error(error);
  process.exitCode = 1;
});
