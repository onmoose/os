// storeLayout packs the store landing's authored category groups (a curated
// home.yml, docs/specs/APP_STORE.md # Landing page) two-or-more to a row instead
// of one per line, mirroring the row-packing the control plane's own store
// surface does so the box's landing and the control plane's landing read the
// same way. Kept as a small pure helper (no Vue) so the packing itself is a
// plain function the view calls, not logic buried in the template.
import type { CatalogEntry, CatalogSlide, HomeGroupView } from "../api";

// Group is HomeGroupView with apps normalized to a plain array — the generated
// OpenAPI type allows apps to be null (an omitted/empty JSON array round-trips
// that way), but the packing algorithm and the render loop only ever want to
// iterate it.
export interface Group {
  category: string;
  // label is the authored display heading for the row, carried on the payload so
  // the UI never derives one from the category id.
  label: string;
  apps: CatalogEntry[];
}

// PackedRow is one row of the landing's group layout: 1-4 groups whose apps sum
// to at most the row width (4 columns).
export type PackedRow = Group[];

// ROW_WIDTH is the landing row's width in card columns. A group claims one
// column per app (home.yml caps a group at four apps), so a row is any
// combination summing to four: 2+2, 3+1, 2+1+1, 1+1+1+1.
export const ROW_WIDTH = 4;

// packRows lays the authored groups out two-or-more to a row. Order is a
// curator's preference, not a contract (home.yml's groups are not alphabetical
// either), so this walks the groups in order and pulls forward the first later
// group that fits the gap; a group that can't be paired stands alone rather
// than being reordered further. Faithful port of the control plane's own
// row-packing — same algorithm, same reasoning, so the two surfaces lay out the
// same authored groups identically.
export function packRows(groups: HomeGroupView[], width = ROW_WIDTH): PackedRow[] {
  const left: Group[] = groups.map((g) => ({ category: g.category, label: g.label, apps: g.apps ?? [] }));
  const rows: PackedRow[] = [];
  while (left.length) {
    const first = left.shift();
    if (!first) break;
    const row: PackedRow = [first];
    let free = width - first.apps.length;
    while (free > 0) {
      const i = left.findIndex((g) => g.apps.length <= free);
      if (i < 0) break;
      const g = left.splice(i, 1)[0];
      if (!g) break;
      row.push(g);
      free -= g.apps.length;
    }
    rows.push(row);
  }
  return rows;
}

// spanClasses / colsClasses map a group's app count (1-4, the row width) to the
// Tailwind classes it needs. Written out as static literals rather than
// interpolated ("sm:col-span-" + n) because Tailwind's scanner only sees classes
// that appear verbatim in source — the same reasoning behind the control plane's
// own equivalent layout helper.
const SPAN_CLASSES: Record<1 | 2 | 3 | 4, string> = {
  1: "sm:col-span-1",
  2: "sm:col-span-2",
  3: "sm:col-span-3",
  4: "sm:col-span-4",
};
const COLS_CLASSES: Record<1 | 2 | 3 | 4, string> = {
  1: "sm:grid-cols-1",
  2: "sm:grid-cols-2",
  3: "sm:grid-cols-3",
  4: "sm:grid-cols-4",
};

// clampToRow narrows an app count (possibly 0 or >4, defensively) to a valid
// row-width key, so a malformed group never indexes outside the fixed class
// maps above.
function clampToRow(n: number): 1 | 2 | 3 | 4 {
  return Math.min(Math.max(n, 1), ROW_WIDTH) as 1 | 2 | 3 | 4;
}

// groupSpan / groupCols return the column-span / grid-template classes for a
// group of n apps.
export function groupSpan(apps: CatalogEntry[]): string {
  return SPAN_CLASSES[clampToRow(apps.length)];
}
export function groupCols(apps: CatalogEntry[]): string {
  return COLS_CLASSES[clampToRow(apps.length)];
}

// SlidePage is one page of the landing's discover carousel: a lead slide drawn
// large, and up to two side slides beside it.
export interface SlidePage {
  hero: CatalogSlide;
  sides: CatalogSlide[];
}

// slidePages groups the discover slides into carousel pages of one hero and up
// to two side slides, in authored order. A side slide joins the hero before it
// (or, at the start, the next one); a page with no hero promotes its first side
// slide, so every page has a lead. The other store surface pages its slides
// the same way.
export function slidePages(slides: CatalogSlide[]): SlidePage[] {
  type Draft = { hero: CatalogSlide | null; sides: CatalogSlide[] };
  const drafts: Draft[] = [];
  let cur: Draft | null = null;
  for (const s of slides) {
    if (s.size === "hero") {
      if (cur?.hero) {
        drafts.push(cur);
        cur = null;
      }
      cur ??= { hero: null, sides: [] };
      cur.hero = s;
      continue;
    }
    cur ??= { hero: null, sides: [] };
    if (cur.sides.length === 2) {
      drafts.push(cur);
      cur = { hero: null, sides: [] };
    }
    cur.sides.push(s);
  }
  if (cur) drafts.push(cur);
  const pages: SlidePage[] = [];
  for (const d of drafts) {
    const hero = d.hero ?? d.sides.shift();
    if (hero) pages.push({ hero, sides: d.sides });
  }
  return pages;
}
