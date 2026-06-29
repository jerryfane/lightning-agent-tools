import {mkdir, readdir, readFile, rm, writeFile} from 'node:fs/promises';
import path from 'node:path';
import {fileURLToPath} from 'node:url';

const __dirname = path.dirname(fileURLToPath(import.meta.url));
const siteRoot = path.resolve(__dirname, '..');
const repoRoot = path.resolve(siteRoot, '..');
const sourceDocs = path.join(repoRoot, 'docs');
const sourceSkills = path.join(repoRoot, 'skills');
const generatedRoot = path.join(siteRoot, 'docs', 'generated');
const generatedSkillsRoot = path.join(generatedRoot, 'skills');
const repoSourceBase =
  'https://github.com/jerryfane/lightning-agent-tools/blob/main';

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

function parseFrontMatterValue(value) {
  const trimmed = value.trim();
  if (!trimmed) {
    return '';
  }

  if (
    (trimmed.startsWith('"') && trimmed.endsWith('"')) ||
    (trimmed.startsWith("'") && trimmed.endsWith("'"))
  ) {
    try {
      return JSON.parse(trimmed);
    } catch {
      return trimmed.slice(1, -1);
    }
  }

  if (trimmed === 'true') {
    return true;
  }

  if (trimmed === 'false') {
    return false;
  }

  return trimmed;
}

function parseFrontMatter(markdown) {
  const match = markdown.match(/^---\r?\n([\s\S]*?)\r?\n---[ \t]*\r?\n?/);
  if (!match) {
    return {body: markdown, data: {}};
  }

  const data = {};
  for (const line of match[1].split(/\r?\n/)) {
    const entry = line.match(/^([A-Za-z0-9_-]+):\s*(.*)$/);
    if (!entry) {
      continue;
    }

    data[entry[1]] = parseFrontMatterValue(entry[2]);
  }

  return {body: markdown.slice(match[0].length), data};
}

function valueAsString(value) {
  return typeof value === 'string' && value.trim() ? value.trim() : '';
}

function singleLine(value) {
  return value.replace(/\s+/g, ' ').trim();
}

function escapeInlineCodeTablePipes(line) {
  let inInlineCode = false;
  let escaped = '';

  for (const char of line) {
    if (char === '`') {
      inInlineCode = !inInlineCode;
      escaped += char;
      continue;
    }

    if (inInlineCode && char === '|') {
      escaped += '\\|';
      continue;
    }

    escaped += char;
  }

  return escaped;
}

function toDocusaurusMarkdown(markdown) {
  let inFence = false;

  return markdown
    .split('\n')
    .map((line) => {
      if (line.trimStart().startsWith('```')) {
        inFence = !inFence;
        return line;
      }

      if (!inFence && line.trimStart().startsWith('|')) {
        return escapeInlineCodeTablePipes(line);
      }

      return line;
    })
    .join('\n');
}

function rewriteSkillReferenceLinks(markdown, skillDirName) {
  return markdown.replace(
    /\]\((\.?\/?references\/[^)\s]+\.md(?:#[^)]+)?)\)/g,
    (match, target) => {
      const sourcePath = target.replace(/^\.\//, '');
      return `](${repoSourceBase}/skills/${skillDirName}/${sourcePath})`;
    },
  );
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

async function listSkillFiles() {
  const entries = await readdir(sourceSkills, {withFileTypes: true});
  const skills = [];

  for (const entry of entries) {
    if (!entry.isDirectory()) {
      continue;
    }

    const skillPath = path.join(sourceSkills, entry.name, 'SKILL.md');
    try {
      await readFile(skillPath, 'utf8');
      skills.push({dirName: entry.name, sourcePath: skillPath});
    } catch (error) {
      if (error.code === 'ENOENT') {
        continue;
      }

      throw error;
    }
  }

  if (skills.length === 0) {
    throw new Error(`No skills found in ${sourceSkills}`);
  }

  return skills.sort((a, b) => a.dirName.localeCompare(b.dirName));
}

async function writeCategoryMetadata() {
  await writeFile(
    path.join(generatedRoot, '_category_.json'),
    `${JSON.stringify({label: 'Generated Docs', position: 10}, null, 2)}\n`,
  );
}

async function writeSkillCategoryMetadata() {
  await writeFile(
    path.join(generatedSkillsRoot, '_category_.json'),
    `${JSON.stringify({label: 'Skill Reference', position: 20}, null, 2)}\n`,
  );
}

async function generateRepositoryDocs() {
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

async function generateSkillDocs() {
  const skills = await listSkillFiles();
  await mkdir(generatedSkillsRoot, {recursive: true});
  await writeSkillCategoryMetadata();

  const skillIndex = [];
  for (const skill of skills) {
    const markdown = await readFile(skill.sourcePath, 'utf8');
    const {body: markdownBody, data} = parseFrontMatter(markdown);
    const name = valueAsString(data.name) || skill.dirName;
    const description =
      singleLine(valueAsString(data.description)) ||
      `Generated from skills/${skill.dirName}/SKILL.md`;
    const outputPath = path.join(generatedSkillsRoot, `${skill.dirName}.md`);
    const docusaurusBody = toDocusaurusMarkdown(
      rewriteSkillReferenceLinks(markdownBody, skill.dirName),
    );
    const output = [
      '---',
      `title: ${JSON.stringify(name)}`,
      `sidebar_label: ${JSON.stringify(name)}`,
      `description: ${JSON.stringify(description)}`,
      '---',
      '',
      docusaurusBody.trimEnd(),
      '',
    ].join('\n');

    await writeFile(outputPath, output);
    skillIndex.push({description, dirName: skill.dirName, name});
  }

  const indexBody = [
    '---',
    'title: "Skill Reference"',
    'sidebar_label: "Skill Reference"',
    'description: "Generated reference pages for every skills/*/SKILL.md file."',
    '---',
    '',
    '# Skill Reference',
    '',
    'These pages are generated from `skills/*/SKILL.md` before the docs site starts or builds.',
    '',
    ...skillIndex.flatMap((skill) => [
      `- [${skill.name}](/generated/skills/${skill.dirName}) - ${skill.description}`,
    ]),
    '',
  ].join('\n');

  await writeFile(path.join(generatedSkillsRoot, 'index.md'), indexBody);
}

async function generate() {
  await rm(generatedRoot, {recursive: true, force: true});
  await mkdir(generatedRoot, {recursive: true});
  await writeCategoryMetadata();
  await generateRepositoryDocs();
  await generateSkillDocs();
}

generate().catch((error) => {
  console.error(error);
  process.exitCode = 1;
});
