# rollout-man — Agent Evaluation Orchestration Platform 详细设计 v0.4

版本：v0.4（合并稿，唯一设计基线）
日期：2026-08-18
状态：Draft，待评审

**命名**：本系统名为 **rollout-man**，CLI 二进制、Temporal namespace、Docker label
前缀、环境变量前缀（`ROLLOUT_MAN_*`）统一使用该名。文中出现的 **Harbor** 一律指
上游执行系统（Agent runtime / Docker runtime / Verifier / Case format），是本系统的
依赖而非本系统的一部分。

---

## 0. 文档约定

### 0.1 优先级标记

全文用行内标记声明交付批次，汇总见 §15.1：

- **[MVP]** — 不做就跑不起来或会出线上事故
- **[P1]** — MVP 后 1–2 个迭代
- **[P2]** — 有明确需求再做

无标记的段落属于 MVP。

### 0.2 术语

| 术语 | 含义 |
|---|---|
| **Case / CaseVersion** | 评测题目及其某个版本；内容寻址存储（CAS） |
| **Experiment** | 一次评测声明：case × agent × llm_spec × N trials |
| **Task** | matrix 展开后的一个单元；读模型分组，无成败状态 |
| **Trial** | 唯一的执行单位，一个 TrialWorkflow |
| **attempt** | Trial 内部的重试计数（换机重试不产生新 Trial） |
| **Runner** | 执行机器；每台一个专属 Temporal activity queue |
| **Placement** | 决定 trial 何时、在哪台机器执行的自研服务 |
| **准入 / Admission** | Case 版本在被正式使用前必须通过的 oracle/nop 校验（§3.5） |
| **后置清理 / PostProcess** | Trial 产物离开 Runner 前的清洗与打包（§6.4） |
| **pipeline** | 主链之外的可配置钩子，分 `per_case` / `per_trial` / `per_experiment` 三个块，**键名即执行单位**（§11.2） |
| **Harbor** | 上游执行系统（Agent runtime / Docker runtime / Verifier / Case format），本系统的依赖 |

## 怎么读这份文档

- **要实现某个部件** → 直接跳到对应章节；每章开头是结论，`>` 引起的段落是「为什么这样定 / 配错会怎样」，可以跳过不影响实现
- **要评审设计** → 先看 §2 总体架构 与 §15 优先级分级，再按需展开
- **要排期** → §15.1 分级总表 + §15.3 Phase 计划，两张表就够
- **行内 [MVP] / [P1] / [P2]** 标记的是交付批次，无标记即 MVP（§0.1）
- **本文是目标架构**。已实现的 MVP 比它小得多——按第一性原理砍掉了什么、为什么、什么时候加回来，见 **§15.0**，第二波补了什么见 **§15.0.1**；实现与用法见 `README.md`

## 目录

- **0. 文档约定**

**第一部分 · 概览与模型**

- **1. 背景与边界**
- **2. 总体架构**
- **3. 领域模型**

**第二部分 · 运行时**

- **4. 编排层（Temporal）**
- **5. Placement Service**
- **6. Runner Agent**
- **7. 失败分类与重试**

**第三部分 · 安全**

- **8. 安全与信任边界**

**第四部分 · 数据、接口与结果**

- **9. 数据模型**
- **10. API 规格**
- **11. CLI 规格**
- **12. 结果分析**

**第五部分 · 工程与运维**

- **13. 技术选型与工程实践**
- **14. 可观测性与运维**

**第六部分 · 交付计划**

- **15. 优先级分级与开发计划**
- **16. 开放问题**

- **附录 A：设计原则（一句话版）**

---

# 第一部分 · 概览与模型

> 这个系统解决什么问题、由哪些部件组成、领域里有哪些东西。

## 1. 背景与边界

### 1.1 问题定义

Harbor 已提供 Case、Agent、Trial、Verifier 和执行环境等基础能力，缺的是大规模 Agent Evaluation 的统一编排层。

| # | 痛点 | 本系统的职责 |
|---|------|--------------|
| 1 | Case 可达数 GB，无法通过 API 直接上传 | Case Artifact 管理（Git / 对象存储 / CLI 上传 + CAS 去重） |
| 2 | 同一 Case 需多 Agent × 多 Model × 多 Trial | Experiment Matrix 自动展开 |
| 3 | 大量任务需排队而非立即执行 | Placement 排队与授予 |
| 4 | Runner 数量 > 1，需选择合适 Runner | 资源感知调度 |
| 5 | 不同 Project / Series 共享 image 与 build cache | Cache-aware 调度 [P1] |
| 6 | Docker cache / dangling image 持续占盘 | Housekeeper 分级清理 |
| 7 | Agent / Docker / Verifier / Host failure 需区分 | Failure Taxonomy + 状态与失败分离 |
| 8 | 用户需要看到状态、失败原因、日志、资源使用 | 读模型 + Artifact + CLI |
| 9 | Experiment 需要比较不同 Agent / Model | 结果聚合与分析 |

### 1.2 系统边界

本系统回答：**什么时候运行、在哪里运行、运行什么、状态如何、失败为什么、资源是否足够**。
Harbor 回答：**怎么运行**（Agent runtime、Docker runtime、Verifier、Case format）。
Temporal 回答：**怎么保证每一步按顺序做完且不丢**。

### 1.3 非目标

- 不重新实现 Agent runtime / Docker runtime / Verifier / Harbor Case format / Agent SDK
- 不做多租户、Billing、Kubernetes scheduler、跨 Region 调度、autoscaling
- 不做复杂 Web UI（CLI 优先，UI 见 [P2]）
- **不做多租户 ≠ 不做鉴权**，见 §8.4

### 1.4 MVP 的一句话定义

> 单团队、**≤3 台 Runner**、**≤500 trials/experiment**、**无 GPU**；
> 能从一条 YAML 跑到一张结果表，中途挂 Runner 不丢任务，Runner 正常重启不重跑，
> 连跑一周磁盘不会满，key 不会泄漏，排队的任务一定有结局。

任何不服务于这句话的能力一律不进 MVP。**特别地：affinity 调度不进 MVP** —— 它是性能优化，而 MVP 阶段连"cold start 占多少时间"的数据都没有，权重只能拍脑袋（§5.7）。

---

## 2. 总体架构

### 2.1 拓扑

```text
            ┌─────────────┐   REST /api/v1
            │  CLI / Web  │──────────────┐
            └─────────────┘              ▼
                                ┌──────────────────┐      ┌──────────────────────┐
                                │    API Server    │─SQL──│  PostgreSQL          │
                                │  registry        │      │  app db（读模型/注册表）│
                                │  读模型           │      │  temporal db（持久化） │
                                │  placement matcher│      └──────────────────────┘
                                │  workflow worker  │
                                └───────┬──────────┘
                     StartWorkflow / Signal│gRPC
                                          ▼
                                ┌──────────────────┐
                                │  Temporal Server │  namespace: rollout-man
                                └───────┬──────────┘
              long-poll（出方向 gRPC 7233，NAT 友好）
        ┌───────────────────┬───────────┴────────┬───────────────────┐
        ▼                   ▼                    ▼                   ▼
  workflow worker      activity worker      activity worker     activity worker
  queue: orchestrator  queue: runner.01     queue: runner.02    queue: runner.03
  （API Server 进程内）  （Runner Agent 内）   （Runner Agent 内）  （Runner Agent 内）
                                 │
                                 ▼
                       ┌──────────────────┐
                       │  Object Storage  │
                       │ Logs / Artifacts │
                       │  Case archives   │
                       └──────────────────┘
```

### 2.2 关键架构决策

| 决策点 | 结论 | 理由 |
|--------|------|------|
| 编排引擎 | **Temporal（自托管，sdk-go）** | 替换掉最难写对的自研代码：lease、orphan 判定、选主、级联 cancel、断点续跑 |
| 队列 | Temporal Task Queue | 原 PG `FOR UPDATE SKIP LOCKED` 队列删除 |
| 调度 | **Placement Service**（自研）+ Temporal 派发 | 资源/优先级/affinity 是业务智能，保留；派发可靠性交给框架 |
| 状态一致性 | **Temporal event history 是事实源**，PG 是读模型 | 非法状态转移在 Temporal 里不可表达；PG 只服务查询 |
| Runner 连接方向 | Runner **出方向** long-poll Temporal frontend | 与原 pull-based 的 NAT 友好性等价，且不需要自研协议 |
| Runner 亲和 | **每台 Runner 一个专属 activity queue**（`runner.<id>`） | placement 定了 runner，后续步骤全部 pin 到该 queue，天然共享本地 workdir；不需要 Temporal Session |
| HA | API Server 多副本（workflow worker 天然多活）；placement matcher advisory lock 单活 [P1] | matcher 失效后果轻：只是暂停新授予 |
| 大文件 | 全部走外部存储，经**命令行适配器**接入（见 §9.4） | Case 数 GB、日志较大；Temporal payload 上限 2MB |
| Case 去重 | CAS（SHA-256） | CaseFactory 会产生大量重复 Case |

**不引入 Kubernetes / MQ / Redis**，除非规模实测证明必要。

**没有 Plan B。** 引入 Temporal 是不可逆决策：到 Phase 1 结束，workflow 层级、activity 划分、runner worker 模型、读模型投影、错误模型全部按 Temporal 写死，此时"回退到 PG queue"等于重写整个编排层加大半个 Runner Agent。因此判据前移到 **Phase 0（1 周）**，用可证伪的三条去留标准（§15.2），过了就锁死。不设"跑一段时间再评审是否回退"的条款——到那个时点没有人会选择执行它，写着只会让人以为有退路。

### 2.3 Task Queue 规划

| Queue | Worker 所在进程 | 内容 |
|---|---|---|
| `orchestrator` | API Server | 所有 Workflow Task + server 侧 activity（读模型写入、report / deploy 等） |
| `placement` | API Server | `AcquirePlacement` / `ReleasePlacement` |
| `runner.<id>` | 对应 Runner Agent | 该机器上的全部执行类 activity |

Runner worker 以 `DisableWorkflowWorker: true` 启动，只跑 activity，不参与 workflow 决策。

---

## 3. 领域模型

```text
Project ── Series ── Case ── CaseVersion (→ CAS object)
                               │
Experiment ────────────────────┘ (引用 case_version)
    │  matrix 展开
    └── Task (evaluation intent: case × agent × llm_spec × N trials；读模型分组)
           └── Trial (唯一执行单位；一个 TrialWorkflow)
```

- **Task = intent**，只是分组标签，**没有成败状态**（§4.4）
- **Trial = execution**，一个 TrialWorkflow；workflow 内部的 attempt 循环负责换机重试，**不产生新的 trial 行**
- **Series** 是 affinity 的主要依据 [P1]：同 Series 的 Case 常共享 base image 与 build cache

### 3.1 Case 的来源、解析与 CAS

**Experiment 里直接写 Case 在哪，不写注册表 ID。** 没有"先跑一遍注册命令拿到一个不透明 ID、再把 ID 填进 YAML"这一步——那既让 experiment 文件读不懂，又要求人维护一层 ID 到位置的心智映射。

来源有三种，都通过 §9.4 的命令取：

| source | 定位方式 | 取的命令 |
|---|---|---|
| `git` | `repo` + `ref` + `path` | `source_git` |
| `object` | 对象键（+ 可选 `sha256`） | `storage_download` |
| `local` | Runner 本地路径 | `source_local` |

**内容哈希才是版本。** 取到之后本地算 sha256，那个哈希就是 CaseVersion 的标识；CAS 按 `objects/{sha[:2]}/{sha[2:4]}/{sha}` 存，相同内容只存一次。人读的标识是 `path`（如 `spring/CVE-2026-1234`），结果表按它分组；机器认的是 sha256。

**resolve：把"位置"变成"内容"。** confirm 时对每个 case 执行一次：

```text
resolve = 按 source 跑对应命令取内容 → 算 sha256 → 命中 CAS 则复用
                                    → 未命中则入 CAS → 解析 task.toml → 落 case_versions
```

resolve 作为 `pipeline.per_case` 的第一步在 **Runner 上**执行（和 trial 用同一条命令、同一套归因），因此**字节流不经过 Server**，几 GB 的 clone 也不会把 API Server 拖垮。同一个 sha256 第二次遇到直接命中缓存，跳过整步。

**可变 ref 在 confirm 时钉死**：写 `ref: main` 是允许的，但 confirm 会把它解析成具体 commit 并**记进 experiment 记录**，preview 里显示钉死后的值。否则今天和下周跑的"同一个 experiment"根本不是同一个东西，而这种偏差事后无法从结果里看出来。`local` 每次 confirm 都重新算哈希——本地目录随时在变，这是它作为调试来源的代价。

给了 `sha256` 的 case 直接查 CAS，命中就完全跳过 resolve——这是 CI 或重跑历史 experiment 的快路径。

**CaseVersion 状态机**：

```text
RESOLVING → READY       内容已入 CAS、task.toml 解析成功，可用于准入
          → INVALID     取不到 / 解压失败 / task.toml 解析失败，附错误详情
READY     → ADMITTED    通过准入判据（§3.5），可用于正式 experiment
          → REJECTED    未通过准入，附判据明细
```

- **正式 Experiment 只能用 `ADMITTED`**，否则 422 `CASE_NOT_ADMITTED`（除非 `admission.require: any`）
- 准入结果绑定在 **sha256** 上，不绑在路径或 ref 上。同一份内容在别的仓库、别的路径出现，准入照样有效——准入验的是内容，不是标签
- `task.toml` 在 resolve 时解析进 `task_config`，placement 因此在 dispatch 前就知道资源需求（§5.2 硬约束需要它）

### 3.2 Experiment Matrix

```yaml
experiment:
  name: spring-cve-comparison
  case_defaults:                        # 省下每个 case 重复写来源
    source: git
    repo: github.com/org/eval-cases
    ref: main                           # confirm 时钉死为 commit（§3.1）
  cases:
    - path: spring/CVE-2026-1234
    - path: apache/CVE-2026-5678
  matrix:
    agents: [claude-code, codex]
    llm_specs: [opus-prod, gpt5-prod]
    trials: 5
  concurrency: 8
  priority: normal
```

展开规则：`cases × agents × llm_specs → Tasks`，每个 Task 携带 `requested_trials`。上例 = 2 × 2 × 2 = 8 Tasks / 40 Trials。**`requires_llm: false` 的 agent 不与 llm_specs 做笛卡尔积**，每 case 只产生 1 个 Task。

规模上限（`MATRIX_TOO_LARGE`）：**[MVP] 500** / **[P1] 2000（配合 shard 化）** / **[P2] 10000**。

> 上限的作用是防呆，要防在有意义的位置：10000 × 30min ÷ concurrency 8 ≈ 26 天，没有用户会提交这样一个 experiment 然后等下去。

每个 Trial 的 resources / timeouts **来源于 Case 的 `task.toml`**，在 resolve 时解析进 `task_config`（§3.1）；Experiment 可选择性覆盖（§11.2）。

优先级：**Experiment overrides > CaseVersion task.toml > 系统默认 profile**（`cpu: 4, memory: 16Gi, disk: 50Gi, gpu: 0`）。

### 3.3 LLM Spec

Model 不是裸字符串，Agent 访问 LLM 需要三要素 **base_url + model + api_key**：

**Spec 和 experiment 写在同一个文件里**（YAML 多文档，§11.2），提交时 upsert 进注册表——不需要先跑一遍 `llm-spec create` 再回来引用一个名字。

```yaml
---
kind: LLMSpec
name: opus-prod
provider: anthropic                     # 可选，用于统计
base_url: https://api.anthropic.com
model: claude-opus-4-7
api_key_env: ANTHROPIC_API_KEY          # 从 Runner 环境取；与下面二选一
# api_key_cmd: ["pass", "show", "eval/anthropic"]    # 或跑一条命令，stdout 即 key
max_concurrent: 16                      # [P1] per-spec 在途上限
parameters:
  max_tokens: 65536
```

- **key 不写在文件里，也不由本系统保管**：`api_key_env` 指一个 Runner 侧环境变量，`api_key_cmd` 是一条在 Runner 上执行、stdout 即 key 的命令（与 §9.4 同一套契约）。系统只传递*名字*，从不接触值——key 因此天然不进 Temporal history，也就不需要注册表加密后端、一次性 token 和吊销机制
- 注册表仍然存在，但**只由文件驱动**：submit 时按 `name` upsert。它服务于 placement 的 `max_concurrent` 记账（[P1]）与结果表的 spec 维度，不是给人手工 CRUD 的
- 同一 model 可有多个 spec（不同 proxy / region / 计费账号）；结果聚合按 spec 展示，也可按 `model` 折叠

### 3.4 Agent 类型

| 类型 | 例子 | requires_llm | 用途 |
|------|------|--------------|------|
| `llm` | claude-code, codex, openhands | true | 真正的 evaluation 对象 |
| `builtin` | **oracle**（回放参考解）、**nop**（不执行任何操作） | false | Case / Verifier 质量校验 |

- **oracle** 应得满分，否则 Case 有问题（环境坏 / 参考解失效 / verifier bug）
- **nop** 应得 0 分，否则 Verifier 有漏洞
- 推荐流程：新 Case 注册后先跑 oracle + nop 的 validation experiment。**[P1]** CLI 快捷命令 `case validate`
- oracle 失败 **不塞进 `ENVIRONMENT` failure category**（那会污染 ENVIRONMENT 的统计），而是在结果分析里标记 `CASE_SUSPECT`（§12）

### 3.5 Case 准入（Admission Gate）

**没有通过准入的 CaseVersion 不能进正式 experiment。** 理由很直接：一个 Case 若环境是坏的、参考解已失效、或 Verifier 有漏洞，用它跑出来的所有 agent 分数都是噪声——而这种噪声在结果表上看起来和真实的能力差异一模一样，事后无法区分。准入是把这类污染挡在数据产生之前的唯一手段。

**准入判据**（默认值，可在 `admission` 配置里覆盖）：

| builtin agent | 判据 | 含义 |
|---|---|---|
| `oracle` | `reward >= oracle_min`，默认 **1.0** | Case 可解、环境正确、Verifier 认得出正确解 |
| `nop` | `reward <= nop_max`，默认 **0.0** | Verifier 不会平凡通过 |

- 每个 builtin 各跑 `admission.trials` 次（默认 **2**，跑两次而不是一次以排除偶发）
- **全部 trial 都要满足**，任一不满足 → `REJECTED`，不满足的 trial id 记进 `admission_result`
- **trial 自身失败（infra / 环境类）不算 REJECTED**，按普通 retry 重跑；只有"跑完了但分数不对"才构成判据结果
- oracle / nop 都是 `requires_llm: false`，**准入不消耗任何 LLM 额度**，可以放心多跑

**流程**：

```text
POST /cases/{id}/versions/{v}/admit
  → 内部创建 validation experiment（agents: [oracle, nop]，不与 llm_specs 展开）
  → 走与正式 experiment 完全相同的执行链（同一套 placement / runner / 归因 / 后置清理）
  → FinalizeExperiment 按判据回写 case_versions.state = ADMITTED | REJECTED
```

复用同一条执行链而不是另写一套校验逻辑，额外好处是：**准入通过本身就证明了这条链在这台集群上能跑通** —— 环境、镜像构建、verifier、artifact 上传全部正常。新 Case 第一次出问题几乎总是在这一步暴露，而不是等跑完 200 个正式 trial 之后。

**绕过**：experiment 可声明 `admission.allow_unadmitted: true`（仅调试用）。此时结果表对该 case 的所有行打 `⚠ UNADMITTED` 标记，且这些结果**不进入任何跨 experiment 的聚合对比**。

**[P1] 复检**：Case 的环境会漂移（base image 上游更新、依赖源失效、外部服务变更），`admitted_at` 超过 `admission.revalidate_after`（默认 30d）时在 preview 里告警，`rollout-man case admit --all --stale` 批量复检。

**[P1] 归因辅助**：oracle 未达标与 nop 超标是两种完全不同的问题（前者"题目或环境坏了"，后者"判分漏了"），分别标 `CASE_SUSPECT` / `VERIFIER_SUSPECT`（§12），不塞进 failure taxonomy。

---

# 第二部分 · 运行时

> 一个 trial 从提交到出结果，中间每一步由谁执行、怎么保证不丢、失败了怎么归因。

## 4. 编排层（Temporal）

### 4.1 Workflow 层级

```text
ExperimentWorkflow  (workflow id: exp-{experiment_id})
    │  semaphore(concurrency) + 滑动窗口启动 child
    ├── TrialWorkflow (id: trial-{task_id}-{trial_index}[-r{retry_seq}])   × N
    └── ContinueAsNew（history 接近阈值时 drain 后续接）
```

两层，**没有 TaskWorkflow**：experiment 级 concurrency 是跨 task 的全局约束，收敛在一个父 workflow 里最简单；多一层只会把并发控制变成跨 workflow 协调问题。

**[P1] shard 化**：父 workflow 只启动 N 个 `ShardWorkflow`，每个 shard 固定负责一段 trial 列表，父 history 与 trial 数解耦。这是 2000 trials 上限的前提条件。

### 4.2 ExperimentWorkflow

职责三件：限流启动 child、聚合完成度、响应 pause/cancel。matrix 已在 confirm 时确定性展开并作为 workflow 输入。

```go
func ExperimentWorkflow(ctx workflow.Context, in ExperimentInput) error {
    sem := workflow.NewSemaphore(ctx, int64(in.Concurrency))
    paused := false
    workflow.Go(ctx, func(gctx workflow.Context) {
        ch := workflow.GetSignalChannel(gctx, "pause")
        for { var p bool; if ch.Receive(gctx, &p) { paused = p } else { return } }
    })

    pending, inFlight := in.Trials, 0
    for len(pending) > 0 {
        if err := workflow.Await(ctx, func() bool { return !paused }); err != nil { break }
        if err := sem.Acquire(ctx, 1); err != nil { break }          // cancel 从这里退出
        t := pending[0]; pending = pending[1:]; inFlight++

        cctx := workflow.WithChildOptions(ctx, workflow.ChildWorkflowOptions{
            WorkflowID:            trialWorkflowID(t),
            TaskQueue:             "orchestrator",
            ParentClosePolicy:     enumspb.PARENT_CLOSE_POLICY_REQUEST_CANCEL,
            WorkflowIDReusePolicy: enumspb.WORKFLOW_ID_REUSE_POLICY_REJECT_DUPLICATE,
        })
        f := workflow.ExecuteChildWorkflow(cctx, TrialWorkflow, t)
        var started workflow.Future = f.GetChildWorkflowExecution()
        workflow.Go(ctx, func(gctx workflow.Context) {
            defer func() { sem.Release(1); inFlight-- }()
            if err := started.Get(gctx, nil); err != nil {
                // child 根本没起来（ID 冲突 / 永久性 workflow task 失败）：
                // 必须显式记账，否则该 trial 在读模型里永远停在 PENDING
                recordTrialStartFailed(gctx, t, err)
                return
            }
            _ = f.Get(gctx, nil)          // 单个 trial 的成败不终止 experiment
        })

        if workflow.GetInfo(ctx).GetContinueAsNewSuggested() {
            _ = workflow.Await(ctx, func() bool { return inFlight == 0 })   // drain
            return workflow.NewContinueAsNewError(ctx, ExperimentWorkflow,
                in.WithRemainingTrials(pending))
        }
    }
    _ = workflow.Await(ctx, func() bool { return inFlight == 0 })
    return runPerExperiment(ctx, in) // upload / report / deploy，见 §6.5 与 §11.2
}
```

要点：

- **WorkflowID = `exp-{id}`，Start 侧 `RejectDuplicate`**：重复 confirm 天然幂等（对应 §10.1 的 409 语义）
- **cancel**：`client.CancelWorkflow("exp-182")` 一条调用级联到所有 child 与其 activity
- **child 启动失败必须显式记账**：`GetChildWorkflowExecution()` 与最终结果分开等待。若只等最终结果，"没起来"和"跑完失败了"不可区分；而 child 从未运行意味着没有任何 server activity 会写读模型，那一行 trial 永远停在 PENDING
- **ContinueAsNew 用 `GetContinueAsNewSuggested()`**，不写死 20000
- **drain 后再 CAN**：MVP 的 500 trials 上限下几乎不会触发（约 3–4K events）。注意父 run 因 CAN 关闭时 `ParentClosePolicy` 的行为需在 Phase 0 验证（§15.2）——当前"drain 到 inFlight==0"的写法对此免疫，但 [P1] 改 shard 化时必须先有结论
- 进度查询不打扰 workflow：走读模型（§5.9），UI/CLI 不直接 Query

**`concurrency` 的准确语义**：semaphore 计的是**在途 trial（排队 + 执行）**，不是"同时 RUNNING"。资源紧张时 8 个名额可能全被排队中的 trial 占着。CLI preview 里按此如实说明，不要写成"同时 RUNNING 上限"。

> 为什么不把 semaphore 移到 grant 之后：semaphore 同时承担"限并发"和"限制父 workflow 同时持有的 child 数"，后者是 history 增长的闸门。移到 placement 就必须一次性启动全部 child，CAN 会触发得更频繁。正确的解耦方式是 [P1] shard 化。

### 4.3 TrialWorkflow

全系统的核心。结构：**外层 attempt 循环（换机重试）× 内层步骤链（同机步骤级重试）+ defer 补偿**。

```go
func TrialWorkflow(ctx workflow.Context, in TrialInput) (TrialResult, error) {
    // CAN 续接时从输入恢复，绝不从 attempt=1 重来
    attempt, exclude := in.ResumeAttempt(), in.ResumeExclude()
    deadline := workflow.Now(ctx).Add(in.QueueTimeout)     // §5.6
    var lastErr error

    for ; attempt <= in.Retry.MaxTotalAttempts; attempt++ {
        res, runnerID, err := runAttempt(ctx, in, attempt, exclude, deadline)
        if err == nil {
            return res, nil                                 // COMPLETED（reward 高低与此无关）
        }
        lastErr = err
        code := failurecode.FromError(err)                  // §7.2
        recordAttemptFailed(ctx, in.TrialID, attempt, code, err)   // disconnected ctx
        if temporal.IsCanceledError(err) || !in.Retry.Retryable(code) { break }
        if code.ExcludesRunner() && in.Retry.AvoidSameRunner && runnerID != "" {
            exclude = append(exclude, runnerID)
        }
        if err := workflow.Sleep(ctx, in.Retry.Backoff(attempt)); err != nil { break }
    }
    recordTrialFailed(ctx, in.TrialID, lastErr)             // disconnected ctx
    return TrialResult{}, lastErr
}
```

`runAttempt` —— 一次 attempt 的完整步骤链：

```go
func runAttempt(ctx workflow.Context, in TrialInput, attempt int, exclude []string,
    deadline time.Time) (TrialResult, string, error) {

    // ── 补偿先注册，再申请资源 ────────────────────────────────────────────
    // 顺序很关键：placement 循环里有 cancel / CAN 两条提前返回，
    // 若 defer 注册在授予之后，这两条路径会留下幽灵 waiter（§5.8）
    var pl PlacementGrant
    defer func() {
        dctx, cancel := workflow.NewDisconnectedContext(ctx)
        defer cancel()
        dctx, _ = workflow.WithCancelTimeout(dctx, 90*time.Second)   // 补偿本身不能成为卡点
        if pl.Granted {
            _ = workflow.ExecuteActivity(
                runnerCtxOpts(dctx, "runner."+pl.RunnerID,
                    /*startToClose*/ 60*time.Second,
                    /*scheduleToStart*/ 30*time.Second,
                    /*heartbeat*/ 0, /*maxAttempts*/ 1),
                a.CleanupTrial, CleanupRequest{Trial: in.TrialID, Attempt: attempt,
                    KeepWorkdirOnFailure: true}).Get(dctx, nil)
        }
        // 幂等：删 waiter +（若有）删 reservation。cancel/CAN 路径也必须执行
        _ = workflow.ExecuteActivity(placementCtx(dctx),
            a.ReleasePlacement, ReleaseRequest{Trial: in.TrialID, Attempt: attempt}).Get(dctx, nil)
    }()

    // ── 步骤 0：Placement（signal 唤醒 + timer 兜底）────────────────────
    granted := false
    workflow.Go(ctx, func(gctx workflow.Context) {          // matcher 授予后推送
        ch := workflow.GetSignalChannel(gctx, "placement_granted")
        var sig PlacementGrant
        if ch.Receive(gctx, &sig) { pl = sig; granted = true }
    })
    for {
        if err := workflow.ExecuteActivity(placementCtx(ctx), a.AcquirePlacement,
            PlacementRequest{Trial: in.TrialID, Attempt: attempt, Resources: in.Resources,
                LLMSpec: in.LLMSpecName, Priority: in.Priority, Affinity: in.Affinity,
                Exclude: exclude, FirstQueuedAt: in.FirstQueuedAt},
        ).Get(ctx, &pl); err != nil {
            return TrialResult{}, "", err
        }
        if pl.Granted { break }
        _ = workflow.UpsertTypedSearchAttributes(ctx, saBlockedReason.ValueSet(string(pl.BlockedReason)))
        if pl.BlockedReason.Permanent() {                   // 如 CAPABILITY_MISMATCH
            return TrialResult{}, "", failurecode.Wrap(failurecode.Unplaced(pl.BlockedReason))
        }
        if !workflow.Now(ctx).Before(deadline) {            // §5.6 排队超时
            return TrialResult{}, "", failurecode.Wrap(failurecode.QueueTimeout(pl.BlockedReason))
        }
        // signal 是主路径，timer 只是兜底：授予后最长空转从 5min 降到 ~0
        if err := workflow.AwaitWithTimeout(ctx, 60*time.Second,
            func() bool { return granted }); err != nil {
            return TrialResult{}, "", err
        }
        if granted { break }
        if workflow.GetInfo(ctx).GetContinueAsNewSuggested() {
            return TrialResult{}, "", workflow.NewContinueAsNewError(ctx, TrialWorkflow,
                in.WithResumeState(attempt, exclude))       // 续接后不从第 1 次 attempt 重来
        }
    }
    rq := "runner." + pl.RunnerID

    // ── 步骤 1–6：全部 pin 在 rq，每步独立 timeout / retry / 失败归因 ──
    // （超时与重试参数见 §7.4）
    if err := act(ctx, rq, a.FetchCase, in.FetchSpec(attempt)); err != nil { ... }
    if err := act(ctx, rq, a.PrepareEnv, in.EnvSpec(attempt)); err != nil { ... }
    var run RunAgentResult;  if err := act(ctx, rq, a.RunAgent, in.RunSpec(attempt), &run);     err != nil { ... }
    var v   VerifyResult;    if err := act(ctx, rq, a.RunVerifier, in.VerifySpec(attempt), &v); err != nil { ... }
    var arts []ArtifactRef;  if err := act(ctx, rq, a.CollectArtifacts, in.CollectSpec(attempt), &arts); err != nil { ... }

    res := TrialResult{Reward: v.Reward, Metrics: run.Metrics, Artifacts: arts}
    if err := workflow.ExecuteActivity(serverCtx(ctx),
        a.RecordTrialCompleted, in.TrialID, attempt, res).Get(ctx, nil); err != nil {
        return TrialResult{}, pl.RunnerID, err
    }
    return res, pl.RunnerID, nil
}
```

这个函数就是"事务性的先做什么再做什么"的答案：

- **每步完成即持久化**：`FetchCase` 成功后 Runner 崩溃，Temporal 重新投递的是 `PrepareEnv`，不会重拉 case；每步内部再以幂等兜底（§6.2）
- **顺序与超时由声明保证**；total timeout 用 workflow 侧 timer + CancellationScope 实现（**不用 `WorkflowRunTimeout`**，它是硬杀，不给 cleanup 机会），归因见 §7.5
- **补偿是结构化的**：`defer` + `NewDisconnectedContext` 保证 cancel / 失败路径也执行清理与资源释放；`WithCancelTimeout(90s)` 保证补偿自身不会变成新的卡点
- **换机重试是显式控制流**：activity 级 retry 只处理"同机可恢复"；需要换机的错误穿透到外层 attempt 循环 → `exclude` → 重新 placement

**终态记录必须走 disconnected context**（`recordAttemptFailed` / `recordTrialFailed` 内部）。若用普通 ctx，workflow 被 cancel 后 ctx 已取消，这些 activity 会立刻失败 —— 结果是用户点了取消，PG 里所有 trial 停在 RUNNING，要等 5 分钟对账才恢复。reconciler 是对账，不是主路径。

### 4.4 状态：从状态机到投影

Trial 的"状态"不是一张需要维护的转移表，而是 workflow 事实的**读模型投影**：

| 读模型状态 | 对应的 Temporal 事实 |
|---|---|
| QUEUED / BLOCKED | TrialWorkflow 在 placement 循环中（`BlockedReason` search attribute 区分） |
| DISPATCHED / STARTING | 已授予，首个 runner activity Scheduled / Started |
| RUNNING | `RunAgent` activity Started |
| SUCCESS → **COMPLETED** / FAILED / CANCELLED | workflow 结果（COMPLETED 带 reward；FAILED 带 taxonomy code） |
| RETRYING | attempt 循环中的 backoff timer |

合法性不再靠 `WHERE state = :expected` 事务防御 —— 非法转移在 Temporal 里根本不可表达。

**Task 没有成败状态**（"10 个 trial 里 3 个失败时 Task 算成功还是失败"没有唯一答案，因此不做聚合判定）：

```text
Task: PENDING / RUNNING / COMPLETED / CANCELLED
      COMPLETED = 其下所有 trial 均已到终态
      成败以 trial 分布呈现，不做聚合判定
```

相应地，`POST /tasks/{id}/cancel` = 对该 task 下所有非终态 trial workflow 逐个 cancel；`POST /tasks/{id}/retry` = 只对 FAILED 的 trial 重新起 workflow（COMPLETED 的不动）。

### 4.5 确定性与版本纪律

- **workflow 包禁 I/O**：禁 `time.Now` / `rand` / map 迭代依赖 / 直接 I/O，全走 `workflow.Now`、`SideEffect`、activity。CI 用 `go.temporal.io/sdk/contrib/tools/workflowcheck` 强制
- **`Retry.Backoff()` 必须确定性**：一旦加 jitter 就破坏 determinism，且症状只在 replay / worker 重启时暴露，lint 抓不到跨包间接调用。要么不加 jitter，要么用 `workflow.SideEffect` 派生
- **升级兼容**：workflow 逻辑变更一律 `workflow.GetVersion` 打补丁；发版前用 `worker.WorkflowReplayer` 回放生产抽样 history（每晚 CI）。这是手写状态机时代不可能有的安全网
- **payload 纪律**：单 payload 上限 2MB；日志、result.json 一律走 Object Storage，workflow 只传 key + sha256

---

## 5. Placement Service

资源匹配、优先级排序、affinity 评分全部自研；对外形态是「reservation 授予服务」，而不是传统的「调度循环 + assignment + lease」。

### 5.1 结构

```text
placement matcher（API Server 进程内单 goroutine；多副本时 advisory lock 选主 [P1]）
  输入： placement_waiters（等待中的 trial：resources、priority、first_queued_at、exclude、affinity keys）
        runners / runner_heartbeats / runner_cache_state / resource_reservations
  循环： 每 2s 一轮
        1. 按 priority DESC, aging, first_queued_at 排序 waiters
        2. 逐个硬约束过滤 + 评分 + llm_spec 并发记账
        3. 命中 → 单事务内写 resource_reservations + 标记 waiter granted
                 → SignalWorkflow(trial workflow, "placement_granted")
        4. 未命中 → 更新 waiter.blocked_reason
```

`AcquirePlacement` 是薄壳：upsert waiter（幂等键 `trial_id + attempt`）→ 查询是否 granted → 立即返回。等待发生在 workflow 侧，因此 **placement activity 永远是毫秒级短调用**，不占 slot、不需要 heartbeat。

**matcher 单活失效的后果很轻**：只是暂停新授予，在跑的 trial 不受影响。MVP 单副本即可，[P1] 加 advisory lock 选主。

### 5.2 硬约束

```text
runner.status == HEALTHY
runner.available_cpu    >= task.resources.cpu
runner.available_memory >= task.resources.memory
runner.available_disk   >= task.resources.disk       # available 定义见 5.3
runner.capabilities ⊇ task.required_capabilities      # docker / rootless / arch
runner.running_trials < runner.max_concurrent_trials
llm_spec 在途数 < llm_spec.max_concurrent             # [P1]，超限 → BLOCKED(LLM_SPEC_LIMIT)
runner 不在该 attempt 的 exclude 列表中
```

**[MVP] 明确不支持 GPU**：`resources.gpu` 必须为 0，否则 Experiment 创建时 422 拒绝。原因：`runners` 表只有 GPU **数量**没有型号/显存，真实 GPU 调度必须匹配型号，做一半比不做更危险。GPU 调度进 **[P2]**。

### 5.3 资源记账

记账要同时避开两个陷阱：只看 Runner 上报的 free 会超卖（刚授予的还没体现在心跳里），而"上报值再减去全部预留"会整程双算（心跳里已经扣过一次）。中间那条"只减去尚未体现在上报里的预留"听起来对，但 **Server 无从判定某笔占用是否已经反映在上报里**，写不出来。

**取交集**，两个陷阱都避开：

```text
available_X = min( reported_free_X - safety_margin_X ,
                   declared_capacity_X - Σ 活跃 reservation_X )

safety_margin: disk 取 max(30GB, 10%)；cpu/mem 取 5%
```

- 第一项防"实际已经满了"——含失败 workdir 保留、CAS 缓存、非本系统占用等 Server 不知道的部分
- 第二项防"这一轮刚授予但还没体现在心跳里"
- **batch 上传的 staging 占用计入 `reported_free`**（它就实实在在占着盘），因此第一项自动覆盖它；但 Housekeeper 必须钉住 staging 不回收，否则 placement 看到的空闲是假的（§6.5）

**为什么记账必须进 MVP**（而不是当成后期的利用率优化）：matcher 每 2s 一轮、心跳每 15s 一次，**没有记账就意味着最多连续 7 轮基于同一份陈旧快照对同一台机器重复授予**。这在 3 台 Runner 的 MVP 规模下照样会打爆一台机器。记账解决的是超卖，不是利用率。

**释放时机**：在 `CleanupTrial` 之后（资源占用真实结束的时刻），不是"Trial 进入 RUNNING"。见 §4.3 的 defer 顺序。

**失败 workdir 的磁盘**（`workdir_failed: 24h`）由 Runner 的 `reported_free` 自然体现，不需要额外记账 —— 但前提是 Housekeeper 对 CAS 缓存 + workdir 总量有上限（[P1] CAS LRU），否则 `reported_free` 会被慢慢吃到零而 placement 一无所知。

### 5.4 排队排序

Temporal Task Queue 近似 FIFO，但这不重要：**真正的排队竞争发生在这里**。所有 TrialWorkflow 启动后先到 `AcquirePlacement` 排队，priority / aging / FIFO 的排序在 matcher 授予 reservation 时生效。Temporal 只负责「把该做的事可靠地做完」，「谁先做」由 placement 决定。

```text
Priority → aging → （affinity 评分 [P1]）→ FIFO
```

- Priority 枚举：`CRITICAL(30) > HIGH(20) > NORMAL(10) > LOW(0)`，默认 NORMAL
- **aging**：每等待 10 分钟 +1（上限 +9，不跨档），防止 LOW 永久饿死
- **aging 基准用 trial 的 `first_queued_at`，不是 waiter 行的创建时间**。waiter 幂等键含 attempt，换机重试会新建 waiter；若用 waiter 创建时间做基准，**已经失败过一次的 trial 重排时 aging 归零，比没跑过的还靠后**，与重试意图相反

### 5.5 授予推送

matcher 授予后 `SignalWorkflow(trial-xxx, "placement_granted", grant)`；workflow 侧 `AwaitWithTimeout(60s, granted)` —— **signal 是主路径，timer 只是兜底**。

> 不能只靠 workflow 侧轮询。纯指数退避（退到 5 分钟封顶）会导致：matcher 在 T 时刻授予并写入 reservation，trial 要到最多 T+5min 才发现自己被授予，这段时间资源被预留但没人用；集群越忙、排队越久，浪费越严重。空闲集群下还有个每次都发生的轻量版本：首次调用必然未授予，每个 trial 白等第一个 backoff。

### 5.6 排队超时与 BLOCKED 语义

**每个 trial 的排队必须有结局。** 若排队没有终止条件，后果是：trial 永不进终态 → 占住 experiment semaphore 名额 → 后续 trial 一个都起不来 → `FinalizeExperiment` 永不执行 → experiment 永远不结束。

| blocking_reason | 含义 | 处置 |
|-----------------|------|------|
| `NO_RUNNER` | 无任何 HEALTHY Runner | 等待，受 `queue_timeout` 约束 |
| `RESOURCE_PRESSURE` | 有 Runner 但资源不足 | 同上 |
| `CONCURRENCY_LIMIT` | Experiment 并发已满 | 同上 |
| `LLM_SPEC_LIMIT` [P1] | spec 在途数达上限 | 同上 |
| `CAPABILITY_MISMATCH` | 无 Runner 满足 capability | **永久性 → 立即失败**，不等 |
| `AFFINITY_DEFER` [P1] | 主动等待更优 Runner | 受 `affinity.max_wait` 约束 |

- `queue_timeout`：experiment 可覆盖，**默认 24h**。超时 → trial 终态 `FAILED(UNPLACED, blocked_reason)`，结果表单独成列，不计入任何 agent 表现
- **静态可判定的不满足应在 preview / confirm 阶段就拒绝**（如集群中不存在任何 runner 可能满足的 capability 组合），而不是让它跑到 placement 里等 24 小时

### 5.7 Affinity [P1]

**MVP 只做「硬约束过滤 + 选负载最低」。** 一个看起来很合理的线性加权公式是：

```text
100 × same_series_active + 50 × same_project_active + 40 × image_exists
 + 20 × build_cache_exists + 10 × same_environment - 30 × disk_pressure - 20 × normalized_load
```

问题不在形式，而在**这些数字没有任何数据支撑且量纲不可比**（"同 series 在跑"与"磁盘压力"之间凭什么是 100:30？）。没有实测 cold start 占比就调权重是纯猜。

**MVP 期间先埋数据**：把 `image pull/build 实际耗时` 与 `trial 总耗时` 写进 events。[P1] 用真实数据回答"cache 命中能省多少百分比"再定权重 —— 很可能发现只需要保留 `image_exists` 一项。

配套 [P1]：cache state 上报、scheduling group（同组 +30）、affinity defer（`score_best - score_second >= 60` 时允许留队，`max_wait` 默认 10m，**超时强制 dispatch，绝不允许饿死**）。
[P2]：大任务 backfill / runner 锁定（matcher 是全局单点决策，天然可做）。

### 5.8 waiter / reservation 生命周期

| 事件 | 动作 |
|---|---|
| `AcquirePlacement` 首次调用 | upsert waiter（幂等键 `trial_id + attempt`） |
| matcher 授予 | 单事务：写 reservation + waiter.granted=true + Signal |
| attempt 结束（成功/失败/cancel/CAN） | `ReleasePlacement(trial, attempt)` 幂等删 waiter + reservation |
| 兜底 reaper（10min 周期） | **同时扫 waiters 与 reservations**：对应 workflow 已关闭 → 删 |

> 这里有两个容易漏的坑：(a) 若 defer 注册在授予之后，cancel 路径会留下幽灵 waiter，matcher 之后仍会给它授予资源；(b) 若 reaper 只对账 reservation，一个还没拿到 reservation 的幽灵 waiter 不在扫描范围内，要等它被授予后才进入视野 —— experiment 批量 cancel 时就是几十上百个幽灵在抢资源。「defer 前置 + reaper 扫两张表」同时堵住这两条。

### 5.9 读模型

- 写入由 workflow 内的 server activity 完成（at-least-once + 幂等 upsert by `(trial, attempt)`）
- **[P1] reconciler**：每 5min 用 Temporal visibility（按 `ExperimentId` / `State` search attributes `ListWorkflow`）对账修正漂移
- `events` 表继续由各 activity append（**业务语义事件**）；纯执行细节（每次重试、每个 timeout）不再重复记录 —— Temporal Web UI 就是 timeline 的 ground truth
- **注意 30 天边界**：namespace retention 30d，之后 history 归档。**超过 30 天后 PG 读模型是唯一可查询事实源**，reconciler 只覆盖 30 天窗口。因此凡是需要长期留存的信息（结果、失败归因、关键时间点）必须落 PG，不能只留在 history 里

---

## 6. Runner Agent

### 6.1 进程结构

```text
Runner Agent（Go 单二进制，systemd 托管）
├── temporal activity worker   queue=runner.<id>，DisableWorkflowWorker=true
│     └── FetchCase / PrepareEnv / RunAgent / RunVerifier / CollectArtifacts / CleanupTrial
├── in-use registry   trial 级资源登记（Housekeeper 与 drain 共用）
├── CacheScanner      5min 扫描 image / build cache → REST 上报 [P1]
├── Housekeeper       磁盘监控与分级清理（§6.8）
└── Sanitizer         库形式被 PostProcess 与各 activity 调用（§6.4 / §8.5）
```

```yaml
# runner.yaml
runner:
  id: runner-01
  server: https://orchestrator.internal
  temporal: temporal.internal:7233
  token: ${RUNNER_TOKEN}
  heartbeat_interval: 15s            # 唯一的间隔配置（没有独立的 poll 间隔）
  max_concurrent_trials: 4
  docker_host: unix:///var/run/docker.sock   # 必须显式绑定，见 §6.8
  workdir: /data/rollout-man
  resources: {cpu: 32, memory: 128Gi, disk: 1.8Ti, gpu: 0}
  capabilities: {docker: true, rootless: false, arch: amd64}
  sanitizer:
    redact_ips: false                # 默认关闭，见 §8.5
    ip_allowlist: ["127.0.0.1", "::1"]
    extra_patterns: []
housekeeping: {...}                  # 见 §6.8
```

真正的 trial 并发由 placement 记账限制；worker 的 `MaxConcurrentActivityExecutionSize = max_concurrent_trials × 3` 只是防御性上限（一个 trial 同时只有一个 activity 在跑，系数 3 已宽裕，且要给 disconnected 的 cleanup 留槽位）。

### 6.2 Activity 幂等与恢复语义

所有 runner activity 以 `(trial_id, attempt)` 为幂等键，本地状态在 `workdir/{trial_id}/`，容器打 label `rollout-man.trial_id` / `rollout-man.attempt`。

| Activity | 幂等 / 恢复行为 |
|---|---|
| `FetchCase` | CAS 目录按 sha256 命中即返回；下载写临时文件后原子 rename |
| `PrepareEnv` | harbor build 天然增量；重入前检查 image 是否已存在 |
| `RunAgent` | **重入对账**：label 匹配且 attempt 相同的容器仍在运行 → 直接接管监控（续 heartbeat），不新起容器；attempt 不匹配 → kill 旧容器后启动 |
| `RunVerifier` | 同上模式；结果写 `workdir/{trial}/verify-{attempt}.json` 后原子 rename |
| `CollectArtifacts` | 只落 `workdir/{trial}/out/`，不直接上传；写临时文件后原子 rename |
| `PostProcess` | 从 `out/` 重新执行清洗 → 打包 → 上传，全量覆盖写，不复用上次中间态（§6.4） |
| `CleanupTrial` | 目标态操作（容器不存在 = 成功）；失败仅记 event 不阻塞，由 Housekeeper 兜底 |

**`RunAgent` 的重试配置是重入接管能否生效的前提**（§7.4）：

> 这一条容易配错且后果隐蔽。若给 `RunAgent` 配同机 `MaxAttempts=1`：worker 进程消失 → heartbeat 超时 → activity 失败 → **没有同机重投** → 穿透到 workflow 外层 → attempt+1 → attempt 号不再匹配 → 走"kill 旧容器重来"分支。**"attempt 相同"这个条件永远不成立，接管分支成为死代码。**
>
> 实际后果：**Runner Agent 的任何一次正常重启（版本升级、配置变更、systemd restart、OOM kill）都会作废其上所有在跑 trial 的全部进度** —— 每个最多浪费 `timeouts.agent`（默认 30min）的执行时间和相应的 LLM token 花费。
>
> 因此 `RunAgent` 配 `MaximumAttempts: 3` + `NonRetryableErrorTypes` 显式列出所有真实 agent 失败码，让 **heartbeat 超时能在同一个 `runner.<id>` 队列上重投**，重投的 activity 才走得到接管分支。前提是 §7.2 的错误映射先做对，否则区分不了哪种超时该同机重试。

`RunAgent` 是唯一的长 activity：每 15s `activity.RecordHeartbeat(ctx, progress)`，三个作用叠在同一机制上——**存活证明**（`HeartbeatTimeout=60s`，取代 orphan 判定）、**cancel 通道**（Temporal 经 heartbeat 响应下发取消，收到后 kill 容器，取代"下发 kill 指令"）、**断点信息**（重入接管时从 heartbeat detail 恢复监控基线）。

**heartbeat 被拒的处理**：若 `RecordHeartbeat` 返回 activity 已不存在 / 已取消，activity 必须**立即 kill 容器并退出**，不得继续执行。否则 Runner 与 Temporal 短暂失联后会留下一个谁都不认领、Housekeeper 又因 in-use 登记而不敢删的容器。

### 6.3 Trial 执行链

```text
0. placement 授予 → 登记 in-use registry（trial 级）
1. FetchCase       CAS 命中 → 复用；未命中 → 下载 → 校验 sha256 → 解压
2. PrepareEnv      harbor 构建/拉取 image           失败 → IMAGE_BUILD_FAILED
3. RunAgent        启动容器 → 监控 timeout/OOM/输出上限；采集 cpu/mem 峰值、disk delta
                                                    失败 → CONTAINER_START_FAILED / AGENT_*
4. RunVerifier     → reward（无论高低）             失败 → VERIFIER_*
5. CollectArtifacts 收集 stdout/stderr/agent/verifier log + traj.jsonl + result.json
                    → 落 workdir/{trial}/out/
6. PostProcess     清洗（key 强制 / IP 分档）→ 打包 → 上传或暂存 → 注册 artifact（§6.4 / §6.5）
                                                失败 → POSTPROCESS_FAILED
7. RecordTrialCompleted（server activity）
8. CleanupTrial（defer）停容器、按 outcome 保留 workdir → 注销 in-use registry
```

失败归因由 activity 就地判定并映射到 Failure Taxonomy；判不出来的用 `SYSTEM/UNKNOWN`，**绝不吞掉原始错误信息**（message 保留截断并脱敏后的原始 stderr）。

### 6.4 后置清理（PostProcess）

Trial 的产物**不是跑完就能直接用**：agent 的 trajectory 里几乎必然含有 LLM key（它自己就在 prompt 里）、宿主与容器 IP、内部服务地址；而这些产物又恰恰是最需要对外分发的东西（交给别的团队分析、作为数据集发布、附在报告里）。因此在 `CollectArtifacts` 之后、上传之前插入独立的 `PostProcess` 步骤。

**拆成独立 activity 而不是塞进 collect**，是为了让清洗失败能单独归因并**阻断上传** —— 清洗没成功就把原始 traj 传上对象存储，是不可逆的泄漏。

```text
5. CollectArtifacts   收集到 workdir/{trial}/out/：
                        stdout.log  stderr.log  agent.log  verifier.log
                        traj.jsonl（agent ↔ LLM 的完整交互）
                        result.json
6. PostProcess        6a redact    按产物分档清洗（下表）
                      6b bundle    tar + zstd → bundle.tar.zst
                      6c upload    bundle + 便查用单文件都上传
                      6d register  artifacts 行 + sha256（均为清洗后内容）
```

**分档清洗**（一刀切两边都不对）：

| 产物 | key 脱敏 | IP 脱敏 | 进 bundle | 单文件上传 | 依据 |
|---|---|---|---|---|---|
| `traj.jsonl` | **是** | **是** | 是 | 否（通常很大） | 对外分发的主体，也是 key 最可能出现的地方 |
| `result.json` | **是** | **是** | 是 | 是（小，CLI 常读） | 同上，且会进结果聚合 |
| `agent.log` / `stdout.log` / `stderr.log` | **是** | 否（默认，可开） | 是 | 是 | 主要给自己 debug，脱 IP 会毁掉排查价值 |
| `verifier.log` | **是** | 否 | 是 | 是 | 同上 |

**key 脱敏在所有档位上都是强制的，不可关闭**；IP 脱敏按"是否对外分发"分档 —— 这比一个全局 `redact_ips` 开关合理得多，因为分发件和排查件的诉求本来就相反（§8.5）。

**打包**：`bundle.tar.zst` 是该 attempt 的完整产物快照，便于一次性下载与长期归档；同时把小文件单独上传，让 `rollout-man trial logs` 不必为看一行日志下载整个包。两者内容一致，bundle 的 sha256 单独记录。

**失败处置**：

| 子步骤 | 失败后果 |
|---|---|
| `redact` | **阻断**：activity 失败 → 同机重试 → 仍失败则 trial `FAILED(SYSTEM/POSTPROCESS_FAILED)`。绝不允许"清洗失败但照样上传" |
| `bundle` | **降级**：记 event，跳过打包只上传单文件，trial 仍可 `COMPLETED` |
| `upload` | 同机重试（`MaxAttempts=3`）；仍失败则 trial `FAILED` |

**幂等**：以 `(trial_id, attempt)` 为键，产物路径确定（§9.3），覆盖写。重入时从 `workdir/{trial}/out/` 重新执行全部子步骤，不复用上一次的中间态 —— 这也意味着 `CollectArtifacts` 必须把原始产物完整留在 workdir 里，直到 `CleanupTrial` 才清除。

### 6.5 上传时机：逐 trial 还是成批

上传逐 trial 还是成批，**由 `upload` 步骤写在哪个块决定**（§11.2），不需要额外的开关：

| 写法 | 行为 | 外部命令调用次数 |
|---|---|---|
| `per_trial: [..., upload]` | 每个 trial 结束就传走；传完本地即可按 retention 回收 | O(trials × objects) |
| `per_trial: [..., stage]` + `per_experiment: [upload, ...]` | `stage` 只把产物移进本机 staging 目录并登记；`per_experiment` 的 `upload` 让**每台 Runner 把自己 staging 里的全部产物打成一个包传一次** | O(runners) |

500 trials × 2 个对象 = 1000 次命令调用，成批之后是 3 次。对按次限流的后端（§9.4）这是数量级的差别。

**但成批不是免费的**，下面四条必须一起实现，缺一条就会在某个场景下丢产物：

1. **staging 目录进 in-use registry**（§6.8）。它不属于任何还在跑的 trial，Housekeeper 会按 `workdir_success: 1h` 把它当过期 workdir 回收——**产物在等待上传的过程中被自己的清理逻辑删掉**，而且删得完全合理。必须显式钉住。
2. **staging 占用进 placement 的磁盘记账**（§5.3）。一个 500 trials 的 experiment 会在每台 Runner 上堆几十 GB 直到结束；placement 若不知道，会继续往这台机器上派活直到盘满。
3. **`stage.max_pending` 是硬约束**（默认 20Gi/Runner），超过就地提前传一次。"成批"不能变成"无上限堆积"——上限存在的意义是让最坏情况可算。
4. **drain 与 cancel 都要先把 staging 传走**。§6.7 的 drain 收敛条件加一条「staging 已清空」；experiment cancel 按 `stage.on_cancel` 决定传还是丢，默认传——用户取消之后产物既没上传又被清理掉，是最糟的结果。

**固有代价，写清楚不藏着**：Runner 非正常下线（掉电、磁盘故障）时，其 staging 里未上传的产物**就是丢了**。trial 的 reward 和归因在 PG 里还在（那些是 trial 结束时就写的），丢的是日志与 traj。这类 experiment 在结果表上标 `ARTIFACTS_INCOMPLETE` 并列出受影响的 trial —— 不能让人以为产物齐全。要绝对不丢就把 `upload` 放回 `per_trial`，这是两种写法真正的取舍点。

**`per_experiment` 是 ExperimentWorkflow 的一部分**，不是外部脚本：其中的 `upload` 是投到每台参与过的 `runner.<id>` 队列的 activity（因此和 trial 一样有超时、重试、失败归因），`report` / `deploy` 是 server 侧 activity。整块走完 experiment 才算 `COMPLETED`。

### 6.6 心跳与健康

REST 心跳（15s）仍然保留，但**只服务 placement**，不再承担派发：

```json
{
  "runner_id": "runner-01",
  "status": "healthy",                       // healthy | degraded | draining
  "resources": {"cpu_used": 18, "memory_used": "72Gi", "disk_free": "610Gi"},
  "running_trials": ["trial_10021", "trial_10022"],
  "docker": {"healthy": true},
  "cache": {"images": [...], "build_cache": [...]}   // [P1]
}
```

响应携带：`drain` 指令、`pinned_images` [P1]、配置更新。

健康判定取两个信号的并：placement 用 REST 心跳；Temporal `DescribeTaskQueue` 的 poller 存在性作为运维观测。**不再有全局 orphan 扫描** —— 失联的表现就是其上 `RunAgent` 的 heartbeat timeout。

### 6.7 drain 与 EMERGENCY 自保

**drain 必须按 trial 收敛，不能按 activity 收敛。**

```text
1. Server 标记 runner = DRAINING → placement 立即停止向该机授予
2. Runner 收到（心跳响应）→ 停止接受新 trial，但 worker 保持运行
3. 等待 in-use registry 清空（本机没有任何 trial 还在编排链上），**且 staging 已 flush**（§6.5）
4. worker.Stop() → 安全下线
```

> 收敛粒度不能取 activity，因为 **pin 到 runner 的单位是 trial**：`RunAgent` 结束后还有 `RunVerifier` → `CollectArtifacts` → `CleanupTrial` 要投到同一个 `runner.<id>` 队列。若在"无 activity 在跑"时就停 worker，这些 activity 没有 poller，触发 `ScheduleToStartTimeout` → 整个 trial 换机重跑，刚跑完的 30 分钟 agent 执行全部作废。
>
> EMERGENCY 自保走同一条路径时问题更尖锐：**磁盘快满的时候把所有在跑的 trial 作废，然后让它们去别的机器重跑一遍** —— 正好在最需要省资源的时刻浪费最多资源。

**[MVP]** 基础 drain（停止授予 + 等 trial 收敛 + 停 worker）。这在 in-use registry（Housekeeper 的 MVP 项）之上只差一个条件判断，而 MVP 期间一定会有 Runner 升级需求 —— 没有它，每次升级都在赌当时有没有 trial 在跑。
**[P1]** EMERGENCY 自动 drain、DRAINING 状态机细化、`pinned_images` 下发。

### 6.8 Housekeeper

**纯本地 ticker，不经 Temporal。** Housekeeper 是自保机制，必须在 Temporal / Server 都不可用时照常工作（磁盘不会因为控制面停机就停止增长）。清理动作单机、可重入、无跨机事务，不需要 durable execution。

```yaml
housekeeping:
  enabled: true
  disk:
    check_interval: 30m
    thresholds: {warning: 80%, cleanup: 85%, aggressive: 92%, emergency: 95%}
  retention:
    workdir_success: 1h
    workdir_failed: 24h
    image_unused: 72h
    build_cache_unused: 48h
  cas_cache:                       # [P1]
    max_size: 200GB                # 与磁盘 15% 取小者
```

| 级别 | 触发 | 动作 | 批次 |
|------|------|------|---|
| WARNING (≥80%) | 记 event、metrics 告警 | 不清理 | [MVP] |
| CLEANUP (≥85%) | 常规清理 | dangling image、stopped container（本系统创建的）、过期 build cache、过期 workdir | [MVP] |
| AGGRESSIVE (≥92%) | 深度清理 | 全部 reclaimable build cache、所有"未被引用且超过 retention"的 image | [P1] |
| EMERGENCY (≥95%) | 自保 | 上报 `status=degraded` → placement 硬约束自动停止授予；本地 emergency cleanup；仍不恢复 → 主动 drain | [P1] |

**NEVER DELETE 清单** [MVP]。禁止 `docker system prune -af`。删除前构建引用集合：

```text
protected = {
    正在运行的容器及其 image（含所有父层）,
    in-use registry 中登记的 image / volume / workdir,
    Server 下发的 pinned_images [P1],
    now - last_used < image_unused retention 的 image,      ← 注意方向
    非本系统创建的资源（无 rollout-man.* label 的容器/volume 默认不动）,
}
删除对象 = 候选集合 - protected
逐项执行并记 event（删了什么、释放多少），不用批量 prune。
```

> 注意这一行的方向：受保护的是**最近用过**的 image。写成 `last_used < retention` 就正好反了——热镜像被删、冷镜像永久保留，症状是"莫名其妙的 cold start 变多"，极难排查。

**in-use registry**：每个 trial 在 placement 授予时登记 `(trial_id, attempt) → {workdir, image refs, volumes}`，`CleanupTrial` 完成时注销。Housekeeper 构建 protected 快照时取读锁——"先登记后使用、先快照后删除"保证与执行中的 activity 无竞态；另加 **10min deletion grace**（近期被登记过的资源不动）覆盖登记间隙。worker 重启后 registry 由 docker label 扫描重建，与 §6.2 的重入对账共用同一机制。

**workdir retention 不依赖 Server 状态**：`CleanupTrial` 终结时在 `workdir/{trial_id}/manifest.json` 写入 outcome 与时间戳，Housekeeper 只读 manifest —— 控制面不可用时 retention 照常生效。

**Docker Context 隔离** [MVP]：Housekeeper 与各 activity 必须使用同一个显式配置的 `DOCKER_HOST`（root 与 rootless Docker 并存的机器上尤为重要）。启动时校验 `docker info` 的 `SecurityOptions` / root 目录与配置预期一致，**不一致直接拒绝启动**，避免误清理另一套 Docker。

**[P1]** CAS 本地缓存纳入治理：LRU + 容量上限，in-use registry 引用的 sha256 除外。没有这一条，`reported_free` 会被慢慢吃到零。

---

## 7. 失败分类与重试

### 7.1 Failure Taxonomy

状态与失败分离：

```yaml
state: FAILED
failure:
  category: INFRASTRUCTURE
  code: DOCKER_ERROR
  message: "failed to create container: ..."     # 已脱敏并截断至 4096
  retryable: true
  runner_id: runner-02
```

枚举固化在代码（`internal/failurecode`）与 DB check constraint 中：

| Category | Codes | 默认 retryable | 换机 |
|----------|-------|----------------|---|
| `AGENT` | AGENT_TIMEOUT, AGENT_CRASH, AGENT_OOM, AGENT_EXIT_NONZERO, AGENT_OUTPUT_LIMIT | 仅 AGENT_OOM | AGENT_OOM |
| `ENVIRONMENT` | IMAGE_BUILD_FAILED, CONTAINER_START_FAILED, NETWORK_ERROR, DEPENDENCY_INSTALL_FAILED, ENVIRONMENT_TIMEOUT | NETWORK_ERROR / CONTAINER_START_FAILED | 是 |
| `INFRASTRUCTURE` | DOCKER_ERROR, DISK_FULL, MEMORY_PRESSURE, RUNNER_UNAVAILABLE, HOST_ERROR, OBJECT_STORE_ERROR | 全部 | 是，`OBJECT_STORE_ERROR` 除外 |
| `VERIFIER` | VERIFIER_ERROR, VERIFIER_TIMEOUT, INVALID_REWARD | 仅 ERROR / TIMEOUT | 是 |
| `SYSTEM` | CANCELLED, UNPLACED, POSTPROCESS_FAILED, INTERNAL_ERROR, UNKNOWN | 仅 INTERNAL_ERROR（一次） | 否 |

原则：**evaluation failure 不 retry；infrastructure failure retry 且优先换 Runner**。

三条容易搞错的约定：

- **`TEST_FAILED` 不在 taxonomy 里**。agent 没解决问题是**正常的测量结果**（reward 低），不是"没测出来"。verifier 正常跑完就是 `COMPLETED`，reward 高低是事实不是失败
- **`DOCKER_ERROR` 是唯一写法**（不存在 `DOCKER_DAEMON_ERROR`；上面的枚举表是唯一来源）
- **`UNPLACED`**：排队超时或永久性不可满足（§5.6），结果表单独成列
- **`POSTPROCESS_FAILED`**：产物清洗失败。清洗重试不成功就必须让 trial 失败——宁可丢一个 trial，也不能把未清洗的 traj 传出去（§6.4）
- **`OBJECT_STORE_ERROR` 可重试但不换机**：存储命令失败通常是后端或凭证的问题，换一台 Runner 一样失败（§9.4）

### 7.2 Temporal 错误 → failure code 映射【关键】

**这一节缺失或写错，系统会在没有任何告警的情况下产出错误的评测结论。**

问题在于 activity 失败远不止 `ApplicationError`，而 `RunAgent` 的 `StartToCloseTimeout` **就等于** `timeouts.agent`：

```text
StartToClose 超时  = agent 自己磨蹭到超时   → 这是被测对象的能力问题
Heartbeat  超时    = Runner 进程/主机失联   → 这是我们的基础设施问题
```

两者在 SDK 里**都是 `*temporal.TimeoutError`**，只有 `TimeoutType()` 不同。如果 `FromError` 只 unwrap 到 `ApplicationError`、其余归到 `SYSTEM/UNKNOWN`（或更糟，按 activity 名兜底归到 AGENT），**每一次 Runner 崩溃都会被记成一次 agent 超时**。trial 有终态、有 code、结果表有数字，只是数字是错的——而这个系统存在的唯一目的就是产出这些数字。

**解包顺序（单一实现，带单测）：**

```text
err → *ActivityError → Unwrap() → 依次判定：
   *ApplicationError  → Type() 即 code
   *TimeoutError      → 查下表
   *CanceledError     → 见 §7.5
   其余                → SYSTEM/INTERNAL_ERROR
禁止 default 到任何 AGENT 类 code。
```

| Activity | TimeoutType | code | retryable | 加入 exclude |
|---|---|---|---|---|
| `RunAgent` | `StartToClose` | `AGENT_TIMEOUT` | 否 | — |
| `RunAgent` | `Heartbeat` | `RUNNER_UNAVAILABLE` | 是 | **是** |
| `RunVerifier` | `StartToClose` | `VERIFIER_TIMEOUT` | 是 | 否 |
| `PrepareEnv` | `StartToClose` | `ENVIRONMENT_TIMEOUT` | 否 | — |
| `FetchCase` / `CollectArtifacts` | `StartToClose` | `INFRASTRUCTURE/HOST_ERROR` | 是 | 是 |
| 任意 | `ScheduleToStart` | `RUNNER_UNAVAILABLE` | 是 | **否**（见下） |

**`ScheduleToStart` 超时不加入 exclude**：它既可能是"runner 挂了"，也可能是"runner 只是忙、activity 槽位排满"。若一律拉黑，一台负载高的机器会被所有 trial 逐个排除，最后无机可用。让 placement 的负载评分自然避开即可。

### 7.3 两级重试

| 层 | 处理的错误 | 机制 |
|---|---|---|
| **Activity RetryPolicy（同机）** | 瞬时抖动：上传失败、registry 超时、docker api 瞬断；**以及 `RunAgent` 的 heartbeat 超时（用于触发重入接管）** | 每步独立 `RetryPolicy{MaximumAttempts, NonRetryableErrorTypes}`；不可同机恢复的 code 全部列入 `NonRetryableErrorTypes`，立即穿透 |
| **TrialWorkflow attempt 循环（换机）** | INFRASTRUCTURE 全类、CONTAINER_START_FAILED、AGENT_OOM、VERIFIER_ERROR、INTERNAL_ERROR | `Retry.Retryable(code)`（experiment 可覆盖）+ `exclude` runner + backoff |

```yaml
retry_policy:
  max_total_attempts: 3           # 含首次（名字里带 total，消除"最多重试 N 次"的歧义）
  backoff: {initial: 30s, multiplier: 2, max: 10m}    # 必须确定性，见 §4.5
  retry_on: [DOCKER_ERROR, RUNNER_UNAVAILABLE, NETWORK_ERROR, AGENT_OOM,
             CONTAINER_START_FAILED, VERIFIER_ERROR, INTERNAL_ERROR]
  avoid_same_runner: true
```

**换机重试不产生新的 trial 行**，attempt 是 trial 内部的循环计数；`trials` 表的唯一键是 `(task_id, trial_index, attempt)`（见 §9.1）。

**[P1]** Experiment 级覆盖 retry_policy、按 code 差异化 backoff。

### 7.4 超时与重试配置表

| 步骤 | ScheduleToStart | StartToClose | Heartbeat | 同机 MaxAttempts |
|---|---|---|---|---|
| `AcquirePlacement` | — | 10s | — | 1（workflow 循环负责） |
| `FetchCase` | **5m** | 15m | 30s | 3 |
| `PrepareEnv` | **5m** | timeouts.build（默认 20m） | 60s | 1 |
| `RunAgent` | **5m** | timeouts.agent | 60s | **3**（见 §6.2） |
| `RunVerifier` | **5m** | timeouts.verifier | 60s | 2 |
| `CollectArtifacts` | **5m** | 15m | 30s | 3 |
| `PostProcess` | **5m** | 30m | 60s | 3 |
| `CleanupTrial` | **30s** | 60s | — | **1** |

**`ScheduleToStartTimeout` 不可省略**，它替代的正是手写 lease / 过期回收。缺了它后果很具体：placement 把 trial 授予 runner-02 → runner-02 的 worker 此刻已经挂了（进程死了，不再 long-poll）→ `FetchCase` 永远停在 Scheduled 状态（Temporal 默认 ScheduleToStart 不限时）→ **reservation 一直占着，trial 永远不动，也不会换机**。

**`CleanupTrial` 单独配短超时 + `MaxAttempts=1`。** defer 里的 `.Get()` 是阻塞的，而 **Runner 失联恰恰是最常见的 cleanup 失败场景** —— 若给它配 `StartToClose=5m, MaxAttempts=3`，每次换机重试都要先在 cleanup 上耗掉最多 15 分钟才能开始下一次 attempt，与"cleanup 失败仅记 event 不阻塞"的意图自相矛盾。因此：runner 不在就 30s 立刻失败，只记 event，由 Housekeeper 兜底（它本来就有 label 对账和 retention）。整个 defer 块另有 90s 上限（§4.3）。

### 7.5 三种"提前结束"的区分

`total timeout`、`queue timeout`、用户 cancel 在控制流上都表现为提前退出，但归因不同，**必须能区分**：

| 场景 | 实现 | 归因 | 读模型终态 |
|---|---|---|---|
| 用户 cancel | `client.CancelWorkflow` → ctx cancel | `SYSTEM/CANCELLED` | CANCELLED |
| total timeout | workflow 内 `NewTimer(timeouts.total)` + CancellationScope 主动 cancel 步骤链 | `ENVIRONMENT/ENVIRONMENT_TIMEOUT` | FAILED |
| queue timeout | placement 循环内比较 `workflow.Now()` 与 deadline | `SYSTEM/UNPLACED` + blocked_reason | FAILED |

实现要点：total timeout 触发时先置一个 workflow 内的标志位，再 cancel scope；`CanceledError` 到达 attempt 循环时查该标志位决定归因。**不能只看 `IsCanceledError`** —— 那样用户取消和超时会被记成同一件事。

`total timeout` 的作用域是**单个 attempt**（不含换机重试的总时长），因为它来自 `task.toml`，描述的是"这个 case 跑一遍要多久"。跨 attempt 的总上限由 `max_total_attempts` 间接约束。

---

# 第三部分 · 安全

> 谁可信、secret 走哪条路、什么东西绝不能落到哪里。

## 8. 安全与信任边界

### 8.1 信任边界

```text
用户 ──user token──> API Server ──┬── PostgreSQL（app db + temporal db）
                                  ├── Temporal Server（内网，不对外暴露）
                                  └── Object Storage
Runner Agent ──runner token/mTLS──> API Server
             ──出方向 gRPC───────> Temporal frontend :7233
```

- **Runner 是可信组件**：它持有 trial 的明文 secret（必须，否则无法注入容器）、能操作本机 Docker。攻破一台 Runner 等于拿到其上正在跑的 trial 的 LLM key。因此 Runner 机器的准入等同于生产主机
- **被测 Agent 容器是不可信的**：它可能故意把 key 往输出里写（这正是 Sanitizer 精确匹配层要覆盖的），也可能试图访问宿主
- **Temporal Server 不对外暴露**，只接受 Runner 的出方向连接
- **外部存储 / VCS 凭证不在信任边界内**：GitHub、OneDrive 等的凭证由 Runner 主机自行持有（`rclone.conf`、`~/.ssh`、`gh auth` 等），本系统不读取、不存储、不下发（§9.4）。系统只管理两样东西：runner token 与 LLM api_key

### 8.2 Secret 与 Temporal History

Temporal 会把 workflow / activity 的输入输出、heartbeat、error message **全部持久化进 event history**，而 history 里的明文**无法事后清除**（只能等 retention 过期或换 namespace）。这与"日志里泄漏了删掉就好"性质不同。

**MVP 的取舍**：单团队内网、Runner 等同生产主机（§8.1），因此 MVP **不做一次性 token、不做吊销、不做加密 Data Converter**。但保留一条几乎零成本的底线 —— **key 不作为 workflow / activity 的入参**：

```text
1. TrialWorkflow 只携带 llm_spec_name
2. RunAgent 从 Server 取该 spec 的 base_url / model / api_key_env|api_key_cmd
   ——注意取到的是**取 key 的方式**，不是 key
3. Runner 按该方式在本机解析出 key（读环境变量或跑命令）→ 只注入容器 env
```

**系统从头到尾没有持有过这个 key**（§3.3）：它存在于 Runner 主机的环境或密码管理器里，Server 只知道该去哪里找。这比"Server 保管 key 再下发"少一整套东西——加密后端、一次性 token、TTL、吊销——同时避开了一个**不可逆**的坑：一旦某个版本误把 key 写进 activity 入参，那批 history 里的明文要等 30 天 retention 过期才消失，期间任何能打开 Temporal Web UI 的人都看得到。

**输出侧的 key 清洗不在此列，MVP 就要做全**（§6.4 / §8.5）—— 被测 agent 会把 key 写进 traj 和日志，而那些产物是要对外分发的。

**[P1] 收紧为一次性 token**：`IssueTrialToken` 签发绑定 `trial_id + attempt + spec` 的短期 token（TTL = total timeout + 余量），Runner 拿 token 换三要素。收益不只是保密，更重要的是**吊销能力** —— 双跑窗口出现时，新 attempt 签发即吊销旧 token，旧容器的 LLM 调用被拒，烧钱止血（§14.3）。

**[P1] 双保险**：namespace 启用加密 Data Converter（AES-GCM PayloadCodec，Web UI 配 codec server）。

**对象存储访问 [MVP]**：history 里**只放对象键 + sha256，不放任何预授权 URL** —— 那种 URL 里的 token 就是一份临时凭证，把它当 activity 入参等于把凭证写进 history。Runner 拿到键之后自己跑 `storage_download` 命令（§9.4），该命令用的是宿主自己的凭证，本系统既不持有也不传递它。这一条零成本：传键比传 URL 还短。

**工程约束**：
- error message / heartbeat detail 在 Runner 侧统一过 Sanitizer，杜绝 stderr 里的 key 经 error 链进入 history
- code review checklist：**activity 入参里任何 URL 型字段必须确认不带签名** —— 光看类型名抓不到 presigned URL

### 8.3 凭证生命周期

| 凭证 | 签发 | 有效期 | 轮换 | 吊销 |
|---|---|---|---|---|
| user token | 管理员 | 长期 | 手动 | 手动 |
| runner token | 注册 Runner 时由管理员签发，写入 systemd 环境文件 | 长期 | [P1] `runner rotate-token` | `runner disable` 立即失效 |
| LLM api_key | **不由本系统签发或保管**（§3.3） | — | 由持有它的一方负责 | — |
| 外部存储 / VCS 凭证 | 同上，属于 Runner 主机 | — | — | — |

系统实际管理的凭证只有两类：user token 与 runner token。LLM key 与外部存储凭证都只有"去哪里取"的描述留在系统里，值本身从不经过它——因此也没有轮换与吊销的设计负担。代价是双跑窗口（§14.3）无法靠吊销 token 止血，只能靠同机重投接管来收窄（§6.2）。

### 8.4 鉴权 [MVP]

只有 user / runner 两类 token 是不够的，那等于**没有授权模型** —— 谁能 cancel 别人的 experiment？谁能删 llm-spec？"不做多租户"不等于"不做鉴权"。

MVP 的最小可用集：

- 所有写操作记录 `created_by`
- **删除 / cancel 类操作限制为资源创建者或管理员**
- `llm_specs` 的 key 永不回显；有引用的 spec 拒绝删除（409）
- Runner API 只接受 runner token，且 token 绑定 runner_id（不能冒充其他 Runner 上报）

[P1]：细化角色（viewer / operator / admin）。

### 8.5 Sanitizer

所有**离开 Runner 的文本** —— artifact 文件、`failure.message`、event payload、heartbeat detail —— 在上传/上报前统一经过 Sanitizer。Object Storage 与 DB 中不落任何明文 secret；脱敏前的原始内容不在本地保留副本。

| 层 | 规则 | 替换为 | 批次 |
|----|------|--------|---|
| **精确匹配** | 本 trial 已下发的 LLM api_key、runner token 等 Runner 持有明文的 secret，**含 base64 / URL-encoded 变体** | `***REDACTED_KEY***` | [MVP] |
| **模式匹配** | 常见 key 形态（`sk-`、`AKIA`、`ghp_` 等前缀）、`Authorization` / `X-Api-Key` header 值、**预授权 URL 的凭证参数**（`tempauth=`、`authkey=`、`X-Amz-Signature`、`Signature=`）、`Bearer` / `access_token` 串 | `***REDACTED_KEY***` | [MVP] |
| IP 脱敏 | IPv4 / IPv6 字面量，`ip_allowlist` 除外 | `***REDACTED_IP***` | **[P2]，默认关闭** |

- **精确匹配层是主要防线**：Runner 明确知道自己给本 trial 下发过哪些 secret，可做零误报的全量替换
- 脱敏在流式上传路径上按行执行，保留跨行滑动窗口以覆盖被换行截断的 secret，避免大日志整体载入内存
- artifact 记录的 `sha256` 是**脱敏后**内容的摘要（下载校验一致）
- 每次命中记 metrics（层级 / 次数，不记原文）。**某 trial 精确匹配层命中次数异常本身就是有价值的信号** —— 说明被测 agent 在往输出里泄漏 key，应在结果分析中标记 [P1]

**IP 脱敏按产物分档，不用一个全局开关**（分档规则见 §6.4）：

- **分发件（`traj.jsonl` / `result.json`）默认脱 IP**：它们会离开团队，里面的 IP 是内网拓扑信息
- **排查件（各类 `.log`）默认不脱**：evaluation 场景下日志里的 IP 多半是容器网络地址、目标服务地址，是排查环境问题的关键信息；而且 IPv4 正则会误伤版本号（`1.2.3.4`）、坐标、hash 前缀、docker 输出里的各种四段数字 —— 这些恰恰是 evaluation 日志里的高频内容，脱完满屏 `***REDACTED_IP***` 且无从还原

runner 级的 `redact_ips: true` 是兜底开关（默认 `false`），打开后排查件也一并纳入，供合规部署使用。

**key 脱敏没有分档，也没有开关** —— 所有离开 Runner 的文本一律执行精确匹配 + 模式匹配两层。

---

# 第四部分 · 数据、接口与结果

> 状态存在哪、对象存在哪、外部怎么访问、结果怎么读。

## 9. 数据模型

### 9.1 PostgreSQL

```text
projects        id, name, metadata(jsonb)
series          id, project_id, name, metadata
cases           id, project_id, series_id, name
case_versions   id, case_id, version, source(jsonb), sha256, size, artifact_key,
                state(PARSING|READY|ADMITTED|REJECTED|INVALID), parse_error,
                admission_result(jsonb), admitted_at, admission_experiment_id,
                task_config(jsonb)          -- 注册时解析 task.toml：resources / timeouts
agents          id, name, type(llm|builtin), requires_llm, version, runtime, command, parameters
llm_specs       name PRIMARY KEY, provider, base_url, model,
                api_key_env, api_key_cmd(jsonb),   -- 取 key 的方式，不是 key 本身
                max_concurrent, parameters(jsonb), updated_at
experiments     id, name, config(jsonb), state,
                created_by, created_at, confirmed_at
tasks           id, experiment_id, case_version_id, agent_id, llm_spec_id,   -- builtin 为 NULL
                state(PENDING|RUNNING|COMPLETED|CANCELLED),                  -- 无成败
                priority, requested_trials, resources(jsonb), scheduling(jsonb),
                created_at, started_at, finished_at
trials          id, task_id, trial_index, attempt, runner_id, state,
                reward, exit_code,
                failure_category, failure_code, failure_message,
                metrics(jsonb),             -- cpu_peak, mem_peak, disk_delta, duration
                first_queued_at,            -- aging 基准，跨 attempt 保持
                started_at, finished_at
placement_waiters  trial_id, attempt, resources(jsonb), priority, first_queued_at,
                   exclude(jsonb), affinity(jsonb), llm_spec_id,
                   granted, blocked_reason, updated_at
resource_reservations  trial_id, attempt, runner_id, cpu, memory, disk, created_at
trial_tokens    token_hash, trial_id, attempt, llm_spec_id, expires_at, revoked_at
events          id, task_id, trial_id, runner_id, type, timestamp, payload(jsonb)
runners         id, status, resources(jsonb), capabilities(jsonb),
                max_concurrent_trials, last_heartbeat, registered_at
runner_cache_state  runner_id, kind(image|build_cache), ref, series_id, size, last_used   -- [P1]
artifacts       id, task_id, trial_id, attempt,
                kind(bundle|traj|log|result), name, object_key, size, sha256,
                redaction(jsonb),           -- 各层命中次数，不含原文
                created_at
```

关键索引与约束：

```sql
UNIQUE (task_id, trial_index, attempt)          -- trials
UNIQUE (trial_id, attempt)                      -- placement_waiters / resource_reservations
INDEX  (state, priority DESC, first_queued_at)  -- placement_waiters 扫描
INDEX  (task_id, timestamp)                     -- events
INDEX  (series_id)                              -- runner_cache_state
```

> `trials` 的唯一键必须带 `trial_index`：`requested_trials = 10` 时同一 task 下 10 个 trial 的 attempt 都是 1，只用 `(task_id, attempt)` 会直接冲突。
>
> `not_before` / `effective_priority` / `blocking_reason` 放在 `placement_waiters` 而不是 `tasks` —— 它们是排队状态，不是 task 属性。

### 9.2 存储分工与 retention

| 数据 | 位置 | retention |
|------|------|---|
| 状态、注册表、事件、结果 | PostgreSQL | 永久（**30 天后是唯一可查询事实源**） |
| Workflow event history | Temporal（temporal db） | namespace 30d；**不归档到 OneDrive**（Temporal archival 不支持它），30 天后以 PG 读模型为准 |
| Case archives（CAS） | 对象存储 `objects/{sha[:2]}/{sha[2:4]}/{sha}` | **永不自动删除**（引用计数删除见 [P2]） |
| Trial 日志与产物 | 对象存储（§9.3） | **[MVP] 应用侧 GC 作业**：成功 trial 90d、失败 trial 180d，**删除后必须清空回收站**（§9.4） |

> 对象存储侧的 retention 最容易被漏掉：Runner 本地磁盘治理有整整一节，而 artifact 与 CAS object 若无人清理就只增不减，也没有 experiment 删除时的级联。跑一年之后这是一笔说不清的账。
>
> **OneDrive 没有 lifecycle 策略**，这件事不能靠配置解决，必须写一个服务端 GC 作业（按 `artifacts.created_at` + trial outcome 删除）。这是选 OneDrive 相对对象存储多出来的一块工作量，不要漏排。

### 9.3 Artifact 路径

```text
/rollout-man/experiments/experiment-{id}/task-{id}/trial-{id}/attempt-{n}/
  ├── bundle.tar.zst                        # 完整产物快照（含 traj.jsonl 与全部 log）
  └── result.json                           # [MVP] 只上传这两个，原因见 §9.4
      stdout.log  stderr.log  agent.log  verifier.log    # [P1] 单文件上传
```

PostgreSQL 只存对象键 + size + sha256 + kind。**所有对象都是 PostProcess 清洗后的内容**（§6.4），原始产物只存在于 Runner 的 workdir，随 retention 过期。**路径含 attempt**，使覆盖写天然幂等且不同 attempt 不互相覆盖。

**[MVP] 每个 attempt 只上传 2 个对象**（bundle + result.json）：每个对象是一次外部命令调用，500 trials × 6 个文件 = 3000 次调用，压到 1000 次在耗时和被限速的概率上差别都很大。`rollout-man trial logs` 下载 bundle 后本地解包并缓存；[P1] 再补单文件上传，让 `logs` 免于下整包。

### 9.4 外部存储：命令行适配器

**系统不内置任何存储 SDK。** 上传 / 下载 / 取链接 / 删除四个动作各配一条命令模板，由系统填参数、执行、看退出码。换后端只改配置不改代码——OneDrive、S3、内网 WebDAV、甚至一台 scp 目标机都是同一套接法。

理由很实际：这个规模（3 台 Runner、单团队）不值得为某个云的 SDK、鉴权、限流、分片协议再写一层适配，而 `rclone` 这类工具已经把这些做完了，顺带支持几十种后端。同样的做法适用于 Case 的 `git` 来源——那本来就是一条 `git clone`。

**连带的好处是凭证问题一起消失了**：命令自己去找它需要的凭证，本系统不读、不存、不下发 GitHub / OneDrive 的任何凭证，也就不用为它们设计轮换、吊销和泄漏面。

```yaml
# server.yaml / runner.yaml
commands:
  timeout: 30m
  max_attempts: 3

  # 每条命令二选一写法：run（argv 数组，用 {{.X}} 模板）或 script（sh 脚本，用环境变量）
  storage_upload:
    run: ["rclone", "copyto", "--", "{{.LocalPath}}", "onedrive:rollout-man/{{.Key}}"]

  storage_download:
    script: |
      set -euo pipefail
      rclone copyto -- "onedrive:rollout-man/${KEY}" "${LOCAL_PATH}"

  storage_link:
    run: ["rclone", "link", "--expire", "24h", "--", "onedrive:rollout-man/{{.Key}}"]

  storage_delete:
    run: ["rclone", "deletefile", "--", "onedrive:rollout-man/{{.Key}}"]

  source_git:                             # Case 的 git 来源同样是一条命令
    script: |
      set -euo pipefail
      git clone --depth 1 --branch "${GIT_REF}" "${GIT_REPO}" "${LOCAL_PATH}"
```

**契约**（实现只需遵守这几条）：

| 项 | 约定 |
|---|---|
| 变量传递 | `run` 形式用 `{{.Key}}` / `{{.LocalPath}}` 模板；`script` 形式用同名大写环境变量 `KEY` / `LOCAL_PATH`（git 来源另有 `GIT_REPO` / `GIT_REF`）。两种写法能力等价，脚本形式适合需要多步或条件判断的场合 |
| 成功判定 | 退出码 0；非 0 → `INFRASTRUCTURE/OBJECT_STORE_ERROR`（可重试，**不换机**——后端不通换台机器也不通） |
| `link` 输出 | stdout 第一行即 URL，`GET /trials/{id}/artifacts/{name}` 302 到它 |
| 凭证 | **本系统不管理外部存储 / VCS 凭证，也不读取、不存储、不下发**。命令自己去找它需要的东西——`rclone.conf`、`~/.ssh`、`gh auth`、宿主环境变量都行。这些凭证属于 Runner 主机，与系统的 runner token / LLM key 完全分开（§8.1） |
| 完整性 | **sha256 由系统在本地算**：上传前算、下载后验，不依赖后端返回的摘要（各家算法不一，商业版 OneDrive 返回的就不是 SHA-256） |
| 输出处理 | 命令的 stdout / stderr 过 Sanitizer 后记 event 便于排查；**不进 workflow history** |
| 执行位置 | `upload` / `download` 在 Runner 上执行；`link` / `delete` 在 Server 上执行 |

**放弃了什么**：拿不到后端的配额、限流细节和服务端校验和。换来的是零 SDK 代码与换后端零改动——MVP 规模下这笔交易划算。真需要配额监控时再加一条可选的 `stat` 命令即可。

**[P1] 任意 exec 步骤**：三个块都支持 `- step: exec, run: [...]`，用同一套契约执行自定义动作（通知、二次归档、投递到别的系统）；写在哪个块就在哪个量级上执行。MVP 只有内置的 resolve / admission / redact / bundle / stage / upload 六步。

**用 OneDrive 时的运维清单** —— 这些是后端本身的坑，不是本系统的代码问题，但踩到一样疼：

- **关掉文档库的版本历史**（或把版本上限设为 1）。PostProcess 重入是全量覆盖写，开着版本历史会让同一份 bundle 按重试次数存好几遍，**配额消耗与重试次数成正比且毫无提示**。
- **删除只进回收站，不释放配额**。§9.2 的 GC 作业跑完要顺带清空回收站，否则「清理」只是把账挪了个地方。
- **配额是硬上限**，没有超额自动扩容。写满就是全部上传失败，而失败点在 trial 的最后一步——产物跑出来了却传不上去。
- **并发上传会被限速**。`commands.max_attempts` 加上 rclone 自身的退避通常够用；不够就降低 Runner 侧并发上传数，或减少每 trial 的对象数（§9.3 默认只传 2 个就是这个考虑）。
- 推荐用 **SharePoint 文档库**而非个人 OneDrive：配额更大、按站点管权限、不绑定某个人的账号（人一离职空间就出问题）。

---

## 10. API 规格

### 10.1 通用约定

- Base path `/api/v1`，JSON，UTF-8。认证：Bearer token（user / runner 两类）
- 分页 `?limit=50&cursor=...`，响应含 `next_cursor`
- 错误格式：`{"error": {"code": "...", "message": "...", "details": {}}}`

| HTTP | code 示例 |
|------|-----------|
| 400 | INVALID_ARGUMENT, INVALID_STATE_TRANSITION, MATRIX_TOO_LARGE |
| 403 | FORBIDDEN（非创建者的删除/取消） |
| 404 | *_NOT_FOUND |
| 409 | CONFLICT（重复注册 / 重复 confirm / 有引用的 spec 删除） |
| 422 | VALIDATION_FAILED、**CASE_NOT_ADMITTED**（引用了未通过准入的 version，§3.5）、gpu != 0、capability 无解 |

### 10.2 Case

```text
POST   /cases/resolve            取内容 → sha256 → 入 CAS → 解析 task.toml（§3.1）
                                 body 即 experiment 里的一个 case 条目；按 sha256 幂等
POST   /cases/{sha256}/admit     准入校验（§3.5）→ 创建 validation experiment
GET    /cases/{sha256}           详情：来源、钉死后的 ref、task_config、准入结果
GET    /cases?path=&state=       列表（按人读路径 / 状态过滤）
```

### 10.3 LLM Spec

```text
GET    /llm-specs  /{name}       列表 / 详情。**没有写接口**——spec 由提交的
                                 YAML 文档 upsert（§3.3），注册表不接受手工 CRUD
```

### 10.4 Experiment

```text
POST   /experiments               创建 → state=CREATED，返回 preview
GET    /experiments/{id}/preview  展开结果
POST   /experiments/{id}/confirm  确认 → 启动 ExperimentWorkflow
POST   /experiments/{id}/cancel   client.CancelWorkflow，级联所有 child
POST   /experiments/{id}/pause    [P1] SignalWorkflow("pause", true)
POST   /experiments/{id}/resume   [P1]
GET    /experiments  /{id}  /{id}/results
```

**confirm 的执行顺序（重要）**：

```text
1. 校验（case version READY、gpu == 0、capability 静态可满足、规模 ≤ 上限）
2. client.ExecuteWorkflow(ID: "exp-{id}", ReusePolicy: RejectDuplicate)
3. tasks / trials 行由 workflow 内的 server activity 落库
```

> 顺序不能反过来。"先展开 matrix 落库 → 再 ExecuteWorkflow"的两步不是原子的，进程在中间崩溃就留下一个**有全套 PENDING 行但没有任何 workflow 在跑**的 experiment，而 `RejectDuplicate` 帮不上忙（问题是压根没起来），用户重试 confirm 还会撞上 409。
>
> 现在这个顺序天然正确：workflow 是事实源，读模型就应该由 workflow 写，at-least-once + 幂等 upsert。matrix 展开的确定性由输入（experiment config + case version）保证，放进 workflow 里展开同样是确定性的。

### 10.5 Task / Trial

```text
GET    /tasks?experiment_id=&state=&priority=
GET    /tasks/{id}                      含 trial 汇总
POST   /tasks/{id}/cancel               逐个 cancel 其下非终态 trial workflow
POST   /tasks/{id}/retry                对 FAILED 的 trial 重起 workflow
GET    /tasks/{id}/trials
GET    /trials/{id}                     含 failure、metrics、artifacts
GET    /trials/{id}/events              timeline
GET    /trials/{id}/artifacts/{name}    302 → storage.link 的输出（§9.4）
GET    /queue?project=&series=          队列视图（读 placement_waiters）
```

**retry 的 WorkflowID 统一带序号**：`trial-{task_id}-{trial_index}-r{retry_seq}`。不要同时存在两套 retry 语义（child 的 `ALLOW_DUPLICATE_FAILED_ONLY` 与端点侧的"带序号"），否则读模型对账要处理两种 ID 格式。统一走带序号 + `RejectDuplicate`。

### 10.6 Runner（runner token 认证，内部）

```text
POST   /runners/register          首次注册（静态 resources / capabilities）
POST   /runners/{id}/heartbeat    15s，资源快照 + cache state；响应含 drain 指令 / pinned_images
POST   /runners/{id}/events       批量事件上报（清理审计等）
POST   /runners/{id}/drain        管理操作（user token）
POST   /runners/{id}/disable      管理操作（user token）
GET    /runners  /runners/{id}
POST   /internal/llm-credentials/exchange   trial token → LLM 三要素（§8.2）
```

没有 `poll`，也没有 `trials/{tid}/transition` —— 派发与状态上报由 Temporal 承担。

---

## 11. CLI 规格

命令名 `rollout-man`。全局参数 `--server` / `--token`（或 `ROLLOUT_MAN_SERVER/TOKEN`）/ `--output json|table`。

### 11.1 命令树

```text
rollout-man
├── case
│   ├── resolve <file.yaml> | --git ... | --object ... | --local ...
│   │                                       取内容、算 sha256、入 CAS、解析 task.toml
│   ├── admit <sha256|path> [--trials 2]    准入：oracle≥1 / nop≤0（§3.5）
│   │       --all --stale                   [P1] 批量复检过期准入
│   └── list [--path] / get <sha256>
├── experiment
│   ├── create <experiment.yaml> [--yes] [--dry-run]
│   ├── get / list / cancel
│   ├── pause / resume <exp_id>               [P1]
│   └── results <exp_id>                      结果聚合表（§12）
├── task     get / retry / cancel
├── trial
│   ├── get / events
│   ├── logs <trial_id> [--type agent|stdout|stderr|verifier]
│   └── logs -f                               [P2] 需 Runner 增量分段上传
├── queue [--project --series]
├── llm-spec list / get <name>          只读；spec 由提交的 YAML 定义（§3.3）
└── runner   list / get / drain / disable
```

### 11.2 提交文件（完整示例）

一个文件由多个 YAML 文档组成（`---` 分隔），`rollout-man experiment create rollout.yaml` 一起处理：

| kind | 作用 | 通常放哪 |
|---|---|---|
| `Commands` | 外部动作怎么执行（§9.4） | 部署配置（server/runner.yaml）；写在这里是为了自包含 |
| `LLMSpec` | 模型三要素（§3.3），可多个 | 与 experiment 同文件，提交时 upsert |
| `Experiment` | 实验本体，一个文件一个 | — |

```yaml
# ===================== rollout.yaml =====================
---
kind: Commands                          # 通常在部署配置里，此处内联便于自包含
timeout: 30m
max_attempts: 3

source_git:
  script: |
    set -euo pipefail
    git clone --depth 1 --branch "${GIT_REF}" "${GIT_REPO}" "${LOCAL_PATH}"

storage_upload:
  run: ["rclone", "copyto", "--", "{{.LocalPath}}", "onedrive:{{.Key}}"]
storage_download:
  run: ["rclone", "copyto", "--", "onedrive:{{.Key}}", "{{.LocalPath}}"]
storage_link:
  run: ["rclone", "link", "--expire", "24h", "--", "onedrive:{{.Key}}"]

---
kind: LLMSpec
name: opus-prod
base_url: https://api.anthropic.com
model: claude-opus-4-7
api_key_env: ANTHROPIC_API_KEY          # 系统只知道去哪儿找，不持有 key（§3.3）
parameters: {max_tokens: 65536}

---
kind: LLMSpec
name: gpt5-prod
base_url: https://api.openai.com/v1
model: gpt-5
api_key_cmd: ["pass", "show", "eval/openai"]

---
kind: Experiment
name: spring-cve-comparison

# ── Case：直接写在哪，不写注册表 ID（§3.1）────────────────────────────
case_defaults:                          # 每个 case 未指定的字段从这里继承
  source: git                           # git | object | local
  repo: https://github.com/org/eval-cases
  ref: main                             # confirm 时钉死为 commit，preview 显示钉死值
  fetch: source_git                     # 用哪条命令取；省略则按 source 取默认

cases:
  - path: spring/CVE-2026-1234
  - path: apache/CVE-2026-5678
    ref: v2.1                           # 单个覆盖
  - source: object                      # 已打包好的 artifact
    key: cases/legacy/cve-2025-0001.tar.zst
    sha256: 3f9a1c...                   # 给了就跳过 resolve，直接命中 CAS
  - source: local                       # 调试用；每次 confirm 重算哈希
    path: /data/wip/my-case

# ── 矩阵 ──────────────────────────────────────────────────────────
matrix:
  agents:
    - name: claude-code
      llm_spec: opus-prod               # agent 级覆盖 matrix.llm_specs
      parameters: {max_tokens: 65536}
    - name: codex
    - name: oracle                      # builtin：不需要 LLM，不参与笛卡尔积
  llm_specs: [opus-prod, gpt5-prod]     # 引用上面 kind: LLMSpec 的 name
  trials: 10

concurrency: 8                          # 在途上限（排队 + 执行），见 §4.2
priority: normal                        # critical | high | normal | low
queue_timeout: 24h                      # 排队超时 → UNPLACED，见 §5.6

# ── pipeline：三个不同单位的钩子，键名即单位 ─────────────────────────
pipeline:

  per_case:                             # ← 每个 case 跑一次，confirm 时，在 Runner 上
    - step: resolve                     # 取内容 → sha256 → 入 CAS → 解析 task.toml（§3.1）
      on_unchanged: skip                # sha256 已在 CAS 中则跳过（默认）

    - step: admission                   # 准入闸门（§3.5）
      require: admitted                 # admitted（默认）| any（调试，结果打 ⚠ UNADMITTED）
      auto_admit: false
      criteria:
        oracle: {min_reward: 1.0}
        nop:    {max_reward: 0.0}
        trials: 2

  # ─────────────────────────────────────────────────────────────────
  #  per_case 全部通过后，matrix 展开成 N 个 trial。每个 trial 的主链
  #  fetch → prepare → run → verify → collect 是系统内置的，不可配置；
  #  下面 per_trial 的步骤紧接主链，在同一台 Runner 上执行。
  # ─────────────────────────────────────────────────────────────────

  per_trial:                            # ← 每个 trial 跑一次，在该 trial 所在的 Runner 上
    - step: redact                      # 清洗（§6.4）；key 强制，IP 分档
      keys: required
      ips: {traj: true, logs: false}
      extra_patterns: []

    - step: bundle
      format: tar.zst
      include: [traj, logs, result]

    - step: stage                       # 暂存本机，等 per_experiment 成批传（§6.5）
      max_pending: 20Gi                 # 每 Runner 暂存上限，超了就地提前传一次
      on_cancel: upload                 # upload（默认）| discard
    # 想逐 trial 就传的话，把上面这步换成 upload 即可：
    #   - step: upload
    #     using: storage_upload
    #     dest: "evals/{{.ExperimentName}}/{{.CasePath}}/{{.TrialID}}/"
    #     objects: [bundle, result]

  per_experiment:                       # ← 整个 experiment 跑一次，全部 trial 到终态后
    - step: upload                      # 各 Runner 把自己暂存的产物成批传走
      using: storage_upload
      dest: "evals/{{.ExperimentName}}/"
      objects: [bundle, result]

    - step: report                      # [P1] 聚合结果文件
      formats: [json, csv]
      dest: "evals/{{.ExperimentName}}/report/"

    - step: deploy                      # [P1] 任意外部投递：发布、通知、进下游流水线
      run: ["./scripts/publish.sh", "{{.ExperimentID}}", "{{.ReportURL}}"]
      on_failure: warn                  # warn（默认，只记 event）| fail（experiment 标失败）

# ── 其余 ──────────────────────────────────────────────────────────
retry_policy:
  max_total_attempts: 3                 # 含首次
  retry_on: [DOCKER_ERROR, NETWORK_ERROR, AGENT_OOM]

overrides:                              # [P1] Experiment > task.toml > 默认
  resources: {cpu: 8, memory: 32Gi, disk: 100Gi, gpu: 0}
  timeouts: {agent: 30m, verifier: 10m, build: 20m, total: 45m}

scheduling:                             # [P1]
  group_by: [project, series]
  affinity: {enabled: false, max_wait: 10m}
```

**pipeline 的单位就是键名**，三个块跑在三个不同的量级上：

```text
experiment                                              ← 提交一次
│
├── per_case          × N_cases        confirm 时，在 Runner 上
│     resolve → admission                               全部通过才入队
│
├── trial             × N_trials       主链，系统内置不可配置
│     fetch → prepare → run → verify → collect
│   └── per_trial     × N_trials       紧接主链，同一台 Runner
│         redact → bundle → (stage | upload)
│   └── cleanup
│
└── per_experiment    × 1              全部 trial 到终态后
      upload(暂存的) → report → deploy
```

| 块 | 跑几次 | 在哪跑 | 失败后果 |
|---|---|---|---|
| `per_case` | 每个 case 一次 | Runner（resolve 要取几 GB，不能走 Server） | 阻断 confirm，experiment 不入队 |
| `per_trial` | 每个 trial 一次 | 该 trial 所在的 Runner | 按步骤不同：`redact` 阻断、`bundle` 降级、`upload`/`stage` 重试（§6.4） |
| `per_experiment` | 一次 | `upload` 在各 Runner；`report` / `deploy` 在 Server | `upload` 失败 → experiment 标 `ARTIFACTS_INCOMPLETE`；其余按 `on_failure` |

**上传逐 trial 还是成批，由 `upload` 写在哪个块决定**，不需要额外的开关：

| 想要 | `per_trial` 写 | `per_experiment` 写 | 外部命令调用次数 |
|---|---|---|---|
| 逐 trial 传 | `upload` | — | O(trials × objects) |
| 成批传 | `stage` | `upload` | O(runners) |

语义完全由位置表达——一个步骤在哪个块里，就在那个量级上执行，没有"写在这里但其实在那里跑"的情况（§6.5）。

**模板变量**：`{{.ExperimentID}}` `{{.ExperimentName}}` `{{.CasePath}}` `{{.TrialID}}` `{{.Attempt}}` `{{.Key}}` `{{.LocalPath}}`；finalize 另有 `{{.ReportURL}}` `{{.ReportPath}}`。script 形式用同名大写下划线环境变量（`EXPERIMENT_ID` 等）。

### 11.3 Preview

```text
$ rollout-man experiment create experiment.yaml

Experiment Preview
  Cases:    spring/CVE-2026-1234@a1b2c3d   apache/CVE-2026-5678@v2.1
  Agents:   claude-code, codex        LLM Specs: opus-prod, gpt5-prod
  Trials:   5 each → Total 20         在途上限: 8    Priority: NORMAL
  能力要求: docker, arch=amd64        GPU: 0
  排队超时: 24h
  per_case:       resolve   — 2 个 case，1 个命中 CAS，1 个需取
                              ref main → 钉死 a1b2c3d
                  admission — 2/2 已 ADMITTED ✓
  per_trial:      redact(keys=required, ips: traj✓ logs✗) → bundle(tar.zst) → stage
  per_experiment: upload(evals/spring-cve-comparison/) → report → deploy

Confirm? [y/N] y
→ experiment exp_182 queued (4 tasks, 20 trials)
```

**preview 只展示确定性信息** —— trial 数、并发、优先级、涉及的 agent/spec、能力要求、各项阈值。

> 不要显示 `Estimated: CPU 160 core-hours, Disk 320 GB` 这类数字：冷启动时只能用 `默认 profile × trials` 算，与实际可能差数倍，但界面上看起来像精确预估。**看起来精确、实际是猜的数字，比不显示更糟。**
>
> **[P1]** 加时间预估，且必须标注样本量：`~4.2h（基于 Spring series 近 30 次 trial 中位数）`；无样本时显示 `未知`。预计超过 24h 时给出警告 —— 这比 `MATRIX_TOO_LARGE` 上限有用得多，因为它防的是"提交了才发现要等一个月"。

---

## 12. 结果分析

`GET /experiments/{id}/results` / `rollout-man experiment results`：

**本系统不定义"什么算成功"。** 它负责把 rollout 跑完、把每个 trial 的原始 reward 和失败归因如实记下来；至于 reward 到多少算通过，是分析阶段的判断，随分析目的而变，不该被冻结进 experiment 配置——同一批 rollout 换个问题就该换个切法。因此结果表默认呈现**分布**，不呈现"达标率"。

```text
Experiment #182

Agent / Model      完成  reward: 均值 中位  P25   P75   Agent-Fail  Agent-TO  Env-TO  Infra  Verifier  Unplaced  Cancel
Claude / Opus        88          0.71  0.83  0.42  0.95      14         4        3      12       6         0        0
Codex   / GPT-5      92          0.76  0.86  0.51  0.97      12         3        2       8       5         0        2

  reward 直方图（Claude / Opus）  0.0 ████▏12   0.2 ██▎7   0.4 ███▍10  0.6 █████▏15  0.8 ████████████▏44
```

**列的定义**：

| 列 | 含义 |
|---|---|
| 完成 | `COMPLETED` 数（verifier 正常跑完并产出 reward，**无论高低**） |
| reward 分布 | 均值 / 中位 / P25 / P75 + 直方图，仅统计 `COMPLETED` |
| Agent-Fail | 崩溃 / 非零退出 / OOM / 输出超限 |
| **Agent-TO** | `AGENT_TIMEOUT` —— agent 自己磨蹭到超时 |
| **Env-TO** | `ENVIRONMENT_TIMEOUT` —— 环境/构建超时 |
| Infra / Verifier / Cancel / Unplaced | 见 §7.1 |

**要看通过率时，阈值是查询参数，不是配置**：

```text
rollout-man experiment results exp_182 --pass-at 0.8
  → 额外输出一列 "通过率 (reward≥0.8)"，同一批数据可以换任意阈值反复看
```

对应 `GET /experiments/{id}/results?pass_at=0.8`。不传就不显示这一列。

**通过率的分母口径**（传了 `--pass-at` 时）：

```text
分母 = 完成 + Agent-Fail + Agent-TO
排除 = Env-TO / Infra / Verifier / Cancelled / Unplaced
```

> Timeout 不能混成一列再整列排除，那会**系统性地高估所有 agent 的通过率**：`AGENT_TIMEOUT`（agent 自己磨蹭到超时）是实打实的能力问题，必须计入分母；只有 `ENVIRONMENT_TIMEOUT` 才该排除。能拆开的前提是 §7.2 的映射表存在。
>
> 注意这里和 §3.5 准入判据的区别：准入的 `oracle >= 1.0` / `nop <= 0.0` 是**必须写死**的门限，因为那两个 builtin 的正确答案是已知的（参考解必然满分、空操作必然零分），判据是二值的、与分析目的无关。被测 agent 的 reward 没有这种性质。

附带指标：平均时长、资源峰值分布、retry 率、queue wait P95。`result.json` 保留每 trial 原始 reward 供离线分析。

**准入状态在结果表里必须可见**：引用了 `allow_unadmitted` case 的行前缀 `⚠`，并在表尾注明 —— 未准入 case 的分数不具备可比性，混进聚合会污染所有对比结论。

**oracle / nop 单独成行**，不与 LLM Agent 混排：

- oracle 未得满分 → 标记该 Case 为 `CASE_SUSPECT`（环境 / 参考解 / verifier 有问题）
- nop 得分非 0 → 标记 Verifier 为 `VERIFIER_SUSPECT`
- **不把 oracle 失败塞进 `ENVIRONMENT` failure category**，那会污染 ENVIRONMENT 的统计口径 —— oracle 失败的三种成因（环境坏 / 参考解失效 / verifier bug）恰恰需要人来判断，归进一个自动化的 category 反而掩盖了它
- 含 SUSPECT Case 的结果在聚合表中打星号提示

**[P2]** 导出 CSV/JSONL 批量作业、跨 experiment 对比。

---

# 第五部分 · 工程与运维

> 用什么写、怎么组织、怎么部署、出问题怎么发现。

## 13. 技术选型与工程实践

| 组件 | 选型 | 说明 |
|------|------|------|
| Workflow Engine | **Temporal（self-hosted，`go.temporal.io/sdk`）** | persistence 复用现有 PostgreSQL 实例的独立 database（`temporal` + `temporal_visibility`） |
| Backend（API Server + placement + workflow worker） | Go（chi/gin + pgx / sqlc） | 单二进制 |
| Runner Agent | Go（单静态二进制，systemd 托管） | 交叉编译分发，无运行时依赖；docker 操作用官方 client |
| CLI | Go（cobra），与 Runner 共享 client SDK | 单二进制 |
| DB | PostgreSQL | 读模型 + 注册表 + placement 记账 |
| 对象存储 | **命令行适配器**（默认 `rclone` → OneDrive） | 不内置任何存储 SDK，换后端只改配置，见 §9.4 |
| Execution | Harbor | activity 内通过子进程 / SDK 调用 |
| Container | Docker（显式绑定 `DOCKER_HOST`） | |
| API 契约 | OpenAPI 3 单一来源 | Go server stub 与 TS client 均由 spec 生成 |
| Frontend | TypeScript + React | [P2] |
| Observability | Prometheus（SDK 自带 + 业务指标） | OpenTelemetry 后续 |

选 Go 的理由：Server / Runner / CLI 全部单二进制交付，Runner 机器零运行时依赖；与 Docker 生态契合；Temporal Go SDK 是最成熟的一个。

### 13.1 代码组织

```text
rollout-man/
├── cmd/{server,runner,rollout-man}/
├── internal/
│   ├── workflows/        experiment.go  trial.go        （纯确定性，禁 I/O）
│   ├── activities/
│   │   ├── server/       placement.go  readmodel.go  llmspec.go  perexperiment.go
│   │   └── runner/       fetch.go  prepare.go  run.go  verify.go  collect.go  cleanup.go
│   ├── runner/
│   │   ├── inuse/        in-use registry（§6.8）
│   │   ├── housekeeper/  分级清理
│   │   └── cachescan/    [P1]
│   ├── placement/        matcher.go  scoring.go  reservations.go
│   ├── failurecode/      taxonomy.go  mapping.go      （§7.2，带完整单测）
│   ├── storage/         命令行适配器（模板渲染、执行、sha256 校验，§9.4）
│   ├── sanitizer/
│   └── harbor/  store/  registry/  api/  cli/
└── deploy/               docker-compose.dev.yaml  helm/
```

### 13.2 测试

- `testsuite.WorkflowTestSuite` 单测 TrialWorkflow 的**全部失败路径**（mock activities，秒级跑完 timeout / retry / cancel 分支）
- `failurecode` 的映射表必须有覆盖每种 `TimeoutType` 的单测 —— §7.2 是唯一一处"错了不会报错、只会让数据错"的逻辑
- 集成测试用 `temporal server start-dev` + 假 Runner activity；§15.3 各 Phase 的验收标准直接写成集成测试用例
- `worker.WorkflowReplayer` 回放生产抽样 history（每晚 CI）[P1]

---

## 14. 可观测性与运维

### 14.1 部署形态

| 组件 | 形态 |
|---|---|
| Temporal Server | **自托管**。dev：`temporal server start-dev`；**MVP 起即用 docker-compose + PG persistence**（见下） |
| namespace | `rollout-man`，retention 30d。**不做 archival**（Temporal 的归档后端不支持 OneDrive），30 天后以 PG 读模型为准（§9.2） |
| API Server | [MVP] 单副本；[P1] 多副本（workflow worker 天然多活，matcher advisory lock 单活） |
| Runner Agent | 单二进制 + systemd；新增出方向 7233 端口要求 |

**`start-dev` 不能作为 MVP 部署形态**：它用内置 SQLite，与 docker-compose + PG persistence 在 **visibility 能力上不等价**（自定义 search attribute 的支持、`ListWorkflow` 的过滤能力），而 §5.9 的 reconciler 和 §14.2 的 blocked_reason 指标都建立在这个能力上。`start-dev` 只用于单元/集成测试。

### 14.2 指标

SDK 自带（`client.Options.MetricsHandler`）：schedule-to-start 延迟（= 原 queue wait time）、activity 失败率、heartbeat 超时数、workflow task 延迟。

业务指标：

- placement wait P95（按 priority 分桶）
- `blocked_reason` 分布
- per-llm-spec 在途数 [P1]
- **image pull/build 实际耗时 与 trial 总耗时**（[MVP] 就要埋 —— 这是 [P1] 决定 affinity 权重的唯一数据来源，见 §5.7）
- Sanitizer 精确匹配层命中次数（按 agent 分组，[P1] 作为泄漏信号）
- 存储命令的失败率与耗时（按动作分）；配额监控见 §9.4 的可选 `stat` 命令

Temporal Web UI 直接提供 per-trial timeline，[P2] 自建 UI 的范围相应缩小（只做结果对比与队列视图）。

### 14.3 残余风险（诚实清单）

1. **Temporal server 长停机后的计时误判**：heartbeat timeout 按 wall clock 判定，若 Temporal 停机超过 `HeartbeatTimeout`，恢复瞬间可能把仍在跑的 `RunAgent` 判失败并重新投递 → 双跑窗口。
   缓解：(a) `RunAgent` 同机重投 + 重入对账（§6.2），同机场景下接管而非新起容器，接近零成本；(b) 换机场景靠一次性 trial token —— 新 attempt 签发时吊销旧 token，旧容器的 LLM 调用被 Server 拒绝，烧钱止血（§8.3）。
2. **运维面扩大**：多一个 Temporal server 要养。对冲：它换掉的是最难写对的自研代码（lease / orphan / 选主 / 级联 cancel），且本场景吞吐离 Temporal 的容量红线差几个数量级，几乎不需要调优。
3. **history 限制**：已用 ContinueAsNew（exp / trial 两级）+ 规模上限 + placement 退避约束 event 增速；[P1] shard 化进一步解耦。
4. **团队学习成本**：determinism、versioning 是新概念。对冲：workflow 代码集中在两个文件，纪律靠 lint 与 replay test 机械化，不靠人。
5. **PG 同时承载 app db 与 temporal db**：本场景负载很低，但两者的备份/恢复语义不同 —— PG 单点故障会同时打掉事实源与读模型。MVP 接受；规模上去后 temporal db 独立实例。

---

# 第六部分 · 交付计划

> 什么进 MVP、什么押后、按什么顺序做、每步怎么验收。

## 15. 优先级分级与开发计划

### 15.0 第一性原理下的 MVP（已实现）

本文其余部分描述的是**目标架构**。真正落地的 MVP 比它小得多，因为把需求收敛成四个动词之后，很多东西被证明是在解决 MVP 还没有的问题：

> **编排任务 · 跑任务 · 汇报状态 · ship 产物**

拿这四条去量，删掉的是：

| 删掉 | 原因 |
|---|---|
| **Temporal**（编排层、activity 包装、错误映射，约 1160 行） | 它替代的是 lease / orphan 判定 / 选主 / 级联取消，而这些的前提是**多 Runner placement** —— MVP 只有一台机器，那套机制根本不存在。它保证的"崩溃后从断点续跑"对 eval 也不是刚需：**重跑一个 trial 是正确的，不是事故**。确定性来自「同样的输入展开出同样的 trial 集合」，不来自 durable execution |
| **PostgreSQL 读模型**（约 270 行 + 一个要运维的进程） | 读模型的职责是回答"发生了什么"。500 trials 的规模下，一个 append-only 的 `results.jsonl` 就是读模型，还顺带充当续跑的检查点 |
| **CAS 存储层 / bundle / stage / 批量上传 / flush** | 这套复杂度是为「按次限流的远端后端 + 多 Runner 归集」设计的。单机没有这两个问题：产物就放在 run 目录里，ship 是一条命令 |
| **placement / 队列 / 优先级 / aging / affinity** | 一台机器上没有"在哪里跑"这个问题 |
| **Failure Taxonomy 的 5 类 18 码** | 收敛为 4 类 6 码。它存在的唯一理由是把"没测出来"挡在 agent 的分母外面，比这更细的都是报表细节 |

保留的是：**内容哈希即版本**（§3.1）、**准入闸门**（§3.5）、**清洗分档**（§6.4 / §8.5）、**pipeline 三个作用域**（§11.2）。这四条各自都是"省掉之后结果不可信或不可逆"的东西。

结果：3573 → 2118 行，依赖从 5 个减到 2 个（yaml + toml），部署形态从「Temporal + PostgreSQL + worker」变成一个二进制。实现与用法见 `README.md`。

**什么时候把删掉的加回来**，判据也是第一性的：

- **placement 落地时加回 Temporal** —— 那时才真的出现 lease / orphan / 选主，才轮到它替代最难写对的代码。在此之前它是在解决一个不存在的问题
- **多人共享结果时加回数据库** —— 一个人看 `results.jsonl` 够用，十个人并发查不够
- **产物要发给团队外时加回 bundle / 批量上传** —— 那时才会撞上远端后端的按次限流

### 15.0.1 第二波：先补能出正确数字的路径

瘦身之后从 smoke test 找不足，找到的三条都不在编排层：

| 缺口 | 事实 | 处置 |
|---|---|---|
| **docker executor 从未被执行过** | agent 与 verifier 是两次 `docker run --rm`，中间容器连同 `/app/crash.bin` 一起被丢掉；reward 又从宿主机的 `workdir/state/reward.txt` 读，而真实 case 写的是容器内的 `/logs/verifier/reward.txt`。两个 bug 叠加的表现是**每个 agent 都得 0 分**，而且看不出异常 | 已修：**一个 trial 一个容器**，agent 与 verifier 都用 `docker exec` 进同一个容器；分数从容器内的合同路径读出 |
| **六个失败码在测试里出现次数为 0**，`max_attempts` 的重试分支从未执行 | 分类法唯一的作用是护住分母，而这件事没有任何测试证明 | 已补：五个合成 case 各制造一种失败，外加一个"第一次失败第二次成功"的 case；断言直接落在 `1/3 = 33%` 这个分母上 |
| **超时只结束等待，不结束 agent** | `CommandContext` 只杀 `unshare` 自己，子进程还在写管道，`CombinedOutput` 于是一直等到 agent 自然结束 —— 2 秒的 agent timeout 实际花了 30 秒 | 已修：local 走 `Setpgid` + `kill(-pgid)`；docker 走容器内的 `timeout(1)`（杀宿主机上的 `docker exec` 客户端不会动容器里的进程） |

**为什么这一波不是 Temporal。** Temporal 能保证的是"崩溃后从断点续跑"，而现在的断点粒度是**整个 trial**：worker 一死，前台起的容器跟着没了，续跑只能从头再跑这个 trial。真实 case 的 `agent_timeout=1h`、`build_timeout=90min`，代价是最多一个半小时。要把这个代价降下来，需要的是**容器重连**。容器现在已经是 `docker run -d` 起的（一个 trial 一个容器的直接后果），剩下的只是把容器名记进检查点、重启后先认领再决定跑不跑 —— 那是 executor 的能力，几十行，和用什么编排器无关。在 detach 之前引入 Temporal，是给一条还不能续跑的执行链套一个能续跑的编排器。

§15.0 里"placement 落地时加回 Temporal"的判据不变。下一步是 **detach + 重连**，之后才轮到它。

这一波的代价：Go 代码 +125 行，依赖不变（仍是 yaml + toml），smoke test 从 28 条断言涨到 46 条。

### 15.1 分级总表（目标架构）

> 下表是**目标架构**的分级，不是已实现的 MVP。已实现范围见 §15.0。

| 模块 | **MVP** | **P1**（MVP 后 1–2 迭代） | **P2**（有明确需求再做） |
|---|---|---|---|
| **Case Registry** | CAS（sha256）；`--git` / `--s3+sha256` 注册；CLI 直传 + 服务端校验；`task.toml` 解析 + 状态机 | 完整分块上传协议（断点续传、并发分片） | Case 预热（dispatch 前预下发 artifact / 预拉 image） |
| **前置准入** | oracle≥1 / nop≤0 判据；`case admit` 走正式执行链；正式 experiment 强制 `ADMITTED`；`allow_unadmitted` 逃生口 + 结果打标 | 定期复检（`revalidate_after`）与批量复检；`CASE_SUSPECT` / `VERIFIER_SUSPECT` 归因 | 准入判据按 series 差异化；准入结果趋势看板 |
| **后置清理** | 独立 `PostProcess` activity；key 强制脱敏 + IP 分档；`bundle.tar.zst` 打包；清洗失败阻断上传；`stage` + `per_experiment.upload` 支持成批上传 | 清洗命中 metrics 进结果分析；`extra_patterns`；bundle 分卷 | 产物二次加工流水线 |
| **Experiment** | 多文档 YAML（Commands / LLMSpec / Experiment）；matrix 展开；preview + confirm（幂等）；cancel；**上限 500 trials**；`queue_timeout`；`pipeline` 三个块 | pause / resume；`overrides` 三级优先级；**shard 化** + 上限 2000 | 上限 10000；Experiment 模板 / 复用 |
| **LLM Spec** | 由提交的 YAML 文档 upsert；`api_key_env` / `api_key_cmd`（系统不持有 key） | `max_concurrent` 限流 | 按 spec 的配额与计费归集 |
| **Agent Registry** | `type/requires_llm` + 展开规则；oracle / nop 两个 builtin | `case validate` 快捷命令；`CASE_SUSPECT` 标记 | 自定义 agent 打包分发 |
| **执行链** | fetch → prepare → run → verify → collect → **postprocess** → cleanup 全链路；每步幂等 + 重入对账；**全部配 `ScheduleToStart`**；`RunAgent` 同机重投 | 步骤级 metrics 细化 | — |
| **Failure** | Taxonomy 固化；**Temporal 错误 → code 映射（含超时类型）**；`TEST_FAILED` 不进 taxonomy | 精细 code 归因；归因准确率回归用例 | 失败聚类 / 自动根因提示 |
| **Retry** | Activity 级同机重试 + workflow 级换机重试；`avoid_same_runner`；`max_total_attempts` 统一语义 | Experiment 级覆盖；按 code 差异化 backoff | 自适应 retry 预算 |
| **Placement** | 硬约束过滤；**资源预留记账**；**授予后 Signal 推送**；**排队超时 + waiter 生命周期 + reaper**；同分取负载最低 | priority + aging；affinity 评分（**先采数据**）；cache state 上报；group scheduling；per-llm-spec 限流；matcher 选主 | affinity defer；大任务 backfill；GPU 型号匹配 |
| **Runner** | 注册；REST 心跳；activity worker（`DisableWorkflowWorker`）；**基础 drain（按 trial 收敛）**；disable | EMERGENCY 自动 drain；`pinned_images`；DRAINING 状态机细化 | 自动扩缩容；Runner 分组 / 标签路由 |
| **Housekeeper** | 磁盘阈值监控 + `degraded` 上报停派；**NEVER DELETE 清单 + in-use registry + label 限定**；dangling image / stopped container / workdir retention（本地 manifest 驱动）；**Docker context 校验** | 四档分级；CAS 本地缓存 LRU + 容量上限；清理审计上报 | 跨 Runner 缓存协同 / 全局 GC |
| **Sanitizer** | 精确匹配层（含 base64/URL-encoded 变体）+ 模式匹配层（**含 presigned 签名参数**）；流式按行 + 跨行窗口；**IP 分档脱敏**（§6.4） | 命中 metrics；泄漏信号进结果分析；`extra_patterns` | 语义级脱敏（人名/路径/主机名） |
| **安全** | LLM key 不作为 activity 入参（Runner 用 runner token 取）；对象访问不放预授权 URL 进 history；外部存储凭证不由系统管理；`created_by` + 删除类操作限制；信任边界文档化 | **一次性 trial token + 吊销**；加密 Data Converter；runner token 轮换；角色细化 | Vault / 外部 secret manager |
| **读模型 / API** | Task/Trial/Queue/Experiment 读接口；artifact 短时效 URL 下载；events 表（业务语义）；结果表 reward 分布 | visibility reconciler（5min）；`--pass-at` 查询式阈值；结果聚合完整指标 | 导出 CSV/JSONL 批量作业；跨 experiment 对比 |
| **CLI** | case / experiment / task / trial / queue / llm-spec / runner 基本子命令；`logs`（拉已上传日志） | `-o json` 全覆盖；`results` 聚合表；时间预估（带样本量） | `logs -f` 实时跟随 |
| **UI** | 无（用 Temporal Web UI 看 timeline） | 无 | Web UI：Queue / Runner / Experiment dashboard、结果对比 |
| **外部存储** | 命令行适配器（upload/download/link/delete 四条模板）；客户端 sha256；每 attempt 只传 bundle + result | **GC 作业**（按 retention 删除 + 清空回收站）；单文件上传；可选 `stat` 配额检查 | `exec` 自定义步骤；引用计数删 CAS；experiment 删除级联 |
| **HA / 运维** | Temporal docker-compose + PG persistence；API Server 单副本；Prometheus 基础指标 | API Server 多副本 + matcher 选主；history archival；replay 回归 CI | 多 Region、多租户、Billing、K8s 调度（**明确非目标**） |

**两处最容易分错的**：

- **affinity 属于 P1，不属于 MVP**：它是性能优化，而 MVP 阶段没有数据支撑权重（§5.7）
- **资源预留记账属于 MVP，不属于后期优化**：它是 placement 正确性的一部分，不记账就会基于陈旧心跳重复授予导致 OOM / 盘满（§5.3）

### 15.2 Phase 0：验证与决策（1 周，MVP 前置）

产出一个 walking skeleton：hello-trial workflow 打通 server / runner 两级 worker。**同时验证以下事项，全部通过则锁死 Temporal 选型并删除任何回退讨论。**

**去留判据（可证伪，任一不过则重新评审选型）：**

| # | 判据 |
|---|---|
| 1 | Temporal 能在现有 PostgreSQL 上以 docker-compose 形态部署起来并稳定运行 |
| 2 | Runner 侧出方向 gRPC 7233 被网络策略放行 |
| 3 | 团队能读懂并遵守 determinism 规则（以一次 code review + lint 跑通为准） |

**设计假设验证（不过不改选型，但要改设计）：**

| # | 验证项 | 不成立的后果 |
|---|---|---|
| 4 | 自定义 search attribute 注册 + `ListWorkflow` 按 SA 过滤（**在生产形态部署上验**） | §5.9 reconciler 与 §14.2 指标全部落空 |
| 5 | `ParentClosePolicy` 与父 workflow ContinueAsNew 的交互 | [P1] shard 化方案要重设计（当前"drain 后再 CAN"对此免疫） |
| 6 | `workflow.NewSemaphore` 在目标 SDK 版本可用 | §4.2 要换写法 |
| 7 | `ScheduleToStartTimeout` 超时是否走 activity RetryPolicy | 若会重试，则"穿透到外层换机"不成立（§7.4） |
| 8 | activity cancel → heartbeat 通道 → `docker kill` 的端到端真实时延 | 用户 cancel 的体感、EMERGENCY 自保的有效性 |
| 9 | 一个 hello-trial 跑完后父 / 子 history 的实际事件数 | 500 / 2000 trials 的上限估算 |
| 10 | **存储命令**：在 Runner 上跑通配好的 upload/download/link/delete；3 台并发上传 100 个 bundle 无失败 | 决定 `max_attempts` 与每 trial 对象数上限（§9.4） |
| 11 | **OneDrive 侧**：确认版本历史已关、回收站可编程清空、rclone 能穿过企业代理 | 任一不成立就换后端——命令行适配器让这个切换只是改配置（§9.4） |

都是小实验，一周内做得完，且每一条都可能改变后续设计。

### 15.3 Phase 计划与验收标准

| Phase | 内容 | 分级 | 验收标准 |
|---|---|---|---|
| **0** | Temporal 部署 + walking skeleton + §15.2 验证 | MVP 前置 | 9 项验证有明确结论；判据 1–3 通过 |
| **1** Execution Core | Case Registry、**准入**、Experiment、Trial、执行链（含 **PostProcess**）、单 Runner、Harbor 集成 | MVP | 新 Case → `case admit` 通过 → 一条 YAML → 单 Runner 跑完 N trials → CLI 可查 reward，产物已清洗打包 |
| **2** Reliability | Failure Taxonomy + **错误映射**、两级 retry、artifact、Sanitizer 全量规则、凭证链路 | MVP | 见下方四条 |
| **3** Multi Runner | Runner 注册、心跳、placement 硬约束 + 记账 + 排队超时、基础 drain、Housekeeper 基础清理 | MVP（**收尾即 MVP 可用**） | 3 Runner 并发消费；停 1 台不丢任务；drain 平滑下线；磁盘打满不误删 in-use 资源 |
| **4** Optimization | affinity、cache 上报、group scheduling、aging、pause/resume、EMERGENCY drain、shard 化 | P1 | 同 series 任务集中到有 cache 的 Runner；无任务等待超过 max_wait |
| **5** Resource Mgmt | 分级清理、CAS LRU、pinned_images、reconciler、多副本 HA | P1 | 读模型漂移 5min 内自愈；CAS 缓存不超上限 |
| **6** Productization | Web UI、结果对比、实时日志 | P2 | — |

**Phase 2 的验收必须拆成四条**（只验一条"kill runner 能重排"覆盖不了关键路径）：

1. `kill -9` Runner Agent → trial 自动换机重跑，归因 **`RUNNER_UNAVAILABLE`**（**不是 `AGENT_TIMEOUT`**）
   —— 这一条同时验证 §7.2 的错误映射，是唯一能发现"基础设施故障被记成 agent 失败"的测试
2. `systemctl restart` Runner Agent → 在跑的 trial **不重跑**，同一个容器被接管，最终 reward 正常产出
   —— 这一条验证 §6.2 的重入接管。它极容易被漏掉：表面上与第 1 条像是同一件事，实际走的是完全不同的路径，且配错时不会有任何报错
3. experiment cancel → 5 秒内 PG 里所有 trial 变为 CANCELLED（不依赖 reconciler）
   —— 这一条验证 §4.3 的 disconnected context
4. 构造一个把 api_key 原样写进 traj 与 stdout 的假 agent → 跑完后从对象存储取回 `bundle.tar.zst` 与各单文件，**全文检索 key 明文零命中**；同时 `agent.log` 里的 IP 仍然可读
   —— 这一条同时验证 §6.4 的分档清洗与"清洗失败阻断上传"（把 Sanitizer 规则改坏一次，确认 trial 变 `POSTPROCESS_FAILED` 而不是照样上传）

**Phase 1 的验收补一条**：故意把某个 Case 的 verifier 改成恒返回满分 → `case admit` 必须因 nop 超标而 `REJECTED`，且该 version 无法被正式 experiment 引用（422 `CASE_NOT_ADMITTED`）—— 验证 §3.5，这是准入唯一有意义的验收方式（只验"好 case 能通过"证明不了任何事）。

**Phase 3 的验收补一条**：提交一个要求不存在的 capability 的 experiment → 在 preview/confirm 阶段被拒绝，或（若无法静态判定）在 `queue_timeout` 后终态 `UNPLACED`，experiment 正常结束 —— 验证 §5.6，防止"永远不结束的 experiment"。

---

## 16. 开放问题

| # | 问题 | 当前倾向 |
|---|---|---|
| 1 | LLM Spec 的 secret 后端：加密环境文件 vs KMS vs Vault | MVP 用加密文件 + KMS 解密；Vault 进 P2。下发链路已定（§8.2），待定的只是 Server 侧静态存储 |
| 1b | 准入判据是否需要按 series 差异化（有些 Case 天然拿不到满分） | 先全局 `oracle_min = 1.0`；出现反例再按 series 覆盖，**不要一上来就放松到 0.9**——放松后准入就失去意义 |
| 1c | `bundle` 的格式与是否分卷（超大 traj） | 先 `tar + zstd` 单卷；单 attempt 产物超过 2GB 时再议 |
| 1d | OneDrive 用哪个身份的空间：个人 OneDrive vs SharePoint 文档库 | 倾向 SharePoint 文档库——配额更大、权限按站点管，且不绑定某个人的账号 |
| 1e | 配额写满时的降级策略 | 至少要有「停止新 dispatch + 告警」，而不是让 trial 跑完在最后一步失败。需要 §9.4 的可选 `stat` 命令 |
| 3 | shard 化的 shard 粒度（按 task 还是固定条数） | 倾向固定条数（如每 shard 200 trials），与 matrix 结构解耦 |
| 4 | `queue_timeout` 默认 24h 是否合适 | 需要观察真实排队分布；MVP 先给 24h 并在超过 50% 时告警 |
| 5 | Runner 机器的准入与加固标准 | §8.1 定了信任边界，具体加固清单待运维补 |

---

## 附录 A：设计原则（一句话版）

> Case 是 Artifact，**准入过的** Case 才配进 Experiment；Experiment 是声明，Task 是分组，Trial 是唯一的执行单位；
> **Temporal** 负责可靠地把每一步按顺序做完（状态机、队列、lease、心跳、重试、级联取消全部退役）；
> **Placement** 负责谁先做、在哪台机器做（资源、优先级、affinity 的智能全部保留）；
> **Runner activity** 负责真正做（幂等 + 重入对账 + 清洗打包）；
> **Housekeeper** 保证 Runner 能持续运行；
> PostgreSQL 从事实源退位为读模型，事实源是 Temporal 的 event history —— 但 30 天之后，PG 是唯一还在的那个。
