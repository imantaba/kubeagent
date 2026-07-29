# kubeagent Governance

This document describes how kubeagent is governed. It is deliberately honest
about the project's current size rather than describing a structure that does
not exist yet: kubeagent has **one maintainer today**, and this document sets
out both how decisions are made now and the automatic path to shared
governance as the maintainer group grows.

## Roles

**Contributor** — anyone who opens an issue, comments on one, or sends a pull
request. No prior approval or affiliation is required. See
[CONTRIBUTING.md](CONTRIBUTING.md).

**Maintainer** — a contributor with write access to the repository who reviews
and merges pull requests, triages issues, cuts releases, and is accountable for
the project's technical direction and for enforcing the
[Code of Conduct](CODE_OF_CONDUCT.md). Maintainers are listed in
[MAINTAINERS.md](MAINTAINERS.md).

## Decision making

Decisions are made in public, on GitHub issues and pull requests. Discussion
that happens elsewhere is summarized back onto the relevant issue so that the
record is complete.

**While there are fewer than three maintainers**, the maintainer group decides
by consensus among themselves; where there is a single maintainer, that
maintainer decides. Any user may object to a decision on the issue or pull
request where it was made, and a maintainer must respond to the objection in
writing before merging.

**Once there are three or more maintainers**, this switches automatically —
no separate vote is needed — to the following model:

- **Lazy consensus.** Most changes merge when a maintainer approves them and no
  other maintainer objects. Silence is agreement.
- **Objections block.** A maintainer's "request changes" on a pull request
  blocks the merge until it is withdrawn or overridden by a vote.
- **Votes.** Where consensus cannot be reached, a maintainer may call a vote on
  the issue or pull request. A vote passes on a simple majority of maintainers,
  stays open for at least 72 hours, and is recorded in the thread. Each
  maintainer has one vote.
- **Changes to this document, to the project's scope, and to the
  read-only-by-default invariant** require a two-thirds majority of
  maintainers.

## Becoming a maintainer

There is no quota and no fixed waiting period. A contributor may be nominated
as a maintainer — by an existing maintainer or by nominating themselves in a
public issue — after demonstrating:

- **Sustained, substantial contribution** over at least three months: merged
  pull requests that are not solely documentation or typo fixes.
- **Review quality** — useful, technically substantive review of others'
  changes.
- **Judgment about the project's invariants** — in particular that kubeagent is
  read-only toward the cluster by default, that its diagnostic core works with
  no API key, and that no LLM call is ever on a write path.
- **Adherence to the [Code of Conduct](CODE_OF_CONDUCT.md).**

Nominations are decided by the existing maintainers under the decision-making
rules above. An accepted maintainer is added to
[MAINTAINERS.md](MAINTAINERS.md) in a pull request.

## Stepping down and removal

A maintainer may step down at any time by opening a pull request moving
themselves to the emeritus list in [MAINTAINERS.md](MAINTAINERS.md).

A maintainer who is unreachable and has not contributed or reviewed for **six
months** may be moved to emeritus by the remaining maintainers, after a
good-faith attempt to contact them. This is not a judgment on the person; it
keeps the maintainer list accurate. Emeritus maintainers can be reinstated by
asking, without repeating the nomination process.

A maintainer may be removed for a serious or repeated Code of Conduct
violation, following the enforcement process in
[CODE_OF_CONDUCT.md](CODE_OF_CONDUCT.md).

## Code review

Every change reaches `main` through a pull request. A pull request needs one
maintainer approval and a green CI run (`go vet`, `go test`, `go build`, DCO)
to merge. A maintainer's own change still requires review by another maintainer
where one exists; where there is only one maintainer, that maintainer may merge
their own change, and the resulting commits remain open to post-merge review by
anyone.

Every contribution must carry a
[Developer Certificate of Origin](https://developercertificate.org/) sign-off;
see [CONTRIBUTING.md](CONTRIBUTING.md).

## Releases

Any maintainer may cut a release. kubeagent follows
[Semantic Versioning](https://semver.org/spec/v2.0.0.html), and every user-
visible change is recorded in [CHANGELOG.md](CHANGELOG.md) before the release
is tagged. The release process is documented in the
[README](README.md#cutting-a-release).

## Security

Vulnerability reports are handled privately by the maintainers under
[SECURITY.md](SECURITY.md), and are not discussed in public issues until a fix
is available.

## Changing this document

Changes to this document are proposed as a pull request and decided under the
rules in [Decision making](#decision-making) above.
