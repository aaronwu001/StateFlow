"""
Generate demo/demo.gif -- animated terminal-style GIF of the crash-recovery demo.

Usage:  python demo/make_gif.py   (from project root or demo/ directory)
"""

import pathlib
from PIL import Image, ImageDraw, ImageFont

# ── Layout ──────────────────────────────────────────────────────────────────
WIDTH   = 860
PAD_X   = 18
PAD_Y   = 14
LINE_H  = 18
TITLE_H = 32

BG = (18, 18, 18)

# Palette
WHITE  = (220, 220, 220)
DIM    = (110, 110, 110)
GREEN  = ( 80, 200,  80)
CYAN   = ( 80, 200, 210)
YELLOW = (210, 175,  50)
RED    = (215,  65,  55)
ORANGE = (210, 130,  50)
BLUE   = ( 80, 140, 210)
PURPLE = (170, 100, 210)


def _mono(size=13):
    candidates = [
        "Consolas",
        "Courier New",
        "Lucida Console",
        "DejaVuSansMono",
        "LiberationMono",
    ]
    import os
    win_fonts = pathlib.Path(os.environ.get("WINDIR", "C:/Windows")) / "Fonts"
    for name in candidates:
        for ext in (".ttf", ".TTF"):
            p = win_fonts / (name.replace(" ", "") + ext)
            if not p.exists():
                p = win_fonts / (name + ext)
            try:
                return ImageFont.truetype(str(p), size)
            except Exception:
                pass
        try:
            return ImageFont.truetype(name + ".ttf", size)
        except Exception:
            pass
    return ImageFont.load_default()


FONT = _mono(13)


def _col(line):
    l = line.lstrip()
    if l.startswith("[OCR]"):           return GREEN
    if l.startswith("[NER]"):           return CYAN
    if l.startswith("[SUMM"):           return PURPLE
    if l.startswith("INFO"):            return BLUE
    if "[CRASH]" in l:                  return RED
    if "[BOOT2]" in l or "RESTARTING" in l: return ORANGE
    if "[OK]" in l:                     return GREEN
    if l.startswith("==="):             return YELLOW
    if "DONE" in l and "DEMO" not in l: return GREEN
    if l.startswith("---"):             return DIM
    return WHITE


# Each scene: (new lines to append, frames to hold)
SCENES = [
    # 1: title
    ([
        "=======================================================",
        "   StateFlow -- Crash-Recovery Demo",
        "=======================================================",
        "  Proves: kill orchestrator mid-run -> restart",
        "          -> completed steps NOT re-run",
    ], 30),

    # 2: boot + workers
    ([
        "",
        "  [BUILD] go build ./cmd/stateflow/",
        "  [OK] Binary ready",
        "  [DB]  Creating fresh 'stateflow_demo'...",
        "  [OK] Schema applied",
        "  [WORKERS] Starting Flask workers",
        "  [OK] Workers ready  OCR:5001  NER:5002  Summarize:5003",
        "  INFO RecoverRuns: complete count=0",
        "  INFO starting HTTP server addr=:8080",
        "  [OK] StateFlow (boot 1) ready  pid=12345",
    ], 20),

    # 3: run created
    ([
        "",
        "  [RUN] Creating 3-step workflow",
        "  run_id: run-ff95c25b-...",
        "  Watch: [OCR] logs for step 1 (sync, 2s)...",
    ], 14),

    # 4: OCR runs
    ([
        "",
        "[OCR] Processing document: quarterly_report_2026.pdf",
        "[OCR]     (sync, sleeping 2s to simulate text extraction)",
    ], 18),

    # 5: OCR done
    ([
        "[OCR] [DONE] Extraction complete -- 3 pages, confidence 0.98",
        "  [OK] Step 1 (OCR, sync) DONE",
    ], 12),

    # 6: NER dispatched
    ([
        "",
        "[NER]  [START] Starting entity extraction",
        "[NER]     step_id=run-ff95...:ner  attempt_id=44aa8a13...",
        "[NER]     (async, sleeping 5s to simulate LLM call)",
        "  NER dispatched -- callback arrives in 5s",
    ], 20),

    # 7: CRASH
    ([
        "",
        "  [CRASH] KILLING ORCHESTRATOR -- pid 12345",
        "  [CRASH] NER callback channel dies with the process",
        "  [CRASH] DB: step 2 RUNNING, no output; step 3 never started",
        "  Waiting 5s for NER background thread to complete...",
    ], 28),

    # 8: NER finishes during downtime
    ([
        "",
        "[NER]  [DONE] Extraction done -- 3 entities found",
        "[NER]  [WARN] Callback failed: Connection refused",
        "[NER]         (Orchestrator down -- expected)",
    ], 20),

    # 9: Recovery
    ([
        "",
        "  [BOOT2] RESTARTING ORCHESTRATOR...",
        "  INFO RecoverRuns: complete count=1",
        "  INFO recovery: resuming run run_id=run-ff95c25b-...",
        "  INFO starting HTTP server addr=:8080",
        "  [OK] StateFlow (boot 2 -- recovery) ready  pid=24228",
    ], 28),

    # 10: idempotency + step 3
    ([
        "",
        "[NER]  [CACHE] Already processed step_id=run-ff95...:ner",
        "[NER]     Re-sending callback  NEW attempt_id=9a729430...",
        "[NER]  [SENT] Callback delivered HTTP 200",
        "",
        "[SUMMARIZE] [START] Generating summary: [ocr, ner]",
        "[SUMMARIZE]    (sync, sleeping 2s...)",
    ], 20),

    # 11: DONE
    ([
        "[SUMMARIZE] [DONE] Summary ready -- 17 words",
        "  INFO recovery: run completed run_id=run-ff95c25b-...",
        "",
        "=======================================================",
        "   DEMO COMPLETE",
        "=======================================================",
        "  Run status : DONE",
        "    [DONE] ocr      <- DONE before crash, NOT re-run",
        "    [DONE] ner      <- re-dispatched, idempotency cache hit",
        "    [DONE] summarize <- ran for the FIRST time after recovery",
        "  [OK] Crash-recovery demo successful",
    ], 50),

    # 12: proof
    ([
        "",
        "=======================================================",
        "  PROOF: STEPS 1-2 WERE NOT RE-RUN",
        "=======================================================",
        "  [OCR] appears ONCE (before crash)  <- absent after restart",
        "  [NER] [START] appears ONCE          <- absent after restart",
        "  [NER] [CACHE] after restart = idempotency cache hit",
        "  [SUMMARIZE] after restart = first time ever",
        "  INFO RecoverRuns: complete count=1  <- found RUNNING run",
    ], 80),
]


def _frame(lines):
    max_vis = (480 - TITLE_H - PAD_Y * 2) // LINE_H
    visible = lines[-max_vis:] if len(lines) > max_vis else lines

    img = Image.new("RGB", (WIDTH, 480), BG)
    d   = ImageDraw.Draw(img)

    # title bar
    d.rectangle([(0, 0), (WIDTH, TITLE_H - 1)], fill=(38, 38, 38))
    for i, c in enumerate([(215, 65, 55), (230, 165, 40), (65, 185, 70)]):
        d.ellipse([(12 + i*20, 10), (24 + i*20, 22)], fill=c)
    d.text((WIDTH//2 - 100, 8), "stateflow -- crash-recovery demo",
           fill=(160, 160, 160), font=FONT)

    y = TITLE_H + PAD_Y
    for line in visible:
        try:
            d.text((PAD_X, y), line, fill=_col(line), font=FONT)
        except Exception:
            d.text((PAD_X, y), line.encode("ascii", "replace").decode(),
                   fill=_col(line), font=FONT)
        y += LINE_H
    return img


def make_gif(out: pathlib.Path):
    frames, durations = [], []
    acc = []
    for new_lines, hold in SCENES:
        acc = acc + new_lines
        frames.append(_frame(acc))
        durations.append(hold * 40)

    durations[-1] = 4000

    frames[0].save(
        str(out),
        save_all=True,
        append_images=frames[1:],
        duration=durations,
        loop=0,
        optimize=False,
    )
    print(f"GIF written -> {out}  ({len(frames)} frames, {sum(durations)//1000}s total)")


if __name__ == "__main__":
    out = pathlib.Path(__file__).parent / "demo.gif"
    make_gif(out)
