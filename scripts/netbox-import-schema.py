#!/usr/bin/env python3
"""netbox-import-schema.py <netbox checkout> <version>: NetBox's bulk-import forms as JSON.

Reads (with ast, never importing NetBox) the CSV import forms of dcim, ipam and extras and the choice sets
they use, and prints, per form, the columns it takes (Meta.fields with the parents' declared
fields), each declared field's required flag, its choice set and, for a reference to another
object, the model and the attribute (to_field_name) a value names. internal/day0's NetBox
conformance test checks the day-0 inputs' CSVs against what this prints for the release
internal/day0.NetBoxVersion names (testdata/netbox-import-<version>.json).
"""
import ast
import glob
import json
import subprocess
import sys

root, version = sys.argv[1], sys.argv[2]
FORMS = ["netbox/dcim/forms/bulk_import.py", "netbox/ipam/forms/bulk_import.py", "netbox/extras/forms/bulk_import.py",
         "netbox/dcim/forms/mixins.py",
         "netbox/netbox/forms/base.py"]
CHOICES = ["netbox/dcim/choices.py", "netbox/ipam/choices.py", "netbox/extras/choices.py", "netbox/netbox/choices.py"]


def parse(path):
    with open(f"{root}/{path}") as f:
        return ast.parse(f.read(), path)


def name_of(node):
    if isinstance(node, ast.Name):
        return node.id
    if isinstance(node, ast.Attribute):
        return node.attr
    return None


classes = {}
for path in FORMS:
    for node in parse(path).body:
        if isinstance(node, ast.ClassDef):
            classes[node.name] = node


def fields_expr(e, cls):
    """Meta.fields: a tuple, or a parent's Meta.fields plus a tuple."""
    if isinstance(e, (ast.Tuple, ast.List)):
        return [x.value for x in e.elts if isinstance(x, ast.Constant)]
    if isinstance(e, ast.BinOp):
        return fields_expr(e.left, cls) + fields_expr(e.right, cls)
    if isinstance(e, ast.Attribute) and e.attr == "fields" and isinstance(e.value, ast.Attribute):
        parent = name_of(e.value.value)
        return form(parent)["fields"] if parent in classes else []
    return []


def declared(cls):
    out = {}
    for st in cls.body:
        if not (isinstance(st, ast.Assign) and isinstance(st.value, ast.Call)):
            continue
        target = name_of(st.targets[0])
        kind = name_of(st.value.func) or ""
        if not kind.endswith("Field"):
            continue
        f = {"kind": kind, "required": True}
        for kw in st.value.keywords:
            if kw.arg == "required" and isinstance(kw.value, ast.Constant):
                f["required"] = bool(kw.value.value)
            elif kw.arg == "choices":
                f["choices"] = name_of(kw.value)
            elif kw.arg == "to_field_name" and isinstance(kw.value, ast.Constant):
                f["to"] = kw.value.value
            elif kw.arg == "queryset":
                # Model.objects.all() / .filter(...)
                n = kw.value
                while isinstance(n, (ast.Call, ast.Attribute)):
                    n = n.func if isinstance(n, ast.Call) else n.value
                f["model"] = name_of(n)
        out[target] = f
    return out


cache = {}


def form(name):
    if name in cache:
        return cache[name]
    cls = classes[name]
    fields, decl = None, {}
    for b in cls.bases:
        bn = name_of(b)
        if bn in classes:
            parent = form(bn)
            decl = {**parent["declared"], **decl}
            if fields is None and parent["fields"]:
                fields = list(parent["fields"])
    decl.update(declared(cls))
    for st in cls.body:
        if isinstance(st, ast.ClassDef) and st.name == "Meta":
            for m in st.body:
                if isinstance(m, ast.Assign) and name_of(m.targets[0]) == "fields":
                    fields = fields_expr(m.value, cls)
    cache[name] = {"fields": fields or [], "declared": decl}
    return cache[name]


# the models' fields: a ModelForm makes a model field required unless the model says blank=True
# (Django's formfield()), whatever its default; a BooleanField is never required in a form
models = {}
for path in sorted(glob.glob(f"{root}/netbox/*/models/*.py")):
    rel = path[len(root) + 1:]
    for node in parse(rel).body:
        if isinstance(node, ast.ClassDef):
            models[node.name] = node


def model_fields(name, seen=()):
    cls = models.get(name)
    if cls is None or name in seen:
        return {}
    out = {}
    for b in cls.bases:
        out.update(model_fields(name_of(b), seen + (name,)))
    for st in cls.body:
        if not (isinstance(st, ast.Assign) and isinstance(st.value, ast.Call) and isinstance(st.targets[0], ast.Name)):
            continue
        kind = name_of(st.value.func) or ""
        if not kind.endswith("Field") and not kind.endswith("ForeignKey") and kind not in ("GenericForeignKey",):
            continue
        blank = any(kw.arg == "blank" and isinstance(kw.value, ast.Constant) and kw.value.value is True for kw in st.value.keywords)
        out[st.targets[0].id] = {"kind": kind, "required": not blank and kind != "BooleanField"}
    return out


def meta_model(name):
    cls = classes.get(name)
    if cls is None:
        return None
    for st in cls.body:
        if isinstance(st, ast.ClassDef) and st.name == "Meta":
            for m in st.body:
                if isinstance(m, ast.Assign) and name_of(m.targets[0]) == "model":
                    return name_of(m.value)
    for b in cls.bases:
        if name_of(b) in classes:
            return meta_model(name_of(b))
    return None


forms = {n: form(n) for n in classes if n.endswith("ImportForm")}
for n, f in forms.items():
    m = meta_model(n)
    mf = model_fields(m) if m else {}
    f["model"] = m
    # a field the form takes but does not declare is the model's form field: required as the model says
    f["modelRequired"] = sorted(k for k in f["fields"] if k not in f["declared"] and mf.get(k, {}).get("required"))

choices = {}
for path in CHOICES:
    for node in parse(path).body:
        if not isinstance(node, ast.ClassDef):
            continue
        consts, values = {}, []
        for st in node.body:
            if isinstance(st, ast.Assign) and isinstance(st.targets[0], ast.Name):
                t = st.targets[0].id
                if t.isupper() and isinstance(st.value, ast.Constant) and isinstance(st.value.value, str):
                    consts[t] = st.value.value
        # the values are what CHOICES lists (nested by group), constants or literals
        for st in node.body:
            if isinstance(st, ast.Assign) and name_of(st.targets[0]) == "CHOICES":
                for n in ast.walk(st.value):
                    if isinstance(n, ast.Tuple) and n.elts:
                        first = n.elts[0]
                        if isinstance(first, ast.Name) and first.id in consts:
                            values.append(consts[first.id])
                        elif isinstance(first, ast.Constant) and isinstance(first.value, str) and \
                                not (len(n.elts) == 2 and isinstance(n.elts[1], (ast.List, ast.Tuple))):
                            values.append(first.value)
        if values:
            choices[node.name] = sorted(set(values))

commit = subprocess.run(["git", "-C", root, "rev-parse", "HEAD"], capture_output=True, text=True).stdout.strip()
json.dump({"version": version, "commit": commit, "source": "https://github.com/netbox-community/netbox",
           "forms": forms, "choices": choices}, sys.stdout, indent=1, sort_keys=True)
print()
