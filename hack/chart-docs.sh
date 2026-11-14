#!/usr/bin/env bash
# Regenerates the chart's values.schema.json and README.md from values.yaml,
# exactly as the pre-commit hooks (helm-schema-beekeeper, helm-docs) check them.
# Run it after every change to helm/beekeeper/values.yaml.
set -euo pipefail

# The versions .pre-commit-config.yaml pins.
schema_version=v2.6.0
schemalint_version=v2.6.3
helm_docs_version=v1.14.2

root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$root"
tmp="$(mktemp)"
trap 'rm -f "$tmp"' EXIT

go run "github.com/losisin/helm-values-schema-json/v2@${schema_version}" --config helm/beekeeper/.schema.yaml -o "$tmp"
python3 -I -c 'import json,sys; h=lambda o: {**{k:v for k,v in o.items() if k!="additionalProperties"},"unevaluatedProperties":False} if ("$ref" in o and o.get("additionalProperties") is False) else o; p=sys.argv[1]; f=open(p,encoding="utf-8"); d=json.load(f,object_hook=h); f.close(); f=open(p,"w",encoding="utf-8"); json.dump(d,f); f.close()' "$tmp"
go run "github.com/giantswarm/schemalint/v2@${schemalint_version}" normalize "$tmp" -o "$tmp" --force
cp "$tmp" helm/beekeeper/values.schema.json

go run "github.com/norwoodj/helm-docs/cmd/helm-docs@${helm_docs_version}" \
	--chart-search-root=helm --chart-to-generate=helm/beekeeper --sort-values-order=file
