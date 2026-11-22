# SeshSwap branding

## Mark

An **S made of two arrows going opposite ways**: the top stroke runs right,
the bottom one runs left, and they meet in the middle. It reads as "S" and as
"swap" at the same time, on a dark rounded tile.

| File | Use |
| --- | --- |
| `seshswap-mark.svg` | Source. 64-unit grid; every size from 24 px up. |
| `seshswap-mark-small.svg` | Source for 16 px, drawn on the 16 px grid so the strokes land on whole pixels. |
| `png/seshswap-{16,24,32,48,64,128,256,512}.png` | Rendered. |
| `seshswap.ico` | Windows icon with 16, 24, 32, 48 and 256 px frames. |

`python3 branding/build.py` re-renders the PNGs and the ICO from the SVGs
(needs Pillow, Playwright and Chrome/Chromium).

Where it's used:

- Wallet page: sidebar and welcome header (inline SVG) and the favicon
  (`swapd/internal/wallet/web/index.html`).
- Windows: `seshswap.exe` and `SeshSwap-setup.exe` get the icon and version
  info from `scripts/winres.sh` (go-winres, no manifest) at package time, so
  Explorer, the taskbar, the console window and the Start menu entry show it.
- Linux: `packaging/linux/seshswap.svg` (desktop entry icon) is a copy of
  `seshswap-mark.svg`.

## Colors

| Role | Dark | Light |
| --- | --- | --- |
| Accent (jade): buttons, links, the top arrow | `#46D3B0` (text on it `#05261E`) | `#0B8A6F` (text on it `#FFFFFF`) |
| Second (periwinkle): the bottom arrow, small tags | `#A3B1FF` | `#5A6BE0` |
| Background | `#0B0F14` | `#F5F7F9` |
| Sidebar | `#0F151C` | `#ECF0F3` |
| Card | `#141B23` | `#FFFFFF` |
| Line | `#26313D` | `#D9E0E6` |
| Text | `#E6EDF3` | `#0E1620` |
| Muted text | `#8A98A8` | `#566474` |
| Icon tile | `#0E1620` | `#0E1620` |

Warnings stay amber and errors stay red. The wallet follows the system's
light or dark setting.

## Words

The product is **SeshSwap** (one word, two capitals). The site is
`seshswap.app`; the wallet links it once, on the Settings screen.
