#!/usr/bin/env python3
"""Export the Claude Code session log(s) to a readable TRANSCRIPT.txt.

TASK.md asks for the complete agent transcript, and says a raw export is fine.
Claude Code already writes every session to ~/.claude/projects/<escaped-cwd>/*.jsonl,
so this renders that record rather than reconstructing anything after the fact --
the point is to show what actually happened, including the wrong turns.

Multiple session files are concatenated in timestamp order, as TASK.md requires.

What is and is not in the log: prompts, replies, every tool call with its
arguments, and every tool result. The model's internal reasoning is *not* -- those
blocks are persisted with an empty body and only a signature, so there is nothing
to export. The visible record is what the agent did and said, which is the part
that can be checked against the repo anyway.

Usage:
    tools/export_transcript.py                    # -> TRANSCRIPT.txt
    tools/export_transcript.py -o /tmp/t.txt      # somewhere else
    tools/export_transcript.py --max-result 2000  # truncate long tool output
"""
import argparse
import json
import pathlib
import sys

RULE = "=" * 78


def project_dir(cwd):
    """Claude Code escapes the working directory path to name its log folder."""
    return pathlib.Path.home() / ".claude" / "projects" / cwd.replace("/", "-")


def blocks(content):
    """message.content is either a plain string or a list of typed blocks."""
    if isinstance(content, str):
        return [{"type": "text", "text": content}]
    return content if isinstance(content, list) else []


def render_result(content, limit):
    if isinstance(content, list):
        parts = []
        for b in content:
            if isinstance(b, dict) and b.get("type") == "text":
                parts.append(b.get("text", ""))
            elif isinstance(b, dict):
                parts.append(f"[{b.get('type')}]")
            else:
                parts.append(str(b))
        text = "\n".join(parts)
    else:
        text = str(content)
    if limit and len(text) > limit:
        text = text[:limit] + f"\n... [{len(text) - limit} more chars truncated]"
    return text


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("-o", "--out", default="TRANSCRIPT.txt")
    ap.add_argument("-C", "--cwd", default=str(pathlib.Path.cwd()),
                    help="project directory the session ran in")
    ap.add_argument("--max-result", type=int, default=0,
                    help="truncate tool results to N chars (0 = complete, the default)")
    args = ap.parse_args()

    src = project_dir(args.cwd)
    logs = sorted(src.glob("*.jsonl"))
    if not logs:
        sys.exit(f"no session logs found in {src}")

    entries = []
    for log in logs:
        with log.open() as fh:
            for line in fh:
                line = line.strip()
                if not line:
                    continue
                try:
                    entries.append(json.loads(line))
                except json.JSONDecodeError:
                    continue  # a session still being written can end mid-line
    entries.sort(key=lambda e: e.get("timestamp") or "")

    out = []
    turns = 0
    for e in entries:
        if e.get("type") not in ("user", "assistant") or e.get("isMeta"):
            continue
        msg = e.get("message") or {}
        role = msg.get("role", e["type"])
        stamp = (e.get("timestamp") or "")[:19].replace("T", " ")

        rendered = []
        for b in blocks(msg.get("content")):
            if not isinstance(b, dict):
                continue
            kind = b.get("type")
            if kind == "text" and b.get("text", "").strip():
                rendered.append(b["text"].rstrip())
            elif kind == "thinking":
                continue  # persisted empty (signature only); nothing to render
            elif kind == "tool_use":
                params = json.dumps(b.get("input", {}), indent=2, ensure_ascii=False)
                rendered.append(f"[tool: {b.get('name')}]\n{params}")
            elif kind == "tool_result":
                body = render_result(b.get("content", ""), args.max_result)
                if body.strip():
                    rendered.append(f"[tool result]\n{body.rstrip()}")
        if not rendered:
            continue

        turns += 1
        label = "USER" if role == "user" else "ASSISTANT"
        if e.get("isSidechain"):
            label += " (subagent)"
        out.append(f"{RULE}\n{label}  {stamp}\n{RULE}\n")
        out.append("\n\n".join(rendered) + "\n\n")

    header = (
        f"Trading-desk task -- complete Claude Code transcript\n"
        f"Exported by tools/export_transcript.py from {len(logs)} session log(s).\n"
        f"{turns} messages. Tool calls and their results are included inline.\n\n"
    )
    pathlib.Path(args.out).write_text(header + "".join(out))
    print(f"wrote {args.out}: {turns} messages from {len(logs)} log(s)")


if __name__ == "__main__":
    main()
