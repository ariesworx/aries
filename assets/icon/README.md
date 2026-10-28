# Aries icon

Source mark for packaging, the README, favicons and the future site.
The mark is **Celestial Fix — Airy**, on the theme of celestial navigation
(the family of names in the product: Aries, Bearings, Sextant), not
astrology: an open triangle with stars at its vertices and a brass fix at
its center. SVGs are the source; the PNGs and icons are exports of them.

## Palette

| Token | Hex | Use |
|---|---|---|
| Ink | `#0B1D2E` | Strokes and stars, on light grounds |
| Brass | `#C9A227` | The fix |
| Star | `#F4F7FA` | The fix's center |
| Mist | `#E6EDF4` | Strokes and stars, on dark grounds (`aries-dark.svg`) |

## Files

| File | Use |
|---|---|
| `aries.svg` | Source (light grounds) |
| `aries-dark.svg` | Dark grounds |
| `aries-favicon.svg` | Heavier cut used to rasterize 16–48 px |
| `aries-{16,32,48,64,128,256,512,1024}.png` | Raster sizes from the source (16–48 optically tuned) |
| `favicon.svg` / `favicon.ico` | Browser favicon |
| `apple-touch-icon.png` | 180 px touch icon |
| `aries.ico` | Windows multi-size icon |
| `social.png` / `social-dark.png` | GitHub / Open Graph social preview (1280×640) |

## GitHub social preview

In the repository settings → General → Social preview, upload
`social.png` (or `social-dark.png`).

## Site head

```html
<link rel="icon" href="/favicon.svg" type="image/svg+xml">
<link rel="icon" href="/favicon.ico" sizes="any">
<link rel="apple-touch-icon" href="/apple-touch-icon.png">
```

Packaging (`.ico` / `.icns` tooling) is planned in
[docs/design/packaging.md](../../docs/design/packaging.md) §2.
