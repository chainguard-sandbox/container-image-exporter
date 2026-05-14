# Working on container-image-exporter

This is a Prometheus exporter for Kubernetes container-image metadata. Two
components ship from one binary: a cluster-side Deployment (`exporter`) that
fetches OCI metadata from registries, and a per-node DaemonSet (`node-exporter`)
that reads CRI + `/proc/<pid>/root/etc/os-release` from running containers.

## Helm chart is the canonical install path

`deploy/chart/` is the only supported install path. There is no static-manifest
deploy story. When documenting or exposing a flag:

- Plumb it through `values.yaml`, `values.schema.json`, and the relevant
  template (rendered as a `--flag=…` arg).
- In the README, show the `--set` form only. Bare flag examples in user docs
  are deprecated — they belong in the CLI `--help`, not the README.
- Chart unit tests live under `deploy/chart/tests/` (helm-unittest). Cover new
  values with at least an enable-path and, where relevant, a schema-rejection
  case via `tests/values/*.yaml` fixtures and `failedTemplate.errorPattern`.
- `values.schema.json` is strict: `additionalProperties: false` at every level
  where we enumerate keys. User-extensible maps (`*Annotations`, `*Labels`)
  and k8s pass-through shapes (`resources`, `*Probe`, `securityContext`) stay
  open.

## Documentation

Per-component reference (metrics, supported resources, configuration, example
queries) lives in `docs/<component>.md`. The README is a teaser that links
out to those — don't add big tables or per-flag detail to the README itself.

## Branches

Branch names follow the commit-type prefix: `feat/`, `fix/`, `chore/`,
`test/`, `docs/`, `ci/`, `chart/`. Prefer one focused change per branch over
piling unrelated fixes together — it makes review and revert easier.

## Commits

- Concise subject line, conventional-style prefix (`feat:`, `fix:`, `chore:`,
  `test:`, `docs:`, `ci:`, `chart:`).
- Body explains the **why**, not the **what**. Reference incidents, prior
  decisions, or the constraint the change exists to satisfy.
- One coherent change per commit. If you find yourself drafting "and also…",
  it probably belongs in a separate commit on a separate branch.

## Tests

- `make test` — Go tests including envtest.
- `make test-node` — node-exporter integration on a k3d node.
- `make test-helm` — chart lint + helm-unittest + integration on real k3d.
- Run the layers the change actually touches: any Go change runs `make test`
  *and* `make test-node`; chart changes run `make test-helm`; doc-only
  changes skip all three.
- Prefer real over mocked. Integration tests stand up a k3d cluster and scrape
  Prometheus; that's the source of truth for "does the chart work".
- When a metric or label is invariant-bound (e.g. `image_id` must match across
  `node_container_info` and `node_image_*`), the invariant gets its own unit
  test, not just integration coverage.

## CRI / OCI gotchas

- `Container.image_ref` (CRI) and `Image.Id` (CRI) are different fields with
  different formats and different lifetimes. Containerd happens to expose the
  config digest in both, but that is not contractual. Source both sides of
  any cross-metric join from `ImageStatus.Image.Id` — never `image_ref`.
- `Image.Id` is the **config blob digest**, not the manifest digest. `crane
  digest <ref>` returns the manifest digest; `crane config <ref> | sha256sum`
  returns the config digest. Verify empirically (k3d + `docker image inspect
  --format='{{.Id}}'`) before relying on it.
- `k3d image import` uses `docker save` tarballs; manifest and config digests
  align by coincidence for those images. Don't generalise from k3d behaviour
  to upstream registries.

## Prometheus conventions

- Think about cardinality before adding a label. Builder-controlled values
  (SBOMs, build IDs, multi-line descriptions) go behind an allowlist or get
  truncated.
- Prefer standard `promhttp`-style HTTP instrumentation (counter +
  histogram, labelled by `code`/`method`/`host`) over bespoke error-class
  counters.
- Build on upstream defaults rather than reinventing. Search the Prometheus /
  k8s / ggcr ecosystem for the conventional building block before writing one.
  The registry transport uses `remote.DefaultTransport` from
  go-containerregistry as its base; build_info comes from
  `prometheus/client_golang/prometheus/collectors/version.NewCollector` and
  `--version` from `prometheus/common/version.Print` so the labels, metric
  name, and output format match every other Prometheus exporter.
- Metrics that feed adoption-percentage queries are all-or-nothing. Don't
  ship partial results from a failed scrape — set `up=0` and emit nothing
  rather than half a series set, because consumers compute ratios across
  metrics and a partial denominator silently breaks the ratio.

## Controller-runtime

The exporter is a controller-runtime manager. New watches register as
reconcilers on the manager — don't reach for raw informers or
client-go-style work queues. Use `cache.ByObject` + `predicate.NewPredicateFuncs`
when narrowing what gets cached.

## Grafana dashboards

Dashboards in `deploy/chart/dashboards/` must be in classic v1 JSON format.
The kube-prometheus-stack Grafana sidecar uses the legacy `/api/dashboards/db`
endpoint, which rejects v2alpha schemas with a misleading "use the
`/apis/dashboard.grafana.app/v2` API" error and silently drops the dashboard.

## Things to push back on

- **Premature abstraction.** Three similar lines is better than a helper
  designed for a hypothetical fourth caller. If a metric isn't being consumed
  by a dashboard or query today, remove it rather than redesigning it for
  hypothetical use.
- **Backwards-compat shims.** This project pre-1.0; rename, restructure, and
  delete freely. Don't leave `// removed` comments or re-exports.
- **Comments that narrate code.** Default is one short line describing what a
  type/function is for, no more. Long paragraphs explaining ordering rationale,
  argument semantics, or upstream behaviour belong in the code, not above it.
  Preserve pre-existing comments rather than sweeping them out when you touch
  nearby lines.

## Workflow defaults

- For routine local work (edits, tests, lint, branch creation) — just do it.
- For anything that touches shared state — pushing branches, opening PRs,
  force-pushing, amending public commits — confirm first.
- Don't `git push` unless explicitly told to. Stage the branch locally, show
  the diff, and wait. The user pushes when they're ready.
