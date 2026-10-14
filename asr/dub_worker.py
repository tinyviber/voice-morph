#!/usr/bin/env python3
"""voice-dub ASR worker — FunASR/SenseVoiceSmall over stdio JSON-lines.

One JSON object per line, both directions. stdout is protocol only;
all logging goes to stderr. Offline by construction: the model is
always loaded from a local directory, never a hub id, and the parent
process injects *_OFFLINE env vars.

Commands:
  {"cmd":"hello"}
  {"cmd":"load","model_dir":str,"vad_dir":str|None,"device":"cpu","language":"zh","ncpu":int}
  {"cmd":"transcribe","audio":path,"offset_ms":int,"language":str,
   "want_words":bool,"merge_vad":bool,"merge_length_s":int,
   "batch_size_s":int,"use_itn":bool}
  {"cmd":"download","kind":"sensevoice"|"fsmn-vad","dest":dir}   # user-triggered only
  {"cmd":"shutdown"}

Replies: {"type":"hello"|"loaded"|"progress"|"result"|"error"|"bye"|"downloaded"}
"""

import contextlib
import json
import logging
import os
import re
import sys
import traceback

WORKER_VERSION = 1
MODEL_ID = "iic/SenseVoiceSmall"
VAD_ID = "iic/speech_fsmn_vad_zh-cn-16k-common-pytorch"

_state = {"model": None, "model_dir": None, "vad_dir": None, "word_ts": False}


def emit(obj):
    sys.stdout.write(json.dumps(obj, ensure_ascii=False) + "\n")
    sys.stdout.flush()


def err(cmd, code, msg, fatal=False):
    emit({"type": "error", "cmd": cmd, "code": code, "message": msg, "fatal": fatal})


def log(msg):
    sys.stderr.write("[dub_worker] %s\n" % msg)
    sys.stderr.flush()


def py_ver():
    return "%d.%d.%d" % sys.version_info[:3]


def funasr_version():
    try:
        import funasr  # noqa
        return getattr(funasr, "__version__", "unknown")
    except Exception:
        return ""


def torch_version():
    try:
        import torch  # noqa
        return getattr(torch, "__version__", "unknown")
    except Exception:
        return ""


def cmd_hello(_req):
    emit({
        "type": "hello",
        "worker_version": WORKER_VERSION,
        "python": py_ver(),
        "funasr": funasr_version(),
        "torch": torch_version(),
        "backends": ["funasr"],
        "funasr_imported": funasr_version() != "",
    })


def _rich_clean(text):
    """Strip SenseVoice rich tags; keep the raw text for evidence."""
    try:
        from funasr.utils.postprocess_utils import rich_transcription_postprocess
        return rich_transcription_postprocess(text)
    except Exception:
        return re.sub(r"<\|[^|]*\|>", "", text).strip()


def _probe_word_ts(model):
    """Capability probe: does generate() accept output_timestamp and
    return per-word timings? No fabrication — if the field isn't there,
    we report false and the Go side degrades to segment boundaries.
    NOTE: AutoModel.generate swallows kwargs via **cfg, so an
    inspect.signature check can never see output_timestamp — probe the
    actual call instead."""
    # Build a 1s probe WAV (sine + silence); we only inspect fields.
    import wave, struct, math, tempfile
    with tempfile.NamedTemporaryFile(suffix=".wav", delete=False) as f:
        path = f.name
    try:
        with wave.open(path, "wb") as w:
            w.setnchannels(1)
            w.setsampwidth(2)
            w.setframerate(16000)
            for i in range(16000):
                v = int(3000 * math.sin(2 * math.pi * 220 * i / 16000)) if 4000 < i < 12000 else 0
                w.writeframes(struct.pack("<h", v))
        with contextlib.redirect_stdout(sys.stderr):
            res = model.generate(input=path, language="zh", use_itn=False,
                                 output_timestamp=True, batch_size_s=30)
        for item in res or []:
            if not isinstance(item, dict):
                continue
            for s in item.get("sentence_info") or []:
                wds = s.get("words") or s.get("timestamp")
                if isinstance(wds, list) and len(wds) > 0:
                    return True
            if item.get("words") or item.get("timestamp"):
                return True
    except Exception as e:
        log("word-ts probe failed: %s" % e)
    finally:
        try:
            os.unlink(path)
        except OSError:
            pass
    return False


def cmd_load(req):
    model_dir = req.get("model_dir") or ""
    if not os.path.isdir(model_dir):
        err("load", "MODEL_LOAD_FAIL", "model_dir 不存在: %s" % model_dir, True)
        return
    vad_dir = req.get("vad_dir") or None
    if vad_dir and not os.path.isdir(vad_dir):
        vad_dir = None
    try:
        from funasr import AutoModel
    except Exception as e:
        err("load", "MODEL_LOAD_FAIL", "funasr 不可用: %s" % e, True)
        return
    try:
        kw = {"model": model_dir, "device": req.get("device") or "cpu",
              "disable_update": True, "disable_pbar": True}
        if vad_dir:
            kw["vad_model"] = vad_dir
        ncpu = int(req.get("ncpu") or 0)
        if ncpu > 0:
            kw["ncpu"] = ncpu
        # Third-party libs print progress/logs to stdout; the protocol
        # channel must stay clean JSON-lines — force it all to stderr.
        with contextlib.redirect_stdout(sys.stderr):
            model = AutoModel(**kw)
    except Exception as e:
        err("load", "MODEL_LOAD_FAIL", str(e), True)
        return
    _state["model"] = model
    _state["model_dir"] = model_dir
    _state["vad_dir"] = vad_dir
    _state["word_ts"] = _probe_word_ts(model)
    emit({
        "type": "loaded",
        "model_dir": model_dir,
        "vad": bool(vad_dir),
        "word_timestamps": _state["word_ts"],
        "sentence_timestamps": True,
        "detail": "AutoModel(local dir%s)" % ("+vad" if vad_dir else ""),
    })


def _norm_words(item, offset_ms=0, text_hint=""):
    """Extract [{text,start_ms,end_ms}] from whatever field layout this
    funasr version produces; empty list when absent. Never fabricate.
    Real layouts seen in the wild:
      - words: [{text,start,end}, ...] or [[text,start,end], ...]
      - words: ["你","好"] + timestamp: [[s,e], ...] index-paired
      - timestamp alone: [[s,e], ...] paired to text_hint characters
    All times are shifted by offset_ms (chunk reassembly)."""
    out = []
    words = item.get("words")
    ts = item.get("timestamp")
    ts_pairs = [p for p in ts or [] if isinstance(p, (list, tuple)) and len(p) >= 2]
    if isinstance(words, list) and words and isinstance(words[0], str):
        # words: list of strings aligned with timestamp pairs.
        if len(words) == len(ts_pairs):
            for w, (s, e) in zip(words, ts_pairs):
                out.append({"text": str(w), "start_ms": int(s) + offset_ms,
                            "end_ms": int(e) + offset_ms})
            return out
    elif isinstance(words, list):
        candidates = words
    elif ts_pairs:
        # Bare timestamp pairs: pair with the characters of the reported
        # text when counts match exactly; otherwise unusable — skip
        # rather than fabricate.
        chars = [c for c in (text_hint or item.get("text") or "")
                 if not c.isspace()]
        if len(chars) == len(ts_pairs):
            for c, (s, e) in zip(chars, ts_pairs):
                out.append({"text": c, "start_ms": int(s) + offset_ms,
                            "end_ms": int(e) + offset_ms})
        return out
    else:
        candidates = []
    for w in candidates:
        if isinstance(w, dict):
            t = w.get("text") or w.get("word") or w.get("token") or ""
            s = w.get("start", w.get("start_ms", w.get("begin")))
            e = w.get("end", w.get("end_ms"))
        elif isinstance(w, (list, tuple)) and len(w) >= 3:
            t, s, e = str(w[0]), w[1], w[2]
        else:
            continue
        try:
            out.append({"text": str(t), "start_ms": int(s) + offset_ms,
                        "end_ms": int(e) + offset_ms})
        except (TypeError, ValueError):
            continue
    return out


def _segments_from_result(res, offset_ms, want_words, duration_ms=0):
    """Normalize funasr generate() output across field-name variants.
    sentence_info (VAD path) -> top-level text fallback."""
    segs = []
    for item in res or []:
        if not isinstance(item, dict):
            continue
        sent = item.get("sentence_info")
        if isinstance(sent, list) and sent:
            for s in sent:
                if not isinstance(s, dict):
                    continue
                txt = s.get("text") or s.get("sentence") or ""
                st = s.get("start", s.get("begin", 0))
                en = s.get("end", st)
                seg = {
                    "text": _rich_clean(str(txt)),
                    "text_raw": str(txt),
                    "start_ms": int(st) + offset_ms,
                    "end_ms": int(en) + offset_ms,
                    "confidence": None,
                }
                if want_words:
                    seg["words"] = _norm_words(s, offset_ms, str(txt))
                    if not seg["words"]:
                        seg.pop("words")
                segs.append(seg)
            continue
        txt = str(item.get("text") or item.get("value") or item.get("sentence") or "")
        words = _norm_words(item, offset_ms, txt) if want_words else []
        # Non-VAD inference may return only {key, text, timestamp} — no
        # sentence bounds. Derive coarse bounds from token timestamps if
        # present, else the whole chunk duration; mark "coarse" so the
        # Go side flags estimated_boundary instead of emitting an
        # invalid zero-length segment.
        st = item.get("start")
        en = item.get("end", item.get("duration"))
        coarse = False
        if st is None or en is None:
            coarse = True
            if words:
                st = words[0]["start_ms"] - offset_ms
                en = words[-1]["end_ms"] - offset_ms
            else:
                st, en = 0, int(duration_ms or 0)
            if en <= st:
                en = st + 1
        seg = {
            "text": _rich_clean(txt),
            "text_raw": txt,
            "start_ms": int(st) + offset_ms,
            "end_ms": int(en) + offset_ms,
            "confidence": None,
        }
        if coarse:
            seg["coarse"] = True
        if words:
            seg["words"] = words
        segs.append(seg)
    return segs


def cmd_transcribe(req):
    model = _state.get("model")
    if model is None:
        err("transcribe", "MODEL_LOAD_FAIL", "model 未 load", True)
        return
    audio = req.get("audio") or ""
    offset = int(req.get("offset_ms") or 0)
    want_words = bool(req.get("want_words")) and _state["word_ts"]
    kwargs = {
        "input": audio,
        "language": req.get("language") or "zh",
        "use_itn": bool(req.get("use_itn", True)),
        "batch_size_s": int(req.get("batch_size_s") or 300),
    }
    if _state["vad_dir"]:
        kwargs["merge_vad"] = bool(req.get("merge_vad", True))
        kwargs["merge_length_s"] = int(req.get("merge_length_s") or 15)
    if want_words:
        kwargs["output_timestamp"] = True
    try:
        emit({"type": "progress", "done": 0, "total": 1})
        with contextlib.redirect_stdout(sys.stderr):
            res = model.generate(**kwargs)
        emit({"type": "progress", "done": 1, "total": 1})
    except Exception as e:
        err("transcribe", "ASR_FAIL", "%s\n%s" % (e, traceback.format_exc()[-800:]), True)
        return
    segs = _segments_from_result(res, offset, want_words,
                                 int(req.get("duration_ms") or 0))
    fields = []
    for item in res or []:
        if isinstance(item, dict):
            fields = sorted(item.keys())
            break
    emit({
        "type": "result",
        "payload": {
            "segments": segs,
            "backend": {
                "funasr_version": funasr_version(),
                "model_dir": _state["model_dir"],
                "timestamp_fields": fields,
            },
        },
    })


def cmd_download(req):
    """Explicit user-triggered download path — never reached from
    analyze/load. Prefers modelscope, falls back to huggingface_hub."""
    kind = req.get("kind") or "sensevoice"
    dest = req.get("dest") or ""
    model_id = VAD_ID if kind == "fsmn-vad" else MODEL_ID
    if not dest:
        err("download", "E_USAGE", "dest 必填")
        return
    try:
        try:
            from modelscope.hub.snapshot_download import snapshot_download
        except Exception:
            from modelscope import snapshot_download  # older API
        path = snapshot_download(model_id, local_dir=dest)
        emit({"type": "downloaded", "path": path, "model_id": model_id})
        return
    except Exception as e:
        log("modelscope download failed: %s" % e)
    try:
        from huggingface_hub import snapshot_download as hf_dl
        path = hf_dl(model_id, local_dir=dest)
        emit({"type": "downloaded", "path": path, "model_id": model_id})
        return
    except Exception as e:
        err("download", "DOWNLOAD_FAIL", str(e))


def main():
    # Any third-party logging (funasr/modelscope/urllib3) must not reach
    # stdout: the protocol channel carries JSON-lines only.
    logging.basicConfig(stream=sys.stderr, level=logging.WARNING,
                        force=True)
    for line in sys.stdin:
        line = line.strip()
        if not line:
            continue
        try:
            req = json.loads(line)
        except json.JSONDecodeError:
            err("", "E_PROTO", "bad json line")
            continue
        cmd = req.get("cmd")
        try:
            if cmd == "hello":
                cmd_hello(req)
            elif cmd == "load":
                cmd_load(req)
            elif cmd == "transcribe":
                cmd_transcribe(req)
            elif cmd == "download":
                cmd_download(req)
            elif cmd == "shutdown":
                emit({"type": "bye"})
                return
            else:
                err(str(cmd), "E_PROTO", "unknown cmd")
        except Exception as e:
            err(str(cmd), "E_WORKER", "%s\n%s" % (e, traceback.format_exc()[-800:]), True)


if __name__ == "__main__":
    main()
