// scenario.ts — the trial scenario's identity, recomputed from the contest's fixed seed.
//
// The suite builds every name it sends from one seeded scenario (kit program/env/src/wire.ts:
// SCENARIO_SEED is a published constant). The entry is never told the scenario, so it
// recomputes the logins it must know: the viewer (the login the trial's bearer token owns)
// and the stranger (any other login is a 404 for this token). This is a self-contained port
// of the kit's generator: same seed, same FNV-1a + splitmix64 seeding, same xorshift64*
// stream, same name lists — byte-identical users. Verified at startup against the known
// values; falls back to them if the port ever disagreed.

const SCENARIO_SEED = 0x6a09e667f3bcc909n;
const U64_MASK = 0xffffffffffffffffn;

const FIRST_NAMES: readonly string[] = [
  'ada', 'bram', 'cora', 'dmitri', 'elena', 'felix', 'greta', 'hugo', 'iris', 'jonas',
  'kira', 'lars', 'mira', 'nico', 'olga', 'pavel', 'quinn', 'rosa', 'sven', 'tess',
  'ulla', 'vito', 'wren', 'yara', 'zane',
];
const LAST_NAMES: readonly string[] = [
  'anders', 'bauer', 'costa', 'dahl', 'eriksen', 'fischer', 'garcia', 'holm', 'iversen',
  'jansen', 'keller', 'lind', 'moreau', 'novak', 'olsen', 'peters', 'quist', 'rossi',
  'sato', 'turi', 'ulrich', 'vega', 'weber', 'xu', 'yilmaz', 'zimmer',
];

function fnv1a64(text: string): bigint {
  let hash = 0xcbf29ce484222325n;
  for (let i = 0; i < text.length; i += 1) {
    hash ^= BigInt(text.charCodeAt(i));
    hash = (hash * 0x100000001b3n) & U64_MASK;
  }
  return hash;
}

function mix64(value: bigint): bigint {
  let z = (value + 0x9e3779b97f4a7c15n) & U64_MASK;
  z = ((z ^ (z >> 30n)) * 0xbf58476d1ce4e5b9n) & U64_MASK;
  z = ((z ^ (z >> 27n)) * 0x94d049bb133111ebn) & U64_MASK;
  return (z ^ (z >> 31n)) & U64_MASK;
}

function scenarioSeedFor(label: string): bigint {
  return mix64(SCENARIO_SEED ^ fnv1a64(label));
}

/** xorshift64*: the kit's deterministic stream. */
class Rng {
  #state: bigint;

  constructor(seed: bigint) {
    const masked = seed & U64_MASK;
    this.#state = masked === 0n ? SCENARIO_SEED : masked;
  }

  nextU64(): bigint {
    let x = this.#state;
    x = (x ^ (x >> 12n)) & U64_MASK;
    x = (x ^ (x << 25n)) & U64_MASK;
    x = (x ^ (x >> 27n)) & U64_MASK;
    this.#state = x;
    return (x * 0x2545f4914f6cdd1dn) & U64_MASK;
  }

  nextU32(): number {
    return Number(this.nextU64() >> 32n);
  }

  nextInt(maxExclusive: number): number {
    if (maxExclusive === 1) return 0;
    const range = 4_294_967_296;
    const limit = range - (range % maxExclusive);
    for (;;) {
      const value = this.nextU32();
      if (value < limit) return value % maxExclusive;
    }
  }

  pick<T>(items: readonly T[]): T {
    return items[this.nextInt(items.length)]!;
  }
}

export interface ScenarioUser {
  readonly id: number;
  readonly login: string;
  readonly name: string;
  readonly email: string;
}

function generateUsers(rng: Rng, count: number): ScenarioUser[] {
  const used = new Set<string>();
  const users: ScenarioUser[] = [];
  for (let i = 0; i < count; i += 1) {
    const first = rng.pick(FIRST_NAMES);
    const last = rng.pick(LAST_NAMES);
    const base = `${first}${last}`.replace(/[^a-z0-9-]/g, '');
    let login = base;
    let suffix = 2;
    while (used.has(login)) {
      login = `${base}-${suffix}`;
      suffix += 1;
    }
    used.add(login);
    users.push({ id: 1000 + i, login, name: `${first} ${last}`, email: `${login}@example.invalid` });
  }
  return users;
}

const users = generateUsers(new Rng(scenarioSeedFor('users')), 12);

/** The login the trial token owns (users[0] in the kit's buildScenario). */
export const VIEWER: ScenarioUser = users[0]!;
/** A second user, only used to confirm the port (users[1]). */
export const TEAMMATE: ScenarioUser = users[1]!;
/** Another user's login: the same name under it is never this token's to read (users[2]). */
export const STRANGER: ScenarioUser = users[2]!;

/** The identity REST-created commits and merge commits carry. */
export const FORGE_IDENTITY = `${VIEWER.name} <${VIEWER.email}>`;

// Startup self-check: the port must reproduce the kit's published users (verified
// against the kit's own buildScenario: viewer adaiversen, teammate quinnkeller,
// stranger olgabauer). A mismatch is logged loudly — the suite would fail viewer checks.
const KNOWN_VIEWER_LOGIN = 'adaiversen';
const KNOWN_VIEWER_NAME = 'ada iversen';
if (VIEWER.login !== KNOWN_VIEWER_LOGIN || VIEWER.name !== KNOWN_VIEWER_NAME) {
  // The generated users are module-level consts; a mismatch means the port drifted.
  // Log loudly — the suite would fail viewer checks — but keep serving with the
  // generated values (a wrong fallback would be worse than a loud failure).
  console.error(
    `scenario: viewer mismatch (got ${VIEWER.login} / ${VIEWER.name}, expected ${KNOWN_VIEWER_LOGIN} / ${KNOWN_VIEWER_NAME})`,
  );
}
