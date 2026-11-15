#!/usr/bin/env python3
"""Add the labels and tips of our two drivers to a prebuilt OpenList frontend.

The admin panel does not read a driver's `help` tag. It looks up
  drivers.<driver>.<field>          label
  drivers.<driver>.<field>-tips     tip, shown when the Go tag `help` is non-empty
  drivers.config.<driver>.alert     banner text
  drivers.drivers.<driver>          name in the driver dropdown
in the language pack, which is compiled into assets/entry-<hash>.js. Upstream
drivers add these keys to OpenList-Frontend; we only ship the prebuilt dist, so
this script writes them into the matching chunks. Languages without a patched
chunk fall back to English.

Usage: patch-frontend.py <dist-dir> <i18n-dir>
Exits non-zero if a chunk's layout is not what it expects, so a frontend
upgrade that changes the layout fails the build instead of shipping raw keys.
"""
import glob
import json
import os
import sys

# Signature of each patched language: the start of the Seafile block, which
# every OpenList release has and which differs per language.
SIGNATURES = {
    "en": "Seafile:{address:`Address`,password:`Password`",
    "zh-CN": "Seafile:{address:`地址`,password:`密码`",
}
FIELDS_ANCHOR = "Seafile:{address:`"
CONFIG_ANCHOR = 'config:{"115 Cloud":{'
LABELS_ANCHOR = 'drivers:{"115 Cloud":`'


def js_str(s):
    return "`" + s.replace("\\", "\\\\").replace("`", "\\`").replace("${", "\\${") + "`"


def js_obj(d):
    return "{" + ",".join(json.dumps(k, ensure_ascii=False) + ":" + js_str(v) for k, v in d.items()) + "}"


def js_nested(d):
    return ",".join(json.dumps(k, ensure_ascii=False) + ":" + js_obj(v) for k, v in d.items())


def patch_one(src, pack):
    marker = json.dumps(next(iter(pack["fields"])), ensure_ascii=False) + ":{"
    if marker in src:
        return src, False
    for anchor in (FIELDS_ANCHOR, CONFIG_ANCHOR, LABELS_ANCHOR):
        if src.count(anchor) != 1:
            raise SystemExit("anchor %r found %d times, expected 1" % (anchor, src.count(anchor)))
    labels = ",".join(json.dumps(k, ensure_ascii=False) + ":" + js_str(v) for k, v in pack["drivers"].items())
    src = src.replace(FIELDS_ANCHOR, js_nested(pack["fields"]) + "," + FIELDS_ANCHOR)
    src = src.replace(CONFIG_ANCHOR, "config:{" + js_nested(pack["config"]) + ',"115 Cloud":{')
    src = src.replace(LABELS_ANCHOR, "drivers:{" + labels + ',"115 Cloud":`')
    return src, True


def main():
    dist, i18n = sys.argv[1], sys.argv[2]
    done = set()
    for path in sorted(glob.glob(os.path.join(dist, "assets", "entry-*.js"))):
        if "entry-legacy-" in os.path.basename(path):
            continue
        with open(path, encoding="utf-8") as f:
            src = f.read()
        for lang, sig in SIGNATURES.items():
            if sig not in src:
                continue
            with open(os.path.join(i18n, lang + ".json"), encoding="utf-8") as f:
                pack = json.load(f)
            new, changed = patch_one(src, pack)
            if changed:
                with open(path, "w", encoding="utf-8") as f:
                    f.write(new)
            done.add(lang)
            print("%s: %s %s" % (os.path.basename(path), lang, "patched" if changed else "already patched"))
    missing = set(SIGNATURES) - done
    if missing:
        raise SystemExit("no chunk found for: " + ", ".join(sorted(missing)))


if __name__ == "__main__":
    main()
