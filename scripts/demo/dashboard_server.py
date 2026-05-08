#!/usr/bin/env python3
"""Dashboard server for the Spinifex GPU demo.

Proxies streams from the GPU instances and serves the web UI.

Environment variables:
  YOLO_HOST     Base URL of the YOLO instance,   e.g. http://10.0.0.3:8080
  OLLAMA_HOST   Base URL of the Ollama instance, e.g. http://10.0.0.2:11434
  OLLAMA_MODEL  Model name to use for chat       (default: llama3.1:70b)
  PORT          Server port                       (default: 8000)
"""

import asyncio
import json
import os

import httpx
import uvicorn
from fastapi import FastAPI, Request
from fastapi.responses import HTMLResponse, StreamingResponse
from fastapi.staticfiles import StaticFiles

YOLO_HOST = os.environ.get("YOLO_HOST", "http://localhost:8080")
OLLAMA_HOST = os.environ.get("OLLAMA_HOST", "http://localhost:11434")
OLLAMA_MODEL = os.environ.get("OLLAMA_MODEL", "llama3.1:70b")
PORT = int(os.environ.get("PORT", "8000"))

app = FastAPI()
app.mount("/static", StaticFiles(directory="static"), name="static")


@app.get("/", response_class=HTMLResponse)
async def index():
    with open("static/index.html") as f:
        return f.read()


@app.get("/proxy/video")
async def proxy_video():
    async def stream():
        while True:
            try:
                async with httpx.AsyncClient(timeout=None) as client:
                    async with client.stream("GET", f"{YOLO_HOST}/video") as resp:
                        async for chunk in resp.aiter_bytes(8192):
                            yield chunk
            except Exception:
                await asyncio.sleep(5)

    return StreamingResponse(stream(), media_type="multipart/x-mixed-replace; boundary=frame")


@app.get("/proxy/descriptions")
async def proxy_descriptions():
    async def stream():
        while True:
            try:
                async with httpx.AsyncClient(timeout=httpx.Timeout(connect=10.0, read=None, write=None, pool=None)) as client:
                    async with client.stream("GET", f"{YOLO_HOST}/descriptions") as resp:
                        async for line in resp.aiter_lines():
                            yield line + "\n"
            except Exception:
                yield 'data: {"type":"error","text":"YOLO stream offline, retrying..."}\n\n'
                await asyncio.sleep(5)

    return StreamingResponse(stream(), media_type="text/event-stream")


@app.post("/chat")
async def chat(request: Request):
    body = await request.json()
    prompt = body.get("prompt", "").strip()
    if not prompt:
        return {"error": "empty prompt"}

    async def stream():
        async with httpx.AsyncClient(timeout=120) as client:
            async with client.stream(
                "POST",
                f"{OLLAMA_HOST}/api/generate",
                json={"model": OLLAMA_MODEL, "prompt": prompt, "stream": True},
            ) as resp:
                async for line in resp.aiter_lines():
                    if not line:
                        continue
                    try:
                        data = json.loads(line)
                        yield f"data: {json.dumps({'token': data.get('response', ''), 'done': data.get('done', False)})}\n\n"
                    except json.JSONDecodeError:
                        pass

    return StreamingResponse(stream(), media_type="text/event-stream")


@app.get("/status")
async def status():
    results: dict[str, str] = {}
    async with httpx.AsyncClient(timeout=3) as client:
        for name, url in [
            ("yolo", f"{YOLO_HOST}/health"),
            ("ollama", f"{OLLAMA_HOST}/api/tags"),
        ]:
            try:
                r = await client.get(url)
                results[name] = "ok" if r.status_code < 500 else "error"
            except Exception:
                results[name] = "offline"
    return results


if __name__ == "__main__":
    uvicorn.run(app, host="0.0.0.0", port=PORT)
