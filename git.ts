// git.ts — the smart-HTTP git surface: upload-pack (v2 + v0) and receive-pack (v0).
//
// Pinned against the suite's own git-client.ts:
// - upload-pack info/refs answers v2 when the request carries `Git-Protocol: version=2`,
//   otherwise v0. receive-pack info/refs is always v0.
// - v2 ls-refs: `command=ls-refs` + delim + args (peel, symrefs, unborn, ref-prefix ...).
//   HEAD carries symref-target; annotated tags carry peeled:<target>; an empty repo answers
//   `unborn HEAD symref-target:refs/heads/main`. ref-prefix filtering is server-side.
// - v2 fetch: sections `acknowledgments`, `shallow-info`, `wanted-refs`, `packfile`,
//   separated by delim, the last ending in flush. The packfile section is sideband-framed
//   (band 1 = pack); an empty pack is still sent when nothing is new. `deepen N` is an
//   ABSOLUTE depth N from the want tips (depth 2 from a depth-1 boundary delivers the parent).
// - v0 fetch: NAK/ACK/shallow pkt-lines followed by a sideband-framed pack.
// - receive-pack: command list, first line carrying capabilities after NUL; the pack follows
//   the flush raw. The reply is report-status pkt-lines; when side-band-64k was negotiated
//   those lines travel inside band-1 sideband packets. A refused ref is `ng <ref>
//   non-fast-forward` and the ref does not move; other refs in the same push still apply.

import {
  ZERO_OID,
  collectReachable,
  decodePktLines,
  hashObject,
  isObjectId,
  parseCommit,
  parseTag,
  parseTree,
  pktDelim,
  pktFlush,
  pktLine,
  readPack,
  writePack,
  type GitObject,
  type Repo,
  type Store,
} from './store.ts';

export interface GitResponse {
  readonly status: number;
  readonly contentType: string;
  readonly body: Buffer;
}

const AGENT = 'makefun-forge/1.0';

function jsonResponse(status: number, body: unknown): GitResponse {
  return { status, contentType: 'application/json; charset=utf-8', body: Buffer.from(JSON.stringify(body), 'utf8') };
}

function textResponse(status: number, text: string): GitResponse {
  return { status, contentType: 'text/plain; charset=utf-8', body: Buffer.from(text, 'utf8') };
}

function requireRepo(store: Store, owner: string, repoName: string): Repo | GitResponse {
  const repo = store.getRepo(owner, repoName);
  if (!repo) return jsonResponse(404, { message: 'Not Found' });
  return repo;
}

/** All refs of a repo, sorted by name. */
function sortedRefs(repo: Repo): Array<{ name: string; oid: string }> {
  const out: Array<{ name: string; oid: string }> = [];
  for (const [name, oid] of repo.refs) out.push({ name, oid });
  out.sort((a, b) => (a.name < b.name ? -1 : a.name > b.name ? 1 : 0));
  return out;
}

/** Peel a tag object to its commit (or return the oid itself when it is a commit). */
function peelToCommit(repo: Repo, oid: string): string | null {
  let current = oid;
  for (let i = 0; i < 8; i += 1) {
    const object = repo.objects.get(current);
    if (!object) return null;
    if (object.type === 'commit') return current;
    if (object.type === 'tag') {
      current = parseTag(object.data).object;
      continue;
    }
    return null;
  }
  return null;
}

// ---------------------------------------------------------------------------------------------
// info/refs
// ---------------------------------------------------------------------------------------------

// Canonical, collision-free encoding of a repo's refs for cache keys.
// JSON.stringify is injective over strings/numbers/null, so a ref name or oid
// containing '=', ';', '|', ',' or any other separator character can never
// alias a different refs set (the old `${name}=${oid}` joined with ';' could,
// and the same applied to wants/haves/shallow joined with ','). Entries are
// sorted by name so the result is a pure function of the refs set.
function sortedRefEntries(repo: Repo): Array<[string, string]> {
  return [...repo.refs.entries()].sort(([a], [b]) => (a < b ? -1 : a > b ? 1 : 0));
}

function v0Advertisement(repo: Repo, service: 'git-upload-pack' | 'git-receive-pack'): Buffer {
  // The advertisement is a pure function of the service plus the refs (tag
  // targets are fixed by the immutable tag objects the refs name). Cached per
  // repo with the refs fingerprint as the key; any ref change recomputes.
  const key = JSON.stringify([service, sortedRefEntries(repo)]);
  let perRepo = v0AdvertisementCache.get(repo);
  if (perRepo) {
    const hit = perRepo.get(key);
    if (hit) return hit;
  }
  const body = buildV0Advertisement(repo, service);
  if (!perRepo) {
    perRepo = new Map<string, Buffer>();
    v0AdvertisementCache.set(repo, perRepo);
  }
  if (perRepo.size >= 32) {
    const oldest = perRepo.keys().next().value;
    if (oldest !== undefined) perRepo.delete(oldest);
  }
  perRepo.set(key, body);
  return body;
}

// Cache for v0 advertisements (see v0Advertisement).
const v0AdvertisementCache = new WeakMap<Repo, Map<string, Buffer>>();

function buildV0Advertisement(repo: Repo, service: 'git-upload-pack' | 'git-receive-pack'): Buffer {
  const caps =
    service === 'git-upload-pack'
      ? 'symref=HEAD:refs/heads/main agent=makefun-forge/1.0 object-format=sha1 multi_ack side-band-64k thin-pack ofs-delta shallow deepen-since deepen-not filter no-progress include-tag allow-tip-sha1-in-want allow-reachable-sha1-in-want'
      : 'report-status side-band-64k delete-refs agent=makefun-forge/1.0 object-format=sha1';
  const parts: Buffer[] = [pktLine(`# service=${service}\n`), pktFlush()];
  const refs = sortedRefs(repo);
  const headTip = repo.refs.get('refs/heads/main');
  if (refs.length === 0) {
    parts.push(pktLine(`${ZERO_OID} capabilities^{}\u0000${caps}\n`));
  } else {
    const first: Array<{ oid: string; name: string }> = [];
    if (headTip) first.push({ oid: headTip, name: 'HEAD' });
    first.push(...refs);
    first.forEach((ref, index) => {
      const suffix = index === 0 ? `\u0000${caps}` : '';
      parts.push(pktLine(`${ref.oid} ${ref.name}${suffix}\n`));
      const object = repo.objects.get(ref.oid);
      if (object && object.type === 'tag' && ref.name.startsWith('refs/tags/')) {
        const target = parseTag(object.data).object;
        parts.push(pktLine(`${target} ${ref.name}^{}\n`));
      }
    });
  }
  parts.push(pktFlush());
  return Buffer.concat(parts);
}

/** The v2 advertisement is a fixed constant: build it once, not per request. */
const V2_ADVERTISEMENT: Buffer = Buffer.concat([
  pktLine('version 2\n'),
  pktLine(`agent=${AGENT}\n`),
  pktLine('ls-refs=unborn\n'),
  pktLine('fetch=shallow wait-for-done\n'),
  pktLine('server-option\n'),
  pktLine('object-format=sha1\n'),
  pktFlush(),
]);

function v2Advertisement(): Buffer {
  return V2_ADVERTISEMENT;
}

export function handleInfoRefs(
  store: Store,
  owner: string,
  repoName: string,
  service: 'git-upload-pack' | 'git-receive-pack',
  gitProtocolHeader: string | undefined,
): GitResponse {
  const repo = requireRepo(store, owner, repoName);
  if (!('refs' in repo)) return repo;
  const wantsV2 = service === 'git-upload-pack' && (gitProtocolHeader ?? '').includes('version=2');
  const body = wantsV2 ? v2Advertisement() : v0Advertisement(repo, service);
  return { status: 200, contentType: `application/x-${service}-advertisement`, body };
}

// ---------------------------------------------------------------------------------------------
// fetch: shared object selection
// ---------------------------------------------------------------------------------------------

interface FetchPlan {
  /** Commits included (depth-limited when a depth was given). */
  readonly included: Set<string>;
  /** The deepest included layer, for the shallow-info section. */
  readonly boundary: Set<string>;
  /** Objects to send, in a stable order. */
  readonly objects: GitObject[];
  /** Haves that are ancestors of (or equal to) a want. */
  readonly acks: string[];
}

/** Commits within `depth` edges of the wants (depth 1 = the tips only; null = all history). */
function commitsWithinDepth(repo: Repo, wants: readonly string[], depth: number | null): { included: Set<string>; boundary: Set<string> } {
  const included = new Set<string>();
  const boundary = new Set<string>();
  const queue: Array<{ oid: string; level: number }> = wants.map((oid) => ({ oid, level: 1 }));
  const seen = new Set<string>();
  // Queue cursor instead of shift(): shift() moves every remaining element per
  // iteration (quadratic on long histories); the cursor keeps the same FIFO
  // order, depth, and boundary behavior in linear time.
  let cursor = 0;
  while (cursor < queue.length) {
    const { oid, level } = queue[cursor++]!;
    if (seen.has(oid)) continue;
    seen.add(oid);
    const object = repo.objects.get(oid);
    if (!object || object.type !== 'commit') continue;
    included.add(oid);
    if (depth !== null && level >= depth) {
      boundary.add(oid);
      continue;
    }
    for (const parent of parseCommit(object.data).parents) queue.push({ oid: parent, level: level + 1 });
  }
  return { included, boundary };
}

/** The objects for a set of commits: each commit plus its whole tree, never its parents. */
function objectsForCommits(repo: Repo, commits: ReadonlySet<string>, treeCache: Map<string, Set<string>>): Map<string, GitObject> {
  const out = new Map<string, GitObject>();
  for (const oid of commits) {
    const commit = repo.objects.get(oid);
    if (!commit) continue;
    out.set(oid, commit);
    const treeOid = parseCommit(commit.data).tree;
    // Histories usually share one tree across many commits: walk each distinct
    // tree once per fetch and reuse its reachable set.
    let reachable = treeCache.get(treeOid);
    if (!reachable) {
      reachable = collectReachable([treeOid], (id) => repo.objects.get(id));
      treeCache.set(treeOid, reachable);
    }
    for (const reachableOid of reachable) {
      const object = repo.objects.get(reachableOid);
      if (object) out.set(reachableOid, object);
    }
  }
  return out;
}

function isAncestor(repo: Repo, ancestor: string, oid: string): boolean {
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

function planFetch(
  repo: Repo,
  wants: readonly string[],
  haves: readonly string[],
  shallow: readonly string[],
  depth: number | null,
): FetchPlan {
  // A want may be a tag object: send the tag itself, and walk from what it points at.
  const tagObjects: Array<{ oid: string; object: GitObject }> = [];
  const commitWants: string[] = [];
  for (const want of wants) {
    const object = repo.objects.get(want);
    if (!object) continue;
    if (object.type === 'tag') {
      tagObjects.push({ oid: want, object });
      const target = peelToCommit(repo, want);
      if (target) commitWants.push(target);
    } else if (object.type === 'commit') {
      commitWants.push(want);
    }
  }
  const { included, boundary } = commitsWithinDepth(repo, commitWants, depth);
  // Request-local cache: each distinct tree's reachable set, walked once.
  const treeCache = new Map<string, Set<string>>();
  const wantedObjects = objectsForCommits(repo, included, treeCache);
  for (const tag of tagObjects) wantedObjects.set(tag.oid, tag.object);
  // What the client already holds. A shallow client holds only its boundary commits and
  // their trees — never the history below the boundary — so its haves are counted the
  // same way: each held commit plus its tree, no parents. A full client holds the whole
  // history below its haves.
  const isShallow = depth !== null || shallow.length > 0;
  const haveObjects = new Map<string, GitObject>();
  const addHeldCommit = (oid: string): void => {
    const object = repo.objects.get(oid);
    if (!object) return;
    if (object.type !== 'commit') {
      haveObjects.set(oid, object);
      return;
    }
    haveObjects.set(oid, object);
    const treeOid = parseCommit(object.data).tree;
    let reachable = treeCache.get(treeOid);
    if (!reachable) {
      reachable = collectReachable([treeOid], (id) => repo.objects.get(id));
      treeCache.set(treeOid, reachable);
    }
    for (const reachableOid of reachable) {
      const found = repo.objects.get(reachableOid);
      if (found) haveObjects.set(reachableOid, found);
    }
  };
  if (isShallow) {
    for (const oid of [...haves, ...shallow]) addHeldCommit(oid);
  } else {
    for (const have of haves) {
      const object = repo.objects.get(have);
      if (!object) continue;
      if (object.type === 'commit') {
        for (const reachable of collectReachable([have], (id) => repo.objects.get(id))) {
          const found = repo.objects.get(reachable);
          if (found) haveObjects.set(reachable, found);
        }
      } else {
        haveObjects.set(have, object);
      }
    }
  }
  const objects: GitObject[] = [];
  for (const [oid, object] of wantedObjects) {
    if (!haveObjects.has(oid)) objects.push(object);
  }
  const acks: string[] = [];
  for (const have of haves) {
    if (wants.includes(have)) {
      acks.push(have);
      continue;
    }
    for (const want of wants) {
      if (isAncestor(repo, have, want)) {
        acks.push(have);
        break;
      }
    }
  }
  return { included, boundary, objects, acks };
}

/** Split pack bytes into sideband-64k packets (band 1), each a pkt-line of at most 65520 bytes. */
function sidebandPack(pack: Buffer): Buffer {
  const parts: Buffer[] = [];
  const CHUNK = 65515;
  for (let offset = 0; offset < pack.length; offset += CHUNK) {
    const slice = pack.subarray(offset, Math.min(pack.length, offset + CHUNK));
    parts.push(pktLine(Buffer.concat([Buffer.from([1]), slice])));
  }
  if (pack.length === 0) parts.push(pktLine(Buffer.from([1])));
  return Buffer.concat(parts);
}

/** The shallow-info lines for a fetch: new boundary commits, plus old ones now complete. */
function shallowInfoLines(plan: FetchPlan, oldBoundary: readonly string[], depth: number | null): string[] {
  if (depth === null) return [];
  const lines: string[] = [];
  for (const oid of [...plan.boundary].sort()) lines.push(`shallow ${oid}\n`);
  for (const old of oldBoundary) {
    if (!plan.included.has(old)) continue;
    if (plan.boundary.has(old)) continue;
    lines.push(`unshallow ${old}\n`);
  }
  return lines;
}

// ---------------------------------------------------------------------------------------------
// upload-pack
// ---------------------------------------------------------------------------------------------

interface V2FetchArgs {
  wants: string[];
  haves: string[];
  shallow: string[];
  depth: number | null;
}

function parseV2FetchArgs(packets: ReturnType<typeof decodePktLines>['packets']): V2FetchArgs | null {
  const args: V2FetchArgs = { wants: [], haves: [], shallow: [], depth: null };
  let seenDelim = false;
  for (const packet of packets) {
    if (packet.kind === 'delim') {
      seenDelim = true;
      continue;
    }
    if (packet.kind !== 'data') continue;
    const line = packet.payload.toString('utf8').replace(/\n$/, '');
    if (!seenDelim) continue;
    if (line.startsWith('want ')) args.wants.push(line.slice(5).trim());
    else if (line.startsWith('have ')) args.haves.push(line.slice(5).trim());
    else if (line.startsWith('shallow ')) args.shallow.push(line.slice(8).trim());
    else if (line.startsWith('deepen ')) {
      const n = Number.parseInt(line.slice(7).trim(), 10);
      if (Number.isFinite(n) && n > 0) args.depth = n;
    }
  }
  return args.wants.length ? args : null;
}

function v2FetchResponse(repo: Repo, args: V2FetchArgs): GitResponse {
  for (const want of args.wants) {
    if (!isObjectId(want) || !repo.objects.has(want)) return textResponse(400, `want ${want} is not known`);
  }
  // The fetch response is a pure function of the repo's objects/refs plus the
  // fetch args, and git objects are immutable and only added (never deleted or
  // mutated). The key embeds the object count and a full refs fingerprint, so
  // any mutation forces a recompute; the WeakMap is keyed by repo identity so
  // distinct stores/repos can never collide. Bounded to 32 entries per repo.
  // The cached Buffer is never mutated after creation. This matters because a
  // clone round issues 50 identical depth-1 fetches of the same tip.
  const key = fetchCacheKey(repo, args);
  let perRepo = fetchResponseCache.get(repo);
  if (perRepo) {
    const hit = perRepo.get(key);
    if (hit) return { status: 200, contentType: 'application/x-git-upload-pack-result', body: hit };
  }
  const plan = planFetch(repo, args.wants, args.haves, args.shallow, args.depth);
  const parts: Buffer[] = [];
  // acknowledgments
  parts.push(pktLine('acknowledgments\n'));
  if (plan.acks.length) {
    for (const ack of plan.acks) parts.push(pktLine(`ACK ${ack}\n`));
    parts.push(pktLine('ready\n'));
  } else {
    parts.push(pktLine('NAK\n'));
  }
  parts.push(pktDelim());
  // shallow-info
  const shallowLines = shallowInfoLines(plan, args.shallow, args.depth);
  if (shallowLines.length) {
    parts.push(pktLine('shallow-info\n'));
    for (const line of shallowLines) parts.push(pktLine(line));
    parts.push(pktDelim());
  }
  // wanted-refs
  parts.push(pktLine('wanted-refs\n'));
  for (const ref of sortedRefs(repo)) {
    if (args.wants.includes(ref.oid)) parts.push(pktLine(`${ref.oid} ${ref.name}\n`));
  }
  parts.push(pktDelim());
  // packfile
  parts.push(pktLine('packfile\n'));
  parts.push(sidebandPack(writePack(plan.objects)));
  parts.push(pktFlush());
  const body = Buffer.concat(parts);
  if (!perRepo) {
    perRepo = new Map<string, Buffer>();
    fetchResponseCache.set(repo, perRepo);
  }
  if (perRepo.size >= FETCH_CACHE_MAX) {
    const oldest = perRepo.keys().next().value;
    if (oldest !== undefined) perRepo.delete(oldest);
  }
  perRepo.set(key, body);
  return { status: 200, contentType: 'application/x-git-upload-pack-result', body };
}

// Cache for v2 fetch responses (see v2FetchResponse).
const fetchResponseCache = new WeakMap<Repo, Map<string, Buffer>>();
const FETCH_CACHE_MAX = 32;

function fetchCacheKey(repo: Repo, args: V2FetchArgs): string {
  // JSON.stringify over the component arrays: provably unambiguous (a want,
  // have or shallow value containing ',' can no longer collide with two
  // separate values — e.g. haves ["A","B"] vs ["A,B"]), and the refs
  // fingerprint uses the same canonical encoding as v0Advertisement.
  return JSON.stringify([
    repo.objects.size,
    sortedRefEntries(repo),
    args.wants,
    args.haves,
    args.shallow,
    args.depth,
  ]);
}

function v2LsRefsResponse(repo: Repo, packets: ReturnType<typeof decodePktLines>['packets']): GitResponse {
  let peel = false;
  let symrefs = false;
  let unborn = false;
  const prefixes: string[] = [];
  let seenDelim = false;
  for (const packet of packets) {
    if (packet.kind === 'delim') {
      seenDelim = true;
      continue;
    }
    if (packet.kind !== 'data' || !seenDelim) continue;
    const line = packet.payload.toString('utf8').replace(/\n$/, '');
    if (line === 'peel') peel = true;
    else if (line === 'symrefs') symrefs = true;
    else if (line === 'unborn') unborn = true;
    else if (line.startsWith('ref-prefix ')) prefixes.push(line.slice('ref-prefix '.length));
  }
  const keep = (name: string): boolean => prefixes.length === 0 || prefixes.some((prefix) => name.startsWith(prefix));
  const parts: Buffer[] = [];
  const headTip = repo.refs.get('refs/heads/main');
  if (repo.refs.size === 0) {
    if (unborn && keep('HEAD')) parts.push(pktLine('unborn HEAD symref-target:refs/heads/main\n'));
  } else {
    if (keep('HEAD') && headTip) {
      parts.push(pktLine(`${headTip} HEAD${symrefs ? ' symref-target:refs/heads/main' : ''}\n`));
    }
    for (const ref of sortedRefs(repo)) {
      if (!keep(ref.name)) continue;
      let line = `${ref.oid} ${ref.name}`;
      if (peel) {
        const object = repo.objects.get(ref.oid);
        if (object && object.type === 'tag') line += ` peeled:${parseTag(object.data).object}`;
      }
      parts.push(pktLine(`${line}\n`));
    }
  }
  parts.push(pktFlush());
  return { status: 200, contentType: 'application/x-git-upload-pack-result', body: Buffer.concat(parts) };
}

function v0FetchResponse(repo: Repo, body: Buffer): GitResponse {
  const { packets } = decodePktLines(body);
  const wants: string[] = [];
  const haves: string[] = [];
  const shallow: string[] = [];
  let depth: number | null = null;
  for (const packet of packets) {
    if (packet.kind !== 'data') continue;
    const line = packet.payload.toString('utf8').replace(/\n$/, '');
    if (line.startsWith('want ')) {
      const rest = line.slice(5).split(' ');
      if (rest[0]) wants.push(rest[0]);
    } else if (line.startsWith('have ')) haves.push(line.slice(5).trim());
    else if (line.startsWith('shallow ')) shallow.push(line.slice(8).trim());
    else if (line.startsWith('deepen ')) {
      const n = Number.parseInt(line.slice(7).trim(), 10);
      if (Number.isFinite(n) && n > 0) depth = n;
    }
  }
  if (!wants.length) return textResponse(400, 'no want lines');
  for (const want of wants) {
    if (!isObjectId(want) || !repo.objects.has(want)) return textResponse(400, `want ${want} is not known`);
  }
  const plan = planFetch(repo, wants, haves, shallow, depth);
  const parts: Buffer[] = [];
  if (plan.acks.length) {
    for (const ack of plan.acks) parts.push(pktLine(`ACK ${ack}\n`));
    parts.push(pktLine('ready\n'));
  } else {
    parts.push(pktLine('NAK\n'));
  }
  for (const line of shallowInfoLines(plan, shallow, depth)) parts.push(pktLine(line));
  parts.push(sidebandPack(writePack(plan.objects)));
  parts.push(pktFlush());
  return { status: 200, contentType: 'application/x-git-upload-pack-result', body: Buffer.concat(parts) };
}

export function handleUploadPack(store: Store, owner: string, repoName: string, body: Buffer): GitResponse {
  const repo = requireRepo(store, owner, repoName);
  if (!('refs' in repo)) return repo;
  const { packets } = decodePktLines(body);
  const first = packets.find((packet) => packet.kind === 'data');
  if (!first) return textResponse(400, 'empty upload-pack request');
  const firstLine = first.payload.toString('utf8');
  if (firstLine.startsWith('command=ls-refs')) return v2LsRefsResponse(repo, packets);
  if (firstLine.startsWith('command=fetch')) {
    const args = parseV2FetchArgs(packets);
    if (!args) return textResponse(400, 'fetch without wants');
    return v2FetchResponse(repo, args);
  }
  if (firstLine.startsWith('want ')) return v0FetchResponse(repo, body);
  return textResponse(400, 'malformed upload-pack request');
}

// ---------------------------------------------------------------------------------------------
// receive-pack
// ---------------------------------------------------------------------------------------------

interface PushCommand {
  readonly oldOid: string;
  readonly newOid: string;
  readonly ref: string;
}

export function handleReceivePack(store: Store, owner: string, repoName: string, body: Buffer): GitResponse {
  const repo = requireRepo(store, owner, repoName);
  if (!('refs' in repo)) return repo;
  const { packets, offset } = decodePktLines(body);
  const commands: PushCommand[] = [];
  let sideband = false;
  let sawFlush = false;
  for (const packet of packets) {
    if (packet.kind === 'flush') {
      sawFlush = true;
      break;
    }
    if (packet.kind !== 'data') continue;
    const text = packet.payload.toString('utf8').replace(/\n$/, '');
    const nul = text.indexOf('\u0000');
    const commandText = nul >= 0 ? text.slice(0, nul) : text;
    if (nul >= 0) {
      const caps = text.slice(nul + 1).split(' ');
      if (caps.includes('side-band-64k')) sideband = true;
    }
    const parts = commandText.split(' ');
    if (parts.length !== 3 || !isObjectId(parts[0]!) || !isObjectId(parts[1]!)) {
      return textResponse(400, 'malformed push command');
    }
    commands.push({ oldOid: parts[0]!, newOid: parts[1]!, ref: parts[2]! });
  }
  if (!sawFlush || !commands.length) return textResponse(400, 'malformed push command list');
  const packBytes = body.subarray(offset);
  const report = (unpackOk: boolean, unpackDetail: string, refs: Array<{ ref: string; ok: boolean; reason: string }>): GitResponse => {
    const inner: Buffer[] = [pktLine(`unpack ${unpackOk ? 'ok' : unpackDetail}\n`)];
    for (const ref of refs) {
      inner.push(pktLine(ref.ok ? `ok ${ref.ref}\n` : `ng ${ref.ref} ${ref.reason}\n`));
    }
    const innerBytes = Buffer.concat(inner);
    const bodyOut = sideband
      ? Buffer.concat([sidebandPack(innerBytes), pktFlush()])
      : Buffer.concat([innerBytes, pktFlush()]);
    return { status: 200, contentType: 'application/x-git-receive-pack-result', body: bodyOut };
  };
  // Unpack first: every object lands in the store even when a ref is refused.
  if (packBytes.length > 0) {
    let read;
    try {
      read = readPack(packBytes);
    } catch (err) {
      const detail = err instanceof Error ? err.message : 'unpack error';
      return report(false, detail.slice(0, 120), commands.map((command) => ({ ref: command.ref, ok: false, reason: 'unpack error' })));
    }
    if (!read.checksumOk) {
      return report(false, 'pack checksum mismatch', commands.map((command) => ({ ref: command.ref, ok: false, reason: 'unpack error' })));
    }
    for (const [oid, object] of read.objects) {
      if (!repo.objects.has(oid)) repo.objects.set(oid, object);
    }
  } else if (commands.some((command) => command.newOid !== ZERO_OID)) {
    return report(false, 'unpacker error', commands.map((command) => ({ ref: command.ref, ok: false, reason: 'unpack error' })));
  }
  const results: Array<{ ref: string; ok: boolean; reason: string }> = [];
  for (const command of commands) {
    const current = repo.refs.get(command.ref);
    if (!command.ref.startsWith('refs/') || /[\s\u0000-\u001f]/.test(command.ref)) {
      results.push({ ref: command.ref, ok: false, reason: 'invalid ref' });
      continue;
    }
    if (command.newOid === ZERO_OID) {
      if (!current) {
        results.push({ ref: command.ref, ok: false, reason: 'non-fast-forward' });
      } else if (command.oldOid !== ZERO_OID && command.oldOid !== current) {
        results.push({ ref: command.ref, ok: false, reason: 'non-fast-forward' });
      } else {
        repo.refs.delete(command.ref);
        results.push({ ref: command.ref, ok: true, reason: '' });
      }
      continue;
    }
    if (command.oldOid === ZERO_OID) {
      if (current) {
        results.push({ ref: command.ref, ok: false, reason: 'non-fast-forward' });
        continue;
      }
      if (!repo.objects.has(command.newOid)) {
        results.push({ ref: command.ref, ok: false, reason: 'non-fast-forward' });
        continue;
      }
      repo.refs.set(command.ref, command.newOid);
      results.push({ ref: command.ref, ok: true, reason: '' });
      continue;
    }
    if (!current || command.oldOid !== current) {
      results.push({ ref: command.ref, ok: false, reason: 'non-fast-forward' });
      continue;
    }
    if (command.newOid === command.oldOid) {
      results.push({ ref: command.ref, ok: true, reason: '' });
      continue;
    }
    if (!repo.objects.has(command.newOid)) {
      results.push({ ref: command.ref, ok: false, reason: 'non-fast-forward' });
      continue;
    }
    const oldCommit = peelToCommit(repo, command.oldOid);
    const newCommit = peelToCommit(repo, command.newOid);
    if (!oldCommit || !newCommit || !isAncestor(repo, oldCommit, newCommit)) {
      results.push({ ref: command.ref, ok: false, reason: 'non-fast-forward' });
      continue;
    }
    repo.refs.set(command.ref, command.newOid);
    results.push({ ref: command.ref, ok: true, reason: '' });
  }
  return report(true, 'ok', results);
}

/** Re-exported for the REST layer: SHA-1 of an object the entry stores. */
export function objectIdOf(type: Parameters<typeof hashObject>[0], data: Uint8Array): string {
  return hashObject(type, data);
}
