# cache-boundary 设计证据目录

本目录只包含设计与证据，不包含缓存实现，生产代码（`internal/`、`cmd/`）零改动。

- `DESIGN.md`：缓存边界方案、粒度对比、失效规则与可解释性设计。
- `probe/`：可重复实验探针（独立 `package main`，复用生产导出 API 与真实 `render.Render`）。
- `results/`：最近一次各实验的完整输出。

```bash
go run ./design/cache-boundary/probe -exp all      # 全部实验，退出码 0 即通过
go run ./design/cache-boundary/probe -exp exp1     # 跨块接点证否
go run ./design/cache-boundary/probe -exp exp2     # 远处更大峰值证否
```
