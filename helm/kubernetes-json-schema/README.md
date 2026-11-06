# Vendored Kubernetes JSON schema

`v1.33.1/_definitions.json` is `_definitions.json` of
[yannh/kubernetes-json-schema](https://github.com/yannh/kubernetes-json-schema) at v1.33.1, with one
final newline added for the repository's end-of-file hook.

`helm/beekeeper/values.yaml` refers to this file instead of the `$k8s` alias, so generating
`values.schema.json` reads no network and a failed fetch cannot turn the pre-commit check red. The
file sits outside the chart and is not packaged with it.

To change the version, replace the file, update the `itemRef` paths in `values.yaml` and regenerate
`values.schema.json`. A new Kubernetes reference uses the same relative path, not `$k8s`.
