---
name: bench-report
description: Turn raw k6 / bypass-demo outputs in bench/ into bench/REPORT.md tables. Use after any load test or demo run. Never invent, average-away, or extrapolate numbers.
---
Generate or update bench/REPORT.md from raw files in bench/:
1. Every number must be traceable to a raw output file — cite the source filename next to each table.
2. Tables: per-algorithm allowed/rejected counts and latency percentiles; the in-memory vs Redis 3-replica bypass comparison.
3. Add a "reading" section: 3–5 plain-language bullets on what the data shows. No marketing language.
4. If a needed run is missing, print the exact command to produce it instead of estimating.
