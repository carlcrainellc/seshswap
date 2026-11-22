#!/usr/bin/env python3
"""Render the SeshSwap mark to PNG and ICO.

    python3 branding/build.py

Needs: Python 3 with Pillow and Playwright, and a Chromium/Chrome (set
CHROME=/path/to/chrome if it isn't /usr/bin/google-chrome). Writes png/ and
seshswap.ico next to this file. 16 px comes from seshswap-mark-small.svg (drawn
on the 16 px grid); every other size from seshswap-mark.svg.
"""
import asyncio, os, pathlib, urllib.parse
from PIL import Image
from playwright.async_api import async_playwright

HERE = pathlib.Path(__file__).resolve().parent
SIZES = [16, 24, 32, 48, 64, 128, 256, 512]
ICO = [16, 24, 32, 48, 256]
CHROME = os.environ.get("CHROME", "/usr/bin/google-chrome")

def src(size):
    name = "seshswap-mark-small.svg" if size <= 16 else "seshswap-mark.svg"
    return (HERE / name).read_text()

async def render():
    out = HERE / "png"
    out.mkdir(exist_ok=True)
    async with async_playwright() as p:
        b = await p.chromium.launch(executable_path=CHROME, args=["--no-sandbox"])
        for s in SIZES:
            pg = await b.new_page(viewport={"width": s, "height": s}, device_scale_factor=1)
            uri = "data:image/svg+xml," + urllib.parse.quote(src(s))
            await pg.set_content(f"<body style='margin:0'><img src=\"{uri}\" width={s} height={s} style='display:block'></body>")
            await pg.wait_for_timeout(50)
            await pg.screenshot(path=str(out / f"seshswap-{s}.png"), omit_background=True)
            await pg.close()
        await b.close()

def ico():
    ims = [Image.open(HERE / "png" / f"seshswap-{s}.png").convert("RGBA") for s in ICO]
    big = ims[-1]
    big.save(HERE / "seshswap.ico", format="ICO", sizes=[(s, s) for s in ICO], append_images=ims[:-1])

asyncio.run(render())
ico()
print("wrote", HERE / "png", "and", HERE / "seshswap.ico")
