# 协议转换检查记录

检查日期：2026-10-24。范围：本仓库 OpenAI Chat / Responses、Anthropic、Gemini 转换器、统一流事件和 relay；联网检索 GitHub 上 AxonHub、LiteLLM、CLIProxyAPI、New API 的相关问题与改动。

GitHub 的 issue / PR 用于发现检查方向；下文只有本地回归测试复现的问题列为已确认 bug。未调用真实供应商 API，不能据此保证所有模型、第三方网关的线上兼容性。

## 已复现并修复

| 问题 | 触发与影响 | 修复 |
| --- | --- | --- |
| 同批流事件的文本被覆盖 | Gemini 一个 SSE 帧包含多个 text part 时，转 Chat 只剩最后一段；同一函数中的 thinking、refusal 和签名片段也有覆盖问题 | 按候选回答顺序追加片段，保留原有结构化思考块 |
| Gemini 并行工具调用被拆到错误回答 | 将 `ToolCall.Index` 当作候选回答索引，第二个工具调用被放进虚构的 `choices[1]`；非零候选索引同样错误 | 事件使用 candidate index，工具自身保留独立 index；验证转成 Chat SSE 后两个工具仍属于同一回答 |
| 混合思考内容往返丢失 | `ReasoningItems` 与 `redacted_thinking` 同时存在时，前者的转换分支跳过后者；旧格式标量思考与 redacted 同时存在时则可能丢失标量思考 | 分别转换普通思考和不透明 redacted 数据，显式 items 只覆盖普通思考投影，避免重复 |
| 拒绝文本跨帧聚合丢失 | 多段 refusal 在最终内部响应中只留下最后一段，导致日志与客户端已收到的内容不一致 | 聚合器追加 refusal |

新增回归覆盖：统一流事件的多片段重建、拒绝文本跨帧聚合、混合思考的往返和旧字段兼容、Gemini 到 Chat 的并行工具调用及多段文本。

限制：当前依赖 AxonHub 的统一模型只提供一个 `RedactedReasoningContent` 字段。本次修复保证混合内容保留首个非空 redacted 块；多个 redacted 块的数量和原始交错顺序仍需扩展桥接模型才能完整表达。

## GitHub 对照与补充建议

| 参考 | 本仓库情况与建议 | 优先级 |
| --- | --- | --- |
| [LiteLLM #47531](https://github.com/BerriAI/litellm/issues/47531)、[相关 PR #47539](https://github.com/BerriAI/litellm/pull/47539)：Claude Code 动态工具块被静默丢弃 | 本轮已补充新增、移除、引用重新启用，以及 Chat / Responses / Gemini 的出站集成回归，详见下文 | 已处理 |
| [AxonHub #2750](https://github.com/looplj/axonhub/pull/2750)：Claude Code WebSearch 经 Responses 后端转换 | 本轮已补充强制选择、同名函数、来源延迟合并、多个搜索的来源归属和历史重放。使用本地适配，没有升级依赖或直接合并该未合并 PR | 已处理主要场景 |
| [CLIProxyAPI #6258](https://github.com/router-for-me/CLIProxyAPI/issues/6258)：Gemini 分帧用量与截断结束状态 | 本仓库已有 Gemini usage metadata、Responses terminal 及 stream error 测试。建议补充一条完整 relay 回归：finishReason 帧之后才收到 usageMetadata，确认最终 Responses 事件和计费日志都保留用量 | 中 |
| [AxonHub #2117](https://github.com/looplj/axonhub/pull/2117)：工具结果中的图片丢失 | 已检查锁定版本源码，其 Responses 工具结果转换包含图片处理，并默认补 `detail=auto`。不需要重复实现；建议将图片、文件和音频工具结果分别纳入跨协议集成矩阵，不能以图片支持推断所有多模态类型都支持 | 中 |
| [AxonHub #2220](https://github.com/looplj/axonhub/pull/2220)：Responses 到 Chat 工具兼容 | 本仓库已有 parallel tool call、custom tool、namespace 和 previous_response_id 的部分处理。建议按能力核查自定义工具、tool_search、同一 call_id 的多段结果以及中断恢复；该 PR 已关闭且未合并，只适合作为检查线索 | 中 |

## 后续补充实现

- **动态工具**：按消息顺序应用 `tool_addition` / `tool_removal`，将最终可用工具投影到请求的 `tools`。保留混合消息中的文本，移除只用于承载工具变更的空消息；同名工具不重复加入。已定义工具可经 `tool_reference` 移除后重新启用；未知引用、缺少名称或 schema 的定义返回明确错误。原始请求字节仍由现有 relay 保存，用于 Anthropic 同协议直通。
- **搜索请求**：Anthropic 原生搜索定义转换为 Responses 的 `web_search`，保留允许/禁止域名和位置参数，请求 `web_search_call.action.sources`。强制工具选择根据声明区分原生搜索和同名普通函数，名称冲突时普通函数优先；转换 `any` 和并行工具控制，并保留原始内部请求供重试使用。
- **搜索响应**：新增独立的服务端搜索内部事件，避免将搜索变成待客户端执行的函数。按 call ID 合并稀疏更新、延迟来源和重复来源，保留已有查询与标题；流式与非流式均生成匹配的 Anthropic `server_tool_use` / `web_search_tool_result`。
- **结束与历史**：在 completed、incomplete、failed、cancelled 终态处理搜索元数据，搜索失败返回明确的工具结果错误。修复 incomplete / failed / cancelled 快照中的 usage 未进入本地流事件的问题。客户端回传搜索历史后，保留查询、来源和错误为 assistant 历史文本，不重放成未完成的客户端函数。
- **验证**：新增跨协议出站、同名工具冲突、重试数据保留、延迟来源、来源去重、来源归属、失败结果、历史往返和终态用量测试。

当前边界：搜索结果块在流式终态统一输出，以接收最终来源；本轮没有增加实时搜索进度。来源仅使用上游显式返回的列表，缺少标题时以 URL 显示，不把多个搜索的答案引用猜测为某个搜索的结果。不会生成 Anthropic 的不透明 `encrypted_content`；由 Responses 转换出的结果适用于本转换链路，不能视为真实 Anthropic 签名搜索结果。`max_uses` 等缺少直接对应的供应商选项、引用标题补全和真实客户端实测仍需后续单独核查。

## 验证方式

先在未修复代码上运行新增回归，确认混合思考、多片段重建、拒绝文本聚合、Gemini 并行工具索引均失败，再修复并运行转换层与 relay 测试。

```bash
go test ./internal/transformer/... ./internal/relay/...
```

全仓库测试需要 `static/out` 存在；纯后端检验可以按 `.github/workflows/ci.yml` 的做法临时创建 `static/out/.keep`，再执行 `go test ./...`。该占位文件只用于编译测试，不是可部署的前端构建产物。

本次结果（包括后续补充）：转换层与 relay 测试通过；按上述方式准备临时占位后，`go test ./...` 全仓库测试通过；`go vet ./internal/transformer/...`、`git diff --check` 通过。测试后已清理临时占位。
