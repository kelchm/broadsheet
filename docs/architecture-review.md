# Architecture review — provider / catalog / database / archive boundaries

**Date:** 2026-07-14 · **Status:** diagnosis recorded; no structural changes made yet (deliberate, pre-1.0, single user).

This is a design-review snapshot, not a spec. It records deficiencies found in an
adversarial audit of the four-layer model (six attacker lenses → prosecutor
verification against code → defender steelman → this synthesis) so we can
course-correct deliberately when we choose to. Line references are to the tree as
of this date and will drift — treat them as pointers, not coordinates.

## Verdict

**The four-layer model is fundamentally sound.** The recent decisions — archive as
the source of truth for "what I have," a self-describing `.meta.json` sidecar for
portability, and two independent lifetimes (catalog membership governs *polling*;
archive retention governs *addressability*) — are correct and should be preserved.

The deficiencies are real but cluster into **four root causes**, and the biggest one
(the split-ownership `sources` row) is the source of most of the reconcile churn.
None require a rewrite; the highest-leverage fix is a moderate, self-contained
refactor.

## Root causes

### RC1 — The split-ownership `sources` row (catalog ↔ store). *The central one.*
One SQLite row is co-owned *column by column*: the catalog owns
id/display_name/provider_type/provider_config/position (re-asserted every boot via
`INSERT … ON CONFLICT(id) DO UPDATE`, `store.go` seed), the store owns only
`enabled` (`SetSourceEnabled` is its sole writer; the `Enabled` in the seed row is
just the catalog default and is deliberately omitted from the UPDATE). **Nobody owns
the row.** The `sources` table is a near-total *mirror* of the catalog whose only
real justification is that `enabled` needed a home.

This one decision radiates into:
- the upsert-except-`enabled` dance;
- the prune-cascade that deletes user state when a paper leaves the catalog (and on a downgrade to an older binary);
- the display-name **triple**-storage (see RC-adjacent below);
- `Location` being the one catalog column *not* mirrored, so `Catalog()` re-joins it from `catalog.All()` at call time — a read already split across two sources;
- the "next user-writable column a contributor adds (poll interval, crop) gets clobbered to the catalog default every boot" trap;
- DB-loss resets every `enabled` toggle to the catalog default.

**Fix (recommended when we act):** stop mirroring the catalog into SQLite. Replace
`sources` with a small `source_state(id, enabled, position, …)` table holding *only*
user state. A "paper" = catalog entry (name/provider/config/location read directly
from the in-memory `catalog.All()`) ⋈ user override ⋈ archive. Cheaper here than in
a typical system because the catalog is embedded in-memory data — "joining" it is a
map lookup, not a SQL join. This also drops the display-name storage from triple to
the intended *double* (catalog live-name + sidecar durable-name), cleaning up the
Path C work rather than conflicting with it.

### RC2 — Two persistence substrates, no spanning owner (store ↔ archive).
SQLite records and filesystem artifacts have independent lifetimes
(catalog-membership prune vs age-based retention) with no transaction or shared
"membership" abstraction bridging them. Consequences:
- **Non-atomic dual-write:** the reconciler `Put`s the archive then writes versions/health to the store with no spanning transaction (`reconcile.go`); a crash between them yields an edition on disk with no recorded version (harmless-ish: re-fetched next poll) — but it's unowned.
- **Four-plus divergent "which papers" sets** with none authoritative: `knownSource` unions live-set ∪ `archive.Newest` ∪ store-rows; `ArchiveIndex` uses archive dirs only; `Catalog()` uses store rows only; `RenderFor` checks *only* the live set (so it can `ErrUnknownSource` a paper that `knownSource` says is addressable). This divergence is the edge-case *generator* — most of this conversation was patching instances of it.
- **Crop in the wrong layer:** crop is rendering metadata *about archived bytes* but is keyed in the catalog-pruned store, so a dropped-but-archived paper would render without its crop and a re-add loses it.
- **Three uncoordinated retention passes** (`archive.Prune`, `pruneCache`, `store.PruneFetchEvents`) off one cutoff — low impact (all re-derivable), but three walks that can disagree mid-run.

**Fix direction:** name ONE authority for "what I have / is renderable" — the archive
on disk — and route every read path through a single addressability resolver so the
sets stop diverging. Fold the retention passes.

### RC3 — `id` is an accident, not a validated concept (cross-cutting).
A bare string is copied verbatim into: store PK, `provider_versions`/`fetch_events`/
`crop_overrides` FK-by-convention, archive + cache directory names, URL path param,
and ETag/header — with **no owning constructor and no grammar**. `archive.SourceIDs`
returns every directory name unfiltered and feeds it into `filepath.Join`. This
underlies the `bra^pe-jdc` malformation, phantom addressable papers from
hand-dropped junk dirs, the "not user input" `gosec` nolint, and the
convention-only cross-table integrity.

**Fix:** a `SourceID` type with one validating constructor (grammar:
`[a-z0-9-]+` or similar), enforced at catalog load and at archive-dir enumeration.
Small, high-value, and a **prerequisite for safely opening acquisition** (RC4b).

> **Refuted (do not re-raise):** the "path-traversal RCE" via `/paper/..%2F..%2F…`.
> chi v5 delivers the still-encoded segment verbatim; `filepath.Join(root, "..%2F…")`
> stays inside root and just `ENOENT`s. chi won't put a raw `/` in a `{id}` segment.
> This is a hygiene/correctness defect (RC3), **not** an exploit.

### RC4 — Boundaries shipped ahead of behavior; asymmetric openness.
- **4a — Dead crop schema.** `crop_overrides` is created, CHECK-constrained, and
  cascade-DELETEd on prune, and `CropHints.MastheadText` is threaded
  catalog→registry→`source.Source` — yet there is **no read/write path for crop
  anywhere on main**. A contract (and a data-loss cascade) asserted for a payload of
  nothing. *Note:* smart-crop is in-flight in a separate worktree and migrations are
  append-only, so **don't delete the schema** — reserve it, and when crop lands put
  it in `source_state`/sidecar (which fixes "crop in the wrong layer" for free).
- **4b — Asymmetric openness.** The archive is id-*open* (drop in a `<id>/` dir and
  it's named + renderable via `knownSource`, portability by design) but acquisition
  is catalog-*closed* (`loadEnabled` only reads catalog-seeded rows; any non-catalog
  id is a prune target; `catalog.All()` is embed-only; no add-source path).
  Portability is half-built: you can *render* a transplanted paper but never *keep it
  current*. See the forward plan below.

## Confirmed deficiencies (condensed)

| # | Deficiency | Boundary | Sev | Root |
|---|---|---|---|---|
| 1 | Split-ownership `sources` row; catalog mirror + `enabled` in one row | catalog-store | architectural | RC1 |
| 2 | Next user-writable column added to the upsert SET list gets clobbered every boot | catalog-store | architectural | RC1 |
| 3 | `crop_overrides` is dead schema (created + cascade-pruned, never read/written) | catalog-store | major | RC4a |
| 4 | Crop lives in the catalog-pruned store, not with the archived bytes it describes | store-archive | architectural | RC2/RC4a |
| 5 | Display name materialized in 3 places (catalog, store mirror, sidecar) | catalog-archive | minor | RC1 |
| 6 | `id` is an unvalidated stringly-typed join key across all four layers | cross-cutting | major | RC3 |
| 7 | 4+ divergent "which papers" sets; `RenderFor` bypasses `knownSource` | cross-cutting | architectural | RC2 |
| 8 | Reconciler dual-writes store + archive with no spanning transaction | store-archive | major | RC2 |
| 9 | Three uncoordinated retention passes over one cutoff | store-archive | minor | RC2 |
| 10 | Acquisition catalog-closed while archive is id-open (no user-source path) | provider-catalog | major | RC4b |
| 11 | `MediaType` round-trips lossily through the filename (Put discards `Edition.Media`, read re-derives from ext) | provider-archive | minor | RC2 |
| 12 | Provider/catalog config validated only at runtime, per-row, swallowed on failure | provider-catalog | minor | RC3 |
| 13 | No declared SQL foreign keys; integrity is a hand-written DELETE loop | store-store | minor | RC1 |
| 14 | `Config.Sources` embedder path forks control flow via scattered `if cfg.Sources != nil` | cross-cutting | minor | — |
| 15 | User intent (`enabled`, crop) lives only in SQLite; no portable backstop | store-archive | major | RC1/deferred |
| 16 | Archive silently caps at one edition per source per day (`dayUTC` + `<YYYYMMDD>` filename) | provider-archive | minor | RC2 |
| 17 | Asymmetric provider decode (freedomforum hand-rolled in registry vs wapo decoded into its struct) | provider-catalog | minor | RC3 |

## Preserve — do not touch
Archive-as-truth + self-describing sidecar + portability; the two intentional
lifetimes (catalog→polling, archive→addressability); `SeedSources`' core intent
(catalog owns wiring, refreshed every boot; `enabled` never clobbered); the provider
abstraction with opaque, provider-owned version-token keys; the **fail-safe**
token-revert (a token for an edition that failed to store is reverted so the next
poll retries rather than 304-ing past a missing artifact); the `%PDF` sniff guard in
both providers; the render/serve caching correctness (singleflight cold-render
collapse, `renderSem`/`composeSem` bounds, mtime-stamped PNG invalidation).

## Recommended course-correction (tiers, for when we act)
- **Tier 0 (root fix):** the `source_state` refactor (RC1) — dissolves RC1 and most of RC2's user-state issues; cleans up name-triplication.
- **Tier 1 (cheap, high-value):** `SourceID` validated type (RC3); seed-time catalog decode validation (fail-fast, not runtime-swallow); one addressability resolver so all read paths agree (RC2, incl. `RenderFor`).
- **Tier 2 (hygiene/coordinate):** fold the three retention passes; single-source `MediaType`; reserve/wire crop into `source_state`/sidecar with the smart-crop worktree; reconsider the embedder fork only if a `SourceProvider` seam earns its indirection.

## Overreach risks — what NOT to do
- Don't split `sources` in a way that pushes name/position/**location** joins into every read site — route reads through `catalog.All()` (in-memory) + `source_state`, not a SQL join. (Cheap *here*; would be costly if the catalog were a DB table.)
- Don't build a heavyweight "membership authority" component — the divergence is really *two legitimate lifetimes + one leak* (`RenderFor` bypassing `knownSource`), not a missing framework.
- Don't move crop to the sidecar **and** keep a store copy "as an index" — that recreates the multi-writer sync problem we're criticizing for the display name. Pick one writer.
- Don't add `ON DELETE CASCADE` FKs *and* keep the explicit prune loop — the prune blast radius shouldn't depend on FK definitions.
- Don't delete the `crop_overrides` schema/threading while smart-crop is in-flight (append-only migrations; worktree conflict).
- Don't open a runtime user-add path *before* RC3 (SourceID) and fail-fast decode exist — it reintroduces exactly the swallowed-decode / unvalidated-id risks the closed catalog avoids by construction.

## Forward plan — user-defined sources (RC4b), so we don't paint ourselves into a corner

We may or may not build this, but the following keeps the door open. The eventual
shape: **acquisition becomes as open as the archive** — a user can define a source
(`id + name + provider type + provider config`) that isn't in the compile-time
catalog, so a transplanted/foreign paper can be kept current, not just rendered.

**Target approach (when/if we do it):** user source *definitions* live in the data
dir (a `sources.json`, or a `user_sources` store table) and are **merged over** the
embedded catalog by a single seam. The registry validates each user entry
(`SourceID` grammar + provider `Decode`) at load, failing *loudly per-entry* (never
swallowed — this is now untrusted input). Reconcile/prune treat user-origin sources
as **never pruned by catalog-absence**. Adopting a transplanted archive = adding a
matching user source.

**Corner-avoidance guidance for any work we do *before* then:**
1. **Model "the set of known source definitions" as a composable seam**, even while user sources are empty. When we do the `source_state` refactor, introduce a single `resolveSources()` (today: `return catalog.All()`) that every offer/reconcile/prune path goes through — so adding user sources later is *one* place, not a retrofit across N sites.
2. **Do `SourceID` validation (RC3) regardless** — it's a hard prerequisite for accepting user ids, and cheap/valuable on its own.
3. **Make provider-config decode fail-fast and validating (Tier 1)** — prerequisite for accepting user configs.
4. **Model source ORIGIN (catalog vs user) as a first-class attribute** the moment we touch the store/reconcile — so "not in the embedded catalog ⟹ prune" becomes "not in *any* known-source set ⟹ prune," and user sources are spared. This is the specific spot where continuing to deepen the "catalog is the only source-of-definition" assumption would paint us into a corner.
5. The archive's downstream openness (foreign ids render) is already the pattern; the plan just makes the upstream (acquisition) symmetric.

**In short:** the two safe, corner-avoiding investments are `SourceID` validation and
a single `resolveSources()` seam. With those in place, user-defined sources is an
additive feature, not a refactor.
