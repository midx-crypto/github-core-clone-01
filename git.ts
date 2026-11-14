// Git smart HTTP: packfiles, pkt-lines, protocol v2 upload-pack and v0
// receive-pack, implemented on node:zlib / node:crypto only. No git binary.
//
// Pack format (git): "PACK" + u32 version 2 + u32 count, then per object a
// type/size varint header + zlib stream, then the sha1 trailer of all bytes
// before it. This forge writes full objects only (no deltas); it reads full
// objects and OFS/REF deltas, so any client pack parses.

import { createHash } from 'node:crypto';
import { inflateSync, deflateSync } from 'node:zlib';
import {
  ZERO_OID,
  collectReachable,
  commitDistances,
  hashObject,
  parseCommit,
  parseTag,
  parseTree,
  type GitObj,
  type ObjType,
  type Repo,
  type Store,
} from './store.ts';

export const TYPE_CODE: Record<ObjType, number> = { commit: 1, tree: 2, blob: 3, tag: 4 };
const CODE_TYPE: Record<number, ObjType> = { 1: 'commit', 2: 'tree', 3: 'blob', 4: 'tag' };

// -------------------------------------------------------------------------------------------
// pkt-lines
// -------------------------------------------------------------------------------------------

export function pktLine(payload: string | Buffer): Buffer {
  const body = typeof payload === 'string' ? Buffer.from(payload, 'utf8') : payload;
  const len = body.length + 4;
  return Buffer.concat([Buffer.from(len.toString(16).padStart(4, '0'), 'ascii'), body]);
}

export const PKT_FLUSH = Buffer.from('0000', 'ascii');
export const PKT_DELIM = Buffer.from('0001', 'ascii');

export function pktLines(lines: readonly (string | Buffer | 'flush' | 'delim')[]): Buffer {
  const parts: Buffer[] = [];
  for (const line of lines) {
    if (line === 'flush') parts.push(PKT_FLUSH);
    else if (line === 'delim') parts.push(PKT_DELIM);
    else parts.push(pktLine(line));
  }
  return Buffer.concat(parts);
}

export interface Pkt {
  kind: 'data' | 'flush' | 'delim' | 'end';
  payload: Buffer;
}

/** Parse a whole pkt-line stream into packets (tolerates trailing garbage). */
export function parsePktLines(buf: Buffer): Pkt[] {
  const out: Pkt[] = [];
  let offset = 0;
  while (offset + 4 <= buf.length) {
    const hex = buf.subarray(offset, offset + 4).toString('ascii');
    const len = Number.parseInt(hex, 16);
    if (!Number.isFinite(len) || len < 0) break;
    if (len === 0) {
      out.push({ kind: 'flush', payload: Buffer.alloc(0) });
      offset += 4;
      continue;
    }
    if (len === 1) {
      out.push({ kind: 'delim', payload: Buffer.alloc(0) });
      offset += 4;
      continue;
    }
    if (len === 2) {
      out.push({ kind: 'end', payload: Buffer.alloc(0) });
      offset += 4;
      continue;
    }
    if (offset + len > buf.length) break;
    out.push({ kind: 'data', payload: Buffer.from(buf.subarray(offset + 4, offset + len)) });
    offset += len;
  }
  return out;
}

function pktText(packets: readonly Pkt[]): string[] {
  const out: string[] = [];
  for (const packet of packets) {
    if (packet.kind === 'data') out.push(packet.payload.toString('utf8'));
  }
  return out;
}

// -------------------------------------------------------------------------------------------
// packfiles
// -------------------------------------------------------------------------------------------

function packObjectHeader(type: ObjType, size: number): Buffer {
  const code = TYPE_CODE[type];
  const bytes: number[] = [];
  let first = (code << 4) | (size & 0x0f);
  let rest = size >> 4;
  if (rest > 0) first |= 0x80;
  bytes.push(first);
  while (rest > 0) {
    let byte = rest & 0x7f;
    rest >>= 7;
    if (rest > 0) byte |= 0x80;
    bytes.push(byte);
  }
  return Buffer.from(bytes);
}

export function writePack(objects: readonly GitObj[]): Buffer {
  const parts: Buffer[] = [Buffer.from('PACK'), Buffer.alloc(4), Buffer.alloc(4)];
  parts[1]!.writeUInt32BE(2, 0);
  parts[2]!.writeUInt32BE(objects.length, 0);
  for (const obj of objects) {
    parts.push(packObjectHeader(obj.type, obj.data.length));
    parts.push(deflateSync(obj.data, { level: 6 }));
  }
  const body = Buffer.concat(parts);
  const trailer = createHash('sha1').update(body).digest();
  return Buffer.concat([body, trailer]);
}

interface RawEntry {
  offset: number;
  typeCode: number;
  size: number;
  data: Buffer;
  baseOffset?: number;
  baseOid?: string;
}

function readVarintSize(buf: Buffer, start: number): { size: number; next: number; first: number } {
  let byte = buf[start]!;
  let size = byte & 0x0f;
  let shift = 4;
  let offset = start + 1;
  while ((byte & 0x80) !== 0) {
    byte = buf[offset]!;
    offset += 1;
    size |= (byte & 0x7f) << shift;
    shift += 7;
  }
  return { size, next: offset, first: buf[start]! };
}

function readOfsDeltaBase(buf: Buffer, start: number): { baseOffset: number; next: number } {
  let byte = buf[start]!;
  let offset = baseOffsetOf(byte);
  let next = start + 1;
  while ((byte & 0x80) !== 0) {
    byte = buf[next]!;
    next += 1;
    offset = ((offset + 1) << 7) | (byte & 0x7f);
  }
  return { baseOffset: offset, next };
}

function baseOffsetOf(byte: number): number {
  return byte & 0x7f;
}

export function applyDelta(base: Buffer, delta: Buffer): Buffer {
  let offset = 0;
  const readVarint = (): number => {
    let value = 0;
    let shift = 0;
    for (;;) {
      const byte = delta[offset]!;
      offset += 1;
      value |= (byte & 0x7f) << shift;
      shift += 7;
      if ((byte & 0x80) === 0) break;
    }
    return value;
  };
  const sourceSize = readVarint();
  const targetSize = readVarint();
  if (sourceSize !== base.length) throw new Error('delta base size mismatch');
  const out = Buffer.alloc(targetSize);
  let written = 0;
  while (offset < delta.length) {
    const op = delta[offset]!;
    offset += 1;
    if ((op & 0x80) !== 0) {
      let copyOffset = 0;
      let copySize = 0;
      for (let i = 0; i < 4; i += 1) {
        if ((op & (1 << i)) !== 0) {
          copyOffset |= delta[offset]! << (8 * i);
          offset += 1;
        }
      }
      for (let i = 0; i < 3; i += 1) {
        if ((op & (0x10 << i)) !== 0) {
          copySize |= delta[offset]! << (8 * i);
          offset += 1;
        }
      }
      if (copySize === 0) copySize = 0x10000;
      base.copy(out, written, copyOffset, copyOffset + copySize);
      written += copySize;
    } else if (op !== 0) {
      delta.copy(out, written, offset, offset + op);
      written += op;
      offset += op;
    } else {
      throw new Error('delta opcode 0');
    }
  }
  if (written !== targetSize) throw new Error('delta size mismatch');
  return out;
}

/** Read a packfile, resolving deltas against pack entries and `external` objects. */
export function readPack(pack: Buffer, external?: ReadonlyMap<string, GitObj>): Map<string, GitObj> {
  if (pack.length < 12 || pack.subarray(0, 4).toString('ascii') !== 'PACK') throw new Error('not a packfile');
  const count = pack.readUInt32BE(8);
  const trailerStart = pack.length - 20;
  const entries: RawEntry[] = [];
  const byOffset = new Map<number, RawEntry>();
  let offset = 12;
  for (let i = 0; i < count; i += 1) {
    const entryOffset = offset;
    const header = readVarintSize(pack, offset);
    const typeCode = (header.first >> 4) & 0x07;
    offset = header.next;
    const entry: RawEntry = { offset: entryOffset, typeCode, size: header.size, data: Buffer.alloc(0) };
    if (typeCode === 6) {
      const base = readOfsDeltaBase(pack, offset);
      entry.baseOffset = entryOffset - base.baseOffset;
      offset = base.next;
    } else if (typeCode === 7) {
      entry.baseOid = pack.subarray(offset, offset + 20).toString('hex');
      offset += 20;
    } else if (CODE_TYPE[typeCode] === undefined) {
      throw new Error(`unknown pack type ${typeCode}`);
    }
    const inflated = inflateSync(pack.subarray(offset));
    entry.data = inflated;
    offset += inflatedLength(pack, offset);
    entries.push(entry);
    byOffset.set(entryOffset, entry);
  }
  const resolved = new Map<number, { type: ObjType; data: Buffer; oid: string }>();
  const resolving = new Set<number>();
  const resolve = (entry: RawEntry): { type: ObjType; data: Buffer; oid: string } => {
    const cached = resolved.get(entry.offset);
    if (cached !== undefined) return cached;
    if (resolving.has(entry.offset)) throw new Error('delta cycle');
    resolving.add(entry.offset);
    let type: ObjType;
    let data: Buffer;
    if (entry.typeCode === 6) {
      const baseEntry = byOffset.get(entry.baseOffset!);
      if (baseEntry === undefined) throw new Error('ofs-delta base missing');
      const base = resolve(baseEntry);
      type = base.type;
      data = applyDelta(base.data, entry.data);
    } else if (entry.typeCode === 7) {
      const baseOid = entry.baseOid!;
      let base: { type: ObjType; data: Buffer } | undefined;
      for (const candidate of entries) {
        if (candidate.typeCode !== 6 && candidate.typeCode !== 7) {
          const candType = CODE_TYPE[candidate.typeCode]!;
          if (hashObject(candType, candidate.data) === baseOid) {
            base = { type: candType, data: candidate.data };
            break;
          }
        }
      }
      if (base === undefined && external !== undefined) base = external.get(baseOid);
      if (base === undefined) throw new Error(`ref-delta base ${baseOid} missing`);
      type = base.type;
      data = applyDelta(base.data, entry.data);
    } else {
      type = CODE_TYPE[entry.typeCode]!;
      data = entry.data;
    }
    const oid = hashObject(type, data);
    const result = { type, data, oid };
    resolved.set(entry.offset, result);
    resolving.delete(entry.offset);
    return result;
  };
  const out = new Map<string, GitObj>();
  for (const entry of entries) {
    const result = resolve(entry);
    if (!out.has(result.oid)) out.set(result.oid, { type: result.type, data: result.data });
  }
  return out;
}

function inflatedLength(pack: Buffer, start: number): number {
  // inflateSync ignores trailing bytes and throws on a truncated stream, so
  // the stream's exact end is the smallest prefix that inflates cleanly.
  let low = 1;
  let high = pack.length - start;
  while (low < high) {
    const mid = Math.floor((low + high) / 2);
    if (tryInflate(pack.subarray(start, start + mid))) high = mid;
    else low = mid + 1;
  }
  return low;
}

function tryInflate(data: Buffer): boolean {
  try {
    inflateSync(data);
    return true;
  } catch {
    return false;
  }
}

// -------------------------------------------------------------------------------------------
// Smart-HTTP handlers. Each returns {status, contentType, body}; app.ts writes them out.
// -------------------------------------------------------------------------------------------

export interface GitResponse {
  status: number;
  contentType: string;
  body: Buffer;
}

const AGENT = 'forge/1.0';

function sortedRefNames(repo: Repo): string[] {
  const names = [...repo.refs.keys()];
  names.sort((a, b) => {
    const rank = (name: string): number => (name.startsWith('refs/heads/') ? 0 : name.startsWith('refs/tags/') ? 1 : 2);
    const ra = rank(a);
    const rb = rank(b);
    if (ra !== rb) return ra - rb;
    return a < b ? -1 : a > b ? 1 : 0;
  });
  return names;
}

/** The commit an annotated tag peels to (following tag objects). */
function peelToCommit(repo: Repo, oid: string): string | null {
  let current = oid;
  for (let i = 0; i < 10; i += 1) {
    const obj = repo.objects.get(current);
    if (obj === undefined) return null;
    if (obj.type === 'commit') return current;
    if (obj.type === 'tag') {
      current = parseTag(obj.data).object;
      continue;
    }
    return null;
  }
  return null;
}

function v0RefLines(repo: Repo, caps: string): Buffer {
  const lines: (string | 'flush')[] = [];
  const headTip = repo.branchTip('main');
  if (repo.refs.size === 0) {
    lines.push(`${ZERO_OID} capabilities^{}\0${caps}\n`);
    lines.push('flush');
    return pktLines(lines);
  }
  let first = true;
  const headLine = `${headTip ?? ZERO_OID} HEAD${first ? `\0${caps}` : ''}\n`;
  lines.push(headLine);
  first = false;
  for (const name of sortedRefNames(repo)) {
    const oid = repo.refs.get(name)!;
    lines.push(`${oid} ${name}\n`);
    const obj = repo.objects.get(oid);
    if (obj !== undefined && obj.type === 'tag') {
      const peeled = peelToCommit(repo, oid);
      if (peeled !== null) lines.push(`${peeled} ${name}^{}\n`);
    }
  }
  lines.push('flush');
  return pktLines(lines);
}

export function infoRefs(repo: Repo | null, service: string, wantV2: boolean): GitResponse {
  if (repo === null) {
    return { status: 404, contentType: 'application/json; charset=utf-8', body: Buffer.from(JSON.stringify({ message: 'Not Found' }) + '\n', 'utf8') };
  }
  if (service === 'git-upload-pack' && wantV2) {
    const body = pktLines([
      'version 2\n',
      'ls-refs=unborn peel symrefs ref-prefix\n',
      'fetch=shallow filter\n',
      'object-format=sha1\n',
      `agent=${AGENT}\n`,
      'flush',
    ]);
    return { status: 200, contentType: 'application/x-git-upload-pack-advertisement', body };
  }
  if (service === 'git-upload-pack') {
    const caps = `symref=HEAD:refs/heads/main side-band-64k thin-pack ofs-delta shallow multi_ack_detailed agent=${AGENT}`;
    const preamble = pktLine('# service=git-upload-pack\n');
    return { status: 200, contentType: 'application/x-git-upload-pack-advertisement', body: Buffer.concat([preamble, PKT_FLUSH, v0RefLines(repo, caps)]) };
  }
  if (service === 'git-receive-pack') {
    const caps = `report-status side-band-64k delete-refs ofs-delta agent=${AGENT}`;
    const preamble = pktLine('# service=git-receive-pack\n');
    return { status: 200, contentType: 'application/x-git-receive-pack-advertisement', body: Buffer.concat([preamble, PKT_FLUSH, v0RefLines(repo, caps)]) };
  }
  return { status: 400, contentType: 'application/json; charset=utf-8', body: Buffer.from(JSON.stringify({ message: 'unknown service' }) + '\n', 'utf8') };
}

interface FetchPlan {
  wants: string[];
  haves: string[];
  shallowBoundaries: string[];
  depth: number | null;
}

/** The tree and blob oids under one tree, without following any commit. */
function treeClosure(objects: ReadonlyMap<string, GitObj>, treeOid: string): Set<string> {
  const out = new Set<string>();
  const stack = [treeOid];
  while (stack.length > 0) {
    const oid = stack.pop()!;
    if (out.has(oid)) continue;
    const obj = objects.get(oid);
    if (obj === undefined || obj.type !== 'tree') continue;
    out.add(oid);
    for (const entry of parseTree(obj.data)) {
      const member = objects.get(entry.oid);
      if (member === undefined) continue;
      if (member.type === 'tree') stack.push(entry.oid);
      else if (member.type === 'blob') out.add(entry.oid);
    }
  }
  return out;
}

function planFetch(repo: Repo, plan: FetchPlan): { objects: GitObj[]; shallow: string[]; unshallow: string[] } {
  const dist = commitDistances(repo.objects, plan.wants);
  const maxDepth = plan.depth === null ? Infinity : plan.depth;
  const included = new Set<string>();
  for (const [oid, d] of dist) {
    if (d < maxDepth) included.add(oid);
  }
  const shallow: string[] = [];
  if (plan.depth !== null) {
    for (const [oid, d] of dist) {
      if (d === plan.depth - 1) shallow.push(oid);
    }
    shallow.sort();
  }
  const unshallow: string[] = [];
  if (plan.depth !== null) {
    for (const boundary of plan.shallowBoundaries) {
      if (included.has(boundary) && !shallow.includes(boundary)) unshallow.push(boundary);
    }
    unshallow.sort();
  }
  const haveSet = new Set(plan.haves);
  const send = new Set<string>();
  if (plan.depth === null) {
    const wanted = collectReachable(repo.objects, plan.wants);
    const held = collectReachable(repo.objects, plan.haves);
    for (const oid of wanted) if (!held.has(oid)) send.add(oid);
  } else {
    for (const oid of included) {
      const obj = repo.objects.get(oid);
      if (obj === undefined) continue;
      if (obj.type === 'tag') {
        if (!haveSet.has(oid)) send.add(oid);
        continue;
      }
      if (obj.type !== 'commit') continue;
      if (haveSet.has(oid)) continue;
      send.add(oid);
      // The commit's own tree and blobs only: collectReachable would follow
      // parent links and pull in the whole history's trees and blobs, which
      // is what a shallow fetch must not do.
      for (const member of treeClosure(repo.objects, parseCommit(obj.data).tree)) send.add(member);
    }
    // Wanted tag objects themselves (clone wants include tag oids).
    for (const want of plan.wants) {
      const obj = repo.objects.get(want);
      if (obj !== undefined && obj.type === 'tag' && !haveSet.has(want)) send.add(want);
    }
  }
  const objects: GitObj[] = [];
  const ordered = [...send].sort();
  for (const oid of ordered) {
    const obj = repo.objects.get(oid);
    if (obj !== undefined) objects.push(obj);
  }
  return { objects, shallow, unshallow };
}

function sidebandPack(pack: Buffer): Buffer {
  const parts: Buffer[] = [];
  const maxData = 65515;
  for (let offset = 0; offset < pack.length; offset += maxData) {
    const chunk = pack.subarray(offset, Math.min(pack.length, offset + maxData));
    const payload = Buffer.concat([Buffer.from([1]), chunk]);
    parts.push(pktLine(payload));
  }
  if (pack.length === 0) parts.push(pktLine(Buffer.from([1])));
  return Buffer.concat(parts);
}

function parseV2Args(packets: readonly Pkt[]): { command: string; args: string[] } {
  const lines = pktText(packets);
  let command = '';
  const args: string[] = [];
  for (const line of lines) {
    const trimmed = line.replace(/\n$/, '');
    if (trimmed.startsWith('command=')) command = trimmed.slice('command='.length);
    else if (trimmed !== '') args.push(trimmed);
  }
  return { command, args };
}

function parseFetchArgs(args: readonly string[]): FetchPlan {
  const plan: FetchPlan = { wants: [], haves: [], shallowBoundaries: [], depth: null };
  for (const arg of args) {
    if (arg.startsWith('want ')) plan.wants.push(arg.slice(5).trim());
    else if (arg.startsWith('have ')) plan.haves.push(arg.slice(5).trim());
    else if (arg.startsWith('shallow ')) plan.shallowBoundaries.push(arg.slice(8).trim());
    else if (arg.startsWith('deepen ')) {
      const n = Number.parseInt(arg.slice(7).trim(), 10);
      if (Number.isFinite(n)) plan.depth = n;
    }
  }
  return plan;
}

export function uploadPack(repo: Repo | null, body: Buffer, wantV2: boolean): GitResponse {
  if (repo === null) {
    return { status: 404, contentType: 'application/json; charset=utf-8', body: Buffer.from(JSON.stringify({ message: 'Not Found' }) + '\n', 'utf8') };
  }
  const packets = parsePktLines(body);
  const lines = pktText(packets);
  const first = (lines[0] ?? '').replace(/\n$/, '');
  try {
    if (first.startsWith('command=')) {
      const { command, args } = parseV2Args(packets);
      if (command === 'ls-refs') return lsRefs(repo, args);
      if (command === 'fetch') return fetchV2(repo, args);
      throw new Error(`unknown command ${command}`);
    }
    if (first.startsWith('want ')) return fetchV0(repo, lines);
    throw new Error('unrecognised upload-pack request');
  } catch (err) {
    return { status: 400, contentType: 'application/json; charset=utf-8', body: Buffer.from(JSON.stringify({ message: err instanceof Error ? err.message : String(err) }) + '\n', 'utf8') };
  }
}

function lsRefs(repo: Repo, args: readonly string[]): GitResponse {
  let peel = false;
  let symrefs = false;
  let unborn = false;
  const prefixes: string[] = [];
  for (const arg of args) {
    if (arg === 'peel') peel = true;
    else if (arg === 'symrefs') symrefs = true;
    else if (arg === 'unborn') unborn = true;
    else if (arg.startsWith('ref-prefix ')) prefixes.push(arg.slice('ref-prefix '.length));
  }
  const out: string[] = [];
  const headTip = repo.branchTip('main');
  const includeHead = prefixes.length === 0 || prefixes.some((prefix) => 'HEAD'.startsWith(prefix));
  if (includeHead) {
    if (headTip !== undefined) {
      out.push(`${headTip} HEAD${symrefs ? ' symref-target:refs/heads/main' : ''}\n`);
    } else if (unborn) {
      out.push(`unborn HEAD${symrefs ? ' symref-target:refs/heads/main' : ''}\n`);
    }
  }
  for (const name of sortedRefNames(repo)) {
    if (prefixes.length > 0 && !prefixes.some((prefix) => name.startsWith(prefix))) continue;
    const oid = repo.refs.get(name)!;
    let line = `${oid} ${name}`;
    const obj = repo.objects.get(oid);
    if (peel && obj !== undefined && obj.type === 'tag') {
      const peeled = peelToCommit(repo, oid);
      if (peeled !== null) line += ` peeled:${peeled}`;
    }
    out.push(`${line}\n`);
  }
  out.push('flush' as never);
  const parts: Buffer[] = [];
  for (const line of out) {
    if (line === 'flush') parts.push(PKT_FLUSH);
    else parts.push(pktLine(line));
  }
  return { status: 200, contentType: 'application/x-git-upload-pack-result', body: Buffer.concat(parts) };
}

function fetchV2(repo: Repo, args: readonly string[]): GitResponse {
  const plan = parseFetchArgs(args);
  const { objects, shallow, unshallow } = planFetch(repo, plan);
  const pack = writePack(objects);
  const parts: Buffer[] = [];
  // Wire shape, as a real git server sends it: each section is its name line,
  // then its content, then a delim separating it from the next section. There
  // is no leading delim. The acknowledgments section exists only when the
  // client negotiated with "have" lines; a no-haves fetch goes straight to
  // the packfile section. A "have" the store holds is ACKed (plus "ready");
  // unknown haves are answered NAK.
  if (plan.haves.length > 0) {
    parts.push(pktLine('acknowledgments\n'));
    const known = plan.haves.filter((have) => repo.objects.has(have));
    if (known.length === plan.haves.length) {
      for (const have of known) parts.push(pktLine(`ACK ${have}\n`));
      parts.push(pktLine('ready\n'));
    } else {
      parts.push(pktLine('NAK\n'));
    }
    parts.push(PKT_DELIM);
  }
  if (shallow.length > 0 || unshallow.length > 0) {
    parts.push(pktLine('shallow-info\n'));
    for (const oid of shallow) parts.push(pktLine(`shallow ${oid}\n`));
    for (const oid of unshallow) parts.push(pktLine(`unshallow ${oid}\n`));
    parts.push(PKT_DELIM);
  }
  parts.push(pktLine('packfile\n'));
  parts.push(sidebandPack(pack));
  parts.push(PKT_FLUSH);
  return { status: 200, contentType: 'application/x-git-upload-pack-result', body: Buffer.concat(parts) };
}

function fetchV0(repo: Repo, lines: readonly string[]): GitResponse {
  // Two sections split by a flush packet; parsePktLines already split them.
  const plan = parseFetchArgs(lines.filter((line) => line !== 'done'));
  const { objects, shallow, unshallow } = planFetch(repo, plan);
  const pack = writePack(objects);
  const parts: Buffer[] = [];
  parts.push(pktLine('NAK\n'));
  for (const oid of shallow) parts.push(pktLine(`shallow ${oid}\n`));
  for (const oid of unshallow) parts.push(pktLine(`unshallow ${oid}\n`));
  parts.push(sidebandPack(pack));
  parts.push(PKT_FLUSH);
  return { status: 200, contentType: 'application/x-git-upload-pack-result', body: Buffer.concat(parts) };
}

export function receivePack(repo: Repo | null, body: Buffer): GitResponse {
  if (repo === null) {
    return { status: 404, contentType: 'application/json; charset=utf-8', body: Buffer.from(JSON.stringify({ message: 'Not Found' }) + '\n', 'utf8') };
  }
  const packets = parsePktLines(body);
  const commands: Array<{ oldOid: string; newOid: string; ref: string }> = [];
  let packStart = body.length;
  let offset = 0;
  // Walk the raw buffer to find where the pkt-line command section ends.
  for (const packet of packets) {
    if (packet.kind === 'flush') {
      // Recompute the byte offset of this flush packet.
      break;
    }
    if (packet.kind === 'data') {
      const text = packet.payload.toString('utf8').replace(/\n$/, '');
      const nul = text.indexOf('\0');
      const head = nul === -1 ? text : text.slice(0, nul);
      const parts = head.split(' ');
      if (parts.length === 3) commands.push({ oldOid: parts[0]!, newOid: parts[1]!, ref: parts[2]! });
    }
  }
  // Find the pack bytes: everything after the first flush pkt-line.
  {
    let scan = 0;
    while (scan + 4 <= body.length) {
      const len = Number.parseInt(body.subarray(scan, scan + 4).toString('ascii'), 16);
      if (!Number.isFinite(len)) break;
      if (len === 0) {
        packStart = scan + 4;
        break;
      }
      if (len < 4 || scan + len > body.length) break;
      scan += len;
    }
    void offset;
  }
  const results: Array<{ ref: string; ok: boolean; reason: string }> = [];
  try {
    const packBytes = body.subarray(packStart);
    if (packBytes.length > 0) {
      const incoming = readPack(Buffer.from(packBytes), repo.objects);
      for (const [oid, obj] of incoming) {
        if (!repo.objects.has(oid)) repo.objects.set(oid, obj);
      }
    }
    for (const command of commands) {
      const current = repo.refs.get(command.ref);
      if (command.newOid === ZERO_OID) {
        if (current === undefined) results.push({ ref: command.ref, ok: false, reason: 'remote ref does not exist' });
        else {
          repo.deleteRef(command.ref);
          results.push({ ref: command.ref, ok: true, reason: '' });
        }
        continue;
      }
      if (!repo.objects.has(command.newOid)) {
        results.push({ ref: command.ref, ok: false, reason: 'object not found' });
        continue;
      }
      if (command.oldOid === ZERO_OID) {
        if (current !== undefined) {
          results.push({ ref: command.ref, ok: false, reason: 'already exists' });
        } else {
          repo.setRef(command.ref, command.newOid);
          results.push({ ref: command.ref, ok: true, reason: '' });
        }
        continue;
      }
      if (current === undefined) {
        results.push({ ref: command.ref, ok: false, reason: 'remote ref does not exist' });
      } else if (current !== command.oldOid) {
        results.push({ ref: command.ref, ok: false, reason: 'stale info' });
      } else if (!isAncestor(repo.objects, command.oldOid, command.newOid)) {
        results.push({ ref: command.ref, ok: false, reason: 'non-fast-forward' });
      } else {
        repo.setRef(command.ref, command.newOid);
        results.push({ ref: command.ref, ok: true, reason: '' });
      }
    }
  } catch (err) {
    return { status: 400, contentType: 'application/json; charset=utf-8', body: Buffer.from(JSON.stringify({ message: err instanceof Error ? err.message : String(err) }) + '\n', 'utf8') };
  }
  const reportLines: string[] = ['unpack ok\n'];
  for (const result of results) {
    reportLines.push(result.ok ? `ok ${result.ref}\n` : `ng ${result.ref} ${result.reason}\n`);
  }
  // As a real git server sends it: the report pkt-lines plus their own flush
  // live inside one band-1 sideband packet, and a plain flush closes the
  // response after it. Without the inner flush the client keeps reading and
  // sees the connection close as a hangup.
  const report = pktLines(reportLines);
  const parts: Buffer[] = [];
  parts.push(pktLine(Buffer.concat([Buffer.from([1]), report, PKT_FLUSH])));
  parts.push(PKT_FLUSH);
  return { status: 200, contentType: 'application/x-git-receive-pack-result', body: Buffer.concat(parts) };
}

function isAncestor(objects: ReadonlyMap<string, GitObj>, ancestor: string, tip: string): boolean {
  if (ancestor === ZERO_OID) return true;
  if (ancestor === tip) return true;
  return commitDistances(objects, [tip]).has(ancestor);
}
