# Contributing

Use conventional commit subjects and a conventional PR title. The release
workflow derives versions from the merged commit subject using the generated
`cliff.toml` configuration.

Files under `plugin/skills/`, including the worker rules and topic skills, are
embedded in the binary. Changing them changes product behavior. Use
`fix(rules): ...` for a rules correction or `feat(skills): ...` for new behavior
in both the PR title and commit subjects, so squash and other merge methods
produce a releasing commit. Check a proposed subject and changed paths with:

```sh
bash .github/check-embedded-skills-release.sh 'fix(rules): clarify lease rules' plugin/skills/lease/SKILL.md
```

The check rejects non-releasing types when an embedded skill changes. Reviewers
must check the PR title and commit subjects before merging. `go test ./plugin`
checks that this convention stays compatible with the generated release sections.

Use `docs: ...` for documentation outside the embedded skills, such as README
or contributor documentation. Docs-only changes outside `plugin/skills/`
continue to produce no release. Do not change the generated release configuration
to release all docs commits; it is maintained by devctl.
