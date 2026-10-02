# /// script
# requires-python = ">=3.11,<3.13"
# dependencies = [
#     "sentence-transformers>=3.0",
#     "torch>=2.2",
# ]
# ///
"""Сайдкар эмбеддингов: OpenAI-совместимый /v1/embeddings поверх
sentence-transformers.

Запуск:  uv run embedder/server.py [--host 127.0.0.1] [--port 8777]
                                   [--model intfloat/multilingual-e5-base]
                                   [--device cpu|cuda|auto]

Эндпоинты:
  GET  /health         {"ok": true, "model", "dims", "device"}
  GET  /v1/models      {"object": "list", "data": [{"id": model, ...}]}
  POST /v1/embeddings  {"model", "input": str | [str]} →
                       {"object": "list", "data": [{"object": "embedding",
                        "index", "embedding"}], "model", "usage"}

Префиксы e5 («query: »/«passage: ») сервер НЕ добавляет — это делает
клиент: тот же клиент ходит в Ollama и облако, которые о них не знают, и
префикс должен быть в одном месте. Векторы нормированы (normalize_embeddings).

HTTP — только стандартная библиотека (ThreadingHTTPServer): сервер
локальный и однопользовательский, FastAPI с uvicorn тут — лишние
зависимости. Модель одна на процесс, а encode у torch не гарантирует
потокобезопасность, поэтому кодирование под замком; параллелизм потоков
нужен только чтобы /health отвечал, пока идёт длинный батч.
"""

from __future__ import annotations

import argparse
import json
import sys
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

# Предел на тело запроса и число текстов: защита от случайной отправки
# всего корпуса одним запросом (клиент режет на батчи по 32).
MAX_BODY = 64 << 20
MAX_INPUTS = 2048


def pick_device(want: str) -> str:
    """auto → cuda, если torch видит GPU и умеет под неё считать, иначе cpu.

    Свежие CUDA-сборки torch не поддерживают старые карты (GTX 10xx, sm_61):
    is_available() при этом может вернуть True, а первая операция упасть.
    Поэтому auto проверяет пробной операцией, а не только флагом.
    """
    if want != "auto":
        return want
    try:
        import torch

        if torch.cuda.is_available():
            torch.zeros(1, device="cuda").add_(1)
            return "cuda"
    except Exception as e:  # noqa: BLE001 — любая беда с GPU → CPU
        print(f"cuda недоступна ({e}), работаю на cpu", flush=True)
    return "cpu"


class State:
    def __init__(self, model_name: str, device: str):
        from sentence_transformers import SentenceTransformer

        self.name = model_name
        self.device = device
        self.model = SentenceTransformer(model_name, device=device)
        get_dims = getattr(self.model, "get_embedding_dimension", None) or getattr(
            self.model, "get_sentence_embedding_dimension"
        )
        self.dims = int(get_dims())
        self.lock = threading.Lock()

    def encode(self, texts: list[str]) -> list[list[float]]:
        with self.lock:
            vecs = self.model.encode(
                texts,
                batch_size=32,
                normalize_embeddings=True,
                convert_to_numpy=True,
                show_progress_bar=False,
            )
        return vecs.tolist()

    def count_tokens(self, texts: list[str]) -> int:
        """Число токенов для usage — по токенизатору модели; не критично,
        поэтому при любой ошибке — 0."""
        try:
            tok = self.model.tokenizer
            return sum(len(tok.encode(t, add_special_tokens=True)) for t in texts)
        except Exception:  # noqa: BLE001
            return 0


STATE: State | None = None


class Handler(BaseHTTPRequestHandler):
    server_version = "embedder/1"

    def log_message(self, fmt: str, *args) -> None:  # noqa: D401
        # Журнал запросов — в stderr, коротко; stdout оставляем для строки
        # «готов», по которой её ищут скрипты.
        sys.stderr.write("%s %s\n" % (self.address_string(), fmt % args))

    def send_json(self, code: int, obj) -> None:
        body = json.dumps(obj, ensure_ascii=False).encode("utf-8")
        self.send_response(code)
        self.send_header("Content-Type", "application/json; charset=utf-8")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def error(self, code: int, msg: str) -> None:
        self.send_json(code, {"error": msg})

    def do_GET(self) -> None:  # noqa: N802
        path = self.path.split("?", 1)[0].rstrip("/")
        if path == "/health":
            self.send_json(200, {"ok": True, "model": STATE.name, "dims": STATE.dims, "device": STATE.device})
        elif path == "/v1/models":
            self.send_json(
                200,
                {"object": "list", "data": [{"id": STATE.name, "object": "model", "owned_by": "local"}]},
            )
        else:
            self.error(404, f"нет такого пути: {path}")

    def do_POST(self) -> None:  # noqa: N802
        path = self.path.split("?", 1)[0].rstrip("/")
        if path != "/v1/embeddings":
            self.error(404, f"нет такого пути: {path}")
            return
        try:
            n = int(self.headers.get("Content-Length") or 0)
        except ValueError:
            n = -1
        if n <= 0 or n > MAX_BODY:
            self.error(400, "нужно тело запроса (JSON) до 64 МБ")
            return
        try:
            req = json.loads(self.rfile.read(n).decode("utf-8"))
        except Exception as e:  # noqa: BLE001
            self.error(400, f"тело не JSON: {e}")
            return
        inp = req.get("input") if isinstance(req, dict) else None
        if isinstance(inp, str):
            inp = [inp]
        if not isinstance(inp, list) or not all(isinstance(t, str) for t in inp):
            self.error(400, "input — строка или массив строк")
            return
        if len(inp) > MAX_INPUTS:
            self.error(400, f"не больше {MAX_INPUTS} текстов за запрос")
            return
        try:
            vecs = STATE.encode(inp) if inp else []
            tokens = STATE.count_tokens(inp)
        except Exception as e:  # noqa: BLE001
            self.error(500, f"кодирование: {e}")
            return
        self.send_json(
            200,
            {
                "object": "list",
                "data": [{"object": "embedding", "index": i, "embedding": v} for i, v in enumerate(vecs)],
                "model": STATE.name,
                "usage": {"prompt_tokens": tokens, "total_tokens": tokens},
            },
        )


def main() -> None:
    global STATE
    ap = argparse.ArgumentParser(description="Сайдкар эмбеддингов (OpenAI-совместимый /v1/embeddings)")
    ap.add_argument("--host", default="127.0.0.1", help="адрес; по умолчанию только локальный")
    ap.add_argument("--port", type=int, default=8777)
    ap.add_argument("--model", default="intfloat/multilingual-e5-base")
    ap.add_argument("--device", default="cpu", choices=["cpu", "cuda", "auto"])
    args = ap.parse_args()

    # Консоль Windows по умолчанию не UTF-8: без этого print с кириллицей
    # падает при перенаправлении вывода.
    for s in (sys.stdout, sys.stderr):
        try:
            s.reconfigure(encoding="utf-8")
        except Exception:  # noqa: BLE001
            pass

    t0 = time.monotonic()
    device = pick_device(args.device)
    print(f"загрузка модели {args.model} на {device}…", flush=True)
    STATE = State(args.model, device)
    srv = ThreadingHTTPServer((args.host, args.port), Handler)
    srv.daemon_threads = True
    print(
        f"готов: модель {STATE.name}, размерность {STATE.dims}, устройство {STATE.device}"
        f" — http://{args.host}:{args.port} (загрузка {time.monotonic() - t0:.1f} с)",
        flush=True,
    )
    try:
        srv.serve_forever()
    except KeyboardInterrupt:
        pass
    finally:
        srv.server_close()


if __name__ == "__main__":
    main()
