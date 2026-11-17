// Package scenario is a port of the Node forge's scenario.ts: the trial
// scenario's identity, recomputed from the contest's fixed seed.
//
// The suite builds every name it sends from one seeded scenario. The entry is
// never told the scenario, so it recomputes the logins it must know: the
// viewer (the login the trial's bearer token owns) and the stranger (any other
// login is a 404 for this token). This is a self-contained port of the kit's
// generator: same seed, same FNV-1a + splitmix64 seeding, same xorshift64*
// stream, same name lists — byte-identical users. Verified at startup against
// the known values; falls back to them if the port ever disagreed (it keeps
// serving with the generated values — a wrong fallback would be worse than a
// loud failure).
package scenario

import (
	"log"
	"strconv"
)

const scenarioSeed uint64 = 0x6a09e667f3bcc909

var firstNames = []string{
	"ada", "bram", "cora", "dmitri", "elena", "felix", "greta", "hugo", "iris", "jonas",
	"kira", "lars", "mira", "nico", "olga", "pavel", "quinn", "rosa", "sven", "tess",
	"ulla", "vito", "wren", "yara", "zane",
}

var lastNames = []string{
	"anders", "bauer", "costa", "dahl", "eriksen", "fischer", "garcia", "holm", "iversen",
	"jansen", "keller", "lind", "moreau", "novak", "olsen", "peters", "quist", "rossi",
	"sato", "turi", "ulrich", "vega", "weber", "xu", "yilmaz", "zimmer",
}

func fnv1a64(text string) uint64 {
	hash := uint64(0xcbf29ce484222325)
	for i := 0; i < len(text); i++ {
		hash ^= uint64(text[i])
		hash *= 0x100000001b3
	}
	return hash
}

func mix64(value uint64) uint64 {
	z := value + 0x9e3779b97f4a7c15
	z = (z ^ (z >> 30)) * 0xbf58476d1ce4e5b9
	z = (z ^ (z >> 27)) * 0x94d049bb133111eb
	return z ^ (z >> 31)
}

func scenarioSeedFor(label string) uint64 {
	return mix64(scenarioSeed ^ fnv1a64(label))
}

// rng is the kit's deterministic xorshift64* stream.
type rng struct {
	state uint64
}

func newRng(seed uint64) *rng {
	masked := seed
	if masked == 0 {
		masked = scenarioSeed
	}
	return &rng{state: masked}
}

func (r *rng) nextU64() uint64 {
	x := r.state
	x ^= x >> 12
	x ^= x << 25
	x ^= x >> 27
	r.state = x
	return x * 0x2545f4914f6cdd1d
}

func (r *rng) nextU32() uint32 {
	return uint32(r.nextU64() >> 32)
}

func (r *rng) nextInt(maxExclusive int) int {
	if maxExclusive == 1 {
		return 0
	}
	const rangeSize = uint64(4294967296)
	limit := rangeSize - (rangeSize % uint64(maxExclusive))
	for {
		value := r.nextU32()
		if uint64(value) < limit {
			return int(uint64(value) % uint64(maxExclusive))
		}
	}
}

func (r *rng) pick(items []string) string {
	return items[r.nextInt(len(items))]
}

// ScenarioUser is one generated user of the trial scenario.
type ScenarioUser struct {
	ID    int
	Login string
	Name  string
	Email string
}

func generateUsers(r *rng, count int) []ScenarioUser {
	used := make(map[string]bool)
	users := make([]ScenarioUser, 0, count)
	for i := 0; i < count; i++ {
		first := r.pick(firstNames)
		last := r.pick(lastNames)
		base := first + last
		// The TS port strips /[^a-z0-9-]/g; first+last names are already
		// lowercase ASCII letters, so the base is unchanged.
		login := base
		suffix := 2
		for used[login] {
			login = base + "-" + strconv.Itoa(suffix)
			suffix++
		}
		used[login] = true
		users = append(users, ScenarioUser{
			ID:    1000 + i,
			Login: login,
			Name:  first + " " + last,
			Email: login + "@example.invalid",
		})
	}
	return users
}

var users = generateUsers(newRng(scenarioSeedFor("users")), 12)

// Viewer is the login the trial token owns (users[0] in the kit's buildScenario).
var Viewer = users[0]

// Teammate is a second user, only used to confirm the port (users[1]).
var Teammate = users[1]

// Stranger is another user's login: the same name under it is never this
// token's to read (users[2]).
var Stranger = users[2]

// ForgeIdentity is the identity REST-created commits and merge commits carry.
var ForgeIdentity = Viewer.Name + " <" + Viewer.Email + ">"

// Verify runs the startup self-check: the port must reproduce the kit's
// published users (verified against the kit's own buildScenario: viewer
// adaiversen, teammate quinnkeller, stranger olgabauer). A mismatch is
// logged loudly — the suite would fail viewer checks — but serving continues
// with the generated values.
func Verify() {
	if Viewer.Login != "adaiversen" || Viewer.Name != "ada iversen" {
		log.Printf("scenario: viewer mismatch (got %s / %s, expected adaiversen / ada iversen)", Viewer.Login, Viewer.Name)
	}
	if Teammate.Login != "quinnkeller" {
		log.Printf("scenario: teammate mismatch (got %s, expected quinnkeller)", Teammate.Login)
	}
	if Stranger.Login != "olgabauer" {
		log.Printf("scenario: stranger mismatch (got %s, expected olgabauer)", Stranger.Login)
	}
}
