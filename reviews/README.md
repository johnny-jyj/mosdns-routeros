# Code Review Archive

本目录保存代码审查、修复说明和验证结果。每个归档目录同时包含：

- 被审查的 inclusive commit range；
- 完成全部已知修复后的代码 HEAD，或提交前可复核的 working-tree diff hash；
- 机器可读的 `metadata.yaml`；
- 自包含的 `review.md`。

## Reviews

- [`f946944…83e43c7`，修复 HEAD `3288348`](./f946944de231cc27ad027f0d80221ab73b30fc4d_to_83e43c7b0a5bf6e32fd204e8f2576a692c98e5c7/head-3288348df5a4918eaf6f4ec2ab431759f975fc56/review.md) — 7 项问题全部修复；race/vet、50 次确定性交错、零分配 FastHit benchmark，以及 wait true/false RouterOS 实机 E2E 均通过。
- [`f946944…4dfa312`，初始修复 HEAD `efedd59`，当前验证 HEAD `084e096`](./f946944de231cc27ad027f0d80221ab73b30fc4d_to_4dfa312f7e20759baa65a711e51c4077e3467726/head-efedd59a7992d7af4a3f151ff83cf374571367f9/review.md) — 已关闭，15 个有效问题全部修复，本地依赖补丁已限定为原生 TCP Async 必需范围，race/vet/benchmark/RouterOS 实机 E2E 通过。
