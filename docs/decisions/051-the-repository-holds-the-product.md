# 051: The repository holds the product, not its working papers

Status: accepted, 2026-11-23.

## Decision

With 0.5.0 released and the repository public, the repository holds what makes and explains the product: the code,
its tests, the packaging, the installer and the Ansible collection, the specification, the decision records, the
runbooks and the documentation site. The working papers of its development leave the tree:

- the milestone plans, `docs/plan-m1.md` to `docs/plan-m10.md`;
- `docs/open-questions.md`, whose questions are all decided;
- the research notes in `docs/research/`;
- the proof of concept, `poc/`, and the UI mockup, `mockup/`, which the product has replaced.

The git history keeps them: commit 637f069 still has them. (It was tagged v0.5.0, a development release withdrawn when
the numbering started again at 0.1.0.)

A plan is still written before code, but in the issue or the pull request it belongs to. When the work ships, its
answers are in `docs/spec.md`, and the ones that are decisions get a record here.

## Why

- Someone reading a public repository should find the product and its reasons, not drafts, and questions that were
  answered long ago.
- Every answer in the plans that still matters is in the spec or a decision record. Comments that cited a plan now say
  what they mean, or cite the decision.
- Nothing is lost: git keeps all of it.

## Consequences

- Records and comments still name the milestones, M1 to M10, where they say when something was built.
- The documentation, `website/index.html`, is published to GitHub Pages from `main`, at
  https://kabsdk.github.io/holdfast/, and its pictures of the web UI are taken by `make screenshots`.
- CLAUDE.md and CONTRIBUTING.md say where a plan lives now.
