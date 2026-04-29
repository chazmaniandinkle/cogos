<!--
Thanks for the contribution. Fill out the sections below. Delete sections
that don't apply to your PR.
-->

## What and why

<!-- One paragraph: what this PR does and why. Link the issue it fixes. -->

Fixes #

## How

<!-- Brief description of the approach. If the change is non-obvious, walk
through the load-bearing decisions. File:line references are encouraged. -->

## Acceptance

<!-- Copy the acceptance checklist from the linked issue. Tick items as they
become true. Don't open the PR for review until at least one item is ticked
or you've explained why none yet are. -->

- [ ]
- [ ]

## Test evidence

<!-- How did you verify this works? Paste command output, screenshots of
relevant UI, or links to CI runs. "Tests pass" alone is not evidence — quote
the new tests by name. -->

```
$ go test ./internal/engine/...
ok  ...
```

## Risk and rollback

<!-- What's the blast radius if this regresses? How would an operator detect
the regression and roll it back? "Low risk, revert the PR" is a valid answer
for small changes. Required for anything touching the kernel hot path,
persistence, or routing. -->

## Out of scope

<!-- What did you intentionally NOT change in this PR, even though it might
seem related? Prevents scope-creep arguments in review. -->

## Notes for reviewers

<!-- Anything you want a reviewer to know that doesn't fit above. Areas you
want extra eyes on. Decisions you want to defer. Optional. -->
