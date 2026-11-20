# 第二周第 6 天评测与发布复盘

## 执行摘要

- 生成时间：`2026-11-20T03:57:06Z`
- 评测集：`evals/week2-day6.jsonl`
- 每条样例重复次数：`3`
- 评测方式：确定性 Fake Provider 离线评测，不访问真实模型服务。
- `deepseek` 和 `kimi` 仅作为离线供应商身份，不代表真实供应商质量结论。

## 供应商结果

| provider | release | samples | successes | structured | parse failures | repairs | stability | P50 | P95 | total tokens | cost |
| --- | --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| deepseek | v1 | 24 | 12 | 12 | 6 | 3 | 1.00 | 0.26ms | 0.93ms | 450 | 0.000600 |
| kimi | v1 | 24 | 12 | 12 | 6 | 3 | 1.00 | 0.26ms | 0.53ms | 450 | 0.000600 |
| kimi | v2 | 24 | 9 | 9 | 9 | 3 | 1.00 | 0.10ms | 0.55ms | 405 | 0.000540 |

## 发布演示

- 主运行版本：`v1`，成功：`true`
- 影子运行版本：`v2`，成功：`false`，错误分类：`analysis_decode`
- 灰度选择：版本 `v2`，候选命中：`true`，原因：`tenant_allowlist`
- 发布门禁：`block`，原因：`success_rate_below_threshold`
- 回滚版本：`v3`，PreviousVersion：`v2`，RollbackFrom：`v1`
- 历史重放结果一致：`true`

历史版本顺序：`v1`, `v2`, `v3`

## 解释和限制

- 候选版本的失败由 Fake Provider 受控注入，用于验证发布阻断和回滚，不代表真实 DeepSeek 或 Kimi 的输出质量。
- 当前 `.env` 使用单供应商兼容配置，本日没有执行真实双供应商调用。
- 报告只保存指标、错误分类、版本关系和输出摘要哈希，不保存完整 Prompt、模型回答、API Key 或供应商原始错误。
- 历史重放使用确定性 Fake Provider；真实模型重新调用仍然属于非确定性比较，不能等同于确定性重放。
- 当前发布、灰度和回滚均为进程内演示，尚未接入数据库、HTTP 管理接口或自动化发布系统。
