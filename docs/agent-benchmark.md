# Agent benchmark

FAM ships a project guide through `fam new` and `fam migrate`. The benchmark measures whether agents can use that guide and the CLI to change a fleet correctly.

The selected guide is `agentbench/guides/g6-lean.md`. Its content is also the embedded template in `internal/project/guide.md`. The guide has 2,018 bytes after version substitution and whitespace removal, approximately 504 tokens under the benchmark's bytes/4 estimate. This estimate is not a tokenizer measurement. FAM adds two section markers when it writes `AGENTS.md`.

## Selection

The objective has three priorities: correct results, less guide context, then less context across the whole agent run. A shorter guide can increase total context when an agent needs more searches or repair attempts.

Five rounds produced 1,632 attempts. The first rounds explored guide lengths and exposed grader problems. Later rounds added harder tasks and removed example values that matched task answers. These rounds are development measurements, not an independent held-out evaluation.

Round 3 compared four guides across 17 tasks, three agents, and two repetitions:

| Guide | Approximate guide tokens | Passed / scored runs |
|---|---:|---:|
| `g3b-dense` | 365 | 99 / 102 |
| `g4b-lean` | 503 | 102 / 102 |
| `g4-long` | 704 | 102 / 102 |
| `g5-xl` | 1,043 | 102 / 102 |

Round 4 compared the two smaller finalists. `g4c-min` passed 100/102 runs. `g4b-lean` passed 102/102 runs. The lean form therefore passed 204/204 runs across these two complete rounds. Some examples still shared values with task answers. One minimal-guide failure copied its example package instead of the requested package. Another run acknowledged the guide but made no edits.

Round 5 used neutral examples in `g6-min` and `g6-lean`. It also restored the no-guide baseline. API rate limits interrupted 26 cells, and one cell had no record when the process stopped. A resume completed that missing Sol cell. Claude then rejected all 26 retries because its OAuth session expired. Both attempts for each affected cell remain in the records.

| Agent | No guide | Minimal guide | Lean guide |
|---|---:|---:|---:|
| Haiku | 22/30, 4 errors | 30/30, 4 errors | 29/29, 5 errors |
| Sol | 34/34 | 34/34 | 34/34 |
| Sonnet | 25/30, 4 errors | 30/30, 4 errors | 29/29, 5 errors |

Errors are unavailable measurements. They do not count as task failures or successful tasks. The neutral minimal guide has 1,735 bytes, approximately 433 estimated tokens. Its completed results are promising, but its missing Claude cases prevent a complete comparison.

For a fair context comparison, the following table uses only task/repetition cells that completed under all three guide conditions for each agent. Context includes input, cached input, and output across every turn.

| Agent | Matched cells | No guide tokens/run | Minimal tokens/run | Lean tokens/run | Lean reduction |
|---|---:|---:|---:|---:|---:|
| Haiku | 29 | 389,727 | 210,071 | 214,112 | 45% |
| Sol | 34 | 133,544 | 70,685 | 71,537 | 46% |
| Sonnet | 29 | 287,407 | 113,779 | 118,966 | 59% |

On these matched cells, no guide passed 79/92 tasks. Each neutral guide passed 92/92. The release uses the lean form because it also has repeated complete coverage in the earlier rounds. This is the smallest guide form with that evidence. The results do not establish a global optimum or rule out the smaller neutral form. Two repetitions give limited evidence about rare failures.

## Tasks and scoring

The 17 tasks cover global bundles, role bundles, machine bundles, machine exceptions, removal, new machines, local files, secrets, broken Pkl, editor state, discovery, fleet instructions, agent settings, environment variables, local skills, GitHub skills, and system packages.

Each attempt starts with a fresh two-machine project and one task prompt. It receives one guide or no guide. A run passes only when all applicable checks pass. The graders inspect evaluated Pkl data, file changes, the final answer, project validity, and attempts to apply changes. They do not ask another model to assign a score. Some checks require specific answer text or standard bundle calls, so they can reject other valid approaches. The full prompts and graders live in `agentbench/tasks.go`.

The benchmark uses `gpt-6.1-sol` at low reasoning effort, `claude-haiku-5-5`, and `claude-sonnet-5-5`. These are the requested model identifiers. The harness does not independently identify the service's backend model. Claude and Codex use their own tools and permission systems. Compare guide variants within each agent before comparing agents.

FAM uses a separate HOME and seeded Pkl binary cache. The agent process retains its normal authentication and global configuration. These settings can affect context counts. A wrapper logs FAM commands and blocks `apply` and `update`. An SSH configuration directs hosts to an unused loopback port. These controls protect the benchmark workflow. They are not a security sandbox for untrusted agents or prompts.

The input column includes cached tokens. The report lists output separately. Claude cost comes from its CLI. Codex does not return a dollar cost here, so its zero cost field means unavailable. Provider-error attempts do not enter scored-run context, time, or cost averages.

## Run it

Install and authenticate the agent CLIs before a run. Run commands from the repository root. The benchmark makes model requests and can incur charges.

```sh
make build
go build -o /tmp/fam-agentbench ./agentbench
/tmp/fam-agentbench run -fam ./fam -agents haiku,sonnet,sol -guides none,g6-min,g6-lean -repeat 2 -jobs 4 -out /tmp/fam-benchmark
```

Retry provider errors and missing cells with the same matrix, binary, guides, and fixture:

```sh
/tmp/fam-agentbench run -fam ./fam -agents haiku,sonnet,sol -guides none,g6-min,g6-lean -repeat 2 -jobs 4 -out /tmp/fam-benchmark -retry-errors
/tmp/fam-agentbench report /tmp/fam-benchmark/results.jsonl
```

Resume keeps every original record and artifact. It does not retry scored failures. Each retry gets a fresh project directory. Do not run two writers against the same output directory. Reports select the latest attempt for each agent/guide/task/repetition within each file. Separate files represent separate rounds.

An attempt directory contains the project, agent transcript, command log, and diff. `results.jsonl` contains the answer, checks, usage, and artifact path. To repeat the file and answer checks for an earlier attempt:

```sh
/tmp/fam-agentbench grade -fam ./fam -out /tmp/fam-benchmark package /tmp/fam-benchmark/runs/<attempt>
```

The command uses the recorded answer and command context. It does not repeat the agent run.

## Recorded results

`agentbench/results/round-1.jsonl` through `round-5.jsonl` retain per-attempt checks, usage, command logs, and errors. They omit answers and machine-local artifact paths. Consequently, use the original output directories for `grade`.

`agentbench/results/manifest.json` records versions, requested models, guide hashes, and provenance limits. Earlier `embedded` results refer to `original-guide.md` in that folder. They do not refer to the current template. The first two rounds also used earlier graders, so do not pool their scores with the final comparison.

```sh
go run ./agentbench report agentbench/results/round-5.jsonl
```

Before a later guide reduction, restore Claude authentication and resume the missing neutral cases. Then use new tasks that do not reuse the guide examples. Keep a separate evaluation set before further tuning.
