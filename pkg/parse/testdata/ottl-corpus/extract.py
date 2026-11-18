#!/usr/bin/env python3
"""Pulls OTTL statements and conditions out of the contrib READMEs fetched
next to this script (v0.165.0), one JSON object per statement:
{"source": file, "kind": statement|condition, "text": ...}."""
import json, re, sys, glob, os
import yaml

HERE = os.path.dirname(os.path.abspath(__file__))
LIST_KEYS = {
    "statements", "conditions", "trace_statements", "metric_statements", "log_statements",
    "span", "spanevent", "metric", "datapoint", "log", "logrecord", "resource", "scope",
    "profile", "error_mode_conditions",
}
SCALAR_KEYS = {"condition", "statement"}
SKIP_LIST_PARENTS = {"include", "exclude", "match_type"}

FUNC = re.compile(r"^[A-Za-z_][A-Za-z0-9_]*\s*\(")
COND = re.compile(r"(==|!=|<=|>=|<|>|\bwhere\b|\bnot\b|\band\b|\bor\b|IsMatch\(|\btrue\b|\bfalse\b)")


KEYWORDS = {"not", "and", "or", "where", "true", "false", "nil"}


def kind(text):
    """An editor call (lowercase name), alone or with a where clause, is a
    statement; anything else, a bare converter or a math expression
    included, is parsed as a condition."""
    t = text.strip()
    m = re.match(r"^([a-z_][A-Za-z0-9_]*)\s*\(", t)
    if not m or m.group(1) in KEYWORDS:
        return "condition"
    depth, quoted, i = 0, False, m.end() - 1
    while i < len(t):
        c = t[i]
        if quoted:
            if c == "\\":
                i += 1
            elif c == '"':
                quoted = False
        elif c == '"':
            quoted = True
        elif c == "(":
            depth += 1
        elif c == ")":
            depth -= 1
            if depth == 0:
                rest = t[i + 1:].strip()
                return "statement" if rest == "" or rest.startswith("where ") else "condition"
        i += 1
    return "statement"


# Examples in the docs that no Collector accepts (typos and non-OTTL text).
# Kept in the corpus, marked, so a fix upstream shows up as a diff here.
INVALID = {
    'metric.type == 1 AND resource.attributes["resource.foo"] != nil': "uppercase AND (the keywords are lowercase)",
    'FormatTime(TruncateTime(spanevent.time, Duration("10h 20m"))), "%Y-%m-%d %H:%M:%S")': "unbalanced parentheses",
    'IsBool(pcommon.NewValueBool(false))': "Go code, not OTTL",
    'Int(Log(span.attributes["duration_ms"])': "unbalanced parentheses",
    'set(resource.attributes["element_1"], resource.attributes["thingsMap"]["foo\'])': "mismatched quotes",
    'set(resource.attributes["element_2"], resource.attributes["thingsMap"]["bar\'])': "mismatched quotes",
    'sum (otelcol_processor_tail_sampling_count_traces_sampled) by (policy)': "PromQL, not OTTL",
    'copy_metric(name="http.request.status_code", unit="s") where metric.name == "http.status_code': "unterminated string",
}


def looks_ottl(t):
    t = t.strip()
    if not t or "\n" in t.strip() and not FUNC.match(t):
        return False
    return bool(FUNC.match(t) or COND.search(t))


def walk(node, parent, out):
    if isinstance(node, dict):
        for k, v in node.items():
            k = str(k)
            if k in SCALAR_KEYS and isinstance(v, str):
                out.append(v)
            elif k in LIST_KEYS and isinstance(v, list):
                for item in v:
                    if isinstance(item, str):
                        out.append(item)
                    else:
                        walk(item, k, out)
            else:
                walk(v, k, out)
    elif isinstance(node, list):
        for item in node:
            walk(item, parent, out)


def fenced(md):
    for m in re.finditer(r"```(\w*)\n(.*?)```", md, re.S):
        yield m.group(1).lower(), m.group(2)


def main():
    seen = set()
    for path in sorted(glob.glob(os.path.join(HERE, "*.md"))):
        src = os.path.basename(path)
        md = open(path).read()
        texts = []
        for lang, body in fenced(md):
            if lang in ("yaml", "yml", ""):
                try:
                    for doc in yaml.safe_load_all(body):
                        walk(doc, None, texts)
                except yaml.YAMLError:
                    pass
            if lang in ("", "ottl", "text"):
                for line in body.splitlines():
                    line = line.strip()
                    if line and FUNC.match(line) and line.endswith(")") and ":" not in line.split("(")[0]:
                        texts.append(line)
        # Inline examples in the function reference: "- `delete_key(attributes, "x")`"
        for m in re.finditer(r"^\s*-\s+`([^`]+)`\s*$", md, re.M):
            t = m.group(1)
            if FUNC.match(t) or " where " in t:
                texts.append(t)
        for t in texts:
            t = " ".join(t.split())
            if not looks_ottl(t) or (src, t) in seen or " -> " in t or "..." in t or "<" in t and ">" in t and "<=" not in t and ">=" not in t:
                continue
            seen.add((src, t))
            e = {"source": src, "kind": kind(t), "text": t}
            if t in INVALID:
                e["invalid"] = INVALID[t]
            print(json.dumps(e))


if __name__ == "__main__":
    main()
