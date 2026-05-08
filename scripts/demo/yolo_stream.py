#!/usr/bin/env python3
"""YOLO inference server for the Spinifex demo.

Reads a video file, runs YOLO detection, and serves:
  GET /video        — MJPEG stream of annotated frames
  GET /descriptions — SSE stream of qwen3-vl scene descriptions
  GET /health       — {"ok": true}

Environment variables:
  VIDEO_PATH        Path to input .mp4 (required)
  QWEN_HOST         Base URL of the qwen3-vl Ollama instance, e.g. http://10.0.0.5:11434
  YOLO_MODEL        Model name (default: yolo11n.pt)
  YOLO_DEVICE       GPU device index (default: 0)
  DESCRIBE_INTERVAL Seconds between qwen3-vl calls (default: 5.0)
  PORT              Server port (default: 8080)
"""

import asyncio
import base64
import json
import os
import queue
import threading
import time

import cv2
import httpx
import uvicorn
from fastapi import FastAPI
from fastapi.responses import StreamingResponse
from ultralytics import YOLO

VIDEO_PATH = os.environ["VIDEO_PATH"]
QWEN_HOST = os.environ.get("QWEN_HOST", "")
YOLO_MODEL = os.environ.get("YOLO_MODEL", "yolo11x.pt")
YOLO_DEVICE = os.environ.get("YOLO_DEVICE", "0")
DESCRIBE_INTERVAL = float(os.environ.get("DESCRIBE_INTERVAL", "5.0"))
PORT = int(os.environ.get("PORT", "8080"))

app = FastAPI()

_frame_lock = threading.Lock()
_latest_frame: bytes | None = None
_desc_queue: queue.Queue = queue.Queue(maxsize=100)


def _encode(frame, quality: int = 80) -> bytes:
    _, buf = cv2.imencode(".jpg", frame, [cv2.IMWRITE_JPEG_QUALITY, quality])
    return buf.tobytes()


def _describe(img_b64: str) -> None:
    if not QWEN_HOST:
        return
    try:
        with httpx.Client(timeout=180) as client:
            with client.stream(
                "POST",
                f"{QWEN_HOST}/api/generate",
                json={
                    "model": "qwen3-vl:235b",
                    "prompt": "Briefly describe what is happening in this scene. Focus on the objects detected and any notable activity.",
                    "images": [img_b64],
                    "stream": True,
                },
            ) as resp:
                for line in resp.iter_lines():
                    if not line:
                        continue
                    try:
                        data = json.loads(line)
                        token = data.get("response", "")
                        if token and not _desc_queue.full():
                            _desc_queue.put(("token", token))
                        if data.get("done"):
                            _desc_queue.put(("end", ""))
                    except json.JSONDecodeError:
                        pass
    except Exception as exc:
        _desc_queue.put(("error", str(exc)))


def inference_loop() -> None:
    model = YOLO(YOLO_MODEL)
    cap = cv2.VideoCapture(VIDEO_PATH)
    if not cap.isOpened():
        raise RuntimeError(f"Cannot open video: {VIDEO_PATH}")

    fps = cap.get(cv2.CAP_PROP_FPS) or 25.0
    frame_delay = 1.0 / fps
    last_describe = 0.0

    while True:
        ret, frame = cap.read()
        if not ret:
            cap.set(cv2.CAP_PROP_POS_FRAMES, 0)
            continue

        results = model(frame, device=YOLO_DEVICE, verbose=False)
        annotated = results[0].plot()

        with _frame_lock:
            global _latest_frame
            _latest_frame = _encode(annotated)

        now = time.monotonic()
        if QWEN_HOST and now - last_describe >= DESCRIBE_INTERVAL:
            last_describe = now
            img_b64 = base64.b64encode(_encode(annotated, quality=60)).decode()
            threading.Thread(target=_describe, args=(img_b64,), daemon=True).start()

        time.sleep(frame_delay)


@app.on_event("startup")
async def startup() -> None:
    threading.Thread(target=inference_loop, daemon=True).start()


@app.get("/health")
async def health():
    return {"ok": True}


@app.get("/video")
async def video():
    async def generate():
        while True:
            with _frame_lock:
                frame = _latest_frame
            if frame:
                yield b"--frame\r\nContent-Type: image/jpeg\r\n\r\n" + frame + b"\r\n"
            await asyncio.sleep(0.04)

    return StreamingResponse(
        generate(),
        media_type="multipart/x-mixed-replace; boundary=frame",
    )


@app.get("/descriptions")
async def descriptions():
    async def generate():
        yield 'data: {"type":"ready"}\n\n'
        loop = asyncio.get_event_loop()
        while True:
            try:
                kind, text = await loop.run_in_executor(
                    None, lambda: _desc_queue.get(timeout=30)
                )
                yield f"data: {json.dumps({'type': kind, 'text': text})}\n\n"
            except queue.Empty:
                yield 'data: {"type":"ping"}\n\n'

    return StreamingResponse(generate(), media_type="text/event-stream")


if __name__ == "__main__":
    uvicorn.run(app, host="0.0.0.0", port=PORT)
