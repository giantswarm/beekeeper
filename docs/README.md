# Documentation

- [Design](design.md): the approved design of the deterministic engine for the software development lifecycle: what runs where,
  the lifecycle, scheduling, builds and CI, delivery, failures, people, decision points, metrics, security and the phases.
- Phase 1, the foundation and a short dry run:
  - [Scenarios and test harness](phase1/scenarios.md): the phase 1 contract. What the dry run means, how scenarios run,
    and the format and vocabulary they assert on.
  - [Implementation plan](phase1/plan.md): where the code goes, the layout, Postgres, metrics, deployment, CI and the order of work.
  - [Scenario catalogue](../internal/testing/scenarios/CATALOGUE.md): every phase 1 scenario with its Given/When/Then,
    generated from the YAML files in [`internal/testing/scenarios/`](../internal/testing/scenarios/).
