# ClaudeGo

围绕 Claude 5 小时用量限额设计的本地任务队列与调度器。单个 Go 二进制，无外部依赖。

核心思路：**编排器本身是纯本地代码，不消耗任何 Claude 额度**；只有任务真正执行时才调用 `claude -p`。撞到限额时任务自动暂停并记下重置时间，到点后用 `--resume` 接回同一个会话继续干活，把每个 5 小时窗口榨干。

```
                 ┌──────────────────────────────────────────────┐
   claudego add  │                  任务队列 (~/.claudego/tasks)  │
   assemble ────▶│  queued ──▶ running ──▶ done                 │
   review  plan  │               │  └──▶ failed（退避重试后）      │
   adopt  brief  │               ▼                              │
                 │         limit_paused ──(到达重置时间)──┐        │
                 └───────────────────────────────────┼────────┘
                                                     │
   launchd / daemon 每 5 分钟 tick ──▶ 派发规则选一个任务 ─┘
                                      │
                                      ▼
                    claude -p --model <模型> --resume <会话> ...
```

## 五种任务类型

| 类型 | 用途 | 默认权限 / 模型 |
|---|---|---|
| `design-review` | 设计审核 session：只读审查代码/架构，产出 P0/P1/P2 分级报告 | 只读工具 + git log/diff |
| `prompt-assembly` | prompt 装配 session：调研项目后把目标拆成 prompt 序列，**产出的任务自动入队** | 只读工具 |
| `sequence` | 预设 prompt 序列：多个步骤在同一个会话中依次执行（`--resume` 串联，上下文连续） | acceptEdits + 常用构建/测试命令 |
| `coordinate` | 分工协调 session：读**实时**队列快照 + 各会话进度报告，把目标拆成分工任务（含模型建议）自动入队 | 只读工具，默认 sonnet |
| `progress-pull` | 进度回收 session：`--resume` 某个会话，让它输出结构化进度报告并落盘 | 只读工具，默认 haiku |

任务可以链式衔接：`assemble`（装配）→ 产出 `sequence` 入队 → 执行完成 → `review_after` 自动入队一个 `design-review` 审查刚才的改动。

## 进度回收 → 分工协调 → 自动推进

多个会话并行干活时的编排闭环：

**桌面端也在管辖范围内**：Claude Code 桌面端与 CLI 共用 `~/.claude/projects` 会话存储和订阅额度，
所以桌面端里开的会话同样可以被列出、回收进度、`--resume` 接管。

```bash
# 0) 找会话：列出某项目最近的 claude 会话（桌面端 + CLI 同池），拿到会话 ID
claudego sessions -dir ~/Projects/myapp

# 1) 回收进度。交互式会话（含桌面端）：打印"整理进度"prompt，贴进去后报告自动写回 ~/.claudego/progress/
claudego brief -dir ~/Projects/myapp -title 鉴权重构
#    有会话 ID 的（队列任务 / sessions 列出的桌面会话）：入队 haiku 回收任务，全自动
claudego brief -id t0705-xxxx -auto
claudego brief -session <session-id> -dir ~/Projects/myapp -auto

# 2) 分工。协调任务运行时注入实时队列快照 + 全部进度报告，
#    产出：人话分工说明（每个任务做什么/建议模型/手动接管命令，留在 log 里）
#    + 分工任务自动入队（带 model 字段，被依赖的 priority 更高，可续跑的带 session_id）
claudego plan -dir ~/Projects/myapp "本周把上传模块收尾并补齐测试"

# 3) 自动推进：launchd/daemon 照常 tick，按模型建议逐个执行；随时查看与接管
claudego list                 # 看分工执行到哪了
claudego log <协调任务ID>      # 看人话分工说明
claudego cmd <id>             # 想手动接管某任务：打印 claude 命令 + 当前步骤 prompt（先 hold）
claudego progress             # 看已回收的进度报告；-in 可手动粘贴导入
```

**模型路由**：任务带 `model` 字段则以 `--model` 执行（订阅限额按模型加权，例行工作路由到
sonnet/haiku 能显著拉伸 5 小时窗口）。所有添加命令支持 `-model`，协调任务的分工输出里
按"机械→haiku / 常规实现→sonnet / 高风险→默认最强"自动建议，也可在 `type_defaults.*.model` 配默认值。
杠杆倒置原则：贵模型只做小 token 量的编排与仲裁（coordinate 默认 opus），便宜模型烧大 token 量的执行。

**设计期 profile（fable 出设计、opus 落地）**：设计质量为第一优先级的阶段，把设计三件套切到最强模型——
`type_defaults` 里 coordinate / design-review / prompt-assembly 的 model 设为 `"claude-fable-5"`，
协调模板会按"设计→fable、落地→opus、机械→sonnet、琐碎→haiku"给产出任务指派模型；
`model_weights` 记得加 `"claude-fable-5": 10`。进入密集开发期后可把 design-review 回调到 opus 控制消耗。

**会话内再分层（子 agent）**：`sequence` 任务默认放行 Task 工具，配合用户级子 agent
（`~/.claude/agents/deep-reasoner.md` 绑 opus、`fast-worker.md` 绑 sonnet），执行会话可以把
疑难推理上交、机械劳动下放——跨会话按任务路由 + 会话内按环节路由，两层叠加。

### 文件化状态（fresh_steps）与人工把关（-hold）

推荐把项目状态放在**文件**里（state.md / TASKS.md 等），任务不依赖会话记忆：

- `add -fresh` 或 emit JSON 里 `"fresh_steps": true`：步骤间不 `--resume`，每步全新会话。
  协调模板已内建三段式规约：开工读状态文件 → 只做一个增量 → 收工更新状态文件与任务清单。
- 好处：永不撞会话上下文上限（"Prompt is too long" 类失败绝迹）、限额中断后直接重发本步开新会话
  （无需续跑提示）、codex 备用执行器可接管**任意一步**（不再限单步任务）、审计友好（状态变更全在 git 里）。
- `plan -hold` / `assemble -hold`：分工产出的任务先挂起（held），人工审完 `claudego release <id>` 放行——
  "拆分 → 把关 → 推进 → 审核 → 更新状态" 的完整循环。

### 存量角色会话的接管（此前手动维护的 审核/装配/执行 session）

一个项目文件夹里已经养了一批长驻角色会话时，按角色分流：

```bash
claudego sessions -dir ~/Projects/myapp        # 认领：按首条消息识别各角色会话，拿到 ID

# 有在途工作的（执行/细化 session）→ 先收进度，再决定续跑还是重开
claudego brief -session <ID> -auto             # 存量上下文提炼成进度报告（含 next_prompt）
claudego adopt <ID> -dir ~/Projects/myapp      # 没做完的直接接管续跑

# 角色会话本身 → 对应类型命令 + -session 挂载，新一轮工作续用老会话的积累
claudego review   -session <老审核会话ID> "审查本周改动"
claudego assemble -session <老装配会话ID> "下一个目标"
claudego add -type sequence -session <老执行会话ID> -file 下一批步骤.md

# 或者放弃挂载：把老会话里沉淀的角色要求改进 templates/*.md，以后每轮全新开（上下文更便宜）
```

注意：headless 续跑既有会话是**分叉**（fork 出新 session id，原桌面会话不受影响）；任务首轮跑完后，
后续轮次应挂任务里最新的 session_id（`claudego list -json` 可见），或直接对同一任务追加步骤。
长驻会话上下文会越滚越贵，一般建议：知识沉淀进模板/进度报告，执行用短会话。

## 派发规则（可在 config.json 调整）

1. **续跑优先**（`resume_first`）：被限额打断的任务先于新任务——先把没做完的做完；
2. **priority 大者优先**；
3. **类型顺序**（`type_order`）：默认 审核 > 序列 > 装配（审核便宜且能尽快给出反馈，装配会派生新工作放最后）；
4. 同级按先进先出。

限额是全局的：任何任务撞到限额，写入全局冷却（`cooldown.json`），期间不再派发任何任务、不浪费探测调用；冷却时间优先取错误信息里的重置时间戳，解析不到则回退 `limit_fallback_min` 分钟后重试。

## 快速开始

```bash
make build && make install     # 编译并装到 /opt/homebrew/bin
claudego init                  # 初始化 ~/.claudego（数据目录可用 CLAUDEGO_ROOT 覆盖）

# 1) 预设 prompt 序列：steps.md 里用单独一行 --- 分隔步骤
claudego add -title "重构鉴权" -dir ~/Projects/myapp -priority 5 -review-after -file steps.md

# 2) prompt 装配：让 Claude 先调研再自动生成任务序列并入队
claudego assemble -dir ~/Projects/myapp "给上传模块加断点续传，含测试"

# 3) 设计审核：只读审查
claudego review -dir ~/Projects/myapp "并发与错误处理"

# 4) 接管一个刚被限额打断的交互式会话（桌面端或 CLI；会话 id 用 claudego sessions 查）
claudego adopt <session-id> -dir ~/Projects/myapp

claudego run                   # 手动跑一轮验证
claudego install-launchd       # 安装后台调度：每 5 分钟 tick 一次，开机自启
claudego list                  # 看板；log <id> 看细节；doctor 自检
```

不想装 launchd 时可以直接 `claudego daemon` 前台常驻。

## 5 小时额度红线（保底额度）

给突发/交互任务留余量：红线生效时队列停止派发（多步任务也会在步骤间让位），`-force` 可越线。两条独立通道，`claudego quota` 随时查看：

```jsonc
// ~/.claudego/config.json
"queue_budget_tokens": 2000000,  // ① 本地账本：滑动 5h 窗口内队列最多消耗的加权 token，0 关
"redline_percent": 85,           // ② 外部全局用量源：5h 窗口 usedPercent 达线即停，0 关
"usage_feed": "/Users/you/Library/Application Support/CodexBar/usage-history.jsonl",
"usage_feed_max_age_min": 90,    //    样本过期视为不可用→放行（fail-open）
"model_weights": {"default":1,"opus":5,"sonnet":1,"haiku":0.2}   // 账本的模型加权
```

- ①只统计 claudego 自己的调用（桌面端消耗不可见），语义是"队列预算上限"——保底 = 总额度 − 队列预算。先跑几天 `claudego quota` 看典型消耗再定值。
- ②是全局视角，样本格式兼容 CodexBar 的 usage-history.jsonl（需在 CodexBar 里开启 Claude 用量探测）；任何工具按同格式落一行 JSONL 都能接。
- 真正耗尽时仍有限额冷却兜底（解析重置时间、到点续跑），红线只是提前让路。

**分时段红线**（`redline_windows`）：时段内非零字段覆盖全局阈值，时段外回落全局；跨零点用 from > to。
`redline_lead_min` 给时段加前置缓冲：开始前 N 分钟就停发 claude 任务——单步任务起跑后无法中途让位，
不加缓冲的话踩线起跑的长任务会烧进预留窗口（codex 钉定任务不受影响）。时段 from 建议对齐配额窗口的真实重置时刻。
典型用法——交易早盘给交互留 25% 余量，其他时段队列用满：

```jsonc
"queue_budget_tokens": 0, "redline_percent": 0,   // 全局：不限
"redline_windows": [
  {"from": "06:50", "to": "11:50", "redline_percent": 75, "queue_budget_tokens": 300000}
]
```

## Codex 备用执行器（限额空窗不断档）

调度器本身是纯 Go，不耗额度，限额只会让任务等待、不会让系统瘫痪。但冷却期内没有执行力——
配置 codex CLI 后，claude 被冷却或红线拦住的时段，**单步且无既有 claude 会话**的任务
（协调 / 审核 / 装配 / 单步 add——正是维持管线运转的编排环节）自动切给 `codex exec` 执行：

```jsonc
"codex_bin": "/opt/homebrew/bin/codex",
"codex_fallback": true,
"codex_model": ""        // 可选，-m 透传
```

- 带 claude 会话的多步任务不切换（跨 CLI 无法延续上下文），等重置自动续跑；
- codex 走自己的额度：不记 claude 账本、其错误不写全局冷却、成功也不清冷却；
- 沙箱按类型收窄：只读类任务 `--sandbox read-only`，sequence 用 `workspace-write`；
- 看板与日志标注 `[codex]` / `runner=codex`，emit/进度解析管线照常工作（协调分工在冷却期也能继续入队）。

## 限额中断与自动恢复的细节

- 步骤执行中撞限额：任务标记 `limit_paused` 并记录 `mid_step`。到点续跑时不会重发原 prompt，而是向**同一个会话**发送续跑提示（`config.json` 的 `resume_prompt`），让 Claude 从中断处接着做，避免重复劳动。
- 每一步成功后立刻落盘（任务文件原子写入），进程被杀也不丢进度。
- 单实例锁（`.lock`）保证 launchd 的多次触发不会并发跑任务；持锁进程死掉会自动清锁。
- 其他错误（网络、超时等）按 `retry_backoff_min` 退避重试，超过 `max_attempts_per_step` 次标记失败，`claudego retry <id>` 可带着会话与进度重新入队。

## 权限与安全

任务默认**不**使用 `--dangerously-skip-permissions`：

- 审核/装配任务是只读工具白名单；
- `sequence` 默认 `acceptEdits` + 常用构建测试命令白名单，白名单外的 Bash 命令在无头模式下会被自动拒绝（Claude 会绕开或说明）。

需要完全自主时对单个任务加 `-skip-permissions`，或改 `config.json` 里对应类型的 `skip_permissions`。工具白名单在 `type_defaults.*.allowed_tools` 中按类型调整。

## 配置速查（~/.claudego/config.json）

| 键 | 默认 | 说明 |
|---|---|---|
| `poll_interval_sec` | 300 | launchd/daemon 轮询间隔 |
| `limit_fallback_min` | 30 | 解析不到重置时间时的等待 |
| `cooldown_margin_sec` | 90 | 重置时间上再加的安全余量 |
| `step_timeout_min` | 60 | 单步硬超时（防跑飞） |
| `max_attempts_per_step` | 3 | 单步失败重试上限 |
| `resume_first` | true | 被打断任务优先续跑 |
| `type_order` | 进度回收>协调>审核>序列>装配 | 同优先级时的类型顺序 |
| `resume_prompt` | … | 限额中断后的续跑提示词 |
| `type_defaults.*.model` | 协调 opus；回收 haiku | 各类型默认模型（--model 值），空用账号默认 |
| `queue_budget_tokens` 等 | 0（关） | 5 小时额度红线，见上文专节 |
| `max_parallel` | 1 | 单次 tick 并行任务数（同目录强制串行） |
| `codex_bin` / `codex_fallback` | 空 / false | 冷却期备用执行器，见上文专节 |

提示词模板在 `~/.claudego/templates/*.md`，可直接修改（`{{GOAL}}` `{{DIR}}` `{{FOCUS}}` 会被替换；
`coordinate.md` 里的 `{{QUEUE}}` `{{PROGRESS}}` 在**派发时**替换为实时快照）。

## 测试

```bash
make test   # mock claude 跑完整状态机：调度/限额暂停/冷却/续跑/装配入队/失败退避/模型路由/进度回收/分工协调
```
