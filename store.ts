// store.ts — the forge's in-memory store: git objects, packs, pkt-lines, repos, issues,
// pull requests, and the deterministic clock. No wall clock, no randomness: every
// timestamp derives from a fixed epoch plus a per-process sequence, so two runs on the
// same instance export byte-identical state.

import { deflateSync, inflateSync } from 'node:zlib';
import { createHash } from 'node:crypto';
import { VIEWER, FORGE_IDENTITY } from './scenario.ts';

export const ZERO_OID = '0000000000000000000000000000000000000000';

export type ObjectType = 'commit' | 'tree' | 'blob' | 'tag';

export interface GitObject {
  readonly type: ObjectType;
  readonly data: Buffer;
}

export function isObjectId(value: string): boolean {
  return /^[0-9a-f]{40}$/.test(value);
}

export function hashObject(type: ObjectType, data: Uint8Array): string {
  const header = Buffer.from(`${type} ${data.length}\0`, 'ascii');
  return createHash('sha1').update(header).update(data).digest('hex');
}

// ---------------------------------------------------------------------------------------------
// pkt-line codec (git smart HTTP framing)
// ---------------------------------------------------------------------------------------------

export function pktLine(payload: string | Buffer): Buffer {
  const body = typeof payload === 'string' ? Buffer.from(payload, 'utf8') : payload;
  const length = body.length + 4;
  const header = Buffer.alloc(4);
  header.write(length.toString(16).padStart(4, '0'), 'ascii');
  return Buffer.concat([header, body]);
}

export function pktFlush(): Buffer {
  return Buffer.from('0000', 'ascii');
}

export function pktDelim(): Buffer {
  return Buffer.from('0001', 'ascii');
}

export interface PktPacket {
  readonly kind: 'data' | 'flush' | 'delim' | 'response-end';
  readonly payload: Buffer;
}

/** Decode pkt-lines; `offset` is the byte just past the last packet (start of a pack). */
export function decodePktLines(body: Buffer): { packets: PktPacket[]; offset: number } {
  const packets: PktPacket[] = [];
  let offset = 0;
  while (offset + 4 <= body.length) {
    const length = Number.parseInt(body.toString('ascii', offset, offset + 4), 16);
    if (!Number.isFinite(length)) break;
    if (length === 0) {
      packets.push({ kind: 'flush', payload: Buffer.alloc(0) });
      offset += 4;
      continue;
    }
    if (length === 1) {
      packets.push({ kind: 'delim', payload: Buffer.alloc(0) });
      offset += 4;
      continue;
    }
    if (length === 2) {
      packets.push({ kind: 'response-end', payload: Buffer.alloc(0) });
      offset += 4;
      continue;
    }
    if (offset + length > body.length) break;
    packets.push({ kind: 'data', payload: body.subarray(offset + 4, offset + length) });
    offset += length;
  }
  return { packets, offset };
}

// ---------------------------------------------------------------------------------------------
// zlib stream end: a deflate stream ends with the Adler-32 of its output (big-endian).
// inflateSync ignores trailing bytes, so inflate once, then scan for the checksum bytes.
// ---------------------------------------------------------------------------------------------

function adler32(data: Uint8Array): number {
  let a = 1;
  let b = 0;
  for (let i = 0; i < data.length; i += 1) {
    a = (a + data[i]!) % 65521;
    b = (b + a) % 65521;
  }
  return ((b << 16) | a) >>> 0;
}

export function findZlibEnd(buffer: Buffer, start: number): { end: number; data: Buffer } {
  const data = inflateSync(buffer.subarray(start));
  const checksum = adler32(data);
  const needle = Buffer.alloc(4);
  needle.writeUInt32BE(checksum, 0);
  // The checksum is the stream's last 4 bytes; the first occurrence at or after start+2
  // is the end (a false positive inside deflate data is ~2^-32 per position, and the
  // slice is verified by re-inflating exactly to the candidate end).
  for (let pos = start + 2; pos + 4 <= buffer.length; pos += 1) {
    if (buffer[pos] === needle[0] && buffer[pos + 1] === needle[1] && buffer[pos + 2] === needle[2] && buffer[pos + 3] === needle[3]) {
      const end = pos + 4;
      try {
        if (inflateSync(buffer.subarray(start, end)).equals(data)) return { end, data };
      } catch {
        // Not a real stream end; keep scanning.
      }
    }
  }
  throw new Error('could not find the end of a zlib stream');
}

// ---------------------------------------------------------------------------------------------
// git object parsing
// ---------------------------------------------------------------------------------------------

export interface ParsedCommit {
  readonly tree: string;
  readonly parents: string[];
  readonly author: string;
  readonly committer: string;
  readonly message: string;
}

export function parseCommit(data: Buffer): ParsedCommit {
  const text = data.toString('utf8');
  const blank = text.indexOf('\n\n');
  const header = blank >= 0 ? text.slice(0, blank) : text;
  const message = blank >= 0 ? text.slice(blank + 2) : '';
  let tree = '';
  const parents: string[] = [];
  let author = '';
  let committer = '';
  for (const line of header.split('\n')) {
    if (line.startsWith('tree ')) tree = line.slice(5).trim();
    else if (line.startsWith('parent ')) parents.push(line.slice(7).trim());
    else if (line.startsWith('author ')) author = line.slice(7);
    else if (line.startsWith('committer ')) committer = line.slice(10);
  }
  return { tree, parents, author, committer, message };
}

export interface TreeEntry {
  readonly mode: string;
  readonly name: string;
  readonly oid: string;
}

export function parseTree(data: Buffer): TreeEntry[] {
  const entries: TreeEntry[] = [];
  let offset = 0;
  while (offset < data.length) {
    const space = data.indexOf(0x20, offset);
    if (space < 0) break;
    const nul = data.indexOf(0x00, space);
    if (nul < 0 || nul + 21 > data.length) break;
    entries.push({
      mode: data.toString('ascii', offset, space),
      name: data.toString('utf8', space + 1, nul),
      oid: data.subarray(nul + 1, nul + 21).toString('hex'),
    });
    offset = nul + 21;
  }
  return entries;
}

export function parseTag(data: Buffer): { object: string; type: string; tag: string } {
  const text = data.toString('utf8');
  const blank = text.indexOf('\n\n');
  const header = blank >= 0 ? text.slice(0, blank) : text;
  let object = '';
  let type = '';
  let tag = '';
  for (const line of header.split('\n')) {
    if (line.startsWith('object ')) object = line.slice(7).trim();
    else if (line.startsWith('type ')) type = line.slice(5).trim();
    else if (line.startsWith('tag ')) tag = line.slice(4).trim();
  }
  return { object, type, tag };
}

/** Serialize tree entries in git order (directories compare as if their name ends in '/'). */
export function buildTreeBytes(entries: readonly TreeEntry[]): Buffer {
  const sorted = [...entries].sort((a, b) => {
    const aKey = a.mode === '40000' ? `${a.name}/` : a.name;
    const bKey = b.mode === '40000' ? `${b.name}/` : b.name;
    return aKey < bKey ? -1 : aKey > bKey ? 1 : 0;
  });
  const parts: Buffer[] = [];
  for (const entry of sorted) {
    parts.push(Buffer.from(`${entry.mode} ${entry.name}\0`, 'utf8'));
    parts.push(Buffer.from(entry.oid, 'hex'));
  }
  return Buffer.concat(parts);
}

/** All objects reachable from the roots (trees walked recursively, commits via parents). */
export function collectReachable(
  roots: readonly string[],
  get: (oid: string) => GitObject | undefined,
): Set<string> {
  const out = new Set<string>();
  const stack = [...roots];
  while (stack.length) {
    const oid = stack.pop()!;
    if (out.has(oid)) continue;
    const object = get(oid);
    if (!object) continue;
    out.add(oid);
    if (object.type === 'commit') {
      const commit = parseCommit(object.data);
      stack.push(commit.tree, ...commit.parents);
    } else if (object.type === 'tree') {
      for (const entry of parseTree(object.data)) stack.push(entry.oid);
    } else if (object.type === 'tag') {
      stack.push(parseTag(object.data).object);
    }
  }
  return out;
}

// ---------------------------------------------------------------------------------------------
// pack files (version 2, plain objects only on write; OFS/REF deltas on read)
// ---------------------------------------------------------------------------------------------

const PACK_TYPE_BY_NAME: Record<string, number> = { commit: 1, tree: 2, blob: 3, tag: 4 };
const PACK_NAME_BY_TYPE: Record<number, ObjectType> = { 1: 'commit', 2: 'tree', 3: 'blob', 4: 'tag' };

function applyDelta(base: Buffer, delta: Buffer): Buffer {
  let offset = 0;
  const readVarint = (): number => {
    let value = 0;
    let shift = 0;
    for (;;) {
      const byte = delta[offset++]!;
      value |= (byte & 0x7f) << shift;
      shift += 7;
      if ((byte & 0x80) === 0) return value;
    }
  };
  readVarint(); // base size (trusted: the result is checked against the delta's own size)
  const resultSize = readVarint();
  const chunks: Buffer[] = [];
  let produced = 0;
  while (offset < delta.length) {
    const op = delta[offset++]!;
    if (op & 0x80) {
      let copyOffset = 0;
      let copySize = 0;
      if (op & 0x01) copyOffset = delta[offset++]!;
      if (op & 0x02) copyOffset |= delta[offset++]! << 8;
      if (op & 0x04) copyOffset |= delta[offset++]! << 16;
      if (op & 0x08) copyOffset |= delta[offset++]! << 24;
      if (op & 0x10) copySize = delta[offset++]!;
      if (op & 0x20) copySize |= delta[offset++]! << 8;
      if (op & 0x40) copySize |= delta[offset++]! << 16;
      if (copySize === 0) copySize = 0x10000;
      chunks.push(base.subarray(copyOffset, copyOffset + copySize));
      produced += copySize;
    } else if (op !== 0) {
      chunks.push(delta.subarray(offset, offset + op));
      offset += op;
      produced += op;
    } else {
      throw new Error('invalid delta opcode 0');
    }
  }
  const result = Buffer.concat(chunks);
  if (result.length !== resultSize || produced !== resultSize) throw new Error('delta result size mismatch');
  return result;
}

export interface ReadPackResult {
  readonly objects: Map<string, GitObject>;
  readonly checksumOk: boolean;
}

export function readPack(pack: Buffer): ReadPackResult {
  if (pack.length < 12 || pack.toString('ascii', 0, 4) !== 'PACK') throw new Error('not a packfile');
  const version = pack.readUInt32BE(4);
  if (version !== 2 && version !== 3) throw new Error(`unsupported pack version ${version}`);
  const count = pack.readUInt32BE(8);
  const objects = new Map<string, GitObject>();
  const ofsBases = new Map<number, { oid: string; type: ObjectType; data: Buffer }>();
  let offset = 12;
  for (let i = 0; i < count; i += 1) {
    const start = offset;
    let byte = pack[offset++]!;
    const typeNum = (byte >> 4) & 0x7;
    let size = byte & 0x0f;
    let shift = 4;
    while (byte & 0x80) {
      byte = pack[offset++]!;
      size |= (byte & 0x7f) << shift;
      shift += 7;
    }
    let baseOid: string | null = null;
    let baseOffset: number | null = null;
    if (typeNum === 6) {
      let byte2 = pack[offset++]!;
      let ofs = byte2 & 0x7f;
      while (byte2 & 0x80) {
        byte2 = pack[offset++]!;
        ofs = ((ofs + 1) << 7) | (byte2 & 0x7f);
      }
      baseOffset = start - ofs;
    } else if (typeNum === 7) {
      baseOid = pack.subarray(offset, offset + 20).toString('hex');
      offset += 20;
    }
    const { end, data: inflated } = findZlibEnd(pack, offset);
    if (inflated.length !== size) throw new Error('pack object size mismatch');
    offset = end;
    if (typeNum >= 1 && typeNum <= 4) {
      const type = PACK_NAME_BY_TYPE[typeNum]!;
      const oid = hashObject(type, inflated);
      const object: GitObject = { type, data: Buffer.from(inflated) };
      objects.set(oid, object);
      ofsBases.set(start, { oid, type, data: object.data });
    } else if (typeNum === 6 || typeNum === 7) {
      const base = baseOffset !== null ? ofsBases.get(baseOffset) : baseOid !== null ? objects.get(baseOid) : undefined;
      if (!base) throw new Error('delta base not found');
      const data = applyDelta(base.data, inflated);
      const oid = hashObject(base.type, data);
      const object: GitObject = { type: base.type, data };
      objects.set(oid, object);
      ofsBases.set(start, { oid, type: base.type, data });
    } else {
      throw new Error(`unsupported pack object type ${typeNum}`);
    }
  }
  const trailer = pack.subarray(pack.length - 20).toString('hex');
  const computed = createHash('sha1').update(pack.subarray(0, pack.length - 20)).digest('hex');
  return { objects, checksumOk: trailer === computed };
}

export function writePack(objects: readonly GitObject[]): Buffer {
  // Hash each object once, then sort by oid (hashing inside the comparator would
  // recompute SHA-1 O(n log n) times — visible on the 600-object clone-history pack).
  const keyed = objects.map((object) => ({ oid: hashObject(object.type, object.data), object }));
  keyed.sort((a, b) => (a.oid < b.oid ? -1 : a.oid > b.oid ? 1 : 0));
  const parts: Buffer[] = [];
  const header = Buffer.alloc(12);
  header.write('PACK', 0, 'ascii');
  header.writeUInt32BE(2, 4);
  header.writeUInt32BE(keyed.length, 8);
  parts.push(header);
  for (const { object } of keyed) {
    const typeNum = PACK_TYPE_BY_NAME[object.type]!;
    const size = object.data.length;
    const headerBytes: number[] = [];
    let remaining = size;
    headerBytes.push((typeNum << 4) | (remaining & 0x0f));
    remaining >>= 4;
    while (remaining > 0) {
      headerBytes[headerBytes.length - 1] |= 0x80;
      headerBytes.push(remaining & 0x7f);
      remaining >>= 7;
    }
    parts.push(Buffer.from(headerBytes));
    parts.push(deflateSync(object.data));
  }
  const withoutTrailer = Buffer.concat(parts);
  const trailer = createHash('sha1').update(withoutTrailer).digest();
  return Buffer.concat([withoutTrailer, trailer]);
}

// ---------------------------------------------------------------------------------------------
// global ids: base64url "Kind:part:part"
// ---------------------------------------------------------------------------------------------

export function globalId(kind: string, ...parts: Array<string | number>): string {
  return Buffer.from(`${kind}:${parts.join(':')}`, 'utf8').toString('base64url');
}

export function parseGlobalId(id: string): { kind: string; parts: string[] } | null {
  try {
    const text = Buffer.from(id, 'base64url').toString('utf8');
    const segments = text.split(':');
    if (segments.length < 2) return null;
    return { kind: segments[0]!, parts: segments.slice(1) };
  } catch {
    return null;
  }
}

// ---------------------------------------------------------------------------------------------
// the store
// ---------------------------------------------------------------------------------------------

export interface Label {
  name: string;
  color: string;
  description: string;
}

export interface Comment {
  readonly globalId: string;
  readonly dbId: number;
  body: string;
  readonly createdAt: string;
  updatedAt: string;
}

export interface Issue {
  readonly globalId: string;
  readonly number: number;
  title: string;
  body: string;
  state: 'open' | 'closed';
  labels: Label[];
  comments: Comment[];
  readonly createdAt: string;
  updatedAt: string;
  closedAt: string | null;
  /** Serialized REST shape cache (read workload); cleared by every mutator. */
  cachedJson: string | null;
}

export interface PullRequest {
  readonly globalId: string;
  readonly number: number;
  title: string;
  body: string;
  state: 'open' | 'closed';
  merged: boolean;
  mergeCommitSha: string | null;
  readonly headRef: string;
  readonly baseRef: string;
  readonly headSha: string;
  readonly baseSha: string;
  readonly createdAt: string;
  updatedAt: string;
}

export interface Repo {
  readonly dbId: number;
  readonly createdSeq: string;
  owner: string;
  name: string;
  description: string;
  isPrivate: boolean;
  readonly defaultBranch: string;
  readonly objects: Map<string, GitObject>;
  readonly refs: Map<string, string>;
  readonly issues: Map<number, Issue>;
  readonly pulls: Map<number, PullRequest>;
  readonly labels: Map<string, Label>;
  nextNumber: number;
  commitCounter: number;
}

export const DEFAULT_LABELS: readonly Label[] = [
  { name: 'bug', color: 'd73a4a', description: "Something isn't working" },
  { name: 'enhancement', color: 'a2eeef', description: 'New feature or request' },
  { name: 'documentation', color: '0075ca', description: 'Improvements or additions to documentation' },
  { name: 'question', color: 'd876e3', description: 'Further information is requested' },
  { name: 'good first issue', color: '7057ff', description: 'Good for newcomers' },
  { name: 'help wanted', color: '008672', description: 'Extra attention is needed' },
];

const TIME_BASE_MS = Date.UTC(2025, 0, 1);
const COMMIT_EPOCH = 1_700_000_000;

export class Store {
  readonly repos = new Map<string, Repo>();
  private nextDbId = 1;
  private requestCounter = 0;
  private sequence = 0;
  private nextCommentId = 1;

  nextRequestId(): string {
    this.requestCounter += 1;
    return `req-${String(this.requestCounter).padStart(8, '0')}`;
  }

  /** Deterministic ISO timestamp: base + 1s per sequence step. */
  seqTimestamp(): string {
    this.sequence += 1;
    return new Date(TIME_BASE_MS + this.sequence * 1000).toISOString();
  }

  private key(owner: string, name: string): string {
    return `${owner}/${name}`;
  }

  createRepo(name: string, isPrivate: boolean, description: string): Repo | null {
    if (this.repos.has(this.key(VIEWER.login, name))) return null;
    const dbId = this.nextDbId;
    this.nextDbId += 1;
    const repo: Repo = {
      dbId,
      createdSeq: this.seqTimestamp(),
      owner: VIEWER.login,
      name,
      description,
      isPrivate,
      defaultBranch: 'main',
      objects: new Map(),
      refs: new Map(),
      issues: new Map(),
      pulls: new Map(),
      labels: new Map(DEFAULT_LABELS.map((label) => [label.name, { ...label }])),
      nextNumber: 1,
      commitCounter: 0,
    };
    this.repos.set(this.key(repo.owner, name), repo);
    return repo;
  }

  getRepo(owner: string, name: string): Repo | undefined {
    return this.repos.get(this.key(owner, name));
  }

  listViewerRepos(): Repo[] {
    return [...this.repos.values()]
      .filter((repo) => repo.owner === VIEWER.login)
      .sort((a, b) => (a.dbId < b.dbId ? -1 : 1));
  }

  deleteRepo(owner: string, name: string): boolean {
    return this.repos.delete(this.key(owner, name));
  }

  createIssue(repo: Repo, title: string, body: string, labels: string[]): Issue {
    const number = repo.nextNumber;
    repo.nextNumber += 1;
    const now = this.seqTimestamp();
    const issue: Issue = {
      globalId: globalId('Issue', repo.dbId, number),
      number,
      title,
      body,
      state: 'open',
      labels: labels
        .map((name) => repo.labels.get(name))
        .filter((label): label is Label => label !== undefined)
        .map((label) => ({ ...label })),
      comments: [],
      createdAt: now,
      updatedAt: now,
      closedAt: null,
      cachedJson: null,
    };
    repo.issues.set(number, issue);
    return issue;
  }

  /** Clear an issue's cached serialized shape after any mutation. */
  touchIssue(issue: Issue): void {
    issue.cachedJson = null;
  }

  addComment(repo: Repo, issue: Issue, body: string): Comment {
    const now = this.seqTimestamp();
    const comment: Comment = {
      globalId: globalId('IssueComment', repo.dbId, issue.number, this.nextCommentId),
      dbId: this.nextCommentId,
      body,
      createdAt: now,
      updatedAt: now,
    };
    this.nextCommentId += 1;
    issue.comments.push(comment);
    issue.updatedAt = now;
    this.touchIssue(issue);
    return comment;
  }

  createPull(
    repo: Repo,
    title: string,
    body: string,
    headRef: string,
    baseRef: string,
  ): { ok: true; pull: PullRequest } | { ok: false; message: string } {
    const headOid = repo.refs.get(`refs/heads/${headRef}`);
    if (!headOid) return { ok: false, message: `No such head branch: ${headRef}` };
    const baseOid = repo.refs.get(`refs/heads/${baseRef}`);
    if (!baseOid) return { ok: false, message: `No such base branch: ${baseRef}` };
    const number = repo.nextNumber;
    repo.nextNumber += 1;
    const now = this.seqTimestamp();
    const pull: PullRequest = {
      globalId: globalId('PullRequest', repo.dbId, number),
      number,
      title,
      body,
      state: 'open',
      merged: false,
      mergeCommitSha: null,
      headRef,
      baseRef,
      headSha: headOid,
      baseSha: baseOid,
      createdAt: now,
      updatedAt: now,
    };
    repo.pulls.set(number, pull);
    return { ok: true, pull };
  }

  /** The base branch's CURRENT tip (a PR's stored baseSha is the tip at creation time). */
  currentBaseSha(repo: Repo, pull: PullRequest): string {
    return repo.refs.get(`refs/heads/${pull.baseRef}`) ?? pull.baseSha;
  }

  /** The head branch's CURRENT tip. */
  currentHeadSha(repo: Repo, pull: PullRequest): string {
    return repo.refs.get(`refs/heads/${pull.headRef}`) ?? pull.headSha;
  }

  pullMergeable(repo: Repo, pull: PullRequest): boolean {
    if (pull.merged || pull.state !== 'open') return false;
    const headSha = this.currentHeadSha(repo, pull);
    const baseSha = this.currentBaseSha(repo, pull);
    const head = repo.objects.get(headSha);
    const base = repo.objects.get(baseSha);
    if (!head || !base || head.type !== 'commit' || base.type !== 'commit') return false;
    return this.isAncestor(repo, baseSha, headSha) || headSha === baseSha;
  }

  pullFiles(repo: Repo, pull: PullRequest): Array<{ filename: string; status: string }> {
    const headSha = this.currentHeadSha(repo, pull);
    const baseSha = this.currentBaseSha(repo, pull);
    const head = repo.objects.get(headSha);
    const base = repo.objects.get(baseSha);
    if (!head || !base || head.type !== 'commit' || base.type !== 'commit') return [];
    const headTree = this.flattenTree(repo, parseCommit(head.data).tree);
    const baseTree = this.flattenTree(repo, parseCommit(base.data).tree);
    const files: Array<{ filename: string; status: string }> = [];
    for (const [path, oid] of headTree) {
      const baseOid = baseTree.get(path);
      if (baseOid === undefined) files.push({ filename: path, status: 'added' });
      else if (baseOid !== oid) files.push({ filename: path, status: 'modified' });
    }
    for (const path of baseTree.keys()) {
      if (!headTree.has(path)) files.push({ filename: path, status: 'removed' });
    }
    files.sort((a, b) => (a.filename < b.filename ? -1 : 1));
    return files;
  }

  flattenTree(repo: Repo, treeOid: string, prefix = ''): Map<string, string> {
    const out = new Map<string, string>();
    const walk = (oid: string, at: string): void => {
      const object = repo.objects.get(oid);
      if (!object || object.type !== 'tree') return;
      for (const entry of parseTree(object.data)) {
        const path = at ? `${at}/${entry.name}` : entry.name;
        if (entry.mode === '40000') walk(entry.oid, path);
        else out.set(path, entry.oid);
      }
    };
    walk(treeOid, prefix);
    return out;
  }

  isAncestor(repo: Repo, ancestor: string, oid: string): boolean {
    if (ancestor === oid) return true;
    const seen = new Set<string>();
    const queue = [oid];
    while (queue.length) {
      const current = queue.pop()!;
      if (seen.has(current)) continue;
      seen.add(current);
      const object = repo.objects.get(current);
      if (!object || object.type !== 'commit') continue;
      for (const parent of parseCommit(object.data).parents) {
        if (parent === ancestor) return true;
        queue.push(parent);
      }
    }
    return false;
  }

  mergePull(
    repo: Repo,
    pull: PullRequest,
  ): { ok: true; sha: string } | { ok: false; message: string } {
    if (pull.merged) return { ok: false, message: 'Pull request already merged' };
    if (pull.state !== 'open') return { ok: false, message: 'Pull request is not open' };
    if (!this.pullMergeable(repo, pull)) return { ok: false, message: 'Pull request is not mergeable' };
    const headSha = this.currentHeadSha(repo, pull);
    const baseSha = this.currentBaseSha(repo, pull);
    const head = repo.objects.get(headSha);
    if (!head || head.type !== 'commit') return { ok: false, message: 'Head commit not found' };
    const headTree = parseCommit(head.data).tree;
    const parents = baseSha === headSha ? [baseSha] : [baseSha, headSha];
    repo.commitCounter += 1;
    const timestamp = COMMIT_EPOCH + repo.commitCounter;
    const message = `Merge pull request #${pull.number} from ${pull.headRef}`;
    const commitText =
      `tree ${headTree}\n` +
      parents.map((parent) => `parent ${parent}\n`).join('') +
      `author ${FORGE_IDENTITY} ${timestamp} +0000\n` +
      `committer ${FORGE_IDENTITY} ${timestamp} +0000\n\n${message}`;
    const commitData = Buffer.from(commitText, 'utf8');
    const sha = hashObject('commit', commitData);
    repo.objects.set(sha, { type: 'commit', data: commitData });
    repo.refs.set(`refs/heads/${pull.baseRef}`, sha);
    pull.merged = true;
    pull.state = 'closed';
    pull.mergeCommitSha = sha;
    pull.updatedAt = this.seqTimestamp();
    return { ok: true, sha };
  }

  /** Create or update a file on a branch (REST contents). Returns the new commit sha. */
  createCommitOnBranch(
    repo: Repo,
    branch: string,
    path: string,
    content: Buffer,
    message: string,
  ): string {
    const ref = `refs/heads/${branch}`;
    const current = repo.refs.get(ref);
    const parentTree = new Map<string, string>();
    const parents: string[] = [];
    if (current) {
      const object = repo.objects.get(current);
      if (object && object.type === 'commit') {
        parents.push(current);
        for (const [filePath, oid] of this.flattenTree(repo, parseCommit(object.data).tree)) {
          parentTree.set(filePath, oid);
        }
      }
    }
    const blobOid = hashObject('blob', content);
    repo.objects.set(blobOid, { type: 'blob', data: Buffer.from(content) });
    parentTree.set(path, blobOid);
    const treeOid = this.buildTreeFromFlat(repo, parentTree);
    repo.commitCounter += 1;
    const timestamp = COMMIT_EPOCH + repo.commitCounter;
    const commitText =
      `tree ${treeOid}\n` +
      parents.map((parent) => `parent ${parent}\n`).join('') +
      `author ${FORGE_IDENTITY} ${timestamp} +0000\n` +
      `committer ${FORGE_IDENTITY} ${timestamp} +0000\n\n${message}`;
    const commitData = Buffer.from(commitText, 'utf8');
    const sha = hashObject('commit', commitData);
    repo.objects.set(sha, { type: 'commit', data: commitData });
    repo.refs.set(ref, sha);
    return sha;
  }

  private buildTreeFromFlat(repo: Repo, flat: Map<string, string>): string {
    const nested = new Map<string, Map<string, { mode: string; oid: string }>>();
    const root = new Map<string, { mode: string; oid: string }>();
    nested.set('', root);
    for (const [path, oid] of flat) {
      const parts = path.split('/');
      let at = '';
      for (let i = 0; i < parts.length - 1; i += 1) {
        const part = parts[i]!;
        const next = at ? `${at}/${part}` : part;
        if (!nested.has(next)) nested.set(next, new Map());
        const parent = nested.get(at)!;
        if (!parent.has(part)) parent.set(part, { mode: '40000', oid: '' });
        at = next;
      }
      nested.get(at)!.set(parts[parts.length - 1]!, { mode: '100644', oid });
    }
    const build = (dir: string): string => {
      const entries: TreeEntry[] = [];
      for (const [name, value] of nested.get(dir)!) {
        if (value.mode === '40000') {
          const child = dir ? `${dir}/${name}` : name;
          entries.push({ mode: '40000', name, oid: build(child) });
        } else {
          entries.push({ mode: value.mode, name, oid: value.oid });
        }
      }
      const bytes = buildTreeBytes(entries);
      const oid = hashObject('tree', bytes);
      repo.objects.set(oid, { type: 'tree', data: bytes });
      return oid;
    };
    return build('');
  }
}
