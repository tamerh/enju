# Figures

TikZ sources for the diagrams used in the project README and docs.

- `workflow.tex` — the run-overview hero: a representative (hypothetical,
  domain-neutral) enju run — `discover` fans out with `for_each`, each
  item is processed and checked, results fan back in via `collects`, a
  human `review` gates the deliverable, and `publish` lays it onto the
  base branch. Executors are colour-coded (agent / script / human) and
  the ribbon shows every accepted task landing as a git commit on the
  run branch.

The style palette (node shapes, colours) is shared with the preprint
(`preprint/main.tex`) so the README and the paper read as one system.

## Regenerate

```sh
make            # -> workflow.svg + workflow.png
make clean      # remove LaTeX intermediates
```

Requires `pdflatex`, `dvisvgm`, and `pdftoppm` (texlive + poppler-utils).
The committed `.svg` / `.png` are the rendered outputs so GitHub shows
them without a build step; re-run `make` after editing a `.tex`.
