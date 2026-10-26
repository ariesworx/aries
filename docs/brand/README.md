# Aries brand concepts

Draft logo marks for Aries and ariesworx.com. Theme: **celestial navigation**
(the naming family already in the product: Aries, Bearings, Sextant) — not
astrology, and no ram's head.

These are concepts to try in the repo (README, docs, favicons) and on a future
open-source site. Nothing here is the final mark until one is chosen.

## Palette

| Token | Hex | Use |
|---|---|---|
| Ink | `#0B1D2E` | Primary mark, strokes |
| Brass | `#C9A227` | Accent (instrument metal / guide) |
| Sky | `#4A7A9B` | Secondary stroke (ecliptic, intercardinals) |
| Star | `#F4F7FA` | Small highlights on dark fills |

SVGs are the source of truth; they scale without loss. PNGs are raster
exports for places that want a bitmap (chat, GitHub social, etc.).

## The ten marks

| # | File | Concept | Good for |
|---|---|---|---|
| 01 | [`logos/01-polaris.svg`](logos/01-polaris.svg) | **Polaris** — north star + compass rays | Favicon, app icon, densest at small sizes |
| 02 | [`logos/02-sextant.svg`](logos/02-sextant.svg) | **Sextant** — arc, index arm, horizon glass | Product/family mark; pairs with the Sextant binary |
| 03 | [`logos/03-bearings.svg`](logos/03-bearings.svg) | **Bearings** — cardinal ring + fix | Orchestrator / “heading” metaphor |
| 04 | [`logos/04-first-point.svg`](logos/04-first-point.svg) | **First Point of Aries** — equator × ecliptic | The astronomical meaning of the name |
| 05 | [`logos/05-star-path.svg`](logos/05-star-path.svg) | **Star Path** — course with guide stars | Marketing, “shared heading” story |
| 06 | [`logos/06-compass-rose.svg`](logos/06-compass-rose.svg) | **Compass Rose** — eight-point rose | Charts, classic nav feel |
| 07 | [`logos/07-meridian.svg`](logos/07-meridian.svg) | **Meridian** — horizon + altitude sight | Docs, “measure once” metaphor |
| 08 | [`logos/08-azimuth.svg`](logos/08-azimuth.svg) | **Azimuth** — ring, north, bearing to a star | Status / direction UI |
| 09 | [`logos/09-sight-a.svg`](logos/09-sight-a.svg) | **Sight A** — letter A as a navigational sight | Wordmark lockup, README header |
| 10 | [`logos/10-celestial-fix.svg`](logos/10-celestial-fix.svg) | **Celestial Fix** — three LOPs meet | Multi-agent coordination metaphor |

Open [`preview.html`](preview.html) in a browser to compare them on light and
dark grounds.

## Files

```
docs/brand/
  README.md
  preview.html
  logos/                 # SVG sources for light grounds (512 viewBox)
  logos/on-dark/         # Same marks with light ink for dark grounds
  png/                   # 64 / 128 / 512 px exports (light-ground ink)
  png/on-dark/           # Matching exports of the on-dark SVGs
  review/                # Contact sheets and padded composites for side-by-side review
```

## Suggested next steps

1. Pick a primary mark (favicon / GitHub social) and one wordmark lockup
   (often **09 Sight A** or **01 Polaris** + the word “Aries”).
2. Add a monochrome (currentColor) cut of the winner for inline SVG in docs.
3. When the site lands, reuse the same SVG; do not redraw as a flat PNG hero.

## License

Same as the repository (AGPL-3.0-only). Marks are original geometric
artwork for Ariesworx LLC / the Aries project.
